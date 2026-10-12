// Package workspace gives each run a fresh clone, a branch of its own, and
// the git plumbing to turn whatever the agent changed into a pull request.
//
// The agent never commits or pushes. It edits files; this package decides,
// after policy has looked at the diff, what happens to them. Keeping that
// split is what lets the harness enforce the rules rather than ask for them.
package workspace

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Sousf/marshal/internal/config"
)

// Workspace is one run's checkout.
type Workspace struct {
	Dir    string
	Branch string
	Repo   config.Repo
	// Identity, when set, is the committer. Empty inherits the host's git
	// config.
	Identity config.Identity
	// Env is added to every git and gh invocation; the per-run token goes
	// here as GH_TOKEN so it never lives in the daemon's environment.
	Env []string
	// ReviewErr is set when OpenPR opened the pull request but GitHub
	// refused the review request. The pull request stands either way.
	ReviewErr error
}

// Prepare clones repo into a fresh directory under root and checks out a new
// branch named for the job and run.
func Prepare(ctx context.Context, root string, repo config.Repo, job, runID string, id config.Identity, env []string) (*Workspace, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(root, "run-"+runID+"-")
	if err != nil {
		return nil, err
	}
	w := &Workspace{Dir: dir, Repo: repo, Identity: id, Env: env, Branch: BranchName(job, runID)}
	// A full clone, not a shallow one: the agent may need history to
	// understand why something is the way it is, and the repos in a personal
	// fleet are small.
	args := []string{"clone", "--quiet"}
	if repo.Branch != "" {
		args = append(args, "--branch", repo.Branch)
	}
	if _, err := w.gitIn(ctx, root, append(args, repo.URL, dir)...); err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("clone %s: %w", repo.URL, err)
	}
	if w.Repo.Branch == "" {
		// The clone checked out the remote's HEAD; that is the base branch.
		head, err := w.git(ctx, "symbolic-ref", "--short", "HEAD")
		if err != nil {
			os.RemoveAll(dir)
			return nil, fmt.Errorf("%s: cannot determine default branch: %w", repo.URL, err)
		}
		w.Repo.Branch = strings.TrimSpace(head)
	}
	if _, err := w.git(ctx, "checkout", "--quiet", "-b", w.Branch); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	return w, nil
}

// PrepareExisting clones repo and checks out an existing proposal branch so
// a follow-up can commit and push to it. The branch tracks origin, so Push
// updates the open pull request.
func PrepareExisting(ctx context.Context, root string, repo config.Repo, runID, branch string, id config.Identity, env []string) (*Workspace, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(root, "run-"+runID+"-")
	if err != nil {
		return nil, err
	}
	w := &Workspace{Dir: dir, Repo: repo, Identity: id, Env: env, Branch: branch}
	args := []string{"clone", "--quiet"}
	if repo.Branch != "" {
		args = append(args, "--branch", repo.Branch)
	}
	if _, err := w.gitIn(ctx, root, append(args, repo.URL, dir)...); err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("clone %s: %w", repo.URL, err)
	}
	if w.Repo.Branch == "" {
		head, err := w.git(ctx, "symbolic-ref", "--short", "HEAD")
		if err != nil {
			os.RemoveAll(dir)
			return nil, fmt.Errorf("%s: cannot determine default branch: %w", repo.URL, err)
		}
		w.Repo.Branch = strings.TrimSpace(head)
	}
	if _, err := w.git(ctx, "fetch", "--quiet", "origin", branch+":"+branch); err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("fetch %s: %w", branch, err)
	}
	if _, err := w.git(ctx, "checkout", "--quiet", branch); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	return w, nil
}

