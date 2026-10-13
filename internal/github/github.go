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
	"sort"
	"strconv"
	"strings"
	"time"
)

// Client runs gh.
type Client struct {
	// Env is added to every invocation; GH_TOKEN for a scoped identity.
	Env []string
	// Auth, when set, supplies a fresh environment per call: the GitHub App
	// token, which rotates hourly.
	Auth func(ctx context.Context) ([]string, error)
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
	// Mergeable is MERGEABLE, CONFLICTING or UNKNOWN, which GitHub reports
	// while it is still computing the answer after a push.
	Mergeable string
	Labels    []string
	// MergedBy is the login that merged, when merged.
	MergedBy string
	// HeadSHA is the commit the PR currently points at; HeadRef its branch.
	HeadSHA string
	HeadRef string
	BaseRef string
	Title   string
	Body    string
	// MergeSHA is the merge commit once merged.
	MergeSHA string
	// CreatedAt is when the PR was opened.
	CreatedAt time.Time
	// Author is the login that opened the PR. Association is GitHub's
	// author_association: OWNER, MEMBER, COLLABORATOR, CONTRIBUTOR, NONE...
	Author      string
	Association string
	Draft       bool
}

// Merged reports whether the PR was merged.
func (p PR) Merged() bool { return p.State == "MERGED" }

// Closed reports whether the PR was closed without merging.
func (p PR) Closed() bool { return p.State == "CLOSED" }

