package run

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sousf/marshal/internal/config"
	"github.com/Sousf/marshal/internal/runner"
	"github.com/Sousf/marshal/internal/store"
)

// fake is an adapter that edits files instead of calling a model.
type fake struct {
	edit   func(dir string)
	output string
}

func (fake) Name() string    { return "fake" }
func (fake) Available() bool { return true }
func (f fake) Run(_ context.Context, req runner.Request, emit func(runner.Event)) (runner.Result, error) {
	emit(runner.Event{Kind: runner.Tool, Text: "Edit go.mod"})
	if f.edit != nil {
		f.edit(req.Dir)
	}
	emit(runner.Event{Kind: runner.Text, Text: f.output})
	return runner.Result{Output: f.output, Turns: 2, CostUSD: 0.10}, nil
}

func origin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "--quiet", "--initial-branch=main")
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644)
	os.MkdirAll(filepath.Join(dir, ".github/workflows"), 0o755)
	os.WriteFile(filepath.Join(dir, ".github/workflows/release.yml"), []byte("name: r\n"), 0o644)
	git("add", ".")
	git("commit", "--quiet", "-m", "init")
	return dir
}

func fleet(t *testing.T, repoURL string) *config.Fleet {
	t.Helper()
	f, err := config.Parse([]byte(`
version: 1
defaults: {model: m, effort: low, max_turns: 5, max_usd: 1}
repos:
  - {name: x, url: ` + repoURL + `, jobs: [fix]}
jobs:
  - {name: fix, class: hardening, prompt: "do it", paths: ["go.mod"], max_diff_lines: 10}
  - {name: anywhere, class: hardening, prompt: "do it"}
policy:
  classes: {hardening: {start: review, promote: 3}}
  caps: {prs_per_repo_per_day: 1}
`))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func exec1(t *testing.T, f *config.Fleet, a runner.Adapter, job string) (store.Run, string, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	var log bytes.Buffer
	x := &Executor{Fleet: f, Store: st, Adapter: a, WorkRoot: t.TempDir()}
	rec, err := x.Execute(context.Background(), job, "x", Options{NoPR: true, Log: &log})
	if err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	return rec, log.String(), st
}

func TestExecuteCommits(t *testing.T) {
	f := fleet(t, origin(t))
	a := fake{
		edit:   func(dir string) { os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n\ngo 1.27\n"), 0o644) },
		output: "Declare go version\n\nAdded the go directive.",
	}
	rec, log, st := exec1(t, f, a, "fix")
	if rec.Status != store.Committed || rec.Turns != 2 || rec.CostUSD != 0.10 {
		t.Errorf("%+v\n%s", rec, log)
	}
	if !strings.Contains(log, "policy ok") || !strings.Contains(log, "committed on marshal/fix/") {
		t.Errorf("log:\n%s", log)
	}
	ev, _ := st.Events(context.Background(), rec.ID)
	if len(ev) < 4 {
		t.Errorf("events not recorded: %+v", ev)
	}
}

func TestExecuteNoChange(t *testing.T) {
	f := fleet(t, origin(t))
	rec, _, _ := exec1(t, f, fake{output: "NO CHANGE: already fine"}, "fix")
	if rec.Status != store.NoChange || rec.Summary != "NO CHANGE: already fine" {
		t.Errorf("%+v", rec)
	}
}

func TestExecuteBlockedByPaths(t *testing.T) {
	f := fleet(t, origin(t))
	a := fake{
		edit:   func(dir string) { os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644) },
		output: "Add main\n\nbody",
	}
	rec, _, _ := exec1(t, f, a, "fix")
	if rec.Status != store.Blocked || !strings.Contains(rec.Error, "paths: main.go") {
		t.Errorf("%+v", rec)
	}
}

func TestExecuteBlockedByLockedPath(t *testing.T) {
	f := fleet(t, origin(t))
	a := fake{
		edit: func(dir string) {
			os.WriteFile(filepath.Join(dir, ".github/workflows/release.yml"), []byte("name: evil\n"), 0o644)
		},
		output: "Tweak release\n\nbody",
	}
	rec, _, _ := exec1(t, f, a, "anywhere")
	if rec.Status != store.Blocked || !strings.Contains(rec.Error, "locked") {
		t.Errorf("a job with no path list must still be unable to touch release workflows: %+v", rec)
	}
}

func TestExecuteUnknown(t *testing.T) {
	f := fleet(t, origin(t))
	st, _ := store.Open(filepath.Join(t.TempDir(), "m.db"))
	defer st.Close()
	x := &Executor{Fleet: f, Store: st, Adapter: fake{}, WorkRoot: t.TempDir()}
	if _, err := x.Execute(context.Background(), "nope", "x", Options{}); err == nil {
		t.Error("unknown job should error")
	}
	if _, err := x.Execute(context.Background(), "fix", "nope", Options{}); err == nil {
		t.Error("unknown repo should error")
	}
}

func TestSplitReport(t *testing.T) {
	title, body := splitReport("# Pin actions\n\nDid it.\n", "j")
	if title != "Pin actions" || body != "Did it." {
		t.Errorf("%q %q", title, body)
	}
	title, body = splitReport("", "j")
	if title != "j" || body == "" {
		t.Errorf("%q %q", title, body)
	}
	long := strings.Repeat("x", 100)
	title, _ = splitReport(long, "j")
	if len(title) != 70 {
		t.Errorf("title length %d", len(title))
	}
	title, body = splitReport("NO CHANGE: but actually I did\n\nReal title\n\nReal body", "j")
	if title != "Real title" || body != "Real body" {
		t.Errorf("NO CHANGE line should be dropped when the tree changed: %q %q", title, body)
	}
	title, _ = splitReport("NO CHANGE: nothing to do", "j")
	if title != "NO CHANGE: nothing to do" {
		t.Errorf("a bare NO CHANGE line with no body is kept: %q", title)
	}
}