// PrepareHead clones repo and checks out a proposal's head commit, for the
// verifier. The base branch is fetched too so the diff can be taken against
// the exact point the proposal branched from. Nothing here is ever pushed.
func PrepareHead(ctx context.Context, root string, repo config.Repo, runID, headRef, headSHA string, env []string) (*Workspace, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(root, "verify-"+runID+"-")
	if err != nil {
		return nil, err
	}
	w := &Workspace{Dir: dir, Repo: repo, Env: env, Branch: headRef}
	args := []string{"clone", "--quiet"}
	if repo.Branch != "" {
		args = append(args, "--branch", repo.Branch)
	}
	if _, err := w.gitIn(ctx, root, append(args, repo.URL, dir)...); err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("clone %s: %w", repo.URL, err)
	}
	if w.Repo.Branch == "" {
		head, err := w.git(ctx, "symbolic-ref", "--short", "HEAD")
		if err != nil {
			os.RemoveAll(dir)
			return nil, fmt.Errorf("%s: cannot determine default branch: %w", repo.URL, err)
		}
		w.Repo.Branch = strings.TrimSpace(head)
	}
	if _, err := w.git(ctx, "fetch", "--quiet", "origin", headRef); err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("fetch %s: %w", headRef, err)
	}
	target := headSHA
	if target == "" {
		target = "FETCH_HEAD"
	}
	if _, err := w.git(ctx, "checkout", "--quiet", "--detach", target); err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("checkout %s: %w", target, err)
	}
	return w, nil
}

// MergeBase starts merging the base branch into the checked-out branch and
// stops before committing, so the agent can resolve what git could not and
// Commit then records a merge commit. It returns the paths left in
// conflict; none means git merged everything itself. A merge that fails
// for any other reason is aborted and returned as an error.
func (w *Workspace) MergeBase(ctx context.Context) ([]string, error) {
	_, mergeErr := w.git(ctx, "merge", "--no-ff", "--no-commit", w.Base())
	out, err := w.git(ctx, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil, err
	}
	var conflicts []string
	for _, p := range strings.Split(strings.TrimSpace(out), "\n") {
		if p != "" {
			conflicts = append(conflicts, p)
		}
	}
	if mergeErr != nil && len(conflicts) == 0 {
		_, _ = w.git(ctx, "merge", "--abort")
		return nil, fmt.Errorf("merge %s: %w", w.Base(), mergeErr)
	}
	return conflicts, nil
}

// Merging reports whether a merge is still in progress, so a merge the
// agent aborted is not mistaken for a resolved one.
func (w *Workspace) Merging(ctx context.Context) bool {
	_, err := w.git(ctx, "rev-parse", "--quiet", "--verify", "MERGE_HEAD")
	return err == nil
}

// BaseNumstat is Numstat against the tip of the base branch instead of the
// branch point: after a merge, what the pull request will show, without
// the base's own changes that the merge brought in.
func (w *Workspace) BaseNumstat(ctx context.Context) (string, error) {
	if _, err := w.git(ctx, "add", "--all"); err != nil {
		return "", err
	}
	return w.git(ctx, "diff", "--cached", "--numstat", w.Base())
}

// Revert creates a commit undoing sha on the current branch. A conflict is
// returned as an error with git's explanation; nothing is left half done.
func (w *Workspace) Revert(ctx context.Context, sha, message string) error {
	if _, err := w.git(ctx, "revert", "--no-edit", "--no-commit", sha); err != nil {
		_, _ = w.git(ctx, "revert", "--abort")
		return fmt.Errorf("revert %s: %w", sha, err)
	}
	return w.Commit(ctx, message)
}

// HeadSHA returns the checked-out commit.
func (w *Workspace) HeadSHA(ctx context.Context) (string, error) {
	out, err := w.git(ctx, "rev-parse", "HEAD")
	return strings.TrimSpace(out), err
}

// Base is the ref the proposal is compared against.
func (w *Workspace) Base() string { return "origin/" + w.Repo.Branch }

// DiffNumstat is `git diff --numstat base...HEAD`.
func (w *Workspace) DiffNumstat(ctx context.Context) (string, error) {
	return w.git(ctx, "diff", "--numstat", w.Base()+"...HEAD")
}