// Conflicting reports whether the PR cannot merge into its base until
// someone resolves the conflicts.
func (p PR) Conflicting() bool { return p.Mergeable == "CONFLICTING" }

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
	Mergeable      string    `json:"mergeable"`
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
	HeadRefOid  string `json:"headRefOid"`
	HeadRefName string `json:"headRefName"`
	BaseRefName string `json:"baseRefName"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	MergeCommit struct {
		Oid string `json:"oid"`
	} `json:"mergeCommit"`
	Author struct {
		Login string `json:"login"`
	} `json:"author"`
	IsDraft bool `json:"isDraft"`
}

// ViewPR fetches a pull request by URL.
func (c Client) ViewPR(ctx context.Context, url string) (PR, error) {
	out, err := c.gh(ctx, "pr", "view", url, "--json",
		"number,state,mergedAt,closedAt,reviewDecision,mergeable,mergedBy,labels,statusCheckRollup,headRefOid,headRefName,baseRefName,title,body,mergeCommit,author,isDraft")
	if err != nil {
		return PR{}, err
	}
	var j prJSON
	if err := json.Unmarshal(out, &j); err != nil {
		return PR{}, fmt.Errorf("gh pr view: %w", err)
	}
	p := PR{URL: url, Number: j.Number, State: j.State, MergedAt: j.MergedAt, ClosedAt: j.ClosedAt,
		Review: j.ReviewDecision, MergedBy: j.MergedBy.Login, HeadSHA: j.HeadRefOid, HeadRef: j.HeadRefName,
		BaseRef: j.BaseRefName, Title: j.Title, Body: j.Body, MergeSHA: j.MergeCommit.Oid, Mergeable: j.Mergeable,
		Author: j.Author.Login, Draft: j.IsDraft}
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

// AddLabel puts a label on a PR, creating it in the repo if needed.
func (c Client) AddLabel(ctx context.Context, url, label string) error {
	// Label creation needs the repo; gh resolves it from the PR URL for
	// `pr edit` but not for `label create`, so derive the slug.
	if slug, ok := Slug(url); ok {
		_, _ = c.gh(ctx, "label", "create", label, "--repo", slug, "--color", "d93f0b", "--description", "Set by MARSHAL")
	}
	_, err := c.gh(ctx, "pr", "edit", url, "--add-label", label)
	return err
}

// CreatePR opens a pull request on slug from head into base and returns
// its URL. Used for reverts, which are made from a clone that gh cannot
// resolve a repository from.
func (c Client) CreatePR(ctx context.Context, slug, head, base, title, body string, labels []string) (string, error) {
	args := []string{"pr", "create", "--repo", slug, "--head", head, "--base", base, "--title", title, "--body", body}
	for _, l := range labels {
		_, _ = c.gh(ctx, "label", "create", l, "--repo", slug, "--color", "5319e7", "--description", "Opened by MARSHAL")
		args = append(args, "--label", l)
	}
	out, err := c.gh(ctx, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// RequestReview asks reviewers, GitHub logins or org/team, to review a
// pull request.
func (c Client) RequestReview(ctx context.Context, url string, reviewers []string) error {
	if len(reviewers) == 0 {
		return nil
	}
	_, err := c.gh(ctx, "pr", "edit", url, "--add-reviewer", strings.Join(reviewers, ","))
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

// CommitCI summarises every workflow run for one commit: red if any
// completed run failed, pending if any is still going, green if all
// completed well. ok is false when no run exists for the commit.
func (c Client) CommitCI(ctx context.Context, slug, sha string) (CI, bool, error) {
	out, err := c.gh(ctx, "run", "list", "--repo", slug, "--commit", sha, "--limit", "20", "--json", "status,conclusion,url,updatedAt")
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
	agg := CI{Status: "completed", Conclusion: "success"}
	for _, r := range runs {
		one := CI{Status: r.Status, Conclusion: r.Conclusion, URL: r.URL, UpdatedAt: r.UpdatedAt}
		switch {
		case one.Red():
			return one, true, nil
		case one.Status != "completed":
			agg = CI{Status: one.Status, URL: one.URL, UpdatedAt: one.UpdatedAt}
		}
	}
	return agg, true, nil
}

// FailedLogs returns the failed-step logs of the workflow runs for a commit,
// bounded to limit bytes per run, as context for a fix. Empty when nothing
// failed or the runs are still going.
func (c Client) FailedLogs(ctx context.Context, slug, sha string, limit int) (string, error) {
	out, err := c.gh(ctx, "run", "list", "--repo", slug, "--commit", sha, "--limit", "10",
		"--json", "databaseId,status,conclusion,workflowName,name")
	if err != nil {
		return "", err
	}
	var runs []struct {
		ID           int64  `json:"databaseId"`
		Status       string `json:"status"`
		Conclusion   string `json:"conclusion"`
		WorkflowName string `json:"workflowName"`
		Name         string `json:"name"`
	}
	if err := json.Unmarshal(out, &runs); err != nil {
		return "", fmt.Errorf("gh run list: %w", err)
	}
	if limit <= 0 {
		limit = 64 * 1024
	}
	var b strings.Builder
	for _, r := range runs {
		if r.Status != "completed" || (CI{Status: r.Status, Conclusion: r.Conclusion}).Red() == false {
			continue
		}
		log, err := c.gh(ctx, "run", "view", strconv.FormatInt(r.ID, 10), "--repo", slug, "--log-failed")
		if err != nil {
			// A run with no failed steps (cancelled, startup failure) has
			// no failed log; say so rather than fail the whole fetch.
			fmt.Fprintf(&b, "## %s (run %d): %s, no step log available\n\n", r.WorkflowName, r.ID, r.Conclusion)
			continue
		}
		text := string(log)
		if len(text) > limit {
			text = "[... earlier output trimmed ...]\n" + text[len(text)-limit:]
		}
		fmt.Fprintf(&b, "## %s (run %d): %s\n\n```\n%s\n```\n\n", r.WorkflowName, r.ID, r.Conclusion, strings.TrimSpace(text))
	}
	return b.String(), nil
}

// Comment is something a person wrote on a pull request.
type Comment struct {
	Author string
	At     time.Time
	Body   string
	// Where is "review", "comment" or a file path for an inline comment.
	Where string
}

type prCommentsJSON struct {
	Comments []struct {
		Author struct {
			Login string `json:"login"`
		} `json:"author"`
		Body      string    `json:"body"`
		CreatedAt time.Time `json:"createdAt"`
	} `json:"comments"`
	Reviews []struct {
		Author struct {
			Login string `json:"login"`
		} `json:"author"`
		Body        string    `json:"body"`
		State       string    `json:"state"`
		SubmittedAt time.Time `json:"submittedAt"`
	} `json:"reviews"`
}

type inlineJSON struct {
	User struct {
		Login string `json:"login"`
	} `json:"user"`
	Body      string    `json:"body"`
	Path      string    `json:"path"`
	CreatedAt time.Time `json:"created_at"`
}

// HumanComments returns the comments, reviews and inline review comments on
// a PR written after since by anyone who is not a bot or the given login,
// oldest first.
func (c Client) HumanComments(ctx context.Context, url string, exclude string, since time.Time) ([]Comment, error) {
	out, err := c.gh(ctx, "pr", "view", url, "--json", "comments,reviews,number")
	if err != nil {
		return nil, err
	}
	var pc prCommentsJSON
	if err := json.Unmarshal(out, &pc); err != nil {
		return nil, err
	}
	var inline []inlineJSON
	if slug, ok := Slug(url); ok {
		var num struct {
			Number int `json:"number"`
		}
		_ = json.Unmarshal(out, &num)
		if raw, err := c.gh(ctx, "api", fmt.Sprintf("repos/%s/pulls/%d/comments?per_page=100", slug, num.Number)); err == nil {
			_ = json.Unmarshal(raw, &inline)
		}
	}
	return filterComments(pc, inline, exclude, since), nil
}

func filterComments(pc prCommentsJSON, inline []inlineJSON, exclude string, since time.Time) []Comment {
	human := func(login string) bool {
		l := strings.ToLower(login)
		return l != "" && l != strings.ToLower(exclude) && !strings.HasSuffix(l, "[bot]") && !strings.HasPrefix(l, "app/")
	}
	var out []Comment
	for _, x := range pc.Comments {
		if human(x.Author.Login) && x.CreatedAt.After(since) && strings.TrimSpace(x.Body) != "" {
			out = append(out, Comment{Author: x.Author.Login, At: x.CreatedAt, Body: x.Body, Where: "comment"})
		}
	}
	for _, x := range pc.Reviews {
		if human(x.Author.Login) && x.SubmittedAt.After(since) && strings.TrimSpace(x.Body) != "" {
			out = append(out, Comment{Author: x.Author.Login, At: x.SubmittedAt, Body: x.Body, Where: "review " + strings.ToLower(x.State)})
		}
	}
	for _, x := range inline {
		if human(x.User.Login) && x.CreatedAt.After(since) && strings.TrimSpace(x.Body) != "" {
			out = append(out, Comment{Author: x.User.Login, At: x.CreatedAt, Body: x.Body, Where: x.Path})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

// RenderComments formats comments for a brief or a record.
func RenderComments(cs []Comment) string {
	var b strings.Builder
	for _, c := range cs {
		fmt.Fprintf(&b, "- %s (%s, %s): %s\n", c.Author, c.Where, c.At.Format("Jan 2 15:04"), strings.TrimSpace(strings.ReplaceAll(c.Body, "\n", "\n  ")))
	}
	return b.String()
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

var slugRE = regexp.MustCompile(`github\.com[:/]([^/\s]+)/([^/\s]+?)(?:\.git)?(?:/.*)?$`)

// Slug extracts owner/repo from a GitHub URL in ssh or https form, including
// a pull request URL. ok is false for anything else, such as a local path,
// in which case there is nothing for gh to talk to.
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
	if c.Auth != nil {
		env, err := c.Auth(ctx)
		if err != nil {
			return nil, err
		}
		cmd.Env = append(cmd.Env, env...)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh %s: %w: %s", strings.Join(args[:2], " "), err, bytes.TrimSpace(stderr.Bytes()))
	}
	return out, nil
}

// DependabotPRs lists the open pull requests Dependabot opened on slug.
func (c Client) DependabotPRs(ctx context.Context, slug string) ([]PR, error) {
	out, err := c.gh(ctx, "pr", "list", "--repo", slug, "--author", "app/dependabot", "--state", "open",
		"--json", "number,url,title,headRefName,headRefOid,createdAt", "--limit", "50")
	if err != nil {
		return nil, err
	}
	var list []struct {
		Number    int       `json:"number"`
		URL       string    `json:"url"`
		Title     string    `json:"title"`
		HeadRef   string    `json:"headRefName"`
		HeadSHA   string    `json:"headRefOid"`
		CreatedAt time.Time `json:"createdAt"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, err
	}
	prs := make([]PR, 0, len(list))
	for _, x := range list {
		prs = append(prs, PR{URL: x.URL, Number: x.Number, State: "OPEN", Title: x.Title, HeadRef: x.HeadRef, HeadSHA: x.HeadSHA, CreatedAt: x.CreatedAt})
	}
	return prs, nil
}

