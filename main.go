// Command marshal keeps a fleet of repositories secure by sending agents to
// do the maintenance, gating what they change behind policy, and earning them
// autonomy as their work holds up.
//
// This is the first cut: run one job against one repo from the command line,
// and look at what happened. The daemon and the interface build on these.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"github.com/Sousf/marshal/internal/api"
	"github.com/Sousf/marshal/internal/chat"
	"github.com/Sousf/marshal/internal/config"
	"github.com/Sousf/marshal/internal/daemon"
	"github.com/Sousf/marshal/internal/ghapp"
	"github.com/Sousf/marshal/internal/github"
	"github.com/Sousf/marshal/internal/jobs"
	"github.com/Sousf/marshal/internal/notify"
	"github.com/Sousf/marshal/internal/remote"
	"github.com/Sousf/marshal/internal/run"
	"github.com/Sousf/marshal/internal/runner"
	"github.com/Sousf/marshal/internal/store"
	"github.com/Sousf/marshal/internal/ui"
	"github.com/Sousf/marshal/internal/verifier"
)

// version is stamped at build time with -X main.version.
var version = "dev"

const usage = `marshal — Maintains, Audits, Repairs, Secures, Hardens, And Learns

  marshal                         the interface (same as marshal tui)
  marshal chat [--new]            talk to MARSHAL: Claude Code with MARSHAL's context and rules (c in the interface)
  marshal serve [--tick 1m]       the daemon: schedules jobs, syncs PRs, merges where trusted
      --detach                      run in the background; log to <state>/serve.log
      --no-schedule                 never start jobs on its own; triggers, sync and merges still work
  marshal stop                    stop a detached daemon
  marshal logs [-f]               the detached daemon's log
  marshal tui                     attach to the daemon, or read the database if it is down

  marshal init                    write a starter fleet file
  marshal check                   validate the fleet file
  marshal repos                   list repositories and their jobs
  marshal jobs                    list jobs and their effective settings
  marshal prompt <job> <repo>     print the brief a run would send, without running

  marshal run <job> <repo>        run a job here, in the foreground, and open a PR
      --no-pr                       commit on a local branch only, open nothing
      --keep                        leave the work tree on disk
      --force                       run the agent even if the job's probe finds nothing to do
      --allow-red                   open the PR even if its own checks fail, as long as nothing regressed
  marshal trigger <job> <repo>    ask the daemon to run a job in the background (--force skips the probe)
  marshal fix <run-id>            ask the daemon to push a fix to that proposal's PR now, outside the cap
  marshal task "<instruction>"    run a free-form task via the daemon; --repo <name> ends in a PR, without it a report
  marshal verify <run-id>         run the verifier on a proposal here, in the foreground
      --comment                     also post the verdict on the PR
  marshal runs [repo]             recent runs
  marshal show <run-id>           one run's record and event log
  marshal trust                   where each repo and class sits on the ladder
  marshal status                  is the daemon up, is autonomy on, what is running
  marshal autonomy on|off         the kill switch: off stops every auto-merge
  marshal notify test             send a test notification through the configured channels
  marshal digest                  print the daily summary
  marshal app create [--name N]   create and install the MARSHAL GitHub App (two clicks in a browser)
  marshal app status              which repositories the app installation can reach
  marshal version

  marshal deploy <host> [--with-state] [--dry-run]
                                  install MARSHAL as a systemd user service on another machine

Files:
  fleet   ` + "$MARSHAL_CONFIG" + `  default ~/.config/marshal/fleet.yaml
  state   ` + "$MARSHAL_STATE" + `   default ~/.local/share/marshal (database, work trees, socket)

Remote control:
  ` + "MARSHAL_HOST=hermes1 marshal" + `   every command, the interface and chat included, reaches the
                                  daemon on that host through an ssh tunnel to its socket
  ` + "$MARSHAL_SOCKET" + `                 use this socket path instead of the state directory's
`

func main() {
	err := app(os.Args[1:])
	remote.CloseAll()
	if err != nil {
		fmt.Fprintln(os.Stderr, "marshal:", err)
		os.Exit(1)
	}
}