// DiffPatch is the full textual diff from the merge base to HEAD.
func (w *Workspace) DiffPatch(ctx context.Context) (string, error) {
	return w.git(ctx, "diff", "--no-color", "--no-ext-diff", w.Base()+"...HEAD")
}

// FileAt returns a file's contents at a revision; empty if it does not
// exist there.
func (w *Workspace) FileAt(ctx context.Context, rev, path string) string {
	out, err := w.git(ctx, "show", rev+":"+path)
	if err != nil {
		return ""
	}
	return out
}

// RemoteHead asks the remote for the tip of a branch without cloning. An
// empty branch means the remote's HEAD.
func RemoteHead(ctx context.Context, repo config.Repo, env []string) (string, error) {
	w := &Workspace{Repo: repo, Env: env}
	ref := repo.Branch
	if ref == "" {
		ref = "HEAD"
	}
	out, err := w.gitIn(ctx, "", "ls-remote", repo.URL, ref)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Fields(line)
		if len(f) >= 1 && len(f[0]) == 40 {
			return f[0], nil
		}
	}
	return "", fmt.Errorf("ls-remote %s %s: no such ref", repo.URL, ref)
}

// BranchName is marshal/<job>/<run>, so a glance at the branch list says
// what made it and the run record is one lookup away.
func BranchName(job, runID string) string {
	return "marshal/" + job + "/" + runID
}

// Remove deletes the checkout.
func (w *Workspace) Remove() error { return os.RemoveAll(w.Dir) }

// Conventions returns the repo's AGENTS.md, which the runner appends to the
// system prompt. Missing is not an error: the brief alone has to be enough
// for a repo that has not been onboarded yet.
func (w *Workspace) Conventions() string {
	for _, name := range []string{"AGENTS.md", ".marshal/AGENTS.md"} {
		if b, err := os.ReadFile(filepath.Join(w.Dir, name)); err == nil {
			return string(b)
		}
	}
	return ""
}

// Numstat returns `git diff --numstat` against the branch point, including
// untracked files, which plain diff would miss.
func (w *Workspace) Numstat(ctx context.Context) (string, error) {
	// Stage everything so new files show up in the diff with real counts.
	// Nothing is committed here; a run that fails policy is simply removed.
	if _, err := w.git(ctx, "add", "--all"); err != nil {
		return "", err
	}
	return w.git(ctx, "diff", "--cached", "--numstat")
}

// Changed reports whether the tree differs from the branch point.
func (w *Workspace) Changed(ctx context.Context) (bool, error) {
	out, err := w.git(ctx, "status", "--porcelain")
	return strings.TrimSpace(out) != "", err
}

