// Package run executes one job against one repository end to end: clone,
// brief the agent, record what it does, check the diff against policy,
// commit, and open the pull request.
package run

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/Sousf/marshal/internal/config"
	"github.com/Sousf/marshal/internal/ghapp"
	"github.com/Sousf/marshal/internal/jobs"
	"github.com/Sousf/marshal/internal/policy"
	"github.com/Sousf/marshal/internal/runner"
	"github.com/Sousf/marshal/internal/store"
	"github.com/Sousf/marshal/internal/workspace"
)

// Options tune one execution.
type Options struct {
	// NoPR leaves the commit on a local branch and skips push and PR. For
	// trying a job out, and for tests.
	NoPR bool
	// Keep leaves the work tree on disk after the run.
	Keep bool
	// Log receives a human-readable line per event. May be nil.
	Log io.Writer
	// Env is passed to the agent and to git. The run's GH_TOKEN goes here.
	Env []string
	// ID pre-assigns the run's identifier so a caller can hand it out before
	// the run finishes. Empty means the store picks one.
	ID string
}

// Executor holds what every run needs.
type Executor struct {
	Fleet   *config.Fleet
	Store   *store.Store
	Adapter runner.Adapter
	// WorkRoot is where clones are made. Empty means <state>/work.
	WorkRoot string
	// Auth, when set, makes git, gh and the PR act as the GitHub App. The
	// harness gets a full token; the agent gets a read-only one.
	Auth *ghapp.App
}

// Label is put on every PR MARSHAL opens, so the review screen and a
// GitHub search can find them.
const Label = "marshal"

// Execute runs job against repo and returns the stored run record. The
// returned error is for failures of the harness itself; an agent that found
// nothing to do, or a diff that policy refused, is a normal outcome and is
// reported through the record's Status.
func (x *Executor) Execute(ctx context.Context, jobName, repoName string, opts Options) (store.Run, error) {
	job, ok := x.Fleet.Job(jobName)
	if !ok {
		return store.Run{}, fmt.Errorf("unknown job %q", jobName)
	}
	repo, ok := x.Fleet.Repo(repoName)
	if !ok {
		return store.Run{}, fmt.Errorf("unknown repo %q", repoName)
	}
	res := x.Fleet.Resolve(job)
	brief, err := x.Fleet.PromptText(job, jobs.Prompt)
	if err != nil {
		return store.Run{}, err
	}
	if !x.Adapter.Available() {
		return store.Run{}, fmt.Errorf("adapter %s is not available on this host", x.Adapter.Name())
	}

	// Caps are checked before anything is cloned or spent.
	if err := x.checkCaps(ctx, repo); err != nil {
		return store.Run{}, err
	}

	rec := &store.Run{ID: opts.ID, Job: job.Name, Repo: repo.Name, Class: job.Class, Adapter: x.Adapter.Name(), Model: res.Model}
	if err := x.Store.CreateRun(ctx, rec); err != nil {
		return store.Run{}, err
	}
	rec.Branch = workspace.BranchName(job.Name, rec.ID)
	logf := x.logger(ctx, rec.ID, opts.Log)

	finish := func(st store.Status, errText string) (store.Run, error) {
		rec.Status = st
		rec.Error = errText
		rec.FinishedAt = time.Now()
		if err := x.Store.UpdateRun(ctx, rec); err != nil {
			return *rec, err
		}
		return *rec, nil
	}

	logf("note", "run %s: %s on %s (%s, %s)", rec.ID, job.Name, repo.Name, res.Model, res.Effort)
	harnessEnv, agentEnv := opts.Env, opts.Env
	identity := res.Identity
	if x.Auth != nil {
		full, err := x.Auth.Env(ctx, ghapp.Full)
		if err != nil {
			return finish(store.Failed, err.Error())
		}
		ro, err := x.Auth.Env(ctx, ghapp.ReadOnly)
		if err != nil {
			return finish(store.Failed, err.Error())
		}
		harnessEnv = append(append([]string{}, opts.Env...), full...)
		agentEnv = append(append([]string{}, opts.Env...), ro...)
		repo.URL = ghapp.CloneURL(repo)
		if identity.Name == "" {
			identity = x.Auth.Identity()
		}
		logf("note", "acting as GitHub App %s", identity.Name)
	}
	ws, err := workspace.Prepare(ctx, x.workRoot(), repo, job.Name, rec.ID, identity, harnessEnv)
	if err != nil {
		return finish(store.Failed, err.Error())
	}
	if !opts.Keep {
		defer ws.Remove()
	} else {
		defer logf("note", "work tree kept at %s", ws.Dir)
	}

	req := runner.Request{
		Prompt:          brief + jobs.Contract,
		SystemPrompt:    systemPrompt(repo, ws.Conventions()),
		Dir:             ws.Dir,
		Model:           res.Model,
		Effort:          res.Effort,
		MaxTurns:        res.MaxTurns,
		MaxUSD:          res.MaxUSD,
		PermissionMode:  res.PermissionMode,
		AllowedTools:    res.AllowedTools,
		DisallowedTools: res.DisallowedTools,
		Env:             agentEnv,
	}
	result, runErr := x.Adapter.Run(ctx, req, func(e runner.Event) {
		logf(string(e.Kind), "%s", e.Text)
	})
	rec.Turns = result.Turns
	rec.CostUSD = result.CostUSD
	rec.Summary = result.Output
	if ctx.Err() != nil {
		return finish(store.Cancelled, ctx.Err().Error())
	}
	if runErr != nil {
		return finish(store.Failed, runErr.Error())
	}
	if result.IsError {
		return finish(store.Failed, "agent: "+firstLine(result.Output))
	}

	changed, err := ws.Changed(ctx)
	if err != nil {
		return finish(store.Failed, err.Error())
	}
	if !changed {
		logf("note", "no changes")
		return finish(store.NoChange, "")
	}

	numstat, err := ws.Numstat(ctx)
	if err != nil {
		return finish(store.Failed, err.Error())
	}
	changes := policy.ParseNumstat(numstat)
	if v := policy.Check(job, changes); len(v) > 0 {
		var b strings.Builder
		for _, x := range v {
			b.WriteString(x.String())
			b.WriteString("\n")
			logf("blocked", "%s", x.String())
		}
		return finish(store.Blocked, strings.TrimSpace(b.String()))
	}
	logf("note", "policy ok: %d files", len(changes))

	title, body := splitReport(result.Output, job.Name)
	body = body + trailer(rec, result)
	if err := ws.Commit(ctx, title+"\n\n"+body); err != nil {
		return finish(store.Failed, err.Error())
	}
	if opts.NoPR {
		logf("note", "committed on %s (no PR requested)", ws.Branch)
		return finish(store.Committed, "")
	}
	if err := ws.Push(ctx); err != nil {
		return finish(store.Failed, err.Error())
	}
	url, err := ws.OpenPR(ctx, title, body, []string{Label, Label + ":" + job.Class})
	if err != nil {
		return finish(store.Failed, err.Error())
	}
	rec.PRURL = url
	logf("note", "opened %s", url)
	return finish(store.Proposed, "")
}

