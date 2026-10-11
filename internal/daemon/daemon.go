// Package daemon is the always-on half of MARSHAL. It fires jobs on their
// schedules, watches the pull requests they opened, moves the trust ladder
// when a human decides, and merges on its own only where the ladder says it
// may and every gate agrees.
package daemon

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Sousf/marshal/internal/api"
	"github.com/Sousf/marshal/internal/config"
	"github.com/Sousf/marshal/internal/ghapp"
	"github.com/Sousf/marshal/internal/github"
	"github.com/Sousf/marshal/internal/notify"
	"github.com/Sousf/marshal/internal/policy"
	"github.com/Sousf/marshal/internal/run"
	"github.com/Sousf/marshal/internal/runner"
	"github.com/Sousf/marshal/internal/sched"
	"github.com/Sousf/marshal/internal/store"
	"github.com/Sousf/marshal/internal/verifier"
	"github.com/Sousf/marshal/internal/workspace"
)

// GitHub is what the daemon needs from GitHub, as an interface so tests can
// script it.
type GitHub interface {
	ViewPR(ctx context.Context, url string) (github.PR, error)
	MergePR(ctx context.Context, url string) error
	CommentPR(ctx context.Context, url, body string) error
	AddLabel(ctx context.Context, url, label string) error
	LatestCI(ctx context.Context, slug, branch string) (github.CI, bool, error)
	DefaultBranch(ctx context.Context, slug string) (string, error)
	FailedLogs(ctx context.Context, slug, sha string, limit int) (string, error)
	HumanComments(ctx context.Context, url, exclude string, since time.Time) ([]github.Comment, error)
	CommitCI(ctx context.Context, slug, sha string) (github.CI, bool, error)
	CreatePR(ctx context.Context, slug, head, base, title, body string, labels []string) (string, error)
}

// Daemon holds the loop's state.
type Daemon struct {
	Fleet   *config.Fleet
	Store   *store.Store
	Adapter runner.Adapter
	GH      GitHub
	Version string
	// Tick is how often the loop runs. Zero means one minute.
	Tick time.Duration
	// WorkRoot is passed to the executor. Empty means the default.
	WorkRoot string
	// Auth, when set, is the GitHub App every run and verification acts as.
	Auth *ghapp.App
	Log  *log.Logger
	// NoSchedule keeps the daemon from starting jobs on its own. It still
	// syncs proposals, verifies, merges where trusted, and takes triggers.
	NoSchedule bool
	// Notify is where events go. nil is fine.
	Notify *notify.Multi

	mu        sync.Mutex
	running   map[string]string // repo -> run ID
	verifying map[string]bool   // run ID -> in progress
	started   time.Time
	// base is the lifetime of the daemon. Runs and verifications hang off it
	// rather than off the request that started them, so a closed socket
	// connection does not cancel a run but a stopping daemon does, taking
	// its agent subprocesses with it instead of orphaning them.
	base     context.Context
	lastTick time.Time
	lastErr  string
	// now is swapped in tests.
	now func() time.Time
}

// maxConcurrentTasks bounds repo-less tasks in flight at once.
const maxConcurrentTasks = 3

// autonomyKey is the settings row for the kill switch.
const autonomyKey = "autonomy"

func (d *Daemon) init() {
	if d.running == nil {
		d.running = map[string]string{}
	}
	if d.verifying == nil {
		d.verifying = map[string]bool{}
	}
	if d.now == nil {
		d.now = time.Now
	}
	if d.Log == nil {
		d.Log = log.Default()
	}
	if d.started.IsZero() {
		d.started = d.now()
	}
	if d.base == nil {
		d.base = context.Background()
	}
}

// Run executes ticks until ctx is done.
func (d *Daemon) Run(ctx context.Context) error {
	d.base = ctx
	d.init()
	tick := d.Tick
	if tick == 0 {
		tick = time.Minute
	}
	d.Log.Printf("marshal %s: %d repos, %d jobs, tick %s", d.Version, len(d.Fleet.Repos), len(d.Fleet.Jobs), tick)
	if d.NoSchedule {
		d.Log.Printf("scheduler off: jobs start only by trigger")
	}
	d.Reconcile(ctx)
	t := time.NewTicker(tick)
	defer t.Stop()
	d.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			n := len(d.inFlight())
			d.Log.Printf("stopping; cancelling %d run(s) in flight", n)
			// Give the executors a moment to record the cancellation and
			// remove their work trees before the process exits.
			deadline := time.Now().Add(10 * time.Second)
			for len(d.inFlight()) > 0 && time.Now().Before(deadline) {
				time.Sleep(100 * time.Millisecond)
			}
			return nil
		case <-t.C:
			d.tick(ctx)
		}
	}
}