func app(args []string) error {
	if len(args) == 0 {
		if term.IsTerminal(int(os.Stdout.Fd())) {
			args = []string{"tui"}
		} else {
			fmt.Print(usage)
			return nil
		}
	}
	if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Print(usage)
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "version":
		fmt.Println("marshal", version)
		return nil
	case "init":
		return cmdInit()
	}

	fleet, err := config.Load(config.Path())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("no fleet file at %s; run `marshal init`", config.Path())
		}
		return err
	}

	switch cmd {
	case "check":
		fmt.Printf("%s: ok (%d repos, %d jobs, %d classes)\n", config.Path(), len(fleet.Repos), len(fleet.Jobs), len(fleet.Policy.Classes))
		app, err := auth(fleet)
		switch {
		case err != nil:
			return err
		case app == nil:
			fmt.Println("identity: the host's gh login (no github_app configured)")
		default:
			fmt.Printf("identity: GitHub App %s, installation %d\n", app.Identity().Name, app.InstallationID)
			if _, err := app.Token(ctx, ghapp.ReadOnly); err != nil {
				return fmt.Errorf("github_app: could not mint a token: %w", err)
			}
			fmt.Println("identity: token minted ok")
		}
		return nil
	case "repos":
		return cmdRepos(fleet)
	case "jobs":
		return cmdJobs(fleet)
	case "prompt":
		return cmdPrompt(fleet, rest)
	case "run":
		return cmdRun(ctx, fleet, rest)
	case "runs":
		return cmdRuns(ctx, rest)
	case "show":
		return cmdShow(ctx, rest)
	case "trust":
		return cmdTrust(ctx, fleet)
	case "serve":
		return cmdServe(ctx, fleet, rest)
	case "tui":
		return cmdTUI(ctx, fleet)
	case "chat":
		return cmdChat(ctx, fleet, rest)
	case "status":
		return cmdStatus(ctx)
	case "trigger":
		return cmdTrigger(ctx, rest)
	case "task":
		prompt, repoName := "", ""
		for i := 0; i < len(rest); i++ {
			switch rest[i] {
			case "--repo":
				if i+1 >= len(rest) {
					return errors.New("--repo needs a value")
				}
				repoName = rest[i+1]
				i++
			default:
				if prompt != "" {
					prompt += " "
				}
				prompt += rest[i]
			}
		}
		if strings.TrimSpace(prompt) == "" {
			return errors.New(`usage: marshal task "<instruction>" [--repo <name>]`)
		}
		resp, err := client().Do(ctx, api.Request{Op: "task", Value: prompt, Repo: repoName})
		if err != nil {
			return err
		}
		fmt.Printf("task run %s started; `marshal show %s` to follow\n", resp.ID, resp.ID)
		return nil
	case "fix":
		if len(rest) != 1 {
			return errors.New("usage: marshal fix <run-id>")
		}
		resp, err := client().Do(ctx, api.Request{Op: "fix", ID: rest[0]})
		if err != nil {
			return err
		}
		fmt.Printf("fix run %s started; `marshal show %s` to follow\n", resp.ID, resp.ID)
		return nil
	case "autonomy":
		return cmdAutonomy(ctx, rest)
	case "verify":
		return cmdVerify(ctx, fleet, rest)
	case "app":
		return cmdApp(ctx, fleet, rest)
	case "notify":
		if len(rest) != 1 || rest[0] != "test" {
			return errors.New("usage: marshal notify test")
		}
		n, err := notify.New(fleet.Notify)
		if err != nil {
			return err
		}
		if n == nil {
			return errors.New("no notify channels configured; add notify.ntfy, notify.telegram or notify.webhook to the fleet file")
		}
		var failed []string
		n.Log = func(f string, a ...any) { failed = append(failed, fmt.Sprintf(f, a...)) }
		_ = n.Send(ctx, notify.Event{Kind: notify.Test, Title: "test", Body: "If you can read this, MARSHAL can reach you.", URL: "https://github.com/Sousf/marshal"})
		if len(failed) > 0 {
			return errors.New(strings.Join(failed, "\n"))
		}
		fmt.Println("sent")
		return nil
	case "digest":
		st, err := openStore()
		if err != nil {
			return err
		}
		defer st.Close()
		d := &daemon.Daemon{Fleet: fleet, Store: st}
		text, err := d.DigestText(ctx)
		if err != nil {
			return err
		}
		fmt.Println(text)
		return nil
	case "deploy":
		return cmdDeploy(ctx, fleet, rest)
	case "stop":
		return cmdStop()
	case "logs":
		return cmdLogs(rest)
	}
	return fmt.Errorf("unknown command %q\n%s", cmd, usage)
}

