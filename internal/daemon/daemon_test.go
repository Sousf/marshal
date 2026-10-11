package daemon

import (
	"context"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Sousf/marshal/internal/api"
	"github.com/Sousf/marshal/internal/config"
	"github.com/Sousf/marshal/internal/github"
	"github.com/Sousf/marshal/internal/runner"
	"github.com/Sousf/marshal/internal/store"
)

type fakeGH struct {
	mu     sync.Mutex
	prs    map[string]github.PR
	merged []string
	ci     github.CI
}

func (g *fakeGH) ViewPR(_ context.Context, url string) (github.PR, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.prs[url], nil
}
func (g *fakeGH) MergePR(_ context.Context, url string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.merged = append(g.merged, url)
	p := g.prs[url]
	p.State = "MERGED"
	g.prs[url] = p
	return nil
}
func (g *fakeGH) CommentPR(context.Context, string, string) error { return nil }
func (g *fakeGH) LatestCI(context.Context, string, string) (github.CI, bool, error) {
	return g.ci, true, nil
}
func (g *fakeGH) DefaultBranch(context.Context, string) (string, error) { return "main", nil }

type fakeAdapter struct {
	mu    sync.Mutex
	calls int
}

func (*fakeAdapter) Name() string    { return "fake" }
func (*fakeAdapter) Available() bool { return true }
func (f *fakeAdapter) Run(_ context.Context, req runner.Request, emit func(runner.Event)) (runner.Result, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return runner.Result{Output: "NO CHANGE: fine", Turns: 1, CostUSD: 0.01}, nil
}

func origin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "--quiet", "--initial-branch=main")
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644)
	git("add", ".")
	git("commit", "--quiet", "-m", "init")
	return dir
}

func newDaemon(t *testing.T, gh *fakeGH) (*Daemon, *fakeAdapter) {
	t.Helper()
	f, err := config.Parse([]byte(`
version: 1
defaults: {model: m, max_turns: 3, max_usd: 1}
repos:
  - {name: x, url: ` + origin(t) + `, github: Sousf/x, jobs: [audit, fix]}
jobs:
  - {name: audit, class: hardening, schedule: daily, prompt: p}
  - {name: fix, class: code, schedule: on-failure, prompt: p}
policy:
  classes:
    hardening: {start: review, promote: 2, soak_hours: 24}
    code: {start: review, promote: 5}
  caps: {auto_merges_per_repo_per_day: 1}
`))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	a := &fakeAdapter{}
	d := &Daemon{Fleet: f, Store: st, Adapter: a, GH: gh, Version: "t", WorkRoot: t.TempDir(),
		Log: log.New(io.Discard, "", 0)}
	d.init()
	return d, a
}

