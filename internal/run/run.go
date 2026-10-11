// Package run executes one job against one repository end to end: clone,
// brief the agent, record what it does, check the diff against policy,
// commit, and open the pull request.
package run

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Sousf/marshal/internal/config"
	"github.com/Sousf/marshal/internal/contract"
	"github.com/Sousf/marshal/internal/ghapp"
	"github.com/Sousf/marshal/internal/github"
	"github.com/Sousf/marshal/internal/jobs"
	"github.com/Sousf/marshal/internal/policy"
	"github.com/Sousf/marshal/internal/probe"
	"github.com/Sousf/marshal/internal/runner"
	"github.com/Sousf/marshal/internal/skills"
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
	// Force runs the agent even when the job's probe finds nothing to do.
	Force bool
	// AllowRed opens the pull request even when the repository's own checks
	// fail on it, as long as nothing that passed before was broken.
	AllowRed bool
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
	if job.Name == ReleaseJob {
		return x.release(ctx, job, repo, opts)
	}
	return x.executeJob(ctx, job, repo, opts)
}

// Task runs a free-form instruction. With a repo it is a job whose brief is
// the instruction and whose result is a pull request. Without one it runs
// in an empty scratch directory with read, search, web and write-to-scratch
// tools, no tokens, and its result is a report.
func (x *Executor) Task(ctx context.Context, name, prompt string, repo *config.Repo, class string, opts Options) (store.Run, error) {
	if strings.TrimSpace(prompt) == "" {
		return store.Run{}, fmt.Errorf("task: empty prompt")
	}
	if name == "" {
		name = "task"
	}
	if repo != nil {
		if class == "" {
			class = "code"
		}
		if _, ok := x.Fleet.Policy.Classes[class]; !ok {
			return store.Run{}, fmt.Errorf("task: class %q is not in policy.classes", class)
		}
		job := config.Job{Name: name, Class: class, Prompt: prompt, Description: firstLine(prompt)}
		return x.executeJob(ctx, job, *repo, opts)
	}
	return x.report(ctx, name, prompt, opts)
}