func cmdVerify(ctx context.Context, f *config.Fleet, args []string) error {
	comment := false
	var pos []string
	for _, a := range args {
		switch a {
		case "--comment":
			comment = true
		default:
			if strings.HasPrefix(a, "-") {
				return fmt.Errorf("unknown flag %q", a)
			}
			pos = append(pos, a)
		}
	}
	if len(pos) != 1 {
		return errors.New("usage: marshal verify <run-id> [--comment]")
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	r, err := st.GetRun(ctx, pos[0])
	if err != nil {
		return err
	}
	if r.PRURL == "" {
		return fmt.Errorf("run %s has no pull request to verify", r.ID)
	}
	app, err := auth(f)
	if err != nil {
		return err
	}
	gh := ghClient(app)
	pr, err := gh.ViewPR(ctx, r.PRURL)
	if err != nil {
		return err
	}
	v := &verifier.Verifier{Fleet: f, Adapter: runner.Claude{}, WorkRoot: filepath.Join(config.StateDir(), "work"), Log: os.Stdout, Auth: app}
	res, err := v.Verify(ctx, r, pr)
	if err != nil {
		return err
	}
	if err := st.PutVerdict(ctx, res.Verdict); err != nil {
		return err
	}
	_ = st.AppendEvent(ctx, r.ID, store.Event{Kind: "note", Text: fmt.Sprintf("verifier (manual): %s · %d turns · $%.2f", res.Verdict.Verdict, res.Turns, res.CostUSD)})
	fmt.Println()
	fmt.Println(res.Comment())
	if comment {
		if err := gh.CommentPR(ctx, r.PRURL, res.Comment()); err != nil {
			return err
		}
		fmt.Println("posted on", r.PRURL)
	}
	if res.Verdict.Verdict != verifier.Pass {
		os.Exit(2)
	}
	return nil
}

func socketPath() string {
	if p := os.Getenv("MARSHAL_SOCKET"); p != "" {
		return p
	}
	return filepath.Join(config.StateDir(), "marshal.sock")
}

// auth loads the GitHub App when the fleet configures one. nil means act as
// the host's gh login.
func auth(f *config.Fleet) (*ghapp.App, error) {
	return ghapp.Load(f.GitHubApp)
}

// ghClient is gh acting as the app when there is one.
func ghClient(app *ghapp.App) github.Client {
	c := github.Client{}
	if app != nil {
		c.Auth = func(ctx context.Context) ([]string, error) { return app.Env(ctx, ghapp.Full) }
	}
	return c
}

// client reaches the daemon: through an ssh tunnel when MARSHAL_HOST is
// set, else the local socket. A tunnel that cannot be opened is fatal
// rather than a silent fallback to local state that would be the wrong
// machine's.
func client() api.Client {
	if remote.Host() != "" {
		t, err := remote.Open(context.Background())
		if err != nil {
			fmt.Fprintln(os.Stderr, "marshal:", err)
			os.Exit(1)
		}
		c := api.Client{Path: t.Local}
		if !c.Up() {
			fmt.Fprintf(os.Stderr, "marshal: ssh to %s works but no daemon answers on its socket; is `marshal serve` running there?\n", remote.Host())
			os.Exit(1)
		}
		return c
	}
	return api.Client{Path: socketPath()}
}

func cmdApp(ctx context.Context, f *config.Fleet, args []string) error {
	if len(args) > 0 && args[0] == "status" {
		app, err := auth(f)
		if err != nil {
			return err
		}
		if app == nil {
			fmt.Println("no github_app configured; MARSHAL acts as your gh login")
			return nil
		}
		repos, err := app.Repositories(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("app %s (id %d), installation %d, %d repositories reachable:\n", app.Slug, app.AppID, app.InstallationID, len(repos))
		reach := map[string]bool{}
		for _, r := range repos {
			reach[strings.ToLower(r)] = true
			fmt.Println("  " + r)
		}
		for _, r := range f.Repos {
			slug := r.GitHub
			if slug == "" {
				slug, _ = github.Slug(r.URL)
			}
			if slug != "" && !reach[strings.ToLower(slug)] {
				fmt.Printf("  ! fleet repo %s (%s) is not covered by the installation\n", r.Name, slug)
			}
		}
		return nil
	}
	if len(args) == 0 || args[0] != "create" {
		return errors.New("usage: marshal app create [--name <app-name>] [--homepage <url>] | marshal app status")
	}
	name, homepage := "", "https://github.com/Sousf/marshal"
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--name":
			if i+1 >= len(args) {
				return errors.New("--name needs a value")
			}
			name = args[i+1]
			i++
		case "--homepage":
			if i+1 >= len(args) {
				return errors.New("--homepage needs a value")
			}
			homepage = args[i+1]
			i++
		default:
			return fmt.Errorf("unknown flag %q", args[i])
		}
	}
	if f.GitHubApp.Set() && f.GitHubApp.InstallationID != 0 {
		return fmt.Errorf("the fleet file already has github_app (app %d); remove it first to create another", f.GitHubApp.AppID)
	}
	if name == "" {
		// App names are global on GitHub, so suffix with the login.
		out, err := exec.CommandContext(ctx, "gh", "api", "user", "--jq", ".login").Output()
		if err != nil {
			return fmt.Errorf("could not read your GitHub login from gh; pass --name: %w", err)
		}
		name = "marshal-" + strings.ToLower(strings.TrimSpace(string(out)))
	}
	keyPath := filepath.Join(filepath.Dir(config.Path()), "marshal-app.pem")
	fmt.Printf("creating GitHub App %q\nkey will be saved to %s\n\n", name, keyPath)

	created, err := ghapp.Create(ctx, ghapp.CreateOptions{
		Name: name, Homepage: homepage, KeyPath: keyPath, Out: os.Stdout,
		Open: func(u string) {
			fmt.Printf("opening %s\nif no browser appears, open that address yourself.\n\n", u)
			_ = exec.Command("xdg-open", u).Start()
		},
	})
	// Record whatever was achieved even on error, so a flow that stopped
	// after creation does not strand the key and id.
	if created.AppID != 0 {
		g := config.GitHubApp{AppID: created.AppID, InstallationID: created.InstallationID, PrivateKeyFile: keyPath, Slug: created.Slug}
		if werr := config.SetGitHubApp(config.Path(), g); werr != nil {
			return fmt.Errorf("app created (id %d, slug %s) but fleet file not updated: %v", created.AppID, created.Slug, werr)
		}
		fmt.Printf("fleet file updated: %s\n", config.Path())
	}
	if err != nil {
		if created.AppID != 0 && created.InstallationID == 0 {
			return fmt.Errorf("app created but not installed: %v\ninstall it at %s/installations/new, then put the installation id from the resulting URL into the fleet file", err, created.HTMLURL)
		}
		return err
	}
	fmt.Printf("\nGitHub App %s is created and installed (app %d, installation %d).\n", created.Slug, created.AppID, created.InstallationID)
	fmt.Println("checking that a token can be minted...")
	fl, err := config.Load(config.Path())
	if err != nil {
		return err
	}
	app, err := ghapp.Load(fl.GitHubApp)
	if err != nil {
		return err
	}
	if _, err := app.Token(ctx, ghapp.ReadOnly); err != nil {
		return err
	}
	fmt.Printf("ok. MARSHAL will now act as %s. Restart the daemon to pick it up.\n", app.Identity().Name)
	return nil
}

