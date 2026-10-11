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
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/Sousf/marshal/internal/config"
	"github.com/Sousf/marshal/internal/jobs"
	"github.com/Sousf/marshal/internal/policy"
	"github.com/Sousf/marshal/internal/run"
	"github.com/Sousf/marshal/internal/runner"
	"github.com/Sousf/marshal/internal/store"
)

// version is stamped at build time with -X main.version.
var version = "dev"

const usage = `marshal — Maintains, Audits, Repairs, Secures, Hardens, And Learns

  marshal init                    write a starter fleet file
  marshal check                   validate the fleet file
  marshal repos                   list repositories and their jobs
  marshal jobs                    list jobs and their effective settings
  marshal prompt <job> <repo>     print the brief a run would send, without running
  marshal run <job> <repo>        run a job: clone, brief the agent, check, commit, open a PR
      --no-pr                       commit on a local branch only, open nothing
      --keep                        leave the work tree on disk
  marshal runs [repo]             recent runs
  marshal show <run-id>           one run's record and event log
  marshal trust                   where each repo and class sits on the ladder
  marshal version

Files:
  fleet   ` + "$MARSHAL_CONFIG" + `  default ~/.config/marshal/fleet.yaml
  state   ` + "$MARSHAL_STATE" + `   default ~/.local/share/marshal
`

func main() {
	if err := app(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "marshal:", err)
		os.Exit(1)
	}
}

func app(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
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
	}
	return fmt.Errorf("unknown command %q\n%s", cmd, usage)
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
		default:
			if strings.HasPrefix(a, "-") {
				return fmt.Errorf("unknown flag %q", a)
			}
			pos = append(pos, a)
		}
	}
	if len(pos) != 2 {
		return errors.New("usage: marshal run <job> <repo> [--no-pr] [--keep]")
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	x := &run.Executor{Fleet: f, Store: st, Adapter: runner.Claude{}}
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
	byKey := map[string]store.Trust{}
	for _, t := range recorded {
		byKey[t.Repo+"/"+t.Class] = t
	}
	l := policy.Ladder{Fleet: f}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "REPO\tCLASS\tLEVEL\tSTREAK\tAPPROVED\tREJECTED\tREVERTED")
	for _, r := range f.Repos {
		r, _ = f.Repo(r.Name)
		seen := map[string]bool{}
		for _, jn := range r.Jobs {
			j, ok := f.Job(jn)
			if !ok || seen[j.Class] {
				continue
			}
			seen[j.Class] = true
			t, found := byKey[r.Name+"/"+j.Class]
			t = l.Current(r, j.Class, t, found)
			fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\t%d\t%d\n", r.Name, j.Class, t.Level, t.Streak, t.Approvals, t.Rejections, t.Reverts)
		}
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
