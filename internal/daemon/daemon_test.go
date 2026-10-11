package daemon

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	mu       sync.Mutex
	prs      map[string]github.PR
	merged   []string
	comments []string
	labels   []string
	ci       github.CI
	// humanComments is what HumanComments returns per PR url.
	humanComments map[string][]github.Comment
	// commitCI is what CommitCI reports; commitCINone means no runs exist.
	commitCI     github.CI
	commitCINone bool
	created      []createdPR
	// deps is what DependabotPRs returns; depCalls counts the calls.
	deps     []github.PR
	depCalls int
	approved []string
	closed   []string
}

func (g *fakeGH) DependabotPRs(context.Context, string) ([]github.PR, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.depCalls++
	return g.deps, nil
}

func (g *fakeGH) ApprovePR(_ context.Context, url, _ string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.approved = append(g.approved, url)
	return nil
}

func (g *fakeGH) ClosePR(_ context.Context, url, _ string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closed = append(g.closed, url)
	p := g.prs[url]
	p.State = "CLOSED"
	g.prs[url] = p
	return nil
}

type createdPR struct{ slug, head, base, title string }

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
func (g *fakeGH) CommentPR(_ context.Context, url, body string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.comments = append(g.comments, url+": "+body)
	return nil
}
func (g *fakeGH) AddLabel(_ context.Context, url, label string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.labels = append(g.labels, url+": "+label)
	// Mirror GitHub: the label shows on the next view.
	p := g.prs[url]
	p.Labels = append(p.Labels, label)
	g.prs[url] = p
	return nil
}
func (g *fakeGH) LatestCI(context.Context, string, string) (github.CI, bool, error) {
	return g.ci, true, nil
}
func (g *fakeGH) DefaultBranch(context.Context, string) (string, error) { return "main", nil }
func (g *fakeGH) HumanComments(_ context.Context, url, _ string, since time.Time) ([]github.Comment, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []github.Comment
	for _, c := range g.humanComments[url] {
		if c.At.After(since) {
			out = append(out, c)
		}
	}
	return out, nil
}
func (g *fakeGH) CommitCI(context.Context, string, string) (github.CI, bool, error) {
	if g.commitCINone {
		return github.CI{}, false, nil
	}
	return g.commitCI, true, nil
}
func (g *fakeGH) CreatePR(_ context.Context, slug, head, base, title, body string, labels []string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.created = append(g.created, createdPR{slug, head, base, title})
	url := "revert-" + head
	g.prs[url] = github.PR{State: "OPEN", Title: title}
	return url, nil
}
func (g *fakeGH) FailedLogs(context.Context, string, string, int) (string, error) {
	return "## ci (run 1): failure\n\n```\nFAIL: TestX\n```\n", nil
}

type fakeAdapter struct {
	mu      sync.Mutex
	calls   int
	verdict string // what the verifier run answers; empty means no structured answer
	// fix, when set, is applied to the work tree of a fix-checks run so
	// there is something to push.
	fix     func(dir string)
	prompts []string
	// fail, when set, is what every non-verifier run returns.
	fail error
}