// Reconcile cleans up after a daemon that did not shut down cleanly. Any run
// still marked running belongs to a process that no longer exists, and any
// work tree on disk is an orphan, because every live run holds its own and
// removes it on exit.
func (d *Daemon) Reconcile(ctx context.Context) {
	d.init()
	ids, err := d.Store.CancelRunning(ctx, "daemon restarted while the run was in progress")
	if err != nil {
		d.fail("reconcile: %v", err)
	}
	for _, id := range ids {
		_ = d.Store.AppendEvent(ctx, id, store.Event{Kind: "note", Text: "cancelled: the daemon restarted while this run was in progress"})
		d.Log.Printf("run %s: cancelled, the daemon restarted while it was in progress", id)
	}
	root := d.WorkRoot
	if root == "" {
		root = filepath.Join(config.StateDir(), "work")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	swept := 0
	for _, e := range entries {
		if e.IsDir() && (strings.HasPrefix(e.Name(), "run-") || strings.HasPrefix(e.Name(), "verify-")) {
			if os.RemoveAll(filepath.Join(root, e.Name())) == nil {
				swept++
			}
		}
	}
	if swept > 0 {
		d.Log.Printf("reconcile: removed %d orphaned work tree(s) under %s", swept, root)
	}
}

// tick is one pass: settle what humans decided, merge what may merge, start
// what is due.
func (d *Daemon) tick(ctx context.Context) {
	d.init()
	d.mu.Lock()
	d.lastTick = d.now()
	d.mu.Unlock()
	if err := d.syncProposals(ctx); err != nil {
		d.fail("sync: %v", err)
	}
	if err := d.watchMerges(ctx); err != nil {
		d.fail("watch: %v", err)
	}
	d.maybeDigest(ctx)
	if d.NoSchedule {
		return
	}
	if err := d.schedule(ctx); err != nil {
		d.fail("schedule: %v", err)
	}
	if err := d.scheduleTasks(ctx); err != nil {
		d.fail("tasks: %v", err)
	}
}

func (d *Daemon) fail(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	d.Log.Print(msg)
	d.mu.Lock()
	d.lastErr = msg
	d.mu.Unlock()
}

// Autonomy reports whether the kill switch is on.
func (d *Daemon) Autonomy(ctx context.Context) bool {
	v, err := d.Store.Setting(ctx, autonomyKey, "on")
	return err == nil && v == "on"
}

// syncProposals looks at every open proposal and applies what happened.
func (d *Daemon) syncProposals(ctx context.Context) error {
	runs, err := d.Store.ListByStatus(ctx, store.Proposed)
	if err != nil {
		return err
	}
	autonomy := d.Autonomy(ctx)
	vcfg := d.Fleet.ResolveVerifier()
	ladder := policy.Ladder{Fleet: d.Fleet}
	for _, r := range runs {
		if r.PRURL == "" {
			continue
		}
		pr, err := d.GH.ViewPR(ctx, r.PRURL)
		if err != nil {
			d.fail("run %s: %v", r.ID, err)
			continue
		}
		repo, _ := d.Fleet.Repo(r.Repo)
		switch {
		case pr.Merged():
			r.Status = store.Merged
			r.FinishedAt = d.now()
			if r.MergedBy == "" {
				r.MergedBy = "human"
			}
			r.Feedback = d.feedback(ctx, r)
			r.MergeSHA = pr.MergeSHA
			d.note(ctx, r.ID, "merged by %s", r.MergedBy)
			if r.MergedBy == "human" {
				d.move(ctx, ladder, repo, r.Class, policy.Approved)
				r.Settled = true // a human merge is not watched
				d.send(ctx, notify.Event{Kind: notify.Merged, Title: r.Repo + ": merged by you", Body: r.Job + " · " + firstLine(r.Summary), URL: r.PRURL})
			}
		case pr.Closed():
			r.Status = store.Rejected
			r.FinishedAt = d.now()
			r.Feedback = d.feedback(ctx, r)
			d.note(ctx, r.ID, "closed without merging")
			d.move(ctx, ladder, repo, r.Class, policy.Rejected)
		default:
			// Red checks on MARSHAL's own PR are MARSHAL's to fix, within a
			// cap. Nothing else happens to the PR while it is red.
			if pr.Checks == "FAILURE" {
				d.fixChecks(ctx, repo, r, pr)
				continue
			}
			// A human asked for changes: address them, within the same cap.
			if pr.Review == "CHANGES_REQUESTED" {
				d.addressReview(ctx, repo, r, pr)
				continue
			}
			by, reason := "", ""
			if autonomy {
				by, reason = d.mayMerge(ctx, ladder, repo, r, pr)
			}
			// Verification happens when a merge would otherwise be
			// autonomous, or on every proposal when the policy says so.
			if by == "" && !vcfg.Always {
				if reason != "" {
					d.Log.Printf("run %s: holding: %s", r.ID, reason)
				}
				continue
			}
			verdict, ok, err := d.Store.LatestVerdict(ctx, r.ID)
			if err != nil {
				return err
			}
			stale := !ok || (pr.HeadSHA != "" && verdict.Head != pr.HeadSHA)
			if stale {
				d.startVerify(ctx, r, pr)
				continue
			}
			if by == "" {
				continue
			}
			if verdict.Verdict != verifier.Pass {
				d.Log.Printf("run %s: holding: verifier said %s", r.ID, verdict.Verdict)
				continue
			}
			if err := d.GH.MergePR(ctx, r.PRURL); err != nil {
				d.fail("run %s: merge: %v", r.ID, err)
				continue
			}
			r.Status = store.Merged
			r.MergedBy = by
			r.FinishedAt = d.now()
			if after, err := d.GH.ViewPR(ctx, r.PRURL); err == nil {
				r.MergeSHA = after.MergeSHA
			}
			d.note(ctx, r.ID, "merged by marshal (%s); watching the default branch", by)
			_ = d.GH.CommentPR(ctx, r.PRURL, fmt.Sprintf("Merged by MARSHAL: class `%s` is at level `%s` for this repository, checks were green, the verifier passed head `%s`, and the %s gate passed. MARSHAL watches the default branch and reverts if it goes red.", r.Class, by, short(verdict.Head), by))
			d.send(ctx, notify.Event{Kind: notify.Merged, Title: r.Repo + ": merged by MARSHAL (" + by + ")", Body: r.Job + " · " + firstLine(r.Summary), URL: r.PRURL})
		}
		if err := d.Store.UpdateRun(ctx, &r); err != nil {
			return err
		}
	}
	return nil
}

// mayMerge applies every gate. It returns "auto" or "soak" when the PR may
// merge, else "" and the reason it is held.
func (d *Daemon) mayMerge(ctx context.Context, ladder policy.Ladder, repo config.Repo, r store.Run, pr github.PR) (string, string) {
	t, found, err := d.Store.GetTrust(ctx, repo.Name, r.Class)
	if err != nil {
		return "", err.Error()
	}
	t = ladder.Current(repo, r.Class, t, found)
	level := config.Level(t.Level)
	auto, soak := policy.MayMerge(level)
	if !auto {
		return "", "" // review or locked: nothing to log every tick
	}
	switch {
	case pr.Checks == "":
		return "", "no CI checks on the PR; a repo without CI gets no autonomy"
	case pr.Checks != "SUCCESS":
		return "", "checks " + pr.Checks
	case pr.Review == "CHANGES_REQUESTED":
		return "", "changes requested"
	case pr.HasLabel("hold"):
		return "", "hold label"
	}
	if soak {
		hours := d.Fleet.Policy.Classes[r.Class].SoakHours
		if hours <= 0 {
			hours = 24
		}
		if d.now().Sub(r.StartedAt) < time.Duration(hours)*time.Hour {
			return "", fmt.Sprintf("soaking until %s", r.StartedAt.Add(time.Duration(hours)*time.Hour).Format(time.RFC3339))
		}
	}
	if cap := d.Fleet.Policy.Caps.AutoMergesPerRepoPerDay; cap > 0 {
		n := 0
		for _, by := range []string{"auto", "soak"} {
			c, err := d.Store.CountMergedBy(ctx, repo.Name, by, d.now().Add(-24*time.Hour))
			if err != nil {
				return "", err.Error()
			}
			n += c
		}
		if n >= cap {
			return "", fmt.Sprintf("auto-merge cap reached (%d/24h)", cap)
		}
	}
	if soak {
		return "soak", ""
	}
	return "auto", ""
}

// feedback collects what humans said on a proposal, for the record and for
// the next brief of the same job.
func (d *Daemon) feedback(ctx context.Context, r store.Run) string {
	cs, err := d.GH.HumanComments(ctx, r.PRURL, d.botLogin(), time.Time{})
	if err != nil {
		d.fail("run %s: comments: %v", r.ID, err)
		return ""
	}
	return strings.TrimSpace(github.RenderComments(cs))
}

func (d *Daemon) botLogin() string {
	if d.Auth != nil {
		return d.Auth.Identity().Name
	}
	return ""
}

// addressReview starts a follow-up that acts on a reviewer's comments. Only
// comments newer than the last follow-up count, so the same request is not
// answered twice, and the per-PR cap is shared with check fixes.
func (d *Daemon) addressReview(ctx context.Context, repo config.Repo, r store.Run, pr github.PR) {
	cap := d.Fleet.Policy.Caps.FixAttempts()
	if cap == 0 || d.busy(repo.Name) {
		return
	}
	prior, err := d.Store.Followups(ctx, r.ID)
	if err != nil {
		d.fail("run %s: followups: %v", r.ID, err)
		return
	}
	since := r.StartedAt
	attempts := 0
	for _, f := range prior {
		if f.Status == store.Running {
			return
		}
		attempts++
		if f.StartedAt.After(since) {
			since = f.StartedAt
		}
	}
	cs, err := d.GH.HumanComments(ctx, r.PRURL, d.botLogin(), since)
	if err != nil {
		d.fail("run %s: comments: %v", r.ID, err)
		return
	}
	if len(cs) == 0 {
		return // nothing new since the last follow-up; the ball is with the human
	}
	if attempts >= cap {
		if !pr.HasLabel("marshal:needs-human") {
			d.note(ctx, r.ID, "review feedback after %d follow-up(s); handing to a human", attempts)
			_ = d.GH.CommentPR(ctx, r.PRURL, fmt.Sprintf("MARSHAL has made %d follow-up(s) on this pull request and stopped at the cap. The remaining feedback needs a human.", attempts))
			_ = d.GH.AddLabel(ctx, r.PRURL, "marshal:needs-human")
			d.send(ctx, notify.Event{Kind: notify.NeedsHuman, Title: r.Repo + ": review feedback waiting after " + fmt.Sprint(attempts) + " follow-up(s)", Body: r.Job + " · " + pr.Title, URL: r.PRURL})
		}
		return
	}
	d.mu.Lock()
	id := store.NewID()
	d.running[repo.Name] = id
	d.mu.Unlock()
	d.note(ctx, r.ID, "changes requested; starting follow-up %s to address %d comment(s)", id, len(cs))
	x := &run.Executor{Fleet: d.Fleet, Store: d.Store, Adapter: d.Adapter, WorkRoot: d.WorkRoot, Auth: d.Auth}
	comments := github.RenderComments(cs)
	go func() {
		defer func() {
			d.mu.Lock()
			delete(d.running, repo.Name)
			d.mu.Unlock()
		}()
		rec, err := x.FollowupKind(d.base, "address-review", r, pr, comments, run.Options{ID: id})
		if err != nil {
			d.fail("run %s: %v", id, err)
			return
		}
		d.Log.Printf("run %s: %s (review follow-up for %s)", id, rec.Status, r.ID)
		switch rec.Status {
		case store.Pushed:
			_ = d.GH.CommentPR(d.base, r.PRURL, "MARSHAL addressed the review (run `"+id+"`).\n\n"+rec.Summary)
		case store.NoChange:
			_ = d.GH.CommentPR(d.base, r.PRURL, "MARSHAL read the review and made no change (run `"+id+"`).\n\n"+rec.Summary)
		case store.Failed, store.Blocked:
			msg := rec.Error
			if msg == "" {
				msg = rec.Summary
			}
			_ = d.GH.CommentPR(d.base, r.PRURL, fmt.Sprintf("MARSHAL could not address the review (run `%s`, %s).\n\n%s", id, rec.Status, msg))
		}
	}()
}

// fixChecks starts a follow-up run that pushes a fix to a red proposal.
// Each failing head is attempted once; past the cap the PR is labelled for
// a human and left alone.
func (d *Daemon) fixChecks(ctx context.Context, repo config.Repo, r store.Run, pr github.PR) {
	cap := d.Fleet.Policy.Caps.FixAttempts()
	if cap == 0 {
		return
	}
	prior, err := d.Store.Followups(ctx, r.ID)
	if err != nil {
		d.fail("run %s: followups: %v", r.ID, err)
		return
	}
	attempts, blockedHere := 0, 0
	for _, f := range prior {
		if f.Status == store.Running {
			return // one at a time
		}
		if f.BaseHead == pr.HeadSHA && pr.HeadSHA != "" {
			// The agent already worked on this head. A Blocked attempt is
			// the harness refusing the diff, not the agent failing, so one
			// retry is allowed in case policy changed; a second block on
			// the same head means it has not.
			if f.Status != store.Blocked {
				return
			}
			blockedHere++
			if blockedHere >= 2 {
				return
			}
		}
		// Blocked attempts do not count toward the cap: the agent did its
		// work and the harness said no, which is MARSHAL's cost, not the
		// PR's chance.
		if f.Status == store.Pushed || f.Status == store.NoChange || f.Status == store.Failed {
			attempts++
		}
	}
	if attempts >= cap {
		if !pr.HasLabel("marshal:needs-human") {
			d.note(ctx, r.ID, "checks still failing after %d fix attempt(s); handing to a human", attempts)
			_ = d.GH.CommentPR(ctx, r.PRURL, fmt.Sprintf("MARSHAL tried %d time(s) to make the checks pass on this pull request and stopped at the cap. It needs a human.", attempts))
			_ = d.GH.AddLabel(ctx, r.PRURL, "marshal:needs-human")
			d.send(ctx, notify.Event{Kind: notify.NeedsHuman, Title: r.Repo + ": checks still failing after " + fmt.Sprint(attempts) + " fix attempt(s)", Body: r.Job + " · " + pr.Title, URL: r.PRURL})
		}
		return
	}
	if d.busy(repo.Name) {
		return
	}
	if _, err := d.startFix(ctx, repo, r, pr, fmt.Sprintf("attempt %d of %d", attempts+1, cap)); err != nil {
		d.fail("run %s: %v", r.ID, err)
	}
}

// Fix starts a check-fixing follow-up for a proposal on demand, outside the
// cap. It is the operator's way of saying "try again" after the daemon has
// handed a PR to a human.
func (d *Daemon) Fix(ctx context.Context, runID string) (string, error) {
	d.init()
	r, err := d.Store.GetRun(ctx, runID)
	if err != nil {
		return "", err
	}
	if r.Parent != "" {
		// Point at the proposal, not at one of its follow-ups.
		r, err = d.Store.GetRun(ctx, r.Parent)
		if err != nil {
			return "", err
		}
	}
	if r.Status != store.Proposed || r.PRURL == "" {
		return "", fmt.Errorf("run %s is %s with no open proposal to fix", r.ID, r.Status)
	}
	repo, ok := d.Fleet.Repo(r.Repo)
	if !ok {
		return "", fmt.Errorf("unknown repo %q", r.Repo)
	}
	if d.busy(repo.Name) {
		return "", fmt.Errorf("%s is busy", repo.Name)
	}
	pr, err := d.GH.ViewPR(ctx, r.PRURL)
	if err != nil {
		return "", err
	}
	if !pr.Merged() && !pr.Closed() {
		return d.startFix(ctx, repo, r, pr, "requested by the operator")
	}
	return "", fmt.Errorf("%s is already %s", r.PRURL, strings.ToLower(pr.State))
}

// startFix launches the follow-up goroutine; the caller has checked the
// cap, the busy map and the head.
func (d *Daemon) startFix(ctx context.Context, repo config.Repo, r store.Run, pr github.PR, why string) (string, error) {
	slug := repo.GitHub
	if slug == "" {
		slug, _ = github.Slug(repo.URL)
	}
	logs := ""
	if slug != "" {
		l, err := d.GH.FailedLogs(ctx, slug, pr.HeadSHA, 48*1024)
		if err != nil {
			d.fail("run %s: failed logs: %v", r.ID, err)
		}
		logs = l
	}
	d.mu.Lock()
	if id, ok := d.running[repo.Name]; ok {
		d.mu.Unlock()
		return "", fmt.Errorf("%s is busy with run %s", repo.Name, id)
	}
	id := store.NewID()
	d.running[repo.Name] = id
	d.mu.Unlock()
	d.note(ctx, r.ID, "checks failed on head %s; starting fix run %s (%s)", short(pr.HeadSHA), id, why)
	x := &run.Executor{Fleet: d.Fleet, Store: d.Store, Adapter: d.Adapter, WorkRoot: d.WorkRoot, Auth: d.Auth}
	go func() {
		defer func() {
			d.mu.Lock()
			delete(d.running, repo.Name)
			d.mu.Unlock()
		}()
		rec, err := x.Followup(d.base, r, pr, logs, run.Options{ID: id})
		if err != nil {
			d.fail("run %s: %v", id, err)
			return
		}
		d.Log.Printf("run %s: %s (fix for %s)", id, rec.Status, r.ID)
		switch rec.Status {
		case store.Pushed:
			_ = d.GH.CommentPR(d.base, r.PRURL, "MARSHAL pushed a fix for the failing checks (run `"+id+"`).\n\n"+rec.Summary)
		case store.NoChange, store.Failed, store.Blocked:
			msg := rec.Summary
			if rec.Error != "" {
				msg = rec.Error
			}
			_ = d.GH.CommentPR(d.base, r.PRURL, fmt.Sprintf("MARSHAL could not fix the failing checks (run `%s`, %s).\n\n%s", id, rec.Status, msg))
			if rec.Status != store.NoChange {
				d.send(d.base, notify.Event{Kind: notify.Failed, Title: repo.Name + ": fix attempt " + string(rec.Status), Body: firstLine(msg), URL: r.PRURL})
			}
		}
	}()
	return id, nil
}

// send delivers a notification if any channel is configured.
func (d *Daemon) send(ctx context.Context, e notify.Event) {
	if d.Notify == nil {
		return
	}
	_ = d.Notify.Send(ctx, e)
}

// watchMerges is the safety net under autonomous merges: for each merge
// MARSHAL made on its own, watch the default branch's CI on the merge
// commit. Green settles it. Red reverts it, as a PR the app merges at once,
// and demotes the class two steps. Silence past the window settles it too;
// a repo without CI never reaches auto in the first place.
func (d *Daemon) watchMerges(ctx context.Context) error {
	runs, err := d.Store.Unsettled(ctx)
	if err != nil {
		return err
	}
	ladder := policy.Ladder{Fleet: d.Fleet}
	for _, r := range runs {
		repo, ok := d.Fleet.Repo(r.Repo)
		if !ok {
			continue
		}
		hours := d.Fleet.Policy.Classes[r.Class].SoakHours
		if hours <= 0 {
			hours = 24
		}
		window := time.Duration(hours) * time.Hour
		if r.MergeSHA == "" {
			pr, err := d.GH.ViewPR(ctx, r.PRURL)
			if err != nil || pr.MergeSHA == "" {
				if d.now().Sub(r.FinishedAt) > window {
					r.Settled = true
					_ = d.Store.UpdateRun(ctx, &r)
				}
				continue
			}
			r.MergeSHA = pr.MergeSHA
		}
		slug := repo.GitHub
		if slug == "" {
			slug, _ = github.Slug(repo.URL)
		}
		ci, found, err := d.GH.CommitCI(ctx, slug, r.MergeSHA)
		if err != nil {
			d.fail("run %s: watch: %v", r.ID, err)
			continue
		}
		switch {
		case found && ci.Red():
			d.revert(ctx, ladder, repo, r, ci)
		case found && ci.Status == "completed":
			r.Settled = true
			d.note(ctx, r.ID, "default branch green on %s; settled", short(r.MergeSHA))
			_ = d.Store.UpdateRun(ctx, &r)
		case d.now().Sub(r.FinishedAt) > window:
			r.Settled = true
			d.note(ctx, r.ID, "watch window passed with no verdict from CI; settled")
			_ = d.Store.UpdateRun(ctx, &r)
		default:
			_ = d.Store.UpdateRun(ctx, &r) // persists MergeSHA
		}
	}
	return nil
}

// revert undoes a machine merge whose commit turned the default branch red.
func (d *Daemon) revert(ctx context.Context, ladder policy.Ladder, repo config.Repo, r store.Run, ci github.CI) {
	d.note(ctx, r.ID, "default branch red on %s (%s); reverting", short(r.MergeSHA), ci.URL)
	var env []string
	identity := d.Fleet.Defaults.Identity
	cloneRepo := repo
	if d.Auth != nil {
		e, err := d.Auth.Env(ctx, ghapp.Full)
		if err != nil {
			d.fail("run %s: revert: %v", r.ID, err)
			return
		}
		env = e
		identity = d.Auth.Identity()
		cloneRepo.URL = ghapp.CloneURL(repo)
	}
	root := d.WorkRoot
	if root == "" {
		root = filepath.Join(config.StateDir(), "work")
	}
	ws, err := workspace.Prepare(ctx, root, cloneRepo, "revert", r.ID, identity, env)
	if err != nil {
		d.fail("run %s: revert: %v", r.ID, err)
		return
	}
	defer ws.Remove()
	title := fmt.Sprintf("Revert MARSHAL merge %s (%s)", short(r.MergeSHA), r.Job)
	body := fmt.Sprintf("MARSHAL merged %s on its own and the default branch's CI then failed on that commit: %s\n\nThis reverts it. The `%s` class on this repository has been demoted.", r.PRURL, ci.URL, r.Class)
	if err := ws.Revert(ctx, r.MergeSHA, title+"\n\n"+body); err != nil {
		d.fail("run %s: %v", r.ID, err)
		d.send(ctx, notify.Event{Kind: notify.Reverted, Title: repo.Name + ": default branch red and the revert conflicts", Body: err.Error(), URL: ci.URL, Urgent: true})
		r.Settled = true
		_ = d.Store.UpdateRun(ctx, &r)
		d.move(ctx, ladder, repo, r.Class, policy.Reverted)
		return
	}
	if err := ws.Push(ctx); err != nil {
		d.fail("run %s: revert push: %v", r.ID, err)
		return
	}
	slug := repo.GitHub
	if slug == "" {
		slug, _ = github.Slug(repo.URL)
	}
	url, err := d.GH.CreatePR(ctx, slug, ws.Branch, ws.Repo.Branch, title, body, []string{run.Label, "marshal:revert"})
	if err != nil {
		d.fail("run %s: revert PR: %v", r.ID, err)
		return
	}
	merged := d.GH.MergePR(ctx, url) == nil
	if !merged {
		_ = d.GH.AddLabel(ctx, url, "marshal:needs-human")
	}
	r.Status = store.Reverted
	r.Settled = true
	_ = d.Store.UpdateRun(ctx, &r)
	d.move(ctx, ladder, repo, r.Class, policy.Reverted)
	state := "merged"
	if !merged {
		state = "opened but could not be merged; it needs a human"
	}
	d.note(ctx, r.ID, "revert PR %s %s", url, state)
	_ = d.GH.CommentPR(ctx, r.PRURL, fmt.Sprintf("MARSHAL reverted this merge: the default branch went red on %s. Revert: %s (%s). The `%s` class has been demoted.", short(r.MergeSHA), url, state, r.Class))
	d.send(ctx, notify.Event{Kind: notify.Reverted, Title: repo.Name + ": autonomous merge reverted", Body: r.Job + " · revert " + state, URL: url, Urgent: true})
}

// maybeDigest sends the daily summary once per day at the configured time.
func (d *Daemon) maybeDigest(ctx context.Context) {
	if d.Notify == nil || !d.Notify.Wants(notify.Digest) {
		return
	}
	at := d.Fleet.Notify.Digest
	if at == "" {
		at = "08:00"
	}
	now := d.now()
	due, err := time.ParseInLocation("15:04", at, now.Location())
	if err != nil {
		return
	}
	due = time.Date(now.Year(), now.Month(), now.Day(), due.Hour(), due.Minute(), 0, 0, now.Location())
	if now.Before(due) {
		return
	}
	today := now.Format("2006-01-02")
	if last, _ := d.Store.Setting(ctx, "digest_last", ""); last == today {
		return
	}
	text, err := d.DigestText(ctx)
	if err != nil {
		d.fail("digest: %v", err)
		return
	}
	d.send(ctx, notify.Event{Kind: notify.Digest, Title: "daily digest", Body: text})
	_ = d.Store.SetSetting(ctx, "digest_last", today)
}

// DigestText renders the last 24 hours. Usable on a bare Daemon, as the
// CLI does, so it initialises itself.
func (d *Daemon) DigestText(ctx context.Context) (string, error) {
	d.init()
	sum, err := d.Store.Summarise(ctx, d.now().Add(-24*time.Hour))
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d run(s), $%.2f", sum.Runs, sum.Spend)
	var parts []string
	for _, st := range []store.Status{store.Merged, store.Proposed, store.Pushed, store.NoChange, store.Rejected, store.Reverted, store.Failed, store.Blocked, store.Cancelled} {
		if n := sum.ByStatus[st]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, st))
		}
	}
	if len(parts) > 0 {
		b.WriteString(": " + strings.Join(parts, ", "))
	}
	b.WriteString("\n")
	if len(sum.Proposals) == 0 {
		b.WriteString("Nothing is waiting for you.\n")
	} else {
		fmt.Fprintf(&b, "Waiting for you: %d\n", len(sum.Proposals))
		for _, p := range sum.Proposals {
			fmt.Fprintf(&b, "- %s %s (%s) %s\n", p.Repo, p.Job, ago(p.StartedAt, d.now()), p.PRURL)
		}
	}
	auto := "on"
	if !d.Autonomy(ctx) {
		auto = "OFF"
	}
	fmt.Fprintf(&b, "Autonomy %s.", auto)
	return b.String(), nil
}

