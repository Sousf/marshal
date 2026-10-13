package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Sousf/marshal/internal/api"
	"github.com/Sousf/marshal/internal/config"
	"github.com/Sousf/marshal/internal/contract"
	"github.com/Sousf/marshal/internal/github"
	"github.com/Sousf/marshal/internal/runner"
	"github.com/Sousf/marshal/internal/sandbox"
	"github.com/Sousf/marshal/internal/store"
	"github.com/Sousf/marshal/internal/tapkey"
)

// agent is the adapter for runs started from this command line: the
// fleet's tool floor applied, and the same quota guard the daemon uses, so
// a foreground run that hits the limit parks the daemon too and a held
// fleet does not start one here either.
func agent(f *config.Fleet, st *store.Store) runner.Adapter {
	return runner.Guard{
		Adapter:   runner.Claude{Never: f.Never.Tools},
		HeldUntil: func(ctx context.Context) time.Time { u, _ := st.QuotaHold(ctx); return u },
		Hold:      func(ctx context.Context, u time.Time, why string) { _ = st.SetQuotaHold(ctx, u, why) },
	}
}

// isolate sets the process-wide sandbox from the fleet file. With bwrap
// configured and not working it is an error, so a host that cannot isolate
// never quietly runs agents without it.
func isolate(f *config.Fleet) error {
	if !f.Isolation.On() {
		sandbox.Use(nil)
		return nil
	}
	var hide []string
	if home, err := os.UserHomeDir(); err == nil {
		hide = append(hide, home)
	}
	if d, err := os.UserConfigDir(); err == nil {
		hide = append(hide, filepath.Join(d, "marshal"))
	}
	hide = append(hide, config.StateDir())
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		hide = append(hide, d)
	}
	// Either may sit outside every hidden directory.
	mask := []string{config.Path()}
	if k := f.GitHubApp.PrivateKeyFile; k != "" {
		mask = append(mask, config.ExpandHome(k))
	}
	for i := range mask {
		if abs, err := filepath.Abs(mask[i]); err == nil {
			mask[i] = abs
		}
	}
	expand := func(ps []string) (out []string) {
		for _, p := range ps {
			out = append(out, config.ExpandHome(p))
		}
		return out
	}
	s, err := sandbox.New(sandbox.HostOptions(hide, mask, expand(f.Isolation.ReadOnly), expand(f.Isolation.Writable)))
	if err == nil {
		err = s.Probe()
	}
	if err != nil {
		return err
	}
	sandbox.Use(s)
	return nil
}

// viaDaemon sends req, and when the daemon is down hands the store to
// local instead: every op here is a database read or write the daemon
// would pick up on its next tick anyway.
func viaDaemon(ctx context.Context, req api.Request, local func(*store.Store) (api.Response, error)) (api.Response, error) {
	resp, err := client().Do(ctx, req)
	if !errors.Is(err, api.ErrDown) {
		return resp, err
	}
	st, err := openStore()
	if err != nil {
		return api.Response{}, err
	}
	defer st.Close()
	return local(st)
}

func cmdIncidents(ctx context.Context, args []string) error {
	all := len(args) == 1 && args[0] == "--all"
	if len(args) > 1 || (len(args) == 1 && !all) {
		return errors.New("usage: marshal incidents [--all]")
	}
	val := ""
	if all {
		val = "all"
	}
	resp, err := viaDaemon(ctx, api.Request{Op: "incidents", Value: val}, func(st *store.Store) (api.Response, error) {
		inc, err := st.ListIncidents(ctx, all)
		return api.Response{Incidents: inc}, err
	})
	if err != nil {
		return err
	}
	if len(resp.Incidents) == 0 {
		fmt.Println("no open incidents")
		return nil
	}
	for _, i := range resp.Incidents {
		state := "open"
		switch {
		case !i.ResolvedAt.IsZero():
			state = "resolved"
		case !i.AckedAt.IsZero():
			state = "acked"
		}
		fmt.Printf("%s  %-8s %s %s  ×%d  last %s ago (run %s)\n    %s\n",
			i.Signature, state, i.Repo, i.Job, i.Count, ago(i.LastAt), i.LastRun, i.Sample)
	}
	return nil
}