func (*fakeAdapter) Name() string    { return "fake" }
func (*fakeAdapter) Available() bool { return true }
func (f *fakeAdapter) Run(_ context.Context, req runner.Request, emit func(runner.Event)) (runner.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if req.JSONSchema != "" {
		r := runner.Result{Turns: 2, CostUSD: 0.05}
		if f.verdict != "" {
			r.Structured = []byte(`{"verdict":"` + f.verdict + `","summary":"scripted","findings":[],"tests_ran":true,"tests_passed":true}`)
		}
		return r, nil
	}
	f.calls++
	f.prompts = append(f.prompts, req.Prompt)
	if f.fail != nil {
		return runner.Result{IsError: true}, f.fail
	}
	if f.fix != nil && (strings.Contains(req.Prompt, "Fix failing checks") || strings.Contains(req.Prompt, "Address review feedback")) {
		f.fix(req.Dir)
		return runner.Result{Output: "Fix the test\n\nCorrected the assertion.", Turns: 2, CostUSD: 0.02}, nil
	}
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

// branch creates a proposal branch with one commit in the origin and
// returns its head SHA.
func branch(t *testing.T, dir, name string) string {
	t.Helper()
	git := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("checkout", "-q", "-b", name)
	file := strings.ReplaceAll(name, "/", "-") + ".txt"
	if err := os.WriteFile(filepath.Join(dir, file), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-q", "-m", name)
	sha := git("rev-parse", "HEAD")
	git("checkout", "-q", "main")
	return sha
}

func newDaemon(t *testing.T, gh *fakeGH) (*Daemon, *fakeAdapter) {
	t.Helper()
	d, a, _ := newDaemonDir(t, gh)
	return d, a
}

func newDaemonDir(t *testing.T, gh *fakeGH) (*Daemon, *fakeAdapter, string) {
	t.Helper()
	dir := origin(t)
	f, err := config.Parse([]byte(`
version: 1
defaults: {model: m, max_turns: 3, max_usd: 1}
repos:
  - {name: x, url: ` + dir + `, github: Sousf/x, jobs: [audit, fix]}
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
	a := &fakeAdapter{verdict: "PASS"}
	d := &Daemon{Fleet: f, Store: st, Adapter: a, GH: gh, Version: "t", WorkRoot: t.TempDir(),
		Log: log.New(io.Discard, "", 0)}
	d.init()
	return d, a, dir
}

// proposal records a proposed run. When dir is non-empty a real branch is
// created in the origin so the verifier has something to check out, and the
// fake PR learns its head SHA.
func proposal(t *testing.T, d *Daemon, class, url string, started time.Time) store.Run {
	return proposalIn(t, d, nil, "", class, url, started)
}

func proposalIn(t *testing.T, d *Daemon, gh *fakeGH, dir, class, url string, started time.Time) store.Run {
	t.Helper()
	r := &store.Run{Job: "audit", Repo: "x", Class: class, Status: store.Proposed, StartedAt: started}
	if err := d.Store.CreateRun(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	r.PRURL = url
	r.Branch = "marshal/audit/" + r.ID
	if dir != "" {
		sha := branch(t, dir, r.Branch)
		gh.mu.Lock()
		p := gh.prs[url]
		p.HeadRef = r.Branch
		p.HeadSHA = sha
		gh.prs[url] = p
		gh.mu.Unlock()
	}
	if err := d.Store.UpdateRun(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	return *r
}

// settle runs sync until no verification is in flight, then once more so a
// fresh verdict is acted on.
func settle(t *testing.T, d *Daemon) {
	t.Helper()
	for i := 0; i < 3; i++ {
		if err := d.syncProposals(context.Background()); err != nil {
			t.Fatal(err)
		}
		waitVerify(t, d)
	}
}

func waitVerify(t *testing.T, d *Daemon) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		d.mu.Lock()
		n := len(d.verifying)
		d.mu.Unlock()
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("verification did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
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
	d, _, dir := newDaemonDir(t, gh)
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
		proposalIn(t, d, gh, dir, "hardening", url, time.Now())
		// Reset the daily cap between cases by pretending nothing merged today.
		d.Fleet.Policy.Caps.AutoMergesPerRepoPerDay = 0
		settle(t, d)
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
	// The green case was verified exactly once and the verdict was posted.
	v, ok, _ := d.Store.LatestVerdict(ctx, merged[0].ID)
	if !ok || v.Verdict != "PASS" || v.Head != gh.prs["pr-green"].HeadSHA {
		t.Errorf("verdict = %+v ok=%v", v, ok)
	}
	n := 0
	for _, c := range gh.comments {
		if strings.Contains(c, "MARSHAL verifier: PASS") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("verifier comments = %d, want 1:\n%s", n, strings.Join(gh.comments, "\n"))
	}
}

func TestVerifierBlocksAutoMerge(t *testing.T) {
	ctx := context.Background()
	gh := &fakeGH{prs: map[string]github.PR{"u": {State: "OPEN", Checks: "SUCCESS"}}}
	d, a, dir := newDaemonDir(t, gh)
	a.verdict = "FAIL"
	d.Store.PutTrust(ctx, store.Trust{Repo: "x", Class: "hardening", Level: "auto"})
	r := proposalIn(t, d, gh, dir, "hardening", "u", time.Now())
	settle(t, d)
	if len(gh.merged) != 0 {
		t.Fatal("a FAIL verdict must block the merge")
	}
	if len(gh.labels) != 1 || !strings.Contains(gh.labels[0], "needs-human") {
		t.Errorf("labels = %v", gh.labels)
	}
	v, _, _ := d.Store.LatestVerdict(ctx, r.ID)
	if v.Verdict != "FAIL" {
		t.Errorf("%+v", v)
	}
	// A new head re-verifies; same head does not.
	settle(t, d)
	if n := strings.Count(strings.Join(gh.comments, "\n"), "MARSHAL verifier"); n != 1 {
		t.Errorf("same head verified %d times", n)
	}
	a.verdict = "PASS"
	gh.mu.Lock()
	p := gh.prs["u"]
	p.HeadSHA = branch(t, dir, "marshal/audit/"+r.ID+"-v2")
	p.HeadRef = "marshal/audit/" + r.ID + "-v2"
	gh.prs["u"] = p
	gh.mu.Unlock()
	settle(t, d)
	if len(gh.merged) != 1 {
		t.Error("a new, passing head should merge")
	}

	// No structured answer at all is UNSURE, which also holds.
	gh2 := &fakeGH{prs: map[string]github.PR{"w": {State: "OPEN", Checks: "SUCCESS"}}}
	d2, a2, dir2 := newDaemonDir(t, gh2)
	a2.verdict = ""
	d2.Store.PutTrust(ctx, store.Trust{Repo: "x", Class: "hardening", Level: "auto"})
	r2 := proposalIn(t, d2, gh2, dir2, "hardening", "w", time.Now())
	settle(t, d2)
	v2, _, _ := d2.Store.LatestVerdict(ctx, r2.ID)
	if len(gh2.merged) != 0 || v2.Verdict != "UNSURE" {
		t.Errorf("merged=%v verdict=%+v", gh2.merged, v2)
	}
}

func TestRedChecksGetFixedWithinCap(t *testing.T) {
	ctx := context.Background()
	gh := &fakeGH{prs: map[string]github.PR{"u": {State: "OPEN", Checks: "FAILURE", Title: "Pin things", Body: "body"}}}
	d, a, dir := newDaemonDir(t, gh)
	a.fix = func(wd string) { os.WriteFile(filepath.Join(wd, "fixed.txt"), []byte("ok\n"), 0o644) }
	// The parent job is confined to go.mod; the fix touches another file
	// and must still go through, because follow-ups fix whatever broke.
	d.Fleet.Jobs[0].Paths = []string{"go.mod"}
	r := proposalIn(t, d, gh, dir, "hardening", "u", time.Now())
	head1 := gh.prs["u"].HeadSHA

	settle(t, d)
	wait(t, d)
	fu, _ := d.Store.Followups(ctx, r.ID)
	if len(fu) != 1 || fu[0].Status != store.Pushed || fu[0].BaseHead != head1 || fu[0].Job != "fix-checks" || fu[0].PRURL != "u" {
		t.Fatalf("followups = %+v", fu)
	}
	if !strings.Contains(strings.Join(a.prompts, "\n"), "FAIL: TestX") || !strings.Contains(strings.Join(a.prompts, "\n"), "<pr-body>") {
		t.Error("fix brief should carry the failing log and the PR claim")
	}
	// The branch on the origin moved.
	cmd := exec.Command("git", "log", "-1", "--format=%s", r.Branch)
	cmd.Dir = dir
	out, _ := cmd.Output()
	if strings.TrimSpace(string(out)) != "Fix the test" {
		t.Errorf("origin branch head = %q", out)
	}
	if n := len(gh.comments); n != 1 || !strings.Contains(gh.comments[0], "pushed a fix") {
		t.Errorf("comments = %v", gh.comments)
	}
	// Same head still red: no second attempt on the same head.
	settle(t, d)
	wait(t, d)
	if fu, _ := d.Store.Followups(ctx, r.ID); len(fu) != 1 {
		t.Error("the same failing head must not be attempted twice")
	}
	// New head, still red: second attempt, then the cap.
	p := gh.prs["u"]
	p.HeadSHA = "newhead2"
	gh.prs["u"] = p
	settle(t, d)
	wait(t, d)
	if fu, _ := d.Store.Followups(ctx, r.ID); len(fu) != 2 {
		t.Fatalf("second head should get a second attempt: %d", len(fu))
	}
	p.HeadSHA = "newhead3"
	gh.prs["u"] = p
	settle(t, d)
	wait(t, d)
	fu, _ = d.Store.Followups(ctx, r.ID)
	if len(fu) != 2 {
		t.Errorf("cap of 2 exceeded: %d attempts", len(fu))
	}
	if len(gh.labels) != 1 || !strings.Contains(gh.labels[0], "needs-human") {
		t.Errorf("at the cap the PR is handed to a human: labels=%v", gh.labels)
	}
	// The operator can ask for one more, outside the cap.
	id, err := d.Fix(ctx, r.ID)
	if err != nil || len(id) != 8 {
		t.Fatalf("Fix: %v", err)
	}
	wait(t, d)
	if fu, _ := d.Store.Followups(ctx, r.ID); len(fu) != 3 {
		t.Errorf("manual fix should run: %d", len(fu))
	}
	if _, err := d.Fix(ctx, "nope"); err == nil {
		t.Error("unknown run must fail")
	}
	// Nothing merged, nothing verified while red.
	if len(gh.merged) != 0 {
		t.Error("a red PR never merges")
	}
	if v, ok, _ := d.Store.LatestVerdict(ctx, r.ID); ok {
		t.Errorf("no verification while red: %+v", v)
	}
}

func TestReviewFeedbackIsAddressedAndRecorded(t *testing.T) {
	ctx := context.Background()
	gh := &fakeGH{prs: map[string]github.PR{"u": {State: "OPEN", Checks: "SUCCESS", Review: "CHANGES_REQUESTED", Title: "T", Body: "B"}}}
	d, a, dir := newDaemonDir(t, gh)
	a.fix = func(wd string) { os.WriteFile(filepath.Join(wd, "addressed.txt"), []byte("ok\n"), 0o644) }
	r := proposalIn(t, d, gh, dir, "hardening", "u", time.Now().Add(-time.Hour))
	gh.humanComments = map[string][]github.Comment{"u": {{Author: "pete", At: time.Now().Add(-time.Minute), Body: "please rename the flag", Where: "main.go"}}}

	settle(t, d)
	wait(t, d)
	fu, _ := d.Store.Followups(ctx, r.ID)
	if len(fu) != 1 || fu[0].Job != "address-review" || fu[0].Status != store.Pushed {
		t.Fatalf("followups = %+v", fu)
	}
	if !strings.Contains(strings.Join(a.prompts, "\n"), "please rename the flag") {
		t.Error("the review comment should be in the brief")
	}
	// No new comments since the follow-up: nothing more happens.
	settle(t, d)
	wait(t, d)
	if fu, _ := d.Store.Followups(ctx, r.ID); len(fu) != 1 {
		t.Error("the same comments must not be addressed twice")
	}
	// The human closes it: feedback is captured on the record.
	gh.mu.Lock()
	p := gh.prs["u"]
	p.State = "CLOSED"
	gh.prs["u"] = p
	gh.humanComments["u"] = append(gh.humanComments["u"], github.Comment{Author: "pete", At: time.Now(), Body: "not this way", Where: "comment"})
	gh.mu.Unlock()
	settle(t, d)
	got, _ := d.Store.GetRun(ctx, r.ID)
	if got.Status != store.Rejected || !strings.Contains(got.Feedback, "not this way") || !strings.Contains(got.Feedback, "please rename the flag") {
		t.Errorf("feedback not recorded: %+v", got)
	}
	// And it shows up in the next brief for the same job.
	a.prompts = nil
	if _, err := d.Trigger(ctx, "audit", "x"); err != nil {
		t.Fatal(err)
	}
	wait(t, d)
	if !strings.Contains(strings.Join(a.prompts, "\n"), "not this way") {
		t.Error("history with reviewer feedback should reach the next run of the job")
	}
}

func TestBlockedFixDoesNotBurnTheCap(t *testing.T) {
	ctx := context.Background()
	gh := &fakeGH{prs: map[string]github.PR{"u": {State: "OPEN", Checks: "FAILURE"}}}
	d, a, dir := newDaemonDir(t, gh)
	// The fix touches a locked path, so the harness blocks it.
	a.fix = func(wd string) {
		os.MkdirAll(filepath.Join(wd, ".github/workflows"), 0o755)
		os.WriteFile(filepath.Join(wd, ".github/workflows/release.yml"), []byte("x"), 0o644)
	}
	r := proposalIn(t, d, gh, dir, "hardening", "u", time.Now())
	settle(t, d)
	wait(t, d)
	fu, _ := d.Store.Followups(ctx, r.ID)
	if len(fu) != 1 || fu[0].Status != store.Blocked {
		t.Fatalf("%+v", fu)
	}
	// Same head: one retry after a block, which blocks again; then stop.
	settle(t, d)
	wait(t, d)
	settle(t, d)
	wait(t, d)
	fu, _ = d.Store.Followups(ctx, r.ID)
	if len(fu) != 2 {
		t.Fatalf("one retry after a block on the same head, got %d attempts", len(fu))
	}
	// Blocked attempts did not count: the PR is not handed to a human.
	if len(gh.labels) != 0 {
		t.Errorf("blocked attempts must not reach the cap: %v", gh.labels)
	}
	// Policy is now satisfied: a real attempt follows on a new head.
	a.fix = func(wd string) { os.WriteFile(filepath.Join(wd, "ok.txt"), []byte("x"), 0o644) }
	p := gh.prs["u"]
	p.HeadSHA = "h2"
	gh.prs["u"] = p
	settle(t, d)
	wait(t, d)
	fu, _ = d.Store.Followups(ctx, r.ID)
	if len(fu) != 3 || fu[2].Status != store.Pushed {
		t.Errorf("%+v", fu)
	}
}

func TestFixAttemptsCanBeDisabled(t *testing.T) {
	gh := &fakeGH{prs: map[string]github.PR{"u": {State: "OPEN", Checks: "FAILURE"}}}
	d, a, dir := newDaemonDir(t, gh)
	a.fix = func(wd string) { os.WriteFile(filepath.Join(wd, "f.txt"), []byte("x"), 0o644) }
	d.Fleet.Policy.Caps.CheckFixAttempts = -1
	r := proposalIn(t, d, gh, dir, "hardening", "u", time.Now())
	settle(t, d)
	wait(t, d)
	if fu, _ := d.Store.Followups(context.Background(), r.ID); len(fu) != 0 {
		t.Error("-1 disables follow-ups")
	}
}

func TestVerifierAlwaysOnReviewPRs(t *testing.T) {
	ctx := context.Background()
	gh := &fakeGH{prs: map[string]github.PR{"u": {State: "OPEN", Checks: "SUCCESS"}}}
	d, _, dir := newDaemonDir(t, gh)
	// Class stays at review, autonomy is off: nothing may merge, but with
	// always on the verifier still comments.
	d.Store.SetSetting(ctx, autonomyKey, "off")
	proposalIn(t, d, gh, dir, "hardening", "u", time.Now())
	settle(t, d)
	if len(gh.comments) != 0 {
		t.Fatal("without always, a review-level PR is not verified")
	}
	d.Fleet.Policy.Verifier.Always = true
	settle(t, d)
	if len(gh.comments) != 1 || len(gh.merged) != 0 {
		t.Errorf("comments=%d merged=%d", len(gh.comments), len(gh.merged))
	}
}

func TestSoakAndKillSwitchAndCap(t *testing.T) {
	ctx := context.Background()
	gh := &fakeGH{prs: map[string]github.PR{
		"fresh": {State: "OPEN", Checks: "SUCCESS"},
		"old":   {State: "OPEN", Checks: "SUCCESS"},
		"old2":  {State: "OPEN", Checks: "SUCCESS"},
	}}
	d, _, dir := newDaemonDir(t, gh)
	d.Store.PutTrust(ctx, store.Trust{Repo: "x", Class: "hardening", Level: "soak"})
	proposalIn(t, d, gh, dir, "hardening", "fresh", time.Now())
	proposalIn(t, d, gh, dir, "hardening", "old", time.Now().Add(-25*time.Hour))
	proposalIn(t, d, gh, dir, "hardening", "old2", time.Now().Add(-26*time.Hour))

	d.Store.SetSetting(ctx, autonomyKey, "off")
	settle(t, d)
	if len(gh.merged) != 0 {
		t.Fatal("kill switch off must stop every merge")
	}
	d.Store.SetSetting(ctx, autonomyKey, "on")
	settle(t, d)
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
	settle(t, d)
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

func TestScheduleStaggersAndCanBeOff(t *testing.T) {
	ctx := context.Background()
	gh := &fakeGH{prs: map[string]github.PR{}}
	d, a := newDaemon(t, gh)
	// A second repo so two daily jobs are due at once.
	d.Fleet.Repos = append(d.Fleet.Repos, config.Repo{Name: "y", URL: d.Fleet.Repos[0].URL, Jobs: []string{"audit"}})
	d.NoSchedule = true
	d.tick(ctx)
	wait(t, d)
	if runs, _ := d.Store.ListRuns(ctx, "", 0); len(runs) != 0 {
		t.Fatal("scheduler off must start nothing")
	}
	d.NoSchedule = false
	d.tick(ctx)
	wait(t, d)
	if runs, _ := d.Store.ListRuns(ctx, "", 0); len(runs) != 1 {
		t.Fatalf("one tick starts one job, got %d", len(runs))
	}
	d.tick(ctx)
	wait(t, d)
	if runs, _ := d.Store.ListRuns(ctx, "", 0); len(runs) != 2 {
		t.Fatalf("next tick starts the next, got %d", len(runs))
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.calls != 2 {
		t.Errorf("calls = %d", a.calls)
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

func TestNeverFloorHoldsMerge(t *testing.T) {
	ctx := context.Background()
	gh := &fakeGH{prs: map[string]github.PR{"pr": {State: "OPEN", Checks: "SUCCESS", Review: "APPROVED"}}}
	d, _, dir := newDaemonDir(t, gh)
	d.Store.PutTrust(ctx, store.Trust{Repo: "x", Class: "hardening", Level: "auto"})
	d.Fleet.Never.AutoMerge = []string{"x/*"}
	proposalIn(t, d, gh, dir, "hardening", "pr", time.Now())
	settle(t, d)
	if len(gh.merged) != 0 {
		t.Error("never.auto_merge must beat an auto level and green checks")
	}
}

func TestRepeatFailuresNotifyOnce(t *testing.T) {
	ctx := context.Background()
	d, a := newDaemon(t, &fakeGH{prs: map[string]github.PR{}})
	c := withNotify(t, d)
	for i := 0; i < 3; i++ {
		a.fail = fmt.Errorf("clone failed in /tmp/marshal-%d after %ds", i, i+3)
		if _, err := d.Trigger(ctx, "audit", "x"); err != nil {
			t.Fatal(err)
		}
		wait(t, d)
	}
	if got := strings.Join(c.kinds(), ","); got != "failed" {
		t.Errorf("notifications = %s, want one failed", got)
	}
	st := d.Handle(ctx, api.Request{Op: "status"}).Status
	inc := d.Handle(ctx, api.Request{Op: "incidents"}).Incidents
	if st.Incidents != 1 || len(inc) != 1 || inc[0].Count != 3 {
		t.Fatalf("status=%d incidents=%+v", st.Incidents, inc)
	}
	if r := d.Handle(ctx, api.Request{Op: "ack", ID: inc[0].Signature}); r.Error != "" {
		t.Fatal(r.Error)
	}
	if st := d.Handle(ctx, api.Request{Op: "status"}).Status; st.Incidents != 0 {
		t.Error("acked incident still counted")
	}
	// A success resolves it.
	a.fail = nil
	d.Trigger(ctx, "audit", "x")
	wait(t, d)
	if inc := d.Handle(ctx, api.Request{Op: "incidents"}).Incidents; len(inc) != 0 {
		t.Errorf("still open after success: %+v", inc)
	}
}

func TestQuotaHoldParksTheFleet(t *testing.T) {
	ctx := context.Background()
	d, a := newDaemon(t, &fakeGH{prs: map[string]github.PR{}})
	c := withNotify(t, d)
	now := time.Now()
	d.now = func() time.Time { return now }
	a.fail = &runner.Limit{Text: "You've hit your limit", Until: now.Add(2 * time.Hour)}
	d.Trigger(ctx, "audit", "x")
	wait(t, d)
	st := d.Handle(ctx, api.Request{Op: "status"}).Status
	if !st.QuotaUntil.Equal(now.Add(2*time.Hour).Truncate(time.Second)) || st.Incidents != 0 {
		t.Fatalf("hold=%v incidents=%d", st.QuotaUntil, st.Incidents)
	}
	calls := a.calls
	if _, err := d.Trigger(ctx, "audit", "x"); err == nil || !strings.Contains(err.Error(), "quota hold") {
		t.Errorf("trigger during hold: %v", err)
	}
	d.tick(ctx) // schedules nothing, announces once
	d.tick(ctx)
	wait(t, d)
	if a.calls != calls {
		t.Errorf("agent ran during the hold: %d calls", a.calls-calls)
	}
	if got := strings.TrimPrefix(strings.Join(c.kinds(), ","), "digest,"); got != "needs-human" {
		t.Errorf("notifications = %s, want one hold notice and no failure", got)
	}
	// Past the window, work resumes without anyone touching it.
	now = now.Add(3 * time.Hour)
	a.fail = nil
	if _, err := d.Trigger(ctx, "audit", "x"); err != nil {
		t.Errorf("after the hold: %v", err)
	}
	wait(t, d)
	// The operator can lift a hold early.
	d.Store.SetQuotaHold(ctx, now.Add(time.Hour), "x")
	d.Handle(ctx, api.Request{Op: "quota", Value: "clear"})
	if _, err := d.Trigger(ctx, "audit", "x"); err != nil {
		t.Errorf("after clear: %v", err)
	}
	wait(t, d)
}

func TestNothingElseRunsWhileOnboarding(t *testing.T) {
	ctx := context.Background()
	gh := &fakeGH{prs: map[string]github.PR{"onb": {State: "OPEN", Checks: "PENDING"}}}
	d, a := newDaemon(t, gh)
	d.Store.CreateRun(ctx, &store.Run{Job: "onboard", Repo: "x", Class: "hardening", Status: store.Proposed, PRURL: "onb", StartedAt: time.Now()})
	if err := d.schedule(ctx); err != nil {
		t.Fatal(err)
	}
	wait(t, d)
	if a.calls != 0 {
		t.Errorf("a job ran on a repo whose onboarding PR is open: %d calls", a.calls)
	}
}

func TestNoFollowupsForUnknownJobs(t *testing.T) {
	ctx := context.Background()
	gh := &fakeGH{prs: map[string]github.PR{"sk": {State: "OPEN", Checks: "FAILURE", HeadSHA: "abc"}}}
	d, a := newDaemon(t, gh)
	d.Store.CreateRun(ctx, &store.Run{Job: "skill", Repo: "x", Class: "hardening", Status: store.Proposed, PRURL: "sk", StartedAt: time.Now()})
	if err := d.syncProposals(ctx); err != nil {
		t.Fatal(err)
	}
	wait(t, d)
	if a.calls != 0 || len(d.inFlight()) != 0 {
		t.Errorf("follow-up started for a skill proposal: %d calls", a.calls)
	}
}

// dependabotPR puts an open Dependabot PR with a real branch in the origin.
func dependabotPR(t *testing.T, d *Daemon, gh *fakeGH, dir, url, checks string, level config.Level) {
	t.Helper()
	d.Fleet.Policy.Classes["dependabot"] = config.Class{Start: level, DemoteOnRevert: 2}
	ref := "dependabot/go_modules/x-" + url
	sha := branch(t, dir, ref)
	pr := github.PR{URL: url, State: "OPEN", Checks: checks, HeadRef: ref, HeadSHA: sha, Title: "Bump x from 1 to 2", CreatedAt: time.Now().Add(-time.Hour)}
	gh.mu.Lock()
	gh.prs[url] = pr
	gh.deps = append(gh.deps, pr)
	gh.mu.Unlock()
}

func TestDependabotPassIsApprovedAndMerged(t *testing.T) {
	ctx := context.Background()
	gh := &fakeGH{prs: map[string]github.PR{}}
	d, a, dir := newDaemonDir(t, gh)
	a.verdict = "PASS"
	dependabotPR(t, d, gh, dir, "dep1", "SUCCESS", config.Auto)
	d.discoverDependabot(ctx)
	d.discoverDependabot(ctx) // within the scan interval: no second call
	if gh.depCalls != 1 {
		t.Errorf("dependabot listed %d times", gh.depCalls)
	}
	settle(t, d)
	if len(gh.approved) != 1 || len(gh.merged) != 1 {
		t.Fatalf("approved=%v merged=%v", gh.approved, gh.merged)
	}
	merged, _ := d.Store.ListByStatus(ctx, store.Merged)
	if len(merged) != 1 || merged[0].Job != "dependabot" || merged[0].MergedBy != "auto" {
		t.Errorf("merged runs: %+v", merged)
	}
	// Seen again later: never recorded twice.
	d.scanned = nil
	d.discoverDependabot(ctx)
	if runs, _ := d.Store.ListRuns(ctx, "x", 0); len(runs) != 1 {
		t.Errorf("runs = %d", len(runs))
	}
}

func TestDependabotAtReviewIsApprovedNotMerged(t *testing.T) {
	gh := &fakeGH{prs: map[string]github.PR{}}
	d, a, dir := newDaemonDir(t, gh)
	a.verdict = "PASS"
	dependabotPR(t, d, gh, dir, "dep2", "SUCCESS", config.Review)
	d.discoverDependabot(context.Background())
	settle(t, d)
	settle(t, d)
	if len(gh.approved) != 1 || len(gh.merged) != 0 {
		t.Errorf("approved=%v merged=%v", gh.approved, gh.merged)
	}
}

func TestDependabotFailIsClosedWithoutDemotion(t *testing.T) {
	ctx := context.Background()
	gh := &fakeGH{prs: map[string]github.PR{}}
	d, a, dir := newDaemonDir(t, gh)
	a.verdict = "FAIL"
	dependabotPR(t, d, gh, dir, "dep3", "SUCCESS", config.Soak)
	d.discoverDependabot(ctx)
	settle(t, d)
	if len(gh.closed) != 1 || len(gh.merged) != 0 || len(gh.approved) != 0 {
		t.Fatalf("closed=%v merged=%v approved=%v", gh.closed, gh.merged, gh.approved)
	}
	settle(t, d)
	if got := level(t, d, "dependabot"); got != "soak" && got != "?" {
		t.Errorf("closing on a verdict must not demote the class: %s", got)
	}
}

func TestDependabotWaitsForCI(t *testing.T) {
	gh := &fakeGH{prs: map[string]github.PR{}}
	d, a, dir := newDaemonDir(t, gh)
	a.verdict = "PASS"
	dependabotPR(t, d, gh, dir, "dep4", "PENDING", config.Auto)
	d.discoverDependabot(context.Background())
	settle(t, d)
	if v, ok, _ := d.Store.LatestVerdict(context.Background(), mustRun(t, d).ID); ok {
		t.Errorf("verified while CI was pending: %+v", v)
	}
}

func mustRun(t *testing.T, d *Daemon) store.Run {
	t.Helper()
	runs, _ := d.Store.ListRuns(context.Background(), "x", 0)
	if len(runs) != 1 {
		t.Fatalf("runs = %d", len(runs))
	}
	return runs[0]
}

func TestUnstartedVerifyRecordsNoVerdict(t *testing.T) {
	ctx := context.Background()
	gh := &fakeGH{prs: map[string]github.PR{"nobranch": {State: "OPEN", Checks: "SUCCESS", HeadSHA: "deadbeef"}}}
	d, _ := newDaemon(t, gh)
	r := proposal(t, d, "hardening", "nobranch", time.Now()) // no branch in the origin: checkout fails
	d.startVerify(ctx, r, gh.prs["nobranch"])
	waitVerify(t, d)
	if v, ok, _ := d.Store.LatestVerdict(ctx, r.ID); ok {
		t.Errorf("a verification that never started left a verdict: %+v", v)
	}
	if len(gh.labels) != 0 || len(gh.comments) != 0 {
		t.Errorf("nothing should be posted: labels=%v comments=%v", gh.labels, gh.comments)
	}
}

func TestDependabotRedCIAndUnsureIsClosed(t *testing.T) {
	gh := &fakeGH{prs: map[string]github.PR{}}
	d, a, dir := newDaemonDir(t, gh)
	a.verdict = "UNSURE"
	dependabotPR(t, d, gh, dir, "dep5", "FAILURE", config.Soak)
	d.discoverDependabot(context.Background())
	settle(t, d)
	if len(gh.closed) != 1 {
		t.Errorf("red CI and no PASS should close: closed=%v", gh.closed)
	}
	// Green CI with UNSURE stays open for a human.
	gh2 := &fakeGH{prs: map[string]github.PR{}}
	d2, a2, dir2 := newDaemonDir(t, gh2)
	a2.verdict = "UNSURE"
	dependabotPR(t, d2, gh2, dir2, "dep6", "SUCCESS", config.Soak)
	d2.discoverDependabot(context.Background())
	settle(t, d2)
	if len(gh2.closed) != 0 || len(gh2.approved) != 0 {
		t.Errorf("green and unsure: closed=%v approved=%v", gh2.closed, gh2.approved)
	}
}