func ago(t, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// startVerify runs the verifier for a proposal in the background, once per
// head. The verdict, whatever it is, is recorded against the head so the
// same commit is never paid for twice; a failure to verify is recorded as
// UNSURE for the same reason.
func (d *Daemon) startVerify(ctx context.Context, r store.Run, pr github.PR) {
	d.mu.Lock()
	if d.verifying[r.ID] {
		d.mu.Unlock()
		return
	}
	d.verifying[r.ID] = true
	d.mu.Unlock()
	d.note(ctx, r.ID, "verifying head %s", short(pr.HeadSHA))
	v := &verifier.Verifier{Fleet: d.Fleet, Adapter: d.Adapter, WorkRoot: d.WorkRoot, Auth: d.Auth}
	bg := d.base
	go func() {
		defer func() {
			d.mu.Lock()
			delete(d.verifying, r.ID)
			d.mu.Unlock()
		}()
		res, err := v.Verify(bg, r, pr)
		if err != nil {
			d.fail("run %s: verify: %v", r.ID, err)
			res.RunID = r.ID
			if res.Head == "" {
				res.Head = pr.HeadSHA
			}
			res.Verdict.Verdict = verifier.Unsure
			res.Summary = "The verifier could not complete: " + err.Error()
		}
		if res.Head == "" {
			res.Head = pr.HeadSHA
		}
		if err := d.Store.PutVerdict(bg, res.Verdict); err != nil {
			d.fail("run %s: verdict: %v", r.ID, err)
			return
		}
		d.note(bg, r.ID, "verifier: %s · %d turns · $%.2f · %s", res.Verdict.Verdict, res.Turns, res.CostUSD, firstLine(res.Summary))
		if err := d.GH.CommentPR(bg, r.PRURL, res.Comment()); err != nil {
			d.fail("run %s: comment: %v", r.ID, err)
		}
		if res.Verdict.Verdict != verifier.Pass {
			if err := d.GH.AddLabel(bg, r.PRURL, "marshal:needs-human"); err != nil {
				d.fail("run %s: label: %v", r.ID, err)
			}
			d.send(bg, notify.Event{Kind: notify.NeedsHuman, Title: r.Repo + ": verifier said " + res.Verdict.Verdict, Body: firstLine(res.Summary), URL: r.PRURL, Urgent: res.Verdict.Verdict == verifier.Fail})
		}
	}()
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}

// move applies an outcome to the ladder and persists it.
func (d *Daemon) move(ctx context.Context, ladder policy.Ladder, repo config.Repo, class string, o policy.Outcome) {
	t, found, err := d.Store.GetTrust(ctx, repo.Name, class)
	if err != nil {
		d.fail("trust %s/%s: %v", repo.Name, class, err)
		return
	}
	before := ladder.Current(repo, class, t, found)
	after := ladder.Apply(before, o)
	if err := d.Store.PutTrust(ctx, after); err != nil {
		d.fail("trust %s/%s: %v", repo.Name, class, err)
		return
	}
	if after.Level != before.Level {
		d.Log.Printf("trust %s/%s: %s -> %s", repo.Name, class, before.Level, after.Level)
	}
}

func (d *Daemon) note(ctx context.Context, runID, format string, args ...any) {
	text := fmt.Sprintf(format, args...)
	d.Log.Printf("run %s: %s", runID, text)
	_ = d.Store.AppendEvent(ctx, runID, store.Event{Kind: "note", Text: text})
}

// schedule starts what is due: at most one job per tick across the fleet,
// so a fresh daemon with every weekly job "never run" does not start them
// all in the same minute. The rest follow on later ticks.
func (d *Daemon) schedule(ctx context.Context) error {
	now := d.now()
	started := false
	due := 0
	for _, repo := range d.Fleet.Repos {
		repo, _ = d.Fleet.Repo(repo.Name)
		if d.busy(repo.Name) {
			continue
		}
		var ci *github.CI // fetched at most once per repo per tick
		for _, jn := range repo.Jobs {
			job, ok := d.Fleet.Job(jn)
			if !ok {
				continue
			}
			s, err := sched.Parse(job.Schedule)
			if err != nil {
				d.fail("job %s: %v", job.Name, err)
				continue
			}
			if s.Manual() {
				continue
			}
			last, err := d.Store.LastStarted(ctx, job.Name, repo.Name)
			if err != nil {
				return err
			}
			fire := false
			switch {
			case s.OnFailure:
				if !s.Cooldown(last, now) {
					continue
				}
				if ci == nil {
					c, err := d.defaultCI(ctx, repo)
					if err != nil {
						d.fail("%s: ci: %v", repo.Name, err)
						continue
					}
					ci = &c
				}
				fire = ci.Red()
			default:
				fire = s.Due(last, now)
			}
			if !fire {
				continue
			}
			due++
			if started {
				continue
			}
			if _, err := d.Trigger(ctx, job.Name, repo.Name); err != nil {
				d.fail("%s on %s: %v", job.Name, repo.Name, err)
				continue
			}
			started = true
			break // one job per repo per tick as well
		}
	}
	if due > 1 {
		d.Log.Printf("%d jobs due; starting one per tick", due)
	}
	return nil
}

func (d *Daemon) defaultCI(ctx context.Context, repo config.Repo) (github.CI, error) {
	slug := repo.GitHub
	if slug == "" {
		s, ok := github.Slug(repo.URL)
		if !ok {
			return github.CI{}, fmt.Errorf("%s: not a GitHub URL and no github: slug set; on-failure needs gh", repo.URL)
		}
		slug = s
	}
	branch := repo.Branch
	if branch == "" {
		b, err := d.GH.DefaultBranch(ctx, slug)
		if err != nil {
			return github.CI{}, err
		}
		branch = b
	}
	ci, _, err := d.GH.LatestCI(ctx, slug, branch)
	return ci, err
}

func (d *Daemon) busy(repo string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.running[repo]
	return ok
}

func (d *Daemon) inFlight() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var ids []string
	for _, id := range d.running {
		ids = append(ids, id)
	}
	return ids
}