// Commit stages everything and commits with the given message.
func (w *Workspace) Commit(ctx context.Context, message string) error {
	if _, err := w.git(ctx, "add", "--all"); err != nil {
		return err
	}
	args := []string{"commit", "--quiet", "--file", "-"}
	cmd := w.cmd(ctx, w.Dir, "git", args...)
	cmd.Stdin = strings.NewReader(message)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git commit: %w: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

// Push publishes the branch.
func (w *Workspace) Push(ctx context.Context) error {
	_, err := w.git(ctx, "push", "--quiet", "--set-upstream", "origin", w.Branch)
	return err
}

// OpenPR creates a pull request with the gh CLI and returns its URL. The
// label is created if the repo lacks it, so a fresh repo works first time.
func (w *Workspace) OpenPR(ctx context.Context, title, body string, labels []string) (string, error) {
	for _, l := range labels {
		// Idempotent; errors only when the label exists, which is fine.
		_ = w.cmd(ctx, w.Dir, "gh", "label", "create", l, "--color", "5319e7",
			"--description", "Opened by MARSHAL").Run()
	}
	args := []string{"pr", "create", "--base", w.Repo.Branch, "--head", w.Branch,
		"--title", title, "--body-file", "-"}
	for _, l := range labels {
		args = append(args, "--label", l)
	}
	cmd := w.cmd(ctx, w.Dir, "gh", args...)
	cmd.Stdin = strings.NewReader(body)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		msg := ""
		if ok := asExit(err, &ee); ok {
			msg = string(bytes.TrimSpace(ee.Stderr))
		}
		return "", fmt.Errorf("gh pr create: %w: %s", err, msg)
	}
	url := strings.TrimSpace(string(out))
	// Requested after creating, not with --reviewer, so a login GitHub
	// refuses costs the review request and not the pull request.
	if len(w.Repo.Reviewers) > 0 {
		edit := w.cmd(ctx, w.Dir, "gh", "pr", "edit", url, "--add-reviewer", strings.Join(w.Repo.Reviewers, ","))
		var stderr bytes.Buffer
		edit.Stderr = &stderr
		if err := edit.Run(); err != nil {
			w.ReviewErr = fmt.Errorf("requesting review from %s: %w: %s", strings.Join(w.Repo.Reviewers, ", "), err, firstLine(stderr.String()))
		}
	}
	return url, nil
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

func asExit(err error, target **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*target = e
	}
	return ok
}

func (w *Workspace) git(ctx context.Context, args ...string) (string, error) {
	return w.gitIn(ctx, w.Dir, args...)
}

func (w *Workspace) gitIn(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := w.cmd(ctx, dir, "git", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, bytes.TrimSpace(stderr.Bytes()))
	}
	return string(out), nil
}

func (w *Workspace) cmd(ctx context.Context, dir, name string, args ...string) *exec.Cmd {
	c := exec.CommandContext(ctx, name, args...)
	c.Dir = dir
	c.Env = append(os.Environ(), w.Env...)
	if w.Identity.Name != "" {
		c.Env = append(c.Env, "GIT_AUTHOR_NAME="+w.Identity.Name, "GIT_COMMITTER_NAME="+w.Identity.Name)
	}
	if w.Identity.Email != "" {
		c.Env = append(c.Env, "GIT_AUTHOR_EMAIL="+w.Identity.Email, "GIT_COMMITTER_EMAIL="+w.Identity.Email)
	}
	// Never let a run hang on a credential prompt.
	c.Env = append(c.Env, "GIT_TERMINAL_PROMPT=0")
	return c
}

// Git runs git in the work tree and returns its output. For the harness's
// own deterministic jobs; the agent never reaches it.
func (w *Workspace) Git(ctx context.Context, args ...string) (string, error) {
	return w.git(ctx, args...)
}

// ForcePushTo replaces branch on origin with HEAD. Only for a branch
// MARSHAL owns outright and rebuilds from the base each time, such as the
// release branch; proposals are never force-pushed.
func (w *Workspace) ForcePushTo(ctx context.Context, branch string) error {
	_, err := w.git(ctx, "push", "--quiet", "--force", "origin", "HEAD:refs/heads/"+branch)
	return err
}

// FindPR returns the URL of the open pull request from head into the base
// branch, or "" when there is none.
func (w *Workspace) FindPR(ctx context.Context, head string) (string, error) {
	out, err := w.cmd(ctx, w.Dir, "gh", "pr", "list", "--head", head, "--base", w.Repo.Branch,
		"--state", "open", "--json", "url", "--jq", ".[0].url // empty").Output()
	if err != nil {
		return "", fmt.Errorf("gh pr list: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// EditPR replaces a pull request's title and body.
func (w *Workspace) EditPR(ctx context.Context, url, title, body string) error {
	cmd := w.cmd(ctx, w.Dir, "gh", "pr", "edit", url, "--title", title, "--body-file", "-")
	cmd.Stdin = strings.NewReader(body)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("gh pr edit: %w: %s", err, bytes.TrimSpace(out))
	}
	return nil
}
