package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/Sousf/marshal/internal/api"
	"github.com/Sousf/marshal/internal/config"
	"github.com/Sousf/marshal/internal/github"
	"github.com/Sousf/marshal/internal/runner"
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
