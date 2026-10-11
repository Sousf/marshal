package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Sousf/marshal/internal/config"
	"github.com/Sousf/marshal/internal/github"
	"github.com/Sousf/marshal/internal/notify"
	"github.com/Sousf/marshal/internal/store"
)

// capture is a notify target that remembers what it was sent.
type capture struct {
	mu     sync.Mutex
	events []notify.Event
}

func (c *capture) Send(_ context.Context, e notify.Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
	return nil
}

func (c *capture) kinds() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, e := range c.events {
		out = append(out, string(e.Kind))
	}
	return out
}

func withNotify(t *testing.T, d *Daemon) *capture {
	t.Helper()
	c := &capture{}
	m := notify.NewForTest(c, notify.Proposed, notify.Merged, notify.NeedsHuman, notify.Failed, notify.Reverted, notify.Digest)
	d.Notify = m
	return c
}

func TestReconcile(t *testing.T) {
	ctx := context.Background()
	gh := &fakeGH{prs: map[string]github.PR{}}
	d, _ := newDaemon(t, gh)
	zombie := &store.Run{Job: "audit", Repo: "x", Status: store.Running}
	d.Store.CreateRun(ctx, zombie)
	os.MkdirAll(filepath.Join(d.WorkRoot, "run-dead-123"), 0o755)
	os.MkdirAll(filepath.Join(d.WorkRoot, "verify-dead-456"), 0o755)
	os.MkdirAll(filepath.Join(d.WorkRoot, "keep-me"), 0o755)
	d.Reconcile(ctx)
	got, _ := d.Store.GetRun(ctx, zombie.ID)
	if got.Status != store.Cancelled || !strings.Contains(got.Error, "restarted") {
		t.Errorf("%+v", got)
	}
	if _, err := os.Stat(filepath.Join(d.WorkRoot, "run-dead-123")); !os.IsNotExist(err) {
		t.Error("orphaned run tree should be removed")
	}
	if _, err := os.Stat(filepath.Join(d.WorkRoot, "verify-dead-456")); !os.IsNotExist(err) {
		t.Error("orphaned verify tree should be removed")
	}
	if _, err := os.Stat(filepath.Join(d.WorkRoot, "keep-me")); err != nil {
		t.Error("unrelated directories are left alone")
	}
}

// machineMerge records a run MARSHAL merged itself, with a real commit on
// the origin's main as the merge commit.
func machineMerge(t *testing.T, d *Daemon, dir, by string, when time.Time) store.Run {
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
	name := "merged-" + time.Now().Format("150405.000000000") + ".txt"
	os.WriteFile(filepath.Join(dir, name), []byte("merged\n"), 0o644)
	git("add", ".")
	git("commit", "-q", "-m", "merged change "+name)
	sha := git("rev-parse", "HEAD")
	r := &store.Run{Job: "audit", Repo: "x", Class: "hardening", Status: store.Merged, StartedAt: when}
	d.Store.CreateRun(context.Background(), r)
	r.MergedBy = by
	r.MergeSHA = sha
	r.PRURL = "pr-" + r.ID
	r.FinishedAt = when
	d.Store.UpdateRun(context.Background(), r)
	return *r
}

