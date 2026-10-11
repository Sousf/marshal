package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/Sousf/marshal/internal/config"
	"github.com/Sousf/marshal/internal/github"
	"github.com/Sousf/marshal/internal/notify"
	"github.com/Sousf/marshal/internal/policy"
	"github.com/Sousf/marshal/internal/store"
	"github.com/Sousf/marshal/internal/verifier"
)

// Dependabot's pull requests are triaged like MARSHAL's own proposals,
// under a class of their own: the verifier reads each head, and the trust
// ladder, caps, soak window, kill switch and never: floor decide whether
// MARSHAL may merge. What differs is that MARSHAL did not write them, so
// a verifier FAIL closes the PR, and MARSHAL approves a PR it would merge,
// which is the review a ruleset asks for.

// DependabotJob names the run records of Dependabot PRs, and the policy
// class that must exist for MARSHAL to triage them at all.
const DependabotJob = "dependabot"

// dependabotScan is how often each repo is asked for new Dependabot PRs.
// Dependabot runs daily or weekly; one gh call per repo every ten minutes
// is ample.
const dependabotScan = 10 * time.Minute

// discoverDependabot records each new Dependabot PR as a proposal. Off
// unless the fleet's policy has a dependabot class.
func (d *Daemon) discoverDependabot(ctx context.Context) {
	if _, ok := d.Fleet.Policy.Classes[DependabotJob]; !ok {
		return
	}
	d.mu.Lock()
	if d.scanned == nil {
		d.scanned = map[string]time.Time{}
	}
	d.mu.Unlock()
	for _, repo := range d.Fleet.Repos {
		d.mu.Lock()
		due := d.now().Sub(d.scanned[repo.Name]) >= dependabotScan
		if due {
			d.scanned[repo.Name] = d.now()
		}
		d.mu.Unlock()
		if !due {
			continue
		}
		slug := repo.GitHub
		if slug == "" {
			var ok bool
			if slug, ok = github.Slug(repo.URL); !ok {
				continue
			}
		}
		prs, err := d.GH.DependabotPRs(ctx, slug)
		if err != nil {
			d.fail("%s: dependabot PRs: %v", repo.Name, err)
			continue
		}
		for _, pr := range prs {
			known, err := d.Store.HasPR(ctx, pr.URL)
			if err != nil || known {
				continue
			}
			started := pr.CreatedAt
			if started.IsZero() {
				started = d.now()
			}
			r := &store.Run{Job: DependabotJob, Repo: repo.Name, Class: DependabotJob, Adapter: "dependabot",
				Status: store.Proposed, Branch: pr.HeadRef, PRURL: pr.URL, Summary: pr.Title, StartedAt: started}
			if err := d.Store.CreateRun(ctx, r); err != nil {
				d.fail("%s: %v", pr.URL, err)
				continue
			}
			// CreateRun stores only the run's identity; the PR and title go
			// in with the update.
			_ = d.Store.UpdateRun(ctx, r)
			d.Log.Printf("run %s: dependabot PR on %s: %s", r.ID, repo.Name, pr.Title)
		}
	}
}

// triageDependabot moves one open Dependabot PR along: verify the head once
// CI has finished, then close on FAIL, approve on PASS with green checks,
// and merge when every gate a MARSHAL proposal faces also passes.
func (d *Daemon) triageDependabot(ctx context.Context, ladder policy.Ladder, repo config.Repo, r *store.Run, pr github.PR, autonomy bool) {
	if pr.Checks == "PENDING" {
		return // a verdict on a head whose CI is still running is premature
	}
	verdict, ok, err := d.Store.LatestVerdict(ctx, r.ID)
	if err != nil {
		d.fail("run %s: %v", r.ID, err)
		return
	}
	if !ok || (pr.HeadSHA != "" && verdict.Head != pr.HeadSHA) {
		d.startVerify(ctx, *r, pr)
		return
	}
	// Broken is closed: a FAIL judgment, or red CI the verifier could not
	// pass. A deterministic red flag on an update whose CI is green is a
	// question for the operator, not a reason to close.
	broken := verdict.Verdict == verifier.Fail || (verdict.Verdict != verifier.Pass && pr.Checks == "FAILURE")
	switch {
	case broken:
		why := "the verifier failed head `" + short(verdict.Head) + "`"
		if verdict.Verdict != verifier.Fail {
			why = "its checks fail and the verifier did not pass head `" + short(verdict.Head) + "`"
		}
		comment := fmt.Sprintf("Closed by MARSHAL: %s.\n\n%s\n\nDependabot will propose the next version when there is one; reopen this if you disagree.", why, verdict.Summary)
		if err := d.GH.ClosePR(ctx, r.PRURL, comment); err != nil {
			d.fail("run %s: close: %v", r.ID, err)
			return
		}
		// Recorded as closed here, so the sync that sees it closed next
		// tick does not count it as the operator rejecting the class.
		r.Status = store.Rejected
		r.FinishedAt = d.now()
		d.note(ctx, r.ID, "closed: %s", why)
		d.send(ctx, notify.Event{Kind: notify.Failed, Title: r.Repo + ": closed a Dependabot PR", Body: firstLine(r.Summary) + " · " + firstLine(verdict.Summary), URL: r.PRURL})
		return
	case verdict.Verdict == verifier.Pass:
	default:
		return // UNSURE: labelled and notified when the verdict landed
	}
	if pr.Checks != "SUCCESS" {
		d.Log.Printf("run %s: verifier passed but checks are %q; holding", r.ID, pr.Checks)
		return
	}
	d.approveOnce(ctx, r, verdict.Head)
	by, reason := "", "autonomy is off"
	if autonomy {
		by, reason = d.mayMerge(ctx, ladder, repo, *r, pr)
	}
	if by == "" {
		if reason != "" {
			d.Log.Printf("run %s: approved, not merging: %s", r.ID, reason)
		}
		return
	}
	d.merge(ctx, r, by, verdict.Head)
}

// approveOnce approves a head at most once, so a PR that waits out a soak
// window is not reviewed every tick. The approved head is kept on the run
// record, whose Head a Dependabot run has no other use for.
func (d *Daemon) approveOnce(ctx context.Context, r *store.Run, head string) {
	if r.Head == head {
		return
	}
	body := fmt.Sprintf("Approved by MARSHAL: checks are green and the verifier passed head `%s`.", short(head))
	if err := d.GH.ApprovePR(ctx, r.PRURL, body); err != nil {
		d.fail("run %s: approve: %v", r.ID, err)
		return
	}
	r.Head = head
	d.note(ctx, r.ID, "approved head %s", short(head))
}
