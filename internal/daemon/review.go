package daemon

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Sousf/marshal/internal/github"
	"github.com/Sousf/marshal/internal/store"
	"github.com/Sousf/marshal/internal/verifier"
)

// Pull requests that people open on the fleet's repos are reviewed, not
// triaged: the verifier reads each new head and its verdict is posted, an
// allowlisted author's PR is approved on a PASS with green checks, and a
// FAIL requests changes. MARSHAL never pushes to, merges or closes such a
// PR on its own, and nothing about one moves the trust ladder. Closing is
// the operator's call, through Close.

// ReviewJob names the run records of reviewed pull requests.
const ReviewJob = "review"

// reviewScan is how often each repo is asked for new pull requests.
const reviewScan = 5 * time.Minute

// reviewable is who may have a pull request run on this host. The verifier
// checks out the head and runs its tests as the daemon's user, which can
// read the app key until runs are isolated, so a stranger's PR is not run.
var reviewable = map[string]bool{"OWNER": true, "MEMBER": true, "COLLABORATOR": true}

// discoverReviews records each new pull request a person with write access
// opened. Off unless the fleet enables review.
func (d *Daemon) discoverReviews(ctx context.Context) {
	if !d.Fleet.Review.Enabled {
		return
	}
	d.mu.Lock()
	if d.reviewScanned == nil {
		d.reviewScanned = map[string]time.Time{}
		d.reviewSkipped = map[string]bool{}
	}
	d.mu.Unlock()
	for _, repo := range d.Fleet.Repos {
		d.mu.Lock()
		due := d.now().Sub(d.reviewScanned[repo.Name]) >= reviewScan
		if due {
			d.reviewScanned[repo.Name] = d.now()
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
		prs, err := d.GH.PeoplePRs(ctx, slug)
		if err != nil {
			d.fail("%s: pull requests: %v", repo.Name, err)
			continue
		}
		for _, pr := range prs {
			if pr.Draft {
				continue // recorded once it is ready for review
			}
			if known, err := d.Store.HasPR(ctx, pr.URL); err != nil || known {
				continue
			}
			if !reviewable[pr.Association] {
				d.mu.Lock()
				seen := d.reviewSkipped[pr.URL]
				d.reviewSkipped[pr.URL] = true
				d.mu.Unlock()
				if !seen {
					d.Log.Printf("%s: not reviewing %s by %s (%s): only people with write access are run on this host", repo.Name, pr.URL, pr.Author, pr.Association)
				}
				continue
			}
			r := &store.Run{Job: ReviewJob, Repo: repo.Name, Class: ReviewJob, Adapter: "review",
				Status: store.Proposed, Branch: pullRef(pr.Number), PRURL: pr.URL, Summary: pr.Title, StartedAt: d.now()}
			if err := d.Store.CreateRun(ctx, r); err != nil {
				d.fail("%s: %v", pr.URL, err)
				continue
			}
			// CreateRun stores only the run's identity.
			_ = d.Store.UpdateRun(ctx, r)
			d.Log.Printf("run %s: reviewing %s by %s: %s", r.ID, pr.URL, pr.Author, pr.Title)
		}
	}
}

// pullRef is the ref GitHub keeps on the base repository for a PR's head,
// which a fork's branch does not have.
func pullRef(n int) string { return fmt.Sprintf("pull/%d/head", n) }

// syncReview moves one reviewed pull request along. The run's Head is the
// head MARSHAL last acted on, so each head gets one verdict and one review.
func (d *Daemon) syncReview(ctx context.Context, r *store.Run, pr github.PR, autonomy bool) {
	switch {
	case pr.Merged(), pr.Closed():
		r.Status = store.Rejected
		if pr.Merged() {
			r.Status = store.Merged
			r.MergedBy = "human"
			r.MergeSHA = pr.MergeSHA
		}
		r.FinishedAt = d.now()
		r.Settled = true // MARSHAL merged nothing, so there is nothing to watch
		d.note(ctx, r.ID, "%s; the review is over", strings.ToLower(pr.State))
		return
	case pr.Draft, pr.Checks == "PENDING":
		return // a verdict on a draft, or on a head whose CI is running, is premature
	}
	verdict, ok, err := d.Store.LatestVerdict(ctx, r.ID)
	if err != nil {
		d.fail("run %s: %v", r.ID, err)
		return
	}
	if !ok || (pr.HeadSHA != "" && verdict.Head != pr.HeadSHA) {
		pr.HeadRef = pullRef(pr.Number)
		d.startVerify(ctx, *r, pr)
		return
	}
	if r.Head == verdict.Head {
		return
	}
	switch verdict.Verdict {
	case verifier.Pass:
		if !d.Fleet.Review.MayApprove(pr.Author) {
			r.Head = verdict.Head // the verdict comment is the whole review
			return
		}
		if pr.Checks != "SUCCESS" || !autonomy {
			return // looked at again when CI is green or autonomy is back on
		}
		body := fmt.Sprintf("Approved by MARSHAL: checks are green and the verifier passed head `%s`.", short(verdict.Head))
		if err := d.GH.ApprovePR(ctx, r.PRURL, body); err != nil {
			d.fail("run %s: approve: %v", r.ID, err)
			return
		}
		d.note(ctx, r.ID, "approved head %s", short(verdict.Head))
	case verifier.Fail:
		body := fmt.Sprintf("MARSHAL's verifier failed head `%s`; its findings are in the comment above. A maintainer decides what happens to this pull request.", short(verdict.Head))
		if err := d.GH.RequestChanges(ctx, r.PRURL, body); err != nil {
			d.fail("run %s: request changes: %v", r.ID, err)
			return
		}
		d.note(ctx, r.ID, "requested changes on head %s", short(verdict.Head))
	}
	// UNSURE was labelled and notified when the verdict landed.
	r.Head = verdict.Head
}

// Close closes a pull request on one of the fleet's repos at the operator's
// request. ref is a run id or a PR URL; the reason is posted on the PR.
func (d *Daemon) Close(ctx context.Context, ref, reason string) (string, error) {
	d.init()
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return "", fmt.Errorf("close: give a reason; it is posted on the pull request")
	}
	url := ref
	if !strings.Contains(ref, "/") {
		r, err := d.Store.GetRun(ctx, ref)
		if err != nil {
			return "", err
		}
		if r.PRURL == "" {
			return "", fmt.Errorf("run %s has no pull request", r.ID)
		}
		url = r.PRURL
	}
	slug, ok := github.Slug(url)
	if !ok || !d.managed(slug) {
		return "", fmt.Errorf("close: %s is not a pull request on a fleet repository", url)
	}
	pr, err := d.GH.ViewPR(ctx, url)
	if err != nil {
		return "", err
	}
	if pr.State != "OPEN" {
		return "", fmt.Errorf("close: %s is already %s", url, strings.ToLower(pr.State))
	}
	if err := d.GH.ClosePR(ctx, url, "Closed by MARSHAL at the maintainer's request: "+reason); err != nil {
		return "", err
	}
	d.Log.Printf("closed %s at the operator's request: %s", url, reason)
	return url, nil
}

// managed reports whether owner/name is one of the fleet's repositories.
func (d *Daemon) managed(slug string) bool {
	for _, repo := range d.Fleet.Repos {
		s := repo.GitHub
		if s == "" {
			s, _ = github.Slug(repo.URL)
		}
		if strings.EqualFold(s, slug) {
			return true
		}
	}
	return false
}