func cmdDeploy(ctx context.Context, f *config.Fleet, args []string) error {
	host, withState, dry := "", false, false
	var env []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--with-state":
			withState = true
		case "--dry-run":
			dry = true
		case "--env":
			if i+1 >= len(args) {
				return errors.New("--env needs KEY=value")
			}
			env = append(env, args[i+1])
			i++
		default:
			if strings.HasPrefix(args[i], "-") || host != "" {
				return fmt.Errorf("usage: marshal deploy <host> [--with-state] [--env KEY=value]... [--dry-run]")
			}
			host = args[i]
		}
	}
	if host == "" {
		return errors.New("usage: marshal deploy <host> [--with-state] [--env KEY=value]... [--dry-run]")
	}
	src, err := sourceDir()
	if err != nil {
		return err
	}
	d := remote.Deploy{
		Host:   host,
		Source: src,
		Fleet:  config.Path(),
		Unit:   filepath.Join(src, "packaging", "marshal.service"),
		Env:    env,
		DryRun: dry,
		Out:    os.Stdout,
	}
	if f.GitHubApp.PrivateKeyFile != "" {
		key := f.GitHubApp.PrivateKeyFile
		if strings.HasPrefix(key, "~/") {
			key = filepath.Join(os.Getenv("HOME"), key[2:])
		}
		d.Key = key
	}
	if withState {
		if client().Up() && !dry {
			return errors.New("stop the local daemon before copying its state (marshal stop); two daemons on one history will fight over the same PRs")
		}
		d.DB = filepath.Join(config.StateDir(), "marshal.db")
	}
	if dry {
		fmt.Printf("would deploy to %s:\n", host)
	}
	if err := d.Run(ctx); err != nil {
		return err
	}
	if !dry {
		fmt.Printf("\ndeployed. From any machine: MARSHAL_HOST=%s marshal\n", host)
	}
	return nil
}