func (x *Executor) workRoot() string {
	if x.WorkRoot != "" {
		return x.WorkRoot
	}
	return filepath.Join(config.StateDir(), "work")
}

// checkCaps enforces the fleet-wide daily limits.
func (x *Executor) checkCaps(ctx context.Context, repo config.Repo) error {
	caps := x.Fleet.Policy.Caps
	since := time.Now().Add(-24 * time.Hour)
	if caps.PRsPerRepoPerDay > 0 {
		n, err := x.Store.CountSince(ctx, repo.Name, since, store.Proposed, store.Merged)
		if err != nil {
			return err
		}
		if n >= caps.PRsPerRepoPerDay {
			return fmt.Errorf("cap: %s already has %d PRs in the last 24h (limit %d)", repo.Name, n, caps.PRsPerRepoPerDay)
		}
	}
	if caps.USDPerDay > 0 {
		usd, err := x.Store.SpendSince(ctx, since)
		if err != nil {
			return err
		}
		if vs, err := x.Store.VerdictSpendSince(ctx, since); err == nil {
			usd += vs
		}
		if usd >= caps.USDPerDay {
			return fmt.Errorf("cap: fleet has spent $%.2f in the last 24h (limit $%.2f)", usd, caps.USDPerDay)
		}
	}
	return nil
}

func (x *Executor) logger(ctx context.Context, runID string, w io.Writer) func(kind, format string, args ...any) {
	return func(kind, format string, args ...any) {
		text := fmt.Sprintf(format, args...)
		_ = x.Store.AppendEvent(ctx, runID, store.Event{Kind: kind, Text: text})
		if w != nil {
			fmt.Fprintf(w, "%s %-7s %s\n", time.Now().Format("15:04:05"), kind, text)
		}
	}
}

// systemPrompt frames the run and carries the repo's conventions.
func systemPrompt(repo config.Repo, conventions string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are MARSHAL, an autonomous maintainer working on the repository %q (%s, default branch %s).\n", repo.Name, repo.URL, repo.Branch)
	b.WriteString("You work unattended. Nobody will answer a question; make the conservative call and explain it in your report.\n")
	if conventions != "" {
		b.WriteString("\nThe repository's AGENTS.md follows. Its conventions outrank the brief.\n\n")
		b.WriteString(conventions)
	}
	return b.String()
}

// splitReport turns the agent's final message into a PR title and body. It
// is only called when the tree changed, so a leading "NO CHANGE:" line is
// the agent contradicting git; git wins and the line is dropped from the
// title. A smaller model did exactly this on the first real run.
func splitReport(out, job string) (title, body string) {
	out = strings.TrimSpace(out)
	if out == "" {
		return job, "(the agent gave no report)"
	}
	if first, rest, ok := strings.Cut(out, "\n"); ok && strings.HasPrefix(strings.ToUpper(first), "NO CHANGE") && strings.TrimSpace(rest) != "" {
		out = strings.TrimSpace(rest)
	}
	title, body, _ = strings.Cut(out, "\n")
	title = strings.TrimSpace(strings.TrimLeft(title, "# "))
	if len(title) > 70 {
		title = title[:67] + "..."
	}
	return title, strings.TrimSpace(body)
}

func trailer(rec *store.Run, r runner.Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n\n---\nMARSHAL run `%s` · job `%s` · class `%s` · %s · %d turns · $%.2f",
		rec.ID, rec.Job, rec.Class, rec.Model, r.Turns, r.CostUSD)
	if len(r.Denials) > 0 {
		fmt.Fprintf(&b, "\nDenied during the run: `%s`. The agent may not have finished everything it tried.", strings.Join(uniq(r.Denials), "`, `"))
	}
	return b.String()
}

func uniq(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}