func TestWatchSettlesOnGreen(t *testing.T) {
	ctx := context.Background()
	gh := &fakeGH{prs: map[string]github.PR{}, commitCI: github.CI{Status: "completed", Conclusion: "success"}}
	d, _, dir := newDaemonDir(t, gh)
	r := machineMerge(t, d, dir, "auto", time.Now())
	if err := d.watchMerges(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := d.Store.GetRun(ctx, r.ID)
	if !got.Settled || got.Status != store.Merged {
		t.Errorf("%+v", got)
	}
	if un, _ := d.Store.Unsettled(ctx); len(un) != 0 {
		t.Error("settled runs are not watched again")
	}
}

func TestWatchRevertsOnRed(t *testing.T) {
	ctx := context.Background()
	gh := &fakeGH{prs: map[string]github.PR{}, commitCI: github.CI{Status: "completed", Conclusion: "failure", URL: "https://ci/run/1"}}
	d, _, dir := newDaemonDir(t, gh)
	c := withNotify(t, d)
	d.Store.PutTrust(ctx, store.Trust{Repo: "x", Class: "hardening", Level: "auto"})
	r := machineMerge(t, d, dir, "soak", time.Now())
	if err := d.watchMerges(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := d.Store.GetRun(ctx, r.ID)
	if got.Status != store.Reverted || !got.Settled {
		t.Fatalf("%+v", got)
	}
	// A revert PR was created and merged by the app.
	if len(gh.created) != 1 || !strings.HasPrefix(gh.created[0].title, "Revert MARSHAL merge") || gh.created[0].base != "main" {
		t.Errorf("created = %+v", gh.created)
	}
	if len(gh.merged) != 1 {
		t.Errorf("revert should be merged at once: %v", gh.merged)
	}
	// The revert branch on the origin undoes the merge commit.
	cmd := exec.Command("git", "diff", "--stat", r.MergeSHA+"~1", gh.created[0].head)
	cmd.Dir = dir
	if out, _ := cmd.CombinedOutput(); strings.TrimSpace(string(out)) != "" {
		t.Errorf("revert branch should equal the pre-merge tree, diff:\n%s", out)
	}
	// Two steps down from auto.
	if lvl := level(t, d, "hardening"); lvl != "review" {
		t.Errorf("revert should demote two steps: %s", lvl)
	}
	k := c.kinds()
	if len(k) != 1 || k[0] != "reverted" {
		t.Errorf("notifications = %v", k)
	}
	if !c.events[0].Urgent {
		t.Error("a revert is urgent")
	}
}

func TestWatchWindowAndHumanMergesIgnored(t *testing.T) {
	ctx := context.Background()
	gh := &fakeGH{prs: map[string]github.PR{}, commitCINone: true}
	d, _, dir := newDaemonDir(t, gh)
	old := machineMerge(t, d, dir, "auto", time.Now().Add(-30*time.Hour))
	fresh := machineMerge(t, d, dir, "auto", time.Now())
	human := machineMerge(t, d, dir, "human", time.Now())
	d.watchMerges(ctx)
	o, _ := d.Store.GetRun(ctx, old.ID)
	f, _ := d.Store.GetRun(ctx, fresh.ID)
	h, _ := d.Store.GetRun(ctx, human.ID)
	if !o.Settled {
		t.Error("past the window with no CI: settled")
	}
	if f.Settled {
		t.Error("fresh merge with CI not yet reported: keep watching")
	}
	if h.Settled || h.Status != store.Merged {
		t.Error("human merges are not watched at all (and untouched)")
	}
	if len(gh.created) != 0 {
		t.Error("nothing reverted")
	}
}

func TestNotificationsAndDigest(t *testing.T) {
	ctx := context.Background()
	gh := &fakeGH{prs: map[string]github.PR{"u": {State: "MERGED", MergedBy: "pete"}}}
	d, _, _ := newDaemonDir(t, gh)
	c := withNotify(t, d)
	proposal(t, d, "hardening", "u", time.Now())
	d.syncProposals(ctx)
	if k := c.kinds(); len(k) != 1 || k[0] != "merged" {
		t.Errorf("human merge should notify merged: %v", k)
	}
	// Digest: due at 00:00 so it is always past; sent once per day.
	d.Fleet.Notify.Digest = "00:00"
	d.maybeDigest(ctx)
	d.maybeDigest(ctx)
	k := c.kinds()
	if len(k) != 2 || k[1] != "digest" {
		t.Errorf("digest once: %v", k)
	}
	// The CLI calls DigestText on a Daemon with only Fleet and Store set.
	bare := &Daemon{Fleet: d.Fleet, Store: d.Store}
	if _, err := bare.DigestText(ctx); err != nil {
		t.Fatalf("bare digest: %v", err)
	}
	text, _ := d.DigestText(ctx)
	if !strings.Contains(text, "1 run(s)") || !strings.Contains(text, "1 merged") || !strings.Contains(text, "Nothing is waiting") || !strings.Contains(text, "Autonomy on") {
		t.Errorf("digest:\n%s", text)
	}
}

func TestTasksTriggerAndSchedule(t *testing.T) {
	ctx := context.Background()
	gh := &fakeGH{prs: map[string]github.PR{}}
	d, a, _ := newDaemonDir(t, gh)
	t.Setenv("MARSHAL_STATE", t.TempDir())
	id, err := d.TriggerTask(ctx, "", "write a haiku about CI", "", "")
	if err != nil || len(id) != 8 {
		t.Fatal(err)
	}
	wait(t, d)
	r, _ := d.Store.GetRun(ctx, id)
	if r.Status != store.Reported || r.Job != "task" || r.Repo != "" {
		t.Errorf("%+v", r)
	}
	if _, err := d.TriggerTask(ctx, "", "", "", ""); err == nil {
		t.Error("empty prompt must fail")
	}
	if _, err := d.TriggerTask(ctx, "", "x", "nope", ""); err == nil {
		t.Error("unknown repo must fail")
	}
	// A repo task becomes a PR-style run and respects the busy rule.
	id2, err := d.TriggerTask(ctx, "", "tidy go.mod", "x", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.TriggerTask(ctx, "", "again", "x", ""); err == nil {
		t.Error("repo busy")
	}
	wait(t, d)
	r2, _ := d.Store.GetRun(ctx, id2)
	if r2.Repo != "x" || r2.Class != "code" || r2.Status != store.NoChange {
		t.Errorf("%+v", r2)
	}
	// Scheduled tasks fire once per interval.
	d.Fleet.Tasks = []config.Task{{Name: "weekly", Schedule: "weekly", Prompt: "report drift"}}
	before := a.calls
	d.scheduleTasks(ctx)
	wait(t, d)
	d.scheduleTasks(ctx)
	wait(t, d)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.calls != before+1 {
		t.Errorf("scheduled task should run once, ran %d", a.calls-before)
	}
	if last, _ := d.Store.LastStarted(ctx, "task:weekly", ""); last.IsZero() {
		t.Error("scheduled task recorded under task:<name>")
	}
}