// sourceDir finds the MARSHAL checkout to build from: the directory of a
// go.mod declaring this module, starting from the working directory.
func sourceDir() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for d := dir; ; d = filepath.Dir(d) {
		if b, err := os.ReadFile(filepath.Join(d, "go.mod")); err == nil && strings.HasPrefix(string(b), "module github.com/Sousf/marshal") {
			return d, nil
		}
		if filepath.Dir(d) == d {
			return "", errors.New("run deploy from inside the MARSHAL source checkout, so it can build the binary")
		}
	}
}

func pidPath() string { return filepath.Join(config.StateDir(), "marshal.pid") }
func logPath() string { return filepath.Join(config.StateDir(), "serve.log") }

func cmdServe(ctx context.Context, f *config.Fleet, args []string) error {
	tick := time.Minute
	detach, noSchedule := false, false
	logFile := ""
	var passthrough []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--detach", "-d":
			detach = true
		case "--no-schedule":
			noSchedule = true
			passthrough = append(passthrough, args[i])
		case "--log-file":
			if i+1 >= len(args) {
				return errors.New("--log-file needs a path")
			}
			logFile = args[i+1]
			i++
		case "--tick":
			if i+1 >= len(args) {
				return errors.New("--tick needs a duration")
			}
			d, err := time.ParseDuration(args[i+1])
			if err != nil || d < 5*time.Second {
				return fmt.Errorf("--tick: want a duration of at least 5s, got %q", args[i+1])
			}
			tick = d
			passthrough = append(passthrough, args[i], args[i+1])
			i++
		default:
			return fmt.Errorf("unknown flag %q", args[i])
		}
	}
	if detach {
		return detachServe(passthrough)
	}
	if client().Up() {
		return fmt.Errorf("a daemon is already running on %s", socketPath())
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	logger := log.New(os.Stderr, "", log.LstdFlags)
	app, err := auth(f)
	if err != nil {
		return err
	}
	if app != nil {
		logger.Printf("acting as GitHub App %s (installation %d)", app.Identity().Name, app.InstallationID)
	} else {
		logger.Printf("acting as the host's gh login; set github_app in the fleet file for a bot identity")
	}
	n, err := notify.New(f.Notify)
	if err != nil {
		return err
	}
	if n != nil {
		n.Log = logger.Printf
		logger.Printf("notifications on")
	} else {
		logger.Printf("no notify channels configured; events go to this log only")
	}
	d := &daemon.Daemon{Fleet: f, Store: st, Adapter: runner.Claude{}, GH: ghClient(app), Version: version, Tick: tick, Log: logger, Auth: app, NoSchedule: noSchedule, Notify: n, LogPath: logFile}
	if !d.Adapter.Available() {
		logger.Printf("warning: %s is not on PATH; scheduled runs will fail until it is", "claude")
	}
	errc := make(chan error, 1)
	go func() { errc <- api.Serve(ctx, socketPath(), d) }()
	logger.Printf("listening on %s", socketPath())
	runErr := d.Run(ctx)
	if err := <-errc; err != nil && runErr == nil {
		return err
	}
	return runErr
}

// detachServe re-executes this binary as a daemon in its own session, with
// stdout and stderr in the log file and the pid recorded, then returns.
// Plain enough to work without systemd; the unit in packaging/ is the
// better answer on a host that has it.
func detachServe(args []string) error {
	if client().Up() {
		return fmt.Errorf("a daemon is already running on %s", socketPath())
	}
	if err := os.MkdirAll(config.StateDir(), 0o700); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	logf, err := os.OpenFile(logPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(self, append([]string{"serve", "--log-file", logPath()}, args...)...)
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.Stdin = nil
	cmd.Env = os.Environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := cmd.Process.Pid
	if err := os.WriteFile(pidPath(), []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		return err
	}
	// Do not wait; the child outlives us. Release so no zombie bookkeeping.
	// Release zeroes the Pid field, hence the copy above.
	_ = cmd.Process.Release()
	// Give it a moment to come up so the user sees a real answer.
	deadline := time.Now().Add(5 * time.Second)
	for !client().Up() && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if !client().Up() {
		return fmt.Errorf("daemon did not come up; see %s", logPath())
	}
	fmt.Printf("daemon started, pid %d\nlog   %s\nstop  marshal stop\n", pid, logPath())
	return nil
}

func readPid() (int, error) {
	b, err := os.ReadFile(pidPath())
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("%s: bad pid", pidPath())
	}
	return pid, nil
}

