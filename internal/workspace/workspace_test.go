package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sousf/marshal/internal/config"
)

// origin builds a tiny repository to clone from.
func origin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "--quiet", "--initial-branch=main")
	os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("# Conventions\nrun go test\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644)
	run("add", ".")
	run("commit", "--quiet", "-m", "init")
	return dir
}

func TestPrepareAndCommit(t *testing.T) {
	ctx := context.Background()
	src := origin(t)
	repo := config.Repo{Name: "x", URL: src} // no branch: must be detected
	w, err := Prepare(ctx, t.TempDir(), repo, "posture-audit", "deadbeef", config.Identity{Name: "MARSHAL", Email: "m@example.invalid"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Remove()
	if w.Branch != "marshal/posture-audit/deadbeef" {
		t.Errorf("branch %q", w.Branch)
	}
	if w.Repo.Branch != "main" {
		t.Errorf("default branch detected as %q, want main", w.Repo.Branch)
	}
	if !strings.Contains(w.Conventions(), "go test") {
		t.Errorf("conventions = %q", w.Conventions())
	}
	if ch, _ := w.Changed(ctx); ch {
		t.Error("fresh clone should be clean")
	}
	os.WriteFile(filepath.Join(w.Dir, "go.mod"), []byte("module x\n\ngo 1.27\n"), 0o644)
	os.WriteFile(filepath.Join(w.Dir, "new.txt"), []byte("a\nb\n"), 0o644)
	ns, err := w.Numstat(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ns, "go.mod") || !strings.Contains(ns, "new.txt") {
		t.Errorf("numstat should include modified and new files:\n%s", ns)
	}
	if err := w.Commit(ctx, "Pin things\n\nBody."); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "log", "-1", "--format=%an <%ae> %s")
	cmd.Dir = w.Dir
	out, _ := cmd.Output()
	if got := strings.TrimSpace(string(out)); got != "MARSHAL <m@example.invalid> Pin things" {
		t.Errorf("commit author/subject = %q", got)
	}
	if ch, _ := w.Changed(ctx); ch {
		t.Error("tree should be clean after commit")
	}
}

func TestPrepareHead(t *testing.T) {
	ctx := context.Background()
	src := origin(t)
	// Make a proposal branch in the origin with one commit.
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = src
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("checkout", "-q", "-b", "marshal/j/abc")
	os.WriteFile(filepath.Join(src, "go.mod"), []byte("module x\n\nrequire evil.example/pkg v1.0.0\n"), 0o644)
	run("commit", "-q", "-am", "add dep")
	sha := run("rev-parse", "HEAD")
	run("checkout", "-q", "main")

	w, err := PrepareHead(ctx, t.TempDir(), config.Repo{Name: "x", URL: src}, "abc", "marshal/j/abc", sha, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Remove()
	if got, _ := w.HeadSHA(ctx); got != sha {
		t.Errorf("head = %s want %s", got, sha)
	}
	if w.Repo.Branch != "main" || w.Base() != "origin/main" {
		t.Errorf("base = %s", w.Base())
	}
	ns, _ := w.DiffNumstat(ctx)
	if !strings.Contains(ns, "go.mod") {
		t.Errorf("numstat:\n%s", ns)
	}
	patch, _ := w.DiffPatch(ctx)
	if !strings.Contains(patch, "+require evil.example/pkg") {
		t.Errorf("patch:\n%s", patch)
	}
	if before := w.FileAt(ctx, w.Base(), "go.mod"); strings.Contains(before, "evil") {
		t.Error("base go.mod should not have the dep")
	}
	if w.FileAt(ctx, "HEAD", "nope.txt") != "" {
		t.Error("missing file should be empty")
	}
}

func TestPrepareExistingAndPush(t *testing.T) {
	ctx := context.Background()
	src := origin(t)
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = src
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("checkout", "-q", "-b", "marshal/j/abc")
	os.WriteFile(filepath.Join(src, "new.txt"), []byte("1\n"), 0o644)
	run("add", ".")
	run("commit", "-q", "-m", "proposal")
	run("checkout", "-q", "main")

	w, err := PrepareExisting(ctx, t.TempDir(), config.Repo{Name: "x", URL: src}, "r2", "marshal/j/abc", config.Identity{Name: "M", Email: "m@m.invalid"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Remove()
	if b, _ := os.ReadFile(filepath.Join(w.Dir, "new.txt")); string(b) != "1\n" {
		t.Fatal("proposal branch not checked out")
	}
	os.WriteFile(filepath.Join(w.Dir, "new.txt"), []byte("2\n"), 0o644)
	if err := w.Commit(ctx, "fix"); err != nil {
		t.Fatal(err)
	}
	if err := w.Push(ctx); err != nil {
		t.Fatal(err)
	}
	if got := run("log", "-1", "--format=%s", "marshal/j/abc"); got != "fix" {
		t.Errorf("origin branch head = %q, want the pushed fix", got)
	}
}

func TestRevert(t *testing.T) {
	ctx := context.Background()
	src := origin(t)
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = src
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	os.WriteFile(filepath.Join(src, "go.mod"), []byte("module x\n\nbad\n"), 0o644)
	run("commit", "-q", "-am", "break it")
	bad := run("rev-parse", "HEAD")
	w, err := Prepare(ctx, t.TempDir(), config.Repo{Name: "x", URL: src}, "revert", "r9", config.Identity{Name: "M", Email: "m@m.invalid"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Remove()
	if err := w.Revert(ctx, bad, "Revert \"break it\""); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(w.Dir, "go.mod")); string(b) != "module x\n" {
		t.Errorf("revert did not restore the file: %q", b)
	}
	if err := w.Revert(ctx, "0000000000000000000000000000000000000000", "x"); err == nil {
		t.Error("reverting a missing commit must fail cleanly")
	}
	if ch, _ := w.Changed(ctx); ch {
		t.Error("a failed revert must leave the tree clean")
	}
}

func TestRemoteHead(t *testing.T) {
	src := origin(t)
	sha, err := RemoteHead(context.Background(), config.Repo{URL: src}, nil)
	if err != nil || len(sha) != 40 {
		t.Fatalf("%q %v", sha, err)
	}
	if got, _ := RemoteHead(context.Background(), config.Repo{URL: src, Branch: "main"}, nil); got != sha {
		t.Error("explicit branch should match HEAD here")
	}
	if _, err := RemoteHead(context.Background(), config.Repo{URL: src, Branch: "nope"}, nil); err == nil {
		t.Error("missing branch must fail")
	}
}

func TestPrepareBadURL(t *testing.T) {
	_, err := Prepare(context.Background(), t.TempDir(), config.Repo{Name: "x", URL: filepath.Join(t.TempDir(), "nope"), Branch: "main"}, "j", "r", config.Identity{}, nil)
	if err == nil {
		t.Fatal("want clone failure")
	}
}