func cmdAck(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: marshal ack <signature>")
	}
	resp, err := viaDaemon(ctx, api.Request{Op: "ack", ID: args[0]}, func(st *store.Store) (api.Response, error) {
		inc, err := st.AckIncident(ctx, args[0], time.Now())
		return api.Response{Text: "acked " + inc.Signature + " (" + inc.Repo + " " + inc.Job + ")"}, err
	})
	if err != nil {
		return err
	}
	fmt.Println(resp.Text)
	return nil
}

func cmdQuota(ctx context.Context, args []string) error {
	val := ""
	switch {
	case len(args) == 0:
	case len(args) == 1 && args[0] == "clear":
		val = "clear"
	default:
		return errors.New("usage: marshal quota [clear]")
	}
	resp, err := viaDaemon(ctx, api.Request{Op: "quota", Value: val}, func(st *store.Store) (api.Response, error) {
		if val == "clear" {
			return api.Response{Text: "quota hold cleared"}, st.SetQuotaHold(ctx, time.Time{}, "")
		}
		if u, why := st.QuotaHold(ctx); u.After(time.Now()) {
			return api.Response{Text: "held until " + u.Local().Format("Mon Jan 2 15:04") + ": " + why}, nil
		}
		return api.Response{Text: "no quota hold"}, nil
	})
	if err != nil {
		return err
	}
	fmt.Println(resp.Text)
	return nil
}

// cmdTapKey gives a repository's release workflow a deploy key that can
// write to the Homebrew tap and nothing else. It acts with the operator's
// own gh login, because adding a deploy key and setting a secret need admin
// rights the MARSHAL app deliberately lacks. The private key goes from
// memory to gh's stdin and is never written to disk.
func cmdTapKey(ctx context.Context, f *config.Fleet, args []string) error {
	force := false
	var pos []string
	for _, a := range args {
		if a == "--force" {
			force = true
		} else {
			pos = append(pos, a)
		}
	}
	if len(pos) != 1 {
		return errors.New("usage: marshal tap-key <repo> [--force]")
	}
	owner, name, _, ok := f.Homebrew.TapParts()
	if !ok {
		return errors.New("no homebrew.tap in the fleet file")
	}
	tap := owner + "/" + name
	repo, ok := f.Repo(pos[0])
	if !ok {
		return fmt.Errorf("unknown repo %q", pos[0])
	}
	slug := repo.GitHub
	if slug == "" {
		if slug, ok = github.Slug(repo.URL); !ok {
			return fmt.Errorf("%s: cannot tell its GitHub slug", repo.Name)
		}
	}
	const secret = "HOMEBREW_TAP_DEPLOY_KEY"
	out, err := exec.CommandContext(ctx, "gh", "secret", "list", "--repo", slug).Output()
	if err != nil {
		return fmt.Errorf("gh secret list %s: %w (needs your own gh login with admin on the repo)", slug, err)
	}
	if strings.Contains(string(out), secret) && !force {
		return fmt.Errorf("%s already has %s; --force replaces it (then delete the old deploy key on %s)", slug, secret, tap)
	}
	title := "goreleaser " + slug + " " + time.Now().Format("2006-01-02")
	pub, priv, err := tapkey.Generate(title)
	if err != nil {
		return err
	}
	add := exec.CommandContext(ctx, "gh", "repo", "deploy-key", "add", "/dev/stdin", "--repo", tap, "--allow-write", "--title", title)
	add.Stdin = bytes.NewReader(pub)
	if out, err := add.CombinedOutput(); err != nil {
		return fmt.Errorf("adding the deploy key to %s: %w: %s", tap, err, bytes.TrimSpace(out))
	}
	set := exec.CommandContext(ctx, "gh", "secret", "set", secret, "--repo", slug)
	set.Stdin = bytes.NewReader(priv)
	if out, err := set.CombinedOutput(); err != nil {
		return fmt.Errorf("deploy key %q was added to %s but setting the secret on %s failed; delete that key: %w: %s", title, tap, slug, err, bytes.TrimSpace(out))
	}
	for i := range priv {
		priv[i] = 0
	}
	fmt.Printf("%s can now push formulas to %s (deploy key %q, secret %s)\n", slug, tap, title, secret)
	return nil
}