func cmdStop() error {
	pid, err := readPid()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if client().Up() {
				return errors.New("a daemon is running but was not started with --detach; stop it where it runs")
			}
			fmt.Println("no detached daemon")
			return nil
		}
		return err
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		os.Remove(pidPath())
		fmt.Printf("pid %d is not running; removed stale pid file\n", pid)
		return nil
	}
	deadline := time.Now().Add(20 * time.Second)
	for client().Up() && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	os.Remove(pidPath())
	if client().Up() {
		return fmt.Errorf("pid %d did not stop within 20s; in-flight runs may be finishing", pid)
	}
	fmt.Printf("stopped pid %d\n", pid)
	return nil
}

func cmdLogs(args []string) error {
	follow := len(args) > 0 && (args[0] == "-f" || args[0] == "--follow")
	if remote.Host() != "" {
		if follow {
			return errors.New("-f is not available over a tunnel; run `marshal logs` again, or ssh in and tail the file")
		}
		resp, err := client().Do(context.Background(), api.Request{Op: "log", Limit: 100})
		if err != nil {
			return err
		}
		fmt.Println(resp.Text)
		return nil
	}
	if _, err := os.Stat(logPath()); err != nil {
		return fmt.Errorf("no log at %s; the daemon was not started with --detach", logPath())
	}
	tailArgs := []string{"-n", "100"}
	if follow {
		tailArgs = append(tailArgs, "-f")
	}
	cmd := exec.Command("tail", append(tailArgs, logPath())...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// source picks the daemon when one answers, else the database directly.
// With a remote host there is no sensible local fallback.
func source(f *config.Fleet) (ui.Source, func(), error) {
	c := client()
	if c.Up() {
		return ui.Daemon{Client: c}, func() {}, nil
	}
	if remote.Host() != "" {
		return nil, nil, fmt.Errorf("no daemon answering on %s", remote.Host())
	}
	st, err := openStore()
	if err != nil {
		return nil, nil, err
	}
	return ui.Local{Store: st, Fleet: f}, func() { st.Close() }, nil
}

func cmdTUI(ctx context.Context, f *config.Fleet) error {
	src, done, err := source(f)
	if err != nil {
		return err
	}
	defer done()
	ui.ChatCommand = func(ctx context.Context) (*exec.Cmd, error) {
		st, err := openStore()
		if err != nil {
			return nil, err
		}
		defer st.Close()
		return chatCommand(ctx, f, st, false)
	}
	return ui.Run(ctx, src, f, version)
}

// chatCommand gathers what the chat should know and builds the process.
func chatCommand(ctx context.Context, f *config.Fleet, st *store.Store, fresh bool) (*exec.Cmd, error) {
	var status *api.Status
	if resp, err := client().Do(ctx, api.Request{Op: "status"}); err == nil {
		status = resp.Status
	}
	recent, _ := st.ListRuns(ctx, "", 8)
	return chat.Command(ctx, f, st, status, recent, chat.Options{New: fresh})
}

func cmdChat(ctx context.Context, f *config.Fleet, args []string) error {
	fresh := false
	for _, a := range args {
		switch a {
		case "--new":
			fresh = true
		default:
			return fmt.Errorf("unknown flag %q", a)
		}
	}
	if !(runner.Claude{}).Available() {
		return errors.New("claude is not on PATH")
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	cmd, err := chatCommand(ctx, f, st, fresh)
	if err != nil {
		return err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		// A resume of a conversation Claude Code no longer has fails fast;
		// start over rather than leave the operator stuck.
		if !fresh {
			fmt.Fprintln(os.Stderr, "marshal: could not resume the last conversation; starting a new one")
			_ = chat.Forget(ctx, st)
			cmd, err = chatCommand(ctx, f, st, true)
			if err != nil {
				return err
			}
			cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
			return cmd.Run()
		}
		return err
	}
	return nil
}

func cmdStatus(ctx context.Context) error {
	resp, err := client().Do(ctx, api.Request{Op: "status"})
	if err != nil {
		if errors.Is(err, api.ErrDown) {
			fmt.Println("daemon: down (" + socketPath() + ")")
			return nil
		}
		return err
	}
	s := resp.Status
	auto := "on"
	if !s.Autonomy {
		auto = "OFF"
	}
	fmt.Printf("daemon: up (%s), started %s\nautonomy: %s\nrunning: %d %s\nverifying: %d %s\nspend 24h: $%.2f\nlast tick: %s\n",
		s.Version, ago(s.Started), auto, len(s.Running), strings.Join(s.Running, " "), len(s.Verifying), strings.Join(s.Verifying, " "), s.Spend24h, ago(s.LastTick))
	if s.LastError != "" {
		fmt.Printf("last error: %s\n", s.LastError)
	}
	if pid, err := readPid(); err == nil {
		fmt.Printf("detached: pid %d, log %s\n", pid, logPath())
	}
	return nil
}

func cmdTrigger(ctx context.Context, args []string) error {
	force := false
	var pos []string
	for _, a := range args {
		if a == "--force" {
			force = true
		} else {
			pos = append(pos, a)
		}
	}
	if len(pos) != 2 {
		return errors.New("usage: marshal trigger <job> <repo> [--force]")
	}
	val := ""
	if force {
		val = "force"
	}
	resp, err := client().Do(ctx, api.Request{Op: "trigger", Job: pos[0], Repo: pos[1], Value: val})
	if err != nil {
		return err
	}
	fmt.Printf("run %s started; `marshal show %s` to follow\n", resp.ID, resp.ID)
	return nil
}

func cmdAutonomy(ctx context.Context, args []string) error {
	if len(args) != 1 || (args[0] != "on" && args[0] != "off") {
		return errors.New("usage: marshal autonomy on|off")
	}
	resp, err := client().Do(ctx, api.Request{Op: "autonomy", Value: args[0]})
	if err != nil {
		if errors.Is(err, api.ErrDown) {
			// The daemon reads the setting on every tick, so the database
			// is the source of truth and can be set without it.
			st, err := openStore()
			if err != nil {
				return err
			}
			defer st.Close()
			if err := st.SetSetting(ctx, "autonomy", args[0]); err != nil {
				return err
			}
			fmt.Printf("autonomy %s (daemon down; it will pick this up when started)\n", args[0])
			return nil
		}
		return err
	}
	state := "off"
	if resp.Status.Autonomy {
		state = "on"
	}
	fmt.Println("autonomy", state)
	return nil
}

func openStore() (*store.Store, error) {
	return store.Open(filepath.Join(config.StateDir(), "marshal.db"))
}

func cmdInit() error {
	p := config.Path()
	if _, err := os.Stat(p); err == nil {
		return fmt.Errorf("%s already exists", p)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(p, []byte(starterFleet), 0o600); err != nil {
		return err
	}
	fmt.Println("wrote", p)
	fmt.Println("edit the repos list, then `marshal check`")
	return nil
}

func cmdRepos(f *config.Fleet) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "REPO\tBRANCH\tJOBS\tURL")
	for _, r := range f.Repos {
		branch := r.Branch
		if branch == "" {
			branch = "(remote default)"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.Name, branch, strings.Join(r.Jobs, ","), r.URL)
	}
	return w.Flush()
}

func cmdJobs(f *config.Fleet) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "JOB\tCLASS\tSTART\tSCHEDULE\tMODEL\tEFFORT\tTURNS\tUSD\tPROMPT")
	for _, j := range f.Jobs {
		r := f.Resolve(j)
		src := "embedded"
		switch {
		case j.Prompt != "":
			src = "inline"
		case j.PromptFile != "":
			src = j.PromptFile
		default:
			if _, ok := jobs.Prompt(j.Name); !ok {
				src = "MISSING"
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%.2f\t%s\n", j.Name, j.Class,
			f.Policy.Classes[j.Class].Start, j.Schedule, r.Model, r.Effort, r.MaxTurns, r.MaxUSD, src)
	}
	return w.Flush()
}

func cmdPrompt(f *config.Fleet, args []string) error {
	if len(args) != 2 {
		return errors.New("usage: marshal prompt <job> <repo>")
	}
	j, ok := f.Job(args[0])
	if !ok {
		return fmt.Errorf("unknown job %q", args[0])
	}
	r, ok := f.Repo(args[1])
	if !ok {
		return fmt.Errorf("unknown repo %q", args[1])
	}
	brief, err := f.PromptText(j, jobs.Prompt)
	if err != nil {
		return err
	}
	res := f.Resolve(j)
	fmt.Printf("# %s on %s\n# %s · effort %s · max %d turns · max $%.2f · permission %s\n",
		j.Name, r.Name, res.Model, res.Effort, res.MaxTurns, res.MaxUSD, res.PermissionMode)
	fmt.Printf("# allowed:    %s\n# disallowed: %s\n\n", strings.Join(res.AllowedTools, " "), strings.Join(res.DisallowedTools, " "))
	fmt.Print(brief, jobs.Contract)
	return nil
}

func cmdRun(ctx context.Context, f *config.Fleet, args []string) error {
	opts := run.Options{Log: os.Stdout}
	var pos []string
	for _, a := range args {
		switch a {
		case "--no-pr":
			opts.NoPR = true
		case "--keep":
			opts.Keep = true
		case "--force":
			opts.Force = true
		case "--allow-red":
			opts.AllowRed = true
		default:
			if strings.HasPrefix(a, "-") {
				return fmt.Errorf("unknown flag %q", a)
			}
			pos = append(pos, a)
		}
	}
	if len(pos) != 2 {
		return errors.New("usage: marshal run <job> <repo> [--no-pr] [--keep] [--force] [--allow-red]")
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	app, err := auth(f)
	if err != nil {
		return err
	}
	x := &run.Executor{Fleet: f, Store: st, Adapter: runner.Claude{}, Auth: app}
	rec, err := x.Execute(ctx, pos[0], pos[1], opts)
	if err != nil {
		return err
	}
	fmt.Printf("\nrun %s: %s", rec.ID, rec.Status)
	if rec.PRURL != "" {
		fmt.Printf(" %s", rec.PRURL)
	}
	if rec.Error != "" {
		fmt.Printf("\n%s", rec.Error)
	}
	fmt.Println()
	switch rec.Status {
	case store.Failed, store.Blocked, store.Cancelled:
		os.Exit(2)
	}
	return nil
}

func cmdRuns(ctx context.Context, args []string) error {
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	repo := ""
	if len(args) > 0 {
		repo = args[0]
	}
	runs, err := st.ListRuns(ctx, repo, 30)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "RUN\tWHEN\tREPO\tJOB\tSTATUS\tTURNS\tUSD\tPR")
	for _, r := range runs {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%.2f\t%s\n", r.ID, ago(r.StartedAt), r.Repo, r.Job, r.Status, r.Turns, r.CostUSD, r.PRURL)
	}
	return w.Flush()
}

func cmdShow(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: marshal show <run-id>")
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	r, err := st.GetRun(ctx, args[0])
	if err != nil {
		return err
	}
	fmt.Printf("run %s\n  %s on %s · class %s · %s via %s\n  status %s", r.ID, r.Job, r.Repo, r.Class, r.Model, r.Adapter, r.Status)
	if r.PRURL != "" {
		fmt.Printf(" · %s", r.PRURL)
	}
	fmt.Printf("\n  started %s", r.StartedAt.Format(time.RFC3339))
	if !r.FinishedAt.IsZero() {
		fmt.Printf(" · took %s", r.FinishedAt.Sub(r.StartedAt).Round(time.Second))
	}
	fmt.Printf(" · %d turns · $%.2f\n", r.Turns, r.CostUSD)
	if r.Error != "" {
		fmt.Printf("  error: %s\n", indent(r.Error, "         "))
	}
	if r.Parent != "" {
		fmt.Printf("  follow-up of run %s\n", r.Parent)
	}
	if r.Feedback != "" {
		fmt.Printf("  reviewer: %s\n", indent(r.Feedback, "            "))
	}
	if v, ok, _ := st.LatestVerdict(ctx, r.ID); ok {
		head := v.Head
		if len(head) > 8 {
			head = head[:8]
		}
		fmt.Printf("  verifier: %s on head %s · %d turns · $%.2f\n", v.Verdict, head, v.Turns, v.CostUSD)
		if v.Summary != "" {
			fmt.Printf("            %s\n", indent(v.Summary, "            "))
		}
	}
	ev, err := st.Events(ctx, r.ID)
	if err != nil {
		return err
	}
	fmt.Println()
	for _, e := range ev {
		fmt.Printf("%s %-7s %s\n", e.At.Format("15:04:05"), e.Kind, indent(e.Text, "                 "))
	}
	if r.Summary != "" {
		fmt.Printf("\n--- report ---\n%s\n", r.Summary)
	}
	if r.Status == store.Reported {
		fmt.Printf("\nsaved: %s\n", filepath.Join(config.StateDir(), "reports", r.ID+".md"))
	}
	return nil
}

func cmdTrust(ctx context.Context, f *config.Fleet) error {
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	recorded, err := st.ListTrust(ctx)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "REPO\tCLASS\tLEVEL\tSTREAK\tAPPROVED\tREJECTED\tREVERTED")
	for _, t := range daemon.Ladder(f, recorded) {
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\t%d\t%d\n", t.Repo, t.Class, t.Level, t.Streak, t.Approvals, t.Rejections, t.Reverts)
	}
	return w.Flush()
}

func ago(t time.Time) string {
	d := time.Since(t).Round(time.Minute)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return t.Format("Jan 2")
}

func indent(s, pad string) string {
	return strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n"+pad)
}