func (x *Executor) executeJob(ctx context.Context, job config.Job, repo config.Repo, opts Options) (store.Run, error) {
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
		// The app's identity wins over the fleet's fallback one, so the
		// commits link to the app the PR came from. The fallback is for
		// running without an app.
		identity = x.Auth.Identity()
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

	if head, err := ws.HeadSHA(ctx); err == nil {
		rec.Head = head
	}
	con, _, _ := contract.Load(ws.Dir)
	if !opts.Force {
		last, _, _ := x.Store.LastRunOf(ctx, job.Name, repo.Name)
		pr := probe.Run(ctx, job.Name, probe.Env{Dir: ws.Dir, Contract: con, LastRun: last.StartedAt,
			FeedbackSince: func(t time.Time) bool {
				fb, err := x.Store.FeedbackSince(ctx, t, 1)
				return err == nil && len(fb) > 0
			}})
		if !pr.Needed {
			logf("note", "probe: nothing to do (%s); no agent run", pr.Reason)
			return finish(store.Skipped, "probe: "+pr.Reason)
		}
		logf("note", "probe: %s", pr.Reason)
	}
	con, baseline := x.baseline(ctx, ws.Dir, logf)
	// Stable first: the brief, the rules and the memory snapshot are the
	// same for every run of this job until a reviewed change lands, so a
	// prompt cache can reuse them. What changes every run goes last.
	prompt := brief + jobs.Contract + skills.Contract
	mem := x.openMemory(ctx, rec.ID, identity, harnessEnv, logf)
	defer mem.close()
	if m := mem.brief(job.Name + " " + job.Description + " " + repo.Name + " " + brief); m != "" {
		prompt += "\n" + m
	}
	if c := con.Brief(baseline); c != "" {
		prompt += "\n" + c
	}
	if h := History(ctx, x.Store, repo.Name, job.Name); h != "" {
		prompt += "\n" + h
	}
	if job.Class == "memory" {
		prompt += "\n" + fleetFeedback(ctx, x.Store, time.Now().Add(-7*24*time.Hour))
	}
	req := runner.Request{
		Prompt:          prompt,
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
		ExtraDirs:       mem.extraDirs(),
	}
	tr := &transcript{}
	result, runErr := x.Adapter.Run(ctx, req, func(e runner.Event) {
		logf(string(e.Kind), "%s", e.Text)
		tr.add(e)
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
	x.recordLessons(ws, result.Output, logf)
	if _, props := skills.ParseProposals(result.Output, repo.Name+"/"+rec.ID); len(props) > 0 {
		mem.propose(ctx, x, rec, props, opts.NoPR, logf)
	} else {
		t, c := x.nudge(ctx, mem, rec, job.Name, repo.Name, result.Output, tr, opts.NoPR, logf)
		result.Turns += t
		result.CostUSD += c
		rec.Turns, rec.CostUSD = result.Turns, result.CostUSD
	}

	changed, err := ws.Changed(ctx)
	if err != nil {
		return finish(store.Failed, err.Error())
	}
	if !changed {
		logf("note", "no changes")
		return finish(store.NoChange, "")
	}

	// Pre-flight first, so the policy check sees the tree the agent ends
	// with, fixes included.
	gate, blocked := x.preflight(ctx, ws, baseline, req, &result, rec, opts, logf)
	rec.Summary = result.Output // pre-flight fixes append to the report
	if blocked != "" {
		return finish(store.Blocked, blocked)
	}
	if changed, _ = ws.Changed(ctx); !changed {
		logf("note", "no changes remain after pre-flight")
		return finish(store.NoChange, "")
	}
	numstat, err := ws.Numstat(ctx)
	if err != nil {
		return finish(store.Failed, err.Error())
	}
	changes := policy.ParseNumstat(numstat)
	extraLocked := append([]string{}, x.Fleet.Never.Paths...)
	if con != nil {
		extraLocked = append(extraLocked, con.Locked...)
	}
	if v := policy.CheckWith(job, extraLocked, changes); len(v) > 0 {
		var b strings.Builder
		for _, x := range v {
			b.WriteString(x.String())
			b.WriteString("\n")
			logf("blocked", "%s", x.String())
		}
		return finish(store.Blocked, strings.TrimSpace(b.String()))
	}
	logf("note", "policy ok: %d files", len(changes))
	if con != nil {
		if touched := policy.Touches(con.Sensitive, changes); len(touched) > 0 {
			rec.Sensitive = true
			logf("note", "touches sensitive paths (%s); a human must review", strings.Join(touched, ", "))
		}
	}

	title, body := splitReport(result.Output, job.Name)
	body = body + gateSection(baseline, gate) + trailer(rec, result)
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
	labels := []string{Label, Label + ":" + job.Class}
	if rec.Sensitive {
		labels = append(labels, Label+":sensitive")
	}
	url, err := ws.OpenPR(ctx, title, body, labels)
	if err != nil {
		return finish(store.Failed, err.Error())
	}
	rec.PRURL = url
	logf("note", "opened %s", url)
	return finish(store.Proposed, "")
}

// Followup pushes a fix to one of MARSHAL's own open proposals whose checks
// failed. It is a run like any other, on the proposal's branch instead of a
// new one, with the failing logs in the brief and a PR comment instead of a
// new PR. The parent's job sets the path rules the fix must stay inside.
func (x *Executor) Followup(ctx context.Context, parent store.Run, pr github.PR, logs string, opts Options) (store.Run, error) {
	return x.FollowupKind(ctx, "fix-checks", parent, pr, logs, opts)
}

// FollowupKind is Followup with the brief chosen: "fix-checks" takes the
// failing logs as context, "address-review" the reviewer's comments.
func (x *Executor) FollowupKind(ctx context.Context, kind string, parent store.Run, pr github.PR, extra string, opts Options) (store.Run, error) {
	logs := extra
	job, ok := x.Fleet.Job(parent.Job)
	if !ok {
		return store.Run{}, fmt.Errorf("unknown job %q", parent.Job)
	}
	repo, ok := x.Fleet.Repo(parent.Repo)
	if !ok {
		return store.Run{}, fmt.Errorf("unknown repo %q", parent.Repo)
	}
	res := x.Fleet.Resolve(job)
	brief, ok := jobs.Prompt(kind)
	if !ok {
		return store.Run{}, fmt.Errorf("no brief for follow-up kind %q", kind)
	}
	if !x.Adapter.Available() {
		return store.Run{}, fmt.Errorf("adapter %s is not available on this host", x.Adapter.Name())
	}
	branch := pr.HeadRef
	if branch == "" {
		branch = parent.Branch
	}
	rec := &store.Run{ID: opts.ID, Job: kind, Repo: repo.Name, Class: job.Class, Adapter: x.Adapter.Name(), Model: res.Model,
		Branch: branch, Parent: parent.ID, BaseHead: pr.HeadSHA}
	if err := x.Store.CreateRun(ctx, rec); err != nil {
		return store.Run{}, err
	}
	rec.PRURL = parent.PRURL
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
	logf("note", "run %s: %s on %s (parent %s, head %s)", rec.ID, kind, parent.PRURL, parent.ID, short(pr.HeadSHA))

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
		identity = x.Auth.Identity()
	}
	ws, err := workspace.PrepareExisting(ctx, x.workRoot(), repo, rec.ID, branch, identity, harnessEnv)
	if err != nil {
		return finish(store.Failed, err.Error())
	}
	if !opts.Keep {
		defer ws.Remove()
	}

	con, baseline := x.baseline(ctx, ws.Dir, logf)
	var b strings.Builder
	b.WriteString(brief)
	b.WriteString(jobs.Contract)
	mem := x.openMemory(ctx, rec.ID, identity, harnessEnv, logf)
	defer mem.close()
	if m := mem.brief(kind + " " + job.Name + " " + job.Description + " " + repo.Name + " " + firstLine(logs)); m != "" {
		b.WriteString("\n" + m)
	}
	if c := con.Brief(baseline); c != "" {
		b.WriteString("\n" + c)
	}
	fmt.Fprintf(&b, "\n## The pull request\n\n%s\n\nOriginal job `%s`: %s\n\nTitle: %s\n\n<pr-body>\n%s\n</pr-body>\n", parent.PRURL, job.Name, job.Description, pr.Title, strings.TrimSpace(pr.Body))
	switch kind {
	case "address-review":
		b.WriteString("\n## The reviewer's comments\n\n" + strings.TrimSpace(logs) + "\n")
	default:
		if strings.TrimSpace(logs) == "" {
			b.WriteString("\n## Failing checks\n\nNo failed-step log was available; use `gh run list` and `gh run view --log-failed` to find it.\n")
		} else {
			b.WriteString("\n## Failing checks\n\n" + logs + "\n")
		}
	}

	req := runner.Request{
		Prompt:          b.String(),
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
		ExtraDirs:       mem.extraDirs(),
	}
	result, runErr := x.Adapter.Run(ctx, req, func(e runner.Event) { logf(string(e.Kind), "%s", e.Text) })
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
	x.recordLessons(ws, result.Output, logf)
	if _, props := skills.ParseProposals(result.Output, repo.Name+"/"+rec.ID); len(props) > 0 {
		mem.propose(ctx, x, rec, props, opts.NoPR, logf)
	}
	changed, err := ws.Changed(ctx)
	if err != nil {
		return finish(store.Failed, err.Error())
	}
	if !changed {
		logf("note", "no changes")
		return finish(store.NoChange, "")
	}
	gate, blocked := x.preflight(ctx, ws, baseline, req, &result, rec, opts, logf)
	rec.Summary = result.Output // pre-flight fixes append to the report
	if blocked != "" {
		return finish(store.Blocked, blocked)
	}
	if changed, _ = ws.Changed(ctx); !changed {
		logf("note", "no changes remain after pre-flight")
		return finish(store.NoChange, "")
	}
	numstat, err := ws.Numstat(ctx)
	if err != nil {
		return finish(store.Failed, err.Error())
	}
	changes := policy.ParseNumstat(numstat)
	// A follow-up fixes whatever the checks or the reviewer pointed at,
	// which is often code the original job was confined away from. The
	// job's path list does not apply to it; the locked set and the size
	// cap still do, and the verifier reads the whole diff.
	fixJob := job
	fixJob.Paths = nil
	extraLocked := append([]string{}, x.Fleet.Never.Paths...)
	if con != nil {
		extraLocked = append(extraLocked, con.Locked...)
	}
	if v := policy.CheckWith(fixJob, extraLocked, changes); len(v) > 0 {
		var vb strings.Builder
		for _, x := range v {
			vb.WriteString(x.String() + "\n")
			logf("blocked", "%s", x.String())
		}
		return finish(store.Blocked, strings.TrimSpace(vb.String()))
	}
	if con != nil && len(policy.Touches(con.Sensitive, changes)) > 0 {
		rec.Sensitive = true
	}
	title, body := splitReport(result.Output, kind)
	if err := ws.Commit(ctx, title+"\n\n"+body+gateSection(baseline, gate)+trailer(rec, result)); err != nil {
		return finish(store.Failed, err.Error())
	}
	if opts.NoPR {
		logf("note", "committed on %s (not pushed)", ws.Branch)
		return finish(store.Committed, "")
	}
	if err := ws.Push(ctx); err != nil {
		return finish(store.Failed, err.Error())
	}
	logf("note", "pushed fix to %s", ws.Branch)
	return finish(store.Pushed, "")
}

// baseline loads the repository's contract and measures the clean tree with
// the gate verbs, so a later failure can be told apart from one that was
// already there.
func (x *Executor) baseline(ctx context.Context, dir string, logf func(kind, format string, args ...any)) (*contract.Contract, []contract.Result) {
	con, ok, err := contract.Load(dir)
	if err != nil {
		logf("note", "contract: %v (ignored)", err)
		return nil, nil
	}
	if !ok {
		return nil, nil
	}
	results := con.RunAll(ctx, dir, contract.Gate...)
	for _, r := range results {
		logf("note", "baseline %s", r)
	}
	return con, results
}

// preflightRetries is how many times the agent gets the failing output
// back to fix before the run is blocked.
const preflightRetries = 2

// preflight runs the repository's checks on the changed tree, the way
// GitHub will on the pull request, and gives the agent the failures to
// fix before anything is committed. The contract is reloaded from the work
// tree, so a workflow the agent added or changed is what gets run.
//
// Two rules decide what must pass. A verb that passed at baseline and fails
// now is always a regression. When the change touches the workflows, every
// gate verb must pass outright: a pull request that adds or edits CI and
// then fails that CI is exactly the red PR this exists to prevent. Other
// pre-existing failures are reported, not blamed. Returns the final gate
// results and, if the run must stop, the reason.
func (x *Executor) preflight(ctx context.Context, ws *workspace.Workspace, baseline []contract.Result, req runner.Request, result *runner.Result, rec *store.Run, opts Options, logf func(kind, format string, args ...any)) ([]contract.Result, string) {
	for attempt := 0; ; attempt++ {
		// Reload each time: the agent may have added or changed the
		// workflow the checks derive from.
		con, ok, err := contract.Load(ws.Dir)
		if err != nil || !ok {
			return nil, ""
		}
		after := con.RunAll(ctx, ws.Dir, contract.Gate...)
		for _, r := range after {
			logf("note", "pre-flight %s", r)
		}
		// Everything must pass before a pull request is opened: the
		// operator's job is to review, not to wait for fixes. Regressions
		// are named as such; pre-existing failures are the agent's to fix
		// too, and only --allow-red lets those through.
		regressions := map[string]bool{}
		for _, r := range contract.Regressions(baseline, after) {
			regressions[r.Verb] = true
		}
		var failing []contract.Result
		for _, r := range after {
			if !r.OK && !r.Unavailable {
				failing = append(failing, r)
			}
		}
		if len(failing) == 0 {
			return after, ""
		}
		if attempt >= preflightRetries || ctx.Err() != nil {
			if opts.AllowRed && len(regressions) == 0 {
				logf("note", "pre-flight still fails but --allow-red was given; opening anyway")
				return after, ""
			}
			why := "pre-flight: the pull request's own checks fail"
			if len(regressions) > 0 {
				why = "pre-flight: the change breaks what passed before"
			}
			for _, r := range failing {
				tag := "pre-existing"
				if regressions[r.Verb] {
					tag = "regression"
				}
				logf("blocked", "pre-flight: %s (%s)", r, tag)
			}
			return after, why + ":\n" + contract.Summary(failing) + "\n" + contract.Failures(failing, 2000)
		}
		logf("note", "pre-flight failed; giving the agent the output to fix (attempt %d of %d)", attempt+1, preflightRetries)
		fix := req
		fix.Prompt = preflightBrief(failing) + jobs.Contract
		again, err := x.Adapter.Run(ctx, fix, func(e runner.Event) { logf(string(e.Kind), "%s", e.Text) })
		result.Turns += again.Turns
		result.CostUSD += again.CostUSD
		result.Denials = append(result.Denials, again.Denials...)
		rec.Turns, rec.CostUSD = result.Turns, result.CostUSD
		if err != nil || again.IsError {
			msg := "agent error during pre-flight fix"
			if err != nil {
				msg = err.Error()
			} else if again.Output != "" {
				msg = firstLine(again.Output)
			}
			return after, "pre-flight: " + msg
		}
		if strings.TrimSpace(again.Output) != "" {
			result.Output = strings.TrimSpace(result.Output) + "\n\n## Pre-flight fixes\n\n" + strings.TrimSpace(again.Output)
		}
		// New files the fix added must be staged for the next numstat and
		// for the commit; the gate verbs see the tree either way.
		_, _ = ws.Numstat(ctx)
	}
}

// preflightBrief is the prompt for a fix attempt.
func preflightBrief(failing []contract.Result) string {
	var b strings.Builder
	b.WriteString("# Pre-flight failed\n\nMARSHAL ran the checks this pull request will be held to, in this checkout, after your change, and they fail. Every one of them must pass before the pull request can be opened, including any that were already failing before you started: the operator reviews green pull requests only. Fix them now, in this same checkout.\n\n")
	b.WriteString(contract.Summary(failing) + "\n" + contract.Failures(failing, 6000))
	b.WriteString(`Rules:

- Fix the cause. If the failure is in your change, fix the change. If a check you added found a real problem in the repository, fix the problem; a check that is red on the pull request that adds it is not finished.
- Do not widen the scope beyond what the checks require, and do not skip, delete or loosen a test or a check to make it pass.
- Run the failing commands yourself before you finish.
- Then write your report again in full, since it replaces the earlier one as the pull request body: title line, blank line, what changed and why, how you verified it, and a short section on what pre-flight found and what you did about it.
`)
	return b.String()
}

// gateSection renders the contract results for a PR body.
func gateSection(before, after []contract.Result) string {
	if len(after) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n**Contract checks** (run by MARSHAL, not by the agent)\n\n")
	b.WriteString(contract.Summary(after))
	var pre []string
	for _, r := range before {
		if !r.OK && !r.Unavailable {
			pre = append(pre, r.Verb)
		}
	}
	if len(pre) > 0 {
		fmt.Fprintf(&b, "\nAlready failing before this change: %s.\n", strings.Join(pre, ", "))
	}
	return b.String()
}

// recordLessons writes the report's lessons into the work tree's AGENTS.md
// so they ride in the same commit. Returns how many were new.
func (x *Executor) recordLessons(ws *workspace.Workspace, report string, logf func(kind, format string, args ...any)) int {
	_, lessons := ParseLessons(report)
	if len(lessons) == 0 {
		return 0
	}
	path := filepath.Join(ws.Dir, "AGENTS.md")
	cur, _ := os.ReadFile(path)
	merged, added := MergeLessons(string(cur), lessons)
	if added == 0 {
		return 0
	}
	if err := os.WriteFile(path, []byte(merged), 0o644); err != nil {
		logf("note", "could not record lessons: %v", err)
		return 0
	}
	logf("note", "recorded %d lesson(s) in AGENTS.md", added)
	return added
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// report runs a task that has no repository: a scratch directory, the
// default tool set plus writing into that directory, and REPORT.md (or the
// final message) as the deliverable, saved under the state directory.
func (x *Executor) report(ctx context.Context, name, prompt string, opts Options) (store.Run, error) {
	res := x.Fleet.Resolve(config.Job{Name: name, Class: "code"})
	if !x.Adapter.Available() {
		return store.Run{}, fmt.Errorf("adapter %s is not available on this host", x.Adapter.Name())
	}
	rec := &store.Run{ID: opts.ID, Job: name, Repo: "", Class: "report", Adapter: x.Adapter.Name(), Model: res.Model}
	if err := x.Store.CreateRun(ctx, rec); err != nil {
		return store.Run{}, err
	}
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
	dir, err := os.MkdirTemp(x.workRoot(), "task-"+rec.ID+"-")
	if err != nil {
		return finish(store.Failed, err.Error())
	}
	if !opts.Keep {
		defer os.RemoveAll(dir)
	}
	logf("note", "run %s: task in scratch directory (%s, %s)", rec.ID, res.Model, res.Effort)
	mem := x.openMemory(ctx, rec.ID, res.Identity, nil, logf)
	defer mem.close()

	var b strings.Builder
	b.WriteString("# Task\n\n" + strings.TrimSpace(prompt) + "\n")
	if m := mem.brief(prompt); m != "" {
		b.WriteString("\n" + m)
	}
	b.WriteString(`
## How MARSHAL runs you

- You are in an empty scratch directory with no repository. You may read anything on the web with
  the tools you have, and write files only here. There are no credentials in your environment and
  you must not look for any.
- Write your deliverable to REPORT.md in this directory, in Markdown, with a one-line title first.
  The operator reads it as it is; be specific, cite sources with URLs, and say what you could not
  establish.
- Your final message is a short summary of the report, two or three sentences.
` + skills.Contract)
	allowed := append([]string{"Write"}, res.AllowedTools...)
	req := runner.Request{
		Prompt:          b.String(),
		SystemPrompt:    "You are MARSHAL running a task for the operator, unattended. Nobody will answer a question; make the conservative call and say so in the report.",
		Dir:             dir,
		Model:           res.Model,
		Effort:          res.Effort,
		MaxTurns:        res.MaxTurns,
		MaxUSD:          res.MaxUSD,
		PermissionMode:  res.PermissionMode,
		AllowedTools:    allowed,
		DisallowedTools: res.DisallowedTools,
		ExtraDirs:       mem.extraDirs(),
	}
	tr := &transcript{}
	result, runErr := x.Adapter.Run(ctx, req, func(e runner.Event) { logf(string(e.Kind), "%s", e.Text); tr.add(e) })
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
	if _, props := skills.ParseProposals(result.Output, "task/"+rec.ID); len(props) > 0 {
		mem.propose(ctx, x, rec, props, opts.NoPR, logf)
	} else {
		t, c := x.nudge(ctx, mem, rec, "task", "no repository", result.Output, tr, opts.NoPR, logf)
		rec.Turns += t
		rec.CostUSD += c
	}
	report := result.Output
	if b, err := os.ReadFile(filepath.Join(dir, "REPORT.md")); err == nil && strings.TrimSpace(string(b)) != "" {
		report = string(b)
	}
	rec.Summary = report
	reports := filepath.Join(config.StateDir(), "reports")
	if err := os.MkdirAll(reports, 0o700); err == nil {
		path := filepath.Join(reports, rec.ID+".md")
		if err := os.WriteFile(path, []byte(report), 0o600); err == nil {
			logf("note", "report saved to %s", path)
		}
	}
	return finish(store.Reported, "")
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