// hookMarker identifies a pre-push hook MARSHAL wrote, so it is never
// confused with, or written over, one the operator wrote.
const hookMarker = "# Installed by marshal preflight --install-hook"

// cmdPreflight runs the repository's checks the way CI will, on a clean
// copy of HEAD: the contract's gate verbs, or those derived from its pull
// request workflows, without runner setup and without the host's git
// config. A failure is an error, so a pre-push hook refuses the push.
func cmdPreflight(ctx context.Context, args []string) error {
	install := len(args) == 1 && args[0] == "--install-hook"
	if len(args) > 1 || (len(args) == 1 && !install) {
		return errors.New("usage: marshal preflight [--install-hook]")
	}
	git := func(dir string, a ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", a...)
		cmd.Dir = dir
		out, err := cmd.Output()
		return strings.TrimSpace(string(out)), err
	}
	root, err := git(".", "rev-parse", "--show-toplevel")
	if err != nil {
		return errors.New("not in a git repository")
	}
	if install {
		return installHook(ctx, root)
	}
	head, err := git(root, "rev-parse", "--short", "HEAD")
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "marshal-preflight-")
	if err != nil {
		return err
	}
	defer func() {
		_, _ = git(root, "worktree", "remove", "--force", tmp)
		os.RemoveAll(tmp)
		_, _ = git(root, "worktree", "prune")
	}()
	if _, err := git(root, "worktree", "add", "--quiet", "--detach", tmp, "HEAD"); err != nil {
		return fmt.Errorf("checking out HEAD: %w", err)
	}
	con, ok, err := contract.Load(tmp)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Println("no marshal.yaml and no pull-request workflow: nothing to check")
		return nil
	}
	src := "marshal.yaml"
	if con.Derived {
		src = "the pull-request workflows"
	}
	fmt.Printf("pre-flight on %s, checks from %s, as CI runs them\n", head, src)
	for _, h := range con.HostSetup {
		fmt.Printf("  skip  %s (runner setup)\n", h)
	}
	var results []contract.Result
	for _, v := range contract.Gate {
		if con.Verbs[v] == "" {
			continue
		}
		r := con.Run(ctx, tmp, v)
		results = append(results, r)
		fmt.Printf("  %s\n", r)
		for _, l := range strings.Split(r.Output, "\n") {
			if strings.HasPrefix(l, "[skipped") {
				fmt.Printf("        %s\n", l)
			}
		}
	}
	var failed []contract.Result
	for _, r := range results {
		if !r.OK && !r.Unavailable {
			failed = append(failed, r)
		}
	}
	if len(failed) > 0 {
		fmt.Print("\n" + contract.Failures(failed, 6000))
		return fmt.Errorf("pre-flight failed on %s: CI would fail too (git push --no-verify skips this once)", head)
	}
	fmt.Println("pre-flight passed")
	return nil
}

func installHook(ctx context.Context, root string) error {
	out, err := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "--git-path", "hooks").Output()
	if err != nil {
		return err
	}
	dir := strings.TrimSpace(string(out))
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(root, dir)
	}
	path := filepath.Join(dir, "pre-push")
	if b, err := os.ReadFile(path); err == nil && !strings.Contains(string(b), hookMarker) {
		return fmt.Errorf("%s exists and was not written by marshal; add `marshal preflight` to it yourself", path)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	hook := "#!/bin/sh\n" + hookMarker + "\n# Runs this repository's CI checks on HEAD before a push. Skip once with\n# git push --no-verify.\nexec marshal preflight\n"
	if err := os.WriteFile(path, []byte(hook), 0o755); err != nil {
		return err
	}
	fmt.Println("installed", path)
	return nil
}