// PeoplePRs lists the open pull requests on a repository that a person
// opened, leaving out every bot's, with GitHub's author_association, which
// gh pr list does not report.
func (c Client) PeoplePRs(ctx context.Context, slug string) ([]PR, error) {
	out, err := c.gh(ctx, "api", "repos/"+slug+"/pulls?state=open&per_page=50")
	if err != nil {
		return nil, err
	}
	var list []struct {
		Number  int       `json:"number"`
		URL     string    `json:"html_url"`
		Title   string    `json:"title"`
		Draft   bool      `json:"draft"`
		Created time.Time `json:"created_at"`
		Assoc   string    `json:"author_association"`
		User    struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"user"`
		Head struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, err
	}
	var prs []PR
	for _, x := range list {
		if x.User.Type != "User" {
			continue
		}
		prs = append(prs, PR{URL: x.URL, Number: x.Number, State: "OPEN", Title: x.Title, HeadRef: x.Head.Ref, HeadSHA: x.Head.SHA,
			CreatedAt: x.Created, Author: x.User.Login, Association: x.Assoc, Draft: x.Draft})
	}
	return prs, nil
}

// RequestChanges submits a review that requests changes.
func (c Client) RequestChanges(ctx context.Context, url, body string) error {
	_, err := c.gh(ctx, "pr", "review", url, "--request-changes", "--body", body)
	return err
}

// ApprovePR submits an approving review.
func (c Client) ApprovePR(ctx context.Context, url, body string) error {
	_, err := c.gh(ctx, "pr", "review", url, "--approve", "--body", body)
	return err
}

// ClosePR closes a pull request with a comment saying why.
func (c Client) ClosePR(ctx context.Context, url, comment string) error {
	_, err := c.gh(ctx, "pr", "close", url, "--comment", comment)
	return err
}

// FileText returns a file's contents from a repository's default branch.
func (c Client) FileText(ctx context.Context, slug, path string) (string, error) {
	out, err := c.gh(ctx, "api", "repos/"+slug+"/contents/"+path, "-H", "Accept: application/vnd.github.raw")
	return string(out), err
}