// Trigger starts a job against a repo in the background and returns the run
// ID. A repo with a run in flight refuses a second.
func (d *Daemon) Trigger(ctx context.Context, job, repo string) (string, error) {
	d.init()
	if _, ok := d.Fleet.Job(job); !ok {
		return "", fmt.Errorf("unknown job %q", job)
	}
	if _, ok := d.Fleet.Repo(repo); !ok {
		return "", fmt.Errorf("unknown repo %q", repo)
	}
	d.mu.Lock()
	if id, ok := d.running[repo]; ok {
		d.mu.Unlock()
		return "", fmt.Errorf("%s is busy with run %s", repo, id)
	}
	id := store.NewID()
	d.running[repo] = id
	d.mu.Unlock()

	d.Log.Printf("run %s: starting %s on %s", id, job, repo)
	x := &run.Executor{Fleet: d.Fleet, Store: d.Store, Adapter: d.Adapter, WorkRoot: d.WorkRoot, Auth: d.Auth}
	go func() {
		defer func() {
			d.mu.Lock()
			delete(d.running, repo)
			d.mu.Unlock()
		}()
		rec, err := x.Execute(d.base, job, repo, run.Options{ID: id})
		if err != nil {
			d.fail("run %s: %v", id, err)
			return
		}
		switch rec.Status {
		case store.Failed, store.Blocked, store.Cancelled:
			d.Log.Printf("run %s: %s: %s", id, rec.Status, firstLine(rec.Error))
			if rec.Status != store.Cancelled {
				d.send(d.base, notify.Event{Kind: notify.Failed, Title: repo + ": " + job + " " + string(rec.Status), Body: firstLine(rec.Error), URL: rec.PRURL})
			}
		case store.Proposed:
			d.Log.Printf("run %s: %s %s", id, rec.Status, rec.PRURL)
			d.send(d.base, notify.Event{Kind: notify.Proposed, Title: repo + ": PR opened by " + job, Body: firstLine(rec.Summary), URL: rec.PRURL})
		default:
			d.Log.Printf("run %s: %s %s", id, rec.Status, rec.PRURL)
		}
	}()
	return id, nil
}

