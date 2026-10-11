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

func TestPrepareBadURL(t *testing.T) {
	_, err := Prepare(context.Background(), t.TempDir(), config.Repo{Name: "x", URL: filepath.Join(t.TempDir(), "nope"), Branch: "main"}, "j", "r", config.Identity{}, nil)
	if err == nil {
		t.Fatal("want clone failure")
	}
}
