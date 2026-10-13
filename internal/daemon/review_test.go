package daemon

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Sousf/marshal/internal/config"
	"github.com/Sousf/marshal/internal/github"
	"github.com/Sousf/marshal/internal/store"
)

// personPR puts an open PR by a person into the fake, with its head at
// pull/<n>/head in the origin as GitHub keeps it.
func personPR(t *testing.T, gh *fakeGH, dir string, n int, author, assoc string) string {
	t.Helper()
	url := fmt.Sprintf("https://github.com/Sousf/x/pull/%d", n)
	sha := branch(t, dir, pullRef(n))
	pr := github.PR{URL: url, Number: n, State: "OPEN", Checks: "SUCCESS", HeadRef: "feature-" + author, HeadSHA: sha,
		Title: "change by " + author, Author: author, Association: assoc}
	gh.mu.Lock()
	gh.prs[url] = pr
	gh.people = append(gh.people, pr)
	gh.mu.Unlock()
	return url
}

func reviewDaemon(t *testing.T) (*Daemon, *fakeGH, *fakeAdapter, string) {
	gh := &fakeGH{prs: map[string]github.PR{}}
	d, a, dir := newDaemonDir(t, gh)
	d.Fleet.Review = config.PRReview{Enabled: true, ApproveAuthors: []string{"sousf"}}
	return d, gh, a, dir
}

func TestPeoplesPRsAreReviewed(t *testing.T) {
	ctx := context.Background()
	d, gh, a, dir := reviewDaemon(t)
	owner := personPR(t, gh, dir, 1, "Sousf", "OWNER")
	collab := personPR(t, gh, dir, 2, "alice", "COLLABORATOR")
	stranger := personPR(t, gh, dir, 3, "mallory", "NONE")
	draft := personPR(t, gh, dir, 4, "Sousf", "OWNER")
	gh.mu.Lock()
	gh.people[3].Draft = true
	gh.mu.Unlock()

	d.discoverReviews(ctx)
	settle(t, d)
	wait(t, d)
	settle(t, d)

	for _, url := range []string{stranger, draft} {
		if known, _ := d.Store.HasPR(ctx, url); known {
			t.Errorf("%s should not be recorded", url)
		}
	}
	if len(gh.approved) != 1 || gh.approved[0] != owner {
		t.Errorf("approved = %v; only the allowlisted author's PR may be approved", gh.approved)
	}
	if len(gh.merged) != 0 || len(gh.closed) != 0 {
		t.Errorf("merged=%v closed=%v; a person's PR is never merged or closed by MARSHAL", gh.merged, gh.closed)
	}
	if v, ok, _ := latestFor(t, d, collab); !ok || v != "PASS" {
		t.Errorf("collaborator's PR verdict = %q %v", v, ok)
	}
	if len(a.verifies) != 2 {
		t.Errorf("verifier ran %d times; want the owner's and the collaborator's PRs", len(a.verifies))
	} else if !strings.Contains(a.verifies[0], "a pull request a person opened") {
		t.Error("the verifier should be told this is a person's PR, not MARSHAL's")
	}
	for _, p := range append(a.verifies, a.prompts...) {
		if strings.Contains(p, "mallory") {
			t.Error("a stranger's PR reached an agent")
		}
	}

	// Nothing new: no second verdict, no second approval.
	settle(t, d)
	wait(t, d)
	if len(a.verifies) != 2 || len(gh.approved) != 1 {
		t.Errorf("re-reviewed an unchanged head: %d verifier runs, approved=%v", len(a.verifies), gh.approved)
	}

	// A FAIL requests changes and still closes nothing.
	a.verdict = "FAIL"
	bad := personPR(t, gh, dir, 5, "alice", "COLLABORATOR")
	d.reviewScanned = nil
	d.discoverReviews(ctx)
	settle(t, d)
	wait(t, d)
	settle(t, d)
	if len(gh.changes) != 1 || gh.changes[0] != bad {
		t.Errorf("changes requested on %v", gh.changes)
	}
	if len(gh.closed) != 0 {
		t.Errorf("closed %v on a FAIL", gh.closed)
	}

	// The owner merges theirs: recorded, and the ladder does not move.
	gh.mu.Lock()
	p := gh.prs[owner]
	p.State = "MERGED"
	gh.prs[owner] = p
	gh.mu.Unlock()
	settle(t, d)
	runs, _ := d.Store.ListByStatus(ctx, store.Merged)
	if len(runs) != 1 || runs[0].PRURL != owner || !runs[0].Settled {
		t.Errorf("merged runs = %+v", runs)
	}
	if trust, _ := d.Store.ListTrust(ctx); len(trust) != 0 {
		t.Errorf("a person's PR moved the ladder: %+v", trust)
	}
}

func TestReviewApprovalRespectsKillSwitch(t *testing.T) {
	ctx := context.Background()
	d, gh, _, dir := reviewDaemon(t)
	if err := d.Store.SetSetting(ctx, autonomyKey, "off"); err != nil {
		t.Fatal(err)
	}
	owner := personPR(t, gh, dir, 1, "Sousf", "OWNER")
	d.discoverReviews(ctx)
	settle(t, d)
	wait(t, d)
	settle(t, d)
	if len(gh.approved) != 0 {
		t.Fatalf("approved %v with autonomy off", gh.approved)
	}
	if err := d.Store.SetSetting(ctx, autonomyKey, "on"); err != nil {
		t.Fatal(err)
	}
	settle(t, d)
	if len(gh.approved) != 1 || gh.approved[0] != owner {
		t.Errorf("approved = %v once autonomy is back on", gh.approved)
	}
}

func TestCloseOnlyOnRequest(t *testing.T) {
	ctx := context.Background()
	d, gh, _, dir := reviewDaemon(t)
	url := personPR(t, gh, dir, 1, "alice", "COLLABORATOR")
	if _, err := d.Close(ctx, url, "  "); err == nil {
		t.Error("a close without a reason should be refused")
	}
	gh.prs["https://github.com/someone/else/pull/1"] = github.PR{State: "OPEN"}
	if _, err := d.Close(ctx, "https://github.com/someone/else/pull/1", "spam"); err == nil {
		t.Error("a PR outside the fleet should be refused")
	}
	if _, err := d.Close(ctx, url, "superseded by #2"); err != nil {
		t.Fatal(err)
	}
	if len(gh.closed) != 1 || gh.closed[0] != url {
		t.Errorf("closed = %v", gh.closed)
	}
	if _, err := d.Close(ctx, url, "again"); err == nil {
		t.Error("closing a closed PR should be refused")
	}
}

func latestFor(t *testing.T, d *Daemon, url string) (string, bool, error) {
	t.Helper()
	runs, err := d.Store.ListByStatus(context.Background(), store.Proposed)
	if err != nil {
		return "", false, err
	}
	for _, r := range runs {
		if r.PRURL == url {
			v, ok, err := d.Store.LatestVerdict(context.Background(), r.ID)
			return v.Verdict, ok, err
		}
	}
	return "", false, nil
}
