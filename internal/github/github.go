// Package github is the thin layer over the gh CLI that the daemon needs:
// what state is a pull request in, is the default branch green, merge this.
//
// gh rather than the REST API directly because the host already has gh
// authenticated for the identity MARSHAL merges as, and because every call
// here is one the operator can reproduce by hand to check what happened.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Client runs gh.
type Client struct {
	// Env is added to every invocation; GH_TOKEN for a scoped identity.
	Env []string
}

// PR is the state of a pull request.
type PR struct {
	URL      string
	Number   int
	State    string // OPEN, MERGED, CLOSED
	MergedAt time.Time
	ClosedAt time.Time
	// Review is APPROVED, CHANGES_REQUESTED, REVIEW_REQUIRED or "".
	Review string
	// Checks is SUCCESS, FAILURE, PENDING or "" when there are none.
	Checks string
	Labels []string
	// MergedBy is the login that merged, when merged.
	MergedBy string
}

// Merged reports whether the PR was merged.
func (p PR) Merged() bool { return p.State == "MERGED" }

// Closed reports whether the PR was closed without merging.
func (p PR) Closed() bool { return p.State == "CLOSED" }

// HasLabel reports whether the PR carries a label.
func (p PR) HasLabel(l string) bool {
	for _, x := range p.Labels {
		if strings.EqualFold(x, l) {
			return true
		}
	}
	return false
}

type prJSON struct {
	Number         int       `json:"number"`
	State          string    `json:"state"`
	MergedAt       time.Time `json:"mergedAt"`
	ClosedAt       time.Time `json:"closedAt"`
	ReviewDecision string    `json:"reviewDecision"`
	MergedBy       struct {
		Login string `json:"login"`
	} `json:"mergedBy"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	StatusCheckRollup []struct {
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		State      string `json:"state"`
	} `json:"statusCheckRollup"`
}

// ViewPR fetches a pull request by URL.
func (c Client) ViewPR(ctx context.Context, url string) (PR, error) {
	out, err := c.gh(ctx, "pr", "view", url, "--json",
		"number,state,mergedAt,closedAt,reviewDecision,mergedBy,labels,statusCheckRollup")
	if err != nil {
		return PR{}, err
	}
	var j prJSON
	if err := json.Unmarshal(out, &j); err != nil {
		return PR{}, fmt.Errorf("gh pr view: %w", err)
	}
	p := PR{URL: url, Number: j.Number, State: j.State, MergedAt: j.MergedAt, ClosedAt: j.ClosedAt,
		Review: j.ReviewDecision, MergedBy: j.MergedBy.Login}
	for _, l := range j.Labels {
		p.Labels = append(p.Labels, l.Name)
	}
	p.Checks = rollup(j)
	return p, nil
}

// rollup collapses every check into one word. Any failure is FAILURE, any
// still running is PENDING, otherwise SUCCESS; no checks at all is "".
func rollup(j prJSON) string {
	if len(j.StatusCheckRollup) == 0 {
		return ""
	}
	pending := false
	for _, c := range j.StatusCheckRollup {
		concl := strings.ToUpper(c.Conclusion)
		if concl == "" {
			concl = strings.ToUpper(c.State) // commit statuses use state
		}
		switch concl {
		case "SUCCESS", "NEUTRAL", "SKIPPED":
		case "FAILURE", "ERROR", "TIMED_OUT", "CANCELLED", "ACTION_REQUIRED", "STARTUP_FAILURE":
			return "FAILURE"
		default:
			pending = true
		}
	}
	if pending {
		return "PENDING"
	}
	return "SUCCESS"
}

// MergePR squash-merges and deletes the branch.
func (c Client) MergePR(ctx context.Context, url string) error {
	_, err := c.gh(ctx, "pr", "merge", url, "--squash", "--delete-branch")
	return err
}

// CommentPR posts a comment.
func (c Client) CommentPR(ctx context.Context, url, body string) error {
	_, err := c.gh(ctx, "pr", "comment", url, "--body", body)
	return err
}

// CI is the state of the latest workflow run on a branch.
type CI struct {
	Status     string // completed, in_progress, queued
	Conclusion string // success, failure, cancelled, ...
	URL        string
	UpdatedAt  time.Time
}

// Red reports a completed run that did not succeed.
func (c CI) Red() bool {
	return c.Status == "completed" && c.Conclusion != "" && c.Conclusion != "success" && c.Conclusion != "skipped" && c.Conclusion != "neutral"
}

// LatestCI returns the most recent workflow run on a branch of a repo.
// ok is false when the repo has no runs.
func (c Client) LatestCI(ctx context.Context, slug, branch string) (CI, bool, error) {
	args := []string{"run", "list", "--repo", slug, "--limit", "1", "--json", "status,conclusion,url,updatedAt"}
	if branch != "" {
		args = append(args, "--branch", branch)
	}
	out, err := c.gh(ctx, args...)
	if err != nil {
		return CI{}, false, err
	}
	var runs []struct {
		Status     string    `json:"status"`
		Conclusion string    `json:"conclusion"`
		URL        string    `json:"url"`
		UpdatedAt  time.Time `json:"updatedAt"`
	}
	if err := json.Unmarshal(out, &runs); err != nil {
		return CI{}, false, fmt.Errorf("gh run list: %w", err)
	}
	if len(runs) == 0 {
		return CI{}, false, nil
	}
	r := runs[0]
	return CI{Status: r.Status, Conclusion: r.Conclusion, URL: r.URL, UpdatedAt: r.UpdatedAt}, true, nil
}

// DefaultBranch asks the remote for its default branch.
func (c Client) DefaultBranch(ctx context.Context, slug string) (string, error) {
	out, err := c.gh(ctx, "repo", "view", slug, "--json", "defaultBranchRef", "--jq", ".defaultBranchRef.name")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// OpenPRs lists open pull requests on a repo carrying a label.
func (c Client) OpenPRs(ctx context.Context, slug, label string) ([]PR, error) {
	out, err := c.gh(ctx, "pr", "list", "--repo", slug, "--label", label, "--state", "open", "--json", "number,url,labels")
	if err != nil {
		return nil, err
	}
	var list []struct {
		Number int    `json:"number"`
		URL    string `json:"url"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, err
	}
	var prs []PR
	for _, x := range list {
		p := PR{URL: x.URL, Number: x.Number, State: "OPEN"}
		for _, l := range x.Labels {
			p.Labels = append(p.Labels, l.Name)
		}
		prs = append(prs, p)
	}
	return prs, nil
}

var slugRE = regexp.MustCompile(`github\.com[:/]([^/]+)/([^/\s]+?)(?:\.git)?/?$`)

// Slug extracts owner/repo from a GitHub URL in ssh or https form. ok is
// false for anything else, such as a local path, in which case there is
// nothing for gh to talk to.
func Slug(url string) (string, bool) {
	m := slugRE.FindStringSubmatch(strings.TrimSpace(url))
	if m == nil {
		return "", false
	}
	return m[1] + "/" + m[2], true
}

func (c Client) gh(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Env = append(os.Environ(), c.Env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh %s: %w: %s", strings.Join(args[:2], " "), err, bytes.TrimSpace(stderr.Bytes()))
	}
	return out, nil
}
