// Package daemon is the always-on half of MARSHAL. It fires jobs on their
// schedules, watches the pull requests they opened, moves the trust ladder
// when a human decides, and merges on its own only where the ladder says it
// may and every gate agrees.
package daemon

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/Sousf/marshal/internal/api"
	"github.com/Sousf/marshal/internal/config"
	"github.com/Sousf/marshal/internal/ghapp"
	"github.com/Sousf/marshal/internal/github"
	"github.com/Sousf/marshal/internal/policy"
	"github.com/Sousf/marshal/internal/run"
	"github.com/Sousf/marshal/internal/runner"
	"github.com/Sousf/marshal/internal/sched"
	"github.com/Sousf/marshal/internal/store"
	"github.com/Sousf/marshal/internal/verifier"
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
	if d.NoSchedule {
		return
	}
	if err := d.schedule(ctx); err != nil {
		d.fail("schedule: %v", err)
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
			d.note(ctx, r.ID, "merged by %s", r.MergedBy)
			if r.MergedBy == "human" {
				d.move(ctx, ladder, repo, r.Class, policy.Approved)
			}
		case pr.Closed():
			r.Status = store.Rejected
			r.FinishedAt = d.now()
			d.note(ctx, r.ID, "closed without merging")
			d.move(ctx, ladder, repo, r.Class, policy.Rejected)
		default:
			// Red checks on MARSHAL's own PR are MARSHAL's to fix, within a
			// cap. Nothing else happens to the PR while it is red.
			if pr.Checks == "FAILURE" {
				d.fixChecks(ctx, repo, r, pr)
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
			d.note(ctx, r.ID, "merged by marshal (%s)", by)
			_ = d.GH.CommentPR(ctx, r.PRURL, fmt.Sprintf("Merged by MARSHAL: class `%s` is at level `%s` for this repository, checks were green, the verifier passed head `%s`, and the %s gate passed.", r.Class, by, short(verdict.Head), by))
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
	attempts := 0
	for _, f := range prior {
		if f.Status == store.Running {
			return // one at a time
		}
		if f.BaseHead == pr.HeadSHA && pr.HeadSHA != "" {
			return // this head was already attempted; wait for a new push or a human
		}
		if f.Status == store.Pushed || f.Status == store.NoChange || f.Status == store.Failed || f.Status == store.Blocked {
			attempts++
		}
	}
	if attempts >= cap {
		if !pr.HasLabel("marshal:needs-human") {
			d.note(ctx, r.ID, "checks still failing after %d fix attempt(s); handing to a human", attempts)
			_ = d.GH.CommentPR(ctx, r.PRURL, fmt.Sprintf("MARSHAL tried %d time(s) to make the checks pass on this pull request and stopped at the cap. It needs a human.", attempts))
			_ = d.GH.AddLabel(ctx, r.PRURL, "marshal:needs-human")
		}
		return
	}
	if d.busy(repo.Name) {
		return
	}
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
	id := store.NewID()
	d.running[repo.Name] = id
	d.mu.Unlock()
	d.note(ctx, r.ID, "checks failed on head %s; starting fix run %s (attempt %d of %d)", short(pr.HeadSHA), id, attempts+1, cap)
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
		}
	}()
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
		default:
			d.Log.Printf("run %s: %s %s", id, rec.Status, rec.PRURL)
		}
	}()
	return id, nil
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