func proposal(t *testing.T, d *Daemon, class, url string, started time.Time) store.Run {
	t.Helper()
	r := &store.Run{Job: "audit", Repo: "x", Class: class, Status: store.Proposed, StartedAt: started}
	if err := d.Store.CreateRun(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	r.PRURL = url
	if err := d.Store.UpdateRun(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	return *r
}

func level(t *testing.T, d *Daemon, class string) string {
	t.Helper()
	for _, tr := range Ladder(d.Fleet, mustTrust(t, d)) {
		if tr.Class == class {
			return tr.Level
		}
	}
	return "?"
}

func mustTrust(t *testing.T, d *Daemon) []store.Trust {
	tr, err := d.Store.ListTrust(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func TestHumanDecisionsMoveTheLadder(t *testing.T) {
	gh := &fakeGH{prs: map[string]github.PR{
		"u1": {State: "MERGED", MergedBy: "pete"},
		"u2": {State: "MERGED", MergedBy: "pete"},
		"u3": {State: "CLOSED"},
	}}
	d, _ := newDaemon(t, gh)
	ctx := context.Background()
	proposal(t, d, "hardening", "u1", time.Now())
	proposal(t, d, "hardening", "u2", time.Now())
	if err := d.syncProposals(ctx); err != nil {
		t.Fatal(err)
	}
	if got := level(t, d, "hardening"); got != "soak" {
		t.Errorf("two human merges with promote=2 should reach soak, got %s", got)
	}
	runs, _ := d.Store.ListByStatus(ctx, store.Merged)
	if len(runs) != 2 || runs[0].MergedBy != "human" {
		t.Errorf("%+v", runs)
	}
	proposal(t, d, "hardening", "u3", time.Now())
	d.syncProposals(ctx)
	if got := level(t, d, "hardening"); got != "review" {
		t.Errorf("a rejection should demote back to review, got %s", got)
	}
	if rej, _ := d.Store.ListByStatus(ctx, store.Rejected); len(rej) != 1 {
		t.Errorf("rejected = %+v", rej)
	}
}

func TestAutoMergeGates(t *testing.T) {
	ctx := context.Background()
	open := func(checks, review string, labels ...string) github.PR {
		return github.PR{State: "OPEN", Checks: checks, Review: review, Labels: labels}
	}
	gh := &fakeGH{prs: map[string]github.PR{}}
	d, _ := newDaemon(t, gh)
	// Put hardening at auto directly.
	d.Store.PutTrust(ctx, store.Trust{Repo: "x", Class: "hardening", Level: "auto"})

	cases := []struct {
		name string
		pr   github.PR
		want bool
	}{
		{"green", open("SUCCESS", "APPROVED"), true},
		{"no checks", open("", ""), false},
		{"red", open("FAILURE", ""), false},
		{"pending", open("PENDING", ""), false},
		{"changes requested", open("SUCCESS", "CHANGES_REQUESTED"), false},
		{"hold label", open("SUCCESS", "", "marshal", "hold"), false},
	}
	for _, c := range cases {
		gh.merged = nil
		url := "pr-" + c.name
		gh.prs[url] = c.pr
		proposal(t, d, "hardening", url, time.Now())
		// Reset the daily cap between cases by pretending nothing merged today.
		d.Fleet.Policy.Caps.AutoMergesPerRepoPerDay = 0
		if err := d.syncProposals(ctx); err != nil {
			t.Fatal(err)
		}
		got := len(gh.merged) == 1
		if got != c.want {
			t.Errorf("%s: merged=%v want %v", c.name, got, c.want)
		}
	}
	// Auto merges never count as approvals.
	if got := level(t, d, "hardening"); got != "auto" {
		t.Errorf("level changed by machine merges: %s", got)
	}
	merged, _ := d.Store.ListByStatus(ctx, store.Merged)
	if len(merged) != 1 || merged[0].MergedBy != "auto" {
		t.Errorf("merged_by should be auto: %+v", merged)
	}
}

func TestSoakAndKillSwitchAndCap(t *testing.T) {
	ctx := context.Background()
	gh := &fakeGH{prs: map[string]github.PR{
		"fresh": {State: "OPEN", Checks: "SUCCESS"},
		"old":   {State: "OPEN", Checks: "SUCCESS"},
		"old2":  {State: "OPEN", Checks: "SUCCESS"},
	}}
	d, _ := newDaemon(t, gh)
	d.Store.PutTrust(ctx, store.Trust{Repo: "x", Class: "hardening", Level: "soak"})
	proposal(t, d, "hardening", "fresh", time.Now())
	proposal(t, d, "hardening", "old", time.Now().Add(-25*time.Hour))
	proposal(t, d, "hardening", "old2", time.Now().Add(-26*time.Hour))

	d.Store.SetSetting(ctx, autonomyKey, "off")
	d.syncProposals(ctx)
	if len(gh.merged) != 0 {
		t.Fatal("kill switch off must stop every merge")
	}
	d.Store.SetSetting(ctx, autonomyKey, "on")
	d.syncProposals(ctx)
	if len(gh.merged) != 1 {
		t.Fatalf("exactly one soaked PR should merge under cap 1: %v", gh.merged)
	}
	if gh.merged[0] == "fresh" {
		t.Error("the fresh PR has not soaked")
	}
	runs, _ := d.Store.ListByStatus(ctx, store.Merged)
	if runs[0].MergedBy != "soak" {
		t.Errorf("merged_by = %q", runs[0].MergedBy)
	}
	d.syncProposals(ctx)
	if len(gh.merged) != 1 {
		t.Error("cap of one auto merge per day should hold the second")
	}
}

func TestScheduleFiresOnceAndRespectsBusy(t *testing.T) {
	ctx := context.Background()
	gh := &fakeGH{prs: map[string]github.PR{}, ci: github.CI{Status: "completed", Conclusion: "success"}}
	d, a := newDaemon(t, gh)
	if err := d.schedule(ctx); err != nil {
		t.Fatal(err)
	}
	wait(t, d)
	a.mu.Lock()
	calls := a.calls
	a.mu.Unlock()
	if calls != 1 {
		t.Fatalf("daily job never run should fire once, got %d", calls)
	}
	d.schedule(ctx)
	wait(t, d)
	a.mu.Lock()
	if a.calls != 1 {
		t.Errorf("daily job should not fire again the same day: %d", a.calls)
	}
	a.mu.Unlock()
	runs, _ := d.Store.ListRuns(ctx, "x", 0)
	if len(runs) != 1 || runs[0].Job != "audit" || runs[0].Status != store.NoChange {
		t.Errorf("%+v", runs)
	}

	// Red CI fires the on-failure job; green does not.
	gh.ci = github.CI{Status: "completed", Conclusion: "failure"}
	d.schedule(ctx)
	wait(t, d)
	runs, _ = d.Store.ListRuns(ctx, "x", 0)
	if len(runs) != 2 || runs[0].Job != "fix" {
		t.Errorf("red CI should start fix: %+v", runs)
	}
	d.schedule(ctx)
	wait(t, d)
	runs, _ = d.Store.ListRuns(ctx, "x", 0)
	if len(runs) != 2 {
		t.Error("on-failure must respect its cooldown")
	}
}

func TestTriggerAndHandle(t *testing.T) {
	ctx := context.Background()
	gh := &fakeGH{prs: map[string]github.PR{}}
	d, _ := newDaemon(t, gh)
	resp := d.Handle(ctx, api.Request{Op: "trigger", Job: "audit", Repo: "x"})
	if resp.Error != "" || len(resp.ID) != 8 {
		t.Fatalf("%+v", resp)
	}
	if r := d.Handle(ctx, api.Request{Op: "trigger", Job: "audit", Repo: "x"}); r.Error == "" {
		t.Error("second trigger on a busy repo must refuse")
	}
	wait(t, d)
	if r := d.Handle(ctx, api.Request{Op: "run", ID: resp.ID}); r.Run == nil || r.Run.Status != store.NoChange {
		t.Errorf("%+v", r)
	}
	if r := d.Handle(ctx, api.Request{Op: "events", ID: resp.ID}); len(r.Events) == 0 {
		t.Error("events should be recorded")
	}
	if r := d.Handle(ctx, api.Request{Op: "status"}); r.Status == nil || !r.Status.Autonomy || r.Status.Repos != 1 {
		t.Errorf("%+v", r.Status)
	}
	if r := d.Handle(ctx, api.Request{Op: "autonomy", Value: "off"}); r.Status.Autonomy {
		t.Error("autonomy should be off")
	}
	if r := d.Handle(ctx, api.Request{Op: "autonomy", Value: "maybe"}); r.Error == "" {
		t.Error("bad value should fail")
	}
	if r := d.Handle(ctx, api.Request{Op: "trust"}); len(r.Trust) != 2 {
		t.Errorf("trust rows = %+v", r.Trust)
	}
	if r := d.Handle(ctx, api.Request{Op: "trigger", Job: "nope", Repo: "x"}); r.Error == "" {
		t.Error("unknown job must fail")
	}
	if r := d.Handle(ctx, api.Request{Op: "wat"}); r.Error == "" {
		t.Error("unknown op must fail")
	}
}

func wait(t *testing.T, d *Daemon) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for len(d.inFlight()) > 0 {
		if time.Now().After(deadline) {
			t.Fatal("run did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