// TriggerTask starts a free-form task in the background. With a repo it is
// subject to the repo's one-run-at-a-time rule; without one it runs beside
// anything else.
func (d *Daemon) TriggerTask(ctx context.Context, name, prompt, repoName, class string) (string, error) {
	d.init()
	if strings.TrimSpace(prompt) == "" {
		return "", fmt.Errorf("task: empty prompt")
	}
	if name == "" {
		name = "task"
	}
	var repo *config.Repo
	key := ""
	if repoName != "" {
		r, ok := d.Fleet.Repo(repoName)
		if !ok {
			return "", fmt.Errorf("unknown repo %q", repoName)
		}
		repo = &r
		key = r.Name
	}
	d.mu.Lock()
	id := store.NewID()
	if key == "" {
		// Repo-less tasks run beside each other, but not without limit: each
		// is an agent process and a clone of the memory repo.
		n := 0
		for k := range d.running {
			if strings.HasPrefix(k, "task:") {
				n++
			}
		}
		if n >= maxConcurrentTasks {
			d.mu.Unlock()
			return "", fmt.Errorf("%d tasks already running; try again when one finishes", n)
		}
		key = "task:" + id
	}
	if busy, ok := d.running[key]; ok {
		d.mu.Unlock()
		return "", fmt.Errorf("%s is busy with run %s", key, busy)
	}
	d.running[key] = id
	d.mu.Unlock()
	where := "without a repository"
	if repo != nil {
		where = "on " + repo.Name
	}
	d.Log.Printf("run %s: starting task %q %s", id, firstLine(prompt), where)
	x := &run.Executor{Fleet: d.Fleet, Store: d.Store, Adapter: d.Adapter, WorkRoot: d.WorkRoot, Auth: d.Auth}
	go func() {
		defer func() {
			d.mu.Lock()
			delete(d.running, key)
			d.mu.Unlock()
		}()
		rec, err := x.Task(d.base, name, prompt, repo, class, run.Options{ID: id})
		if err != nil {
			d.fail("run %s: %v", id, err)
			return
		}
		d.Log.Printf("run %s: %s %s", id, rec.Status, rec.PRURL)
		switch rec.Status {
		case store.Reported:
			d.send(d.base, notify.Event{Kind: notify.Proposed, Title: "task report ready: " + firstLine(prompt), Body: firstLine(rec.Summary)})
		case store.Proposed:
			d.send(d.base, notify.Event{Kind: notify.Proposed, Title: repo.Name + ": PR opened by task", Body: firstLine(rec.Summary), URL: rec.PRURL})
		case store.Failed, store.Blocked:
			d.send(d.base, notify.Event{Kind: notify.Failed, Title: "task " + string(rec.Status) + ": " + firstLine(prompt), Body: firstLine(rec.Error)})
		}
	}()
	return id, nil
}

// scheduleTasks starts scheduled tasks that are due, one per tick.
func (d *Daemon) scheduleTasks(ctx context.Context) error {
	now := d.now()
	for _, t := range d.Fleet.Tasks {
		s, err := sched.Parse(t.Schedule)
		if err != nil {
			d.fail("task %s: %v", t.Name, err)
			continue
		}
		if s.Manual() || s.OnFailure {
			continue
		}
		jobName := "task:" + t.Name
		last, err := d.Store.LastStarted(ctx, jobName, t.Repo)
		if err != nil {
			return err
		}
		if !s.Due(last, now) {
			continue
		}
		if t.Repo != "" && d.busy(t.Repo) {
			continue
		}
		if _, err := d.TriggerTask(ctx, jobName, t.Prompt, t.Repo, t.Class); err != nil {
			d.fail("task %s: %v", t.Name, err)
			continue
		}
		return nil
	}
	return nil
}

// Handle implements api.Handler.
func (d *Daemon) Handle(ctx context.Context, req api.Request) api.Response {
	d.init()
	switch req.Op {
	case "status":
		spend, _ := d.Store.SpendSince(ctx, d.now().Add(-24*time.Hour))
		d.mu.Lock()
		st := &api.Status{Version: d.Version, Started: d.started, Running: d.inFlightLocked(),
			Repos: len(d.Fleet.Repos), Jobs: len(d.Fleet.Jobs), Spend24h: spend, LastTick: d.lastTick, LastError: d.lastErr}
		for id := range d.verifying {
			st.Verifying = append(st.Verifying, id)
		}
		d.mu.Unlock()
		if vs, err := d.Store.VerdictSpendSince(ctx, d.now().Add(-24*time.Hour)); err == nil {
			st.Spend24h += vs
		}
		st.Autonomy = d.Autonomy(ctx)
		return api.Response{Status: st}
	case "runs":
		runs, err := d.Store.ListRuns(ctx, req.Repo, req.Limit)
		if err != nil {
			return api.Fail("%v", err)
		}
		return api.Response{Runs: runs}
	case "run":
		r, err := d.Store.GetRun(ctx, req.ID)
		if err != nil {
			return api.Fail("%v", err)
		}
		return api.Response{Run: &r}
	case "events":
		r, err := d.Store.GetRun(ctx, req.ID)
		if err != nil {
			return api.Fail("%v", err)
		}
		ev, err := d.Store.EventsAfter(ctx, r.ID, req.After)
		if err != nil {
			return api.Fail("%v", err)
		}
		return api.Response{Run: &r, Events: ev}
	case "trigger":
		id, err := d.Trigger(ctx, req.Job, req.Repo)
		if err != nil {
			return api.Fail("%v", err)
		}
		return api.Response{ID: id}
	case "task":
		id, err := d.TriggerTask(ctx, req.Job, req.Value, req.Repo, "")
		if err != nil {
			return api.Fail("%v", err)
		}
		return api.Response{ID: id}
	case "fix":
		id, err := d.Fix(ctx, req.ID)
		if err != nil {
			return api.Fail("%v", err)
		}
		return api.Response{ID: id}
	case "autonomy":
		switch req.Value {
		case "on", "off":
			if err := d.Store.SetSetting(ctx, autonomyKey, req.Value); err != nil {
				return api.Fail("%v", err)
			}
			d.Log.Printf("autonomy %s", req.Value)
		case "":
		default:
			return api.Fail("autonomy: want on or off")
		}
		return api.Response{Status: &api.Status{Autonomy: d.Autonomy(ctx)}}
	case "verdict":
		r, err := d.Store.GetRun(ctx, req.ID)
		if err != nil {
			return api.Fail("%v", err)
		}
		v, ok, err := d.Store.LatestVerdict(ctx, r.ID)
		if err != nil {
			return api.Fail("%v", err)
		}
		resp := api.Response{Run: &r}
		if ok {
			resp.Verdict = &v
		}
		return resp
	case "trust":
		recorded, err := d.Store.ListTrust(ctx)
		if err != nil {
			return api.Fail("%v", err)
		}
		return api.Response{Trust: Ladder(d.Fleet, recorded)}
	}
	return api.Fail("unknown op %q", req.Op)
}

func (d *Daemon) inFlightLocked() []string {
	var ids []string
	for _, id := range d.running {
		ids = append(ids, id)
	}
	return ids
}

// Ladder lists every repo and class with its current level, seeding unseen
// pairs from policy. Shared with the CLI so both show the same table.
func Ladder(f *config.Fleet, recorded []store.Trust) []store.Trust {
	byKey := map[string]store.Trust{}
	for _, t := range recorded {
		byKey[t.Repo+"/"+t.Class] = t
	}
	l := policy.Ladder{Fleet: f}
	var out []store.Trust
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
			out = append(out, l.Current(r, j.Class, t, found))
		}
	}
	return out
}
