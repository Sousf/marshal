package run

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sousf/marshal/internal/runner"
	"github.com/Sousf/marshal/internal/store"
)

// stepwise acts differently on each call, so a run with pre-flight retries
// can be scripted.
type stepwise struct {
	calls   int
	steps   []func(dir string)
	outputs []string
	prompts []string
}

func (*stepwise) Name() string    { return "stepwise" }
func (*stepwise) Available() bool { return true }
func (s *stepwise) Run(_ context.Context, req runner.Request, emit func(runner.Event)) (runner.Result, error) {
	i := s.calls
	s.calls++
	s.prompts = append(s.prompts, req.Prompt)
	if i < len(s.steps) && s.steps[i] != nil {
		s.steps[i](req.Dir)
	}
	out := "Did it\n\nbody"
	if i < len(s.outputs) {
		out = s.outputs[i]
	}
	return runner.Result{Output: out, Turns: 1, CostUSD: 0.1}, nil
}

func TestPreflightGivesTheAgentAnotherGo(t *testing.T) {
	ctx := context.Background()
	o := origin(t)
	withCleanContract(t, o) // test: 'test -f keep.txt'
	f := fleet(t, o)
	st, _ := store.Open(filepath.Join(t.TempDir(), "m.db"))
	defer st.Close()
	a := &stepwise{steps: []func(string){
		func(d string) { os.Remove(filepath.Join(d, "keep.txt")) }, // breaks the test
		func(d string) { // fixes it, and leaves a real change behind
			os.WriteFile(filepath.Join(d, "keep.txt"), []byte("x"), 0o644)
			os.WriteFile(filepath.Join(d, "extra.txt"), []byte("y"), 0o644)
		},
	}, outputs: []string{"Remove marker\n\nfirst body", "Restore marker\n\nfixed body"}}
	var log bytes.Buffer
	x := &Executor{Fleet: f, Store: st, Adapter: a, WorkRoot: t.TempDir()}
	rec, err := x.Execute(ctx, "anywhere", "x", Options{NoPR: true, Log: &log})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != store.Committed || a.calls != 2 {
		t.Fatalf("status=%s calls=%d err=%q\n%s", rec.Status, a.calls, rec.Error, log.String())
	}
	if !strings.Contains(a.prompts[1], "# Pre-flight failed") || !strings.Contains(a.prompts[1], "test: FAIL") {
		t.Errorf("fix prompt:\n%s", a.prompts[1])
	}
	if rec.Turns != 2 || rec.CostUSD < 0.19 || !strings.Contains(rec.Summary, "## Pre-flight fixes") {
		t.Errorf("accounting: %+v", rec)
	}
	if !strings.Contains(log.String(), "attempt 1 of 2") || strings.Contains(log.String(), "attempt 2 of 2") {
		t.Errorf("log:\n%s", log.String())
	}

	// A fix that undoes the whole change nets to nothing: no commit, no
	// error, status no-change.
	undo := &stepwise{steps: []func(string){
		func(d string) { os.Remove(filepath.Join(d, "keep.txt")) },
		func(d string) { os.WriteFile(filepath.Join(d, "keep.txt"), []byte("x"), 0o644) },
	}}
	x.Adapter = undo
	rec, err = x.Execute(ctx, "anywhere", "x", Options{NoPR: true})
	if err != nil || rec.Status != store.NoChange {
		t.Errorf("net-zero fix: %+v %v", rec, err)
	}
}

func TestPreflightBlocksAfterRetries(t *testing.T) {
	ctx := context.Background()
	o := origin(t)
	withCleanContract(t, o)
	f := fleet(t, o)
	st, _ := store.Open(filepath.Join(t.TempDir(), "m.db"))
	defer st.Close()
	breaker := func(d string) { os.Remove(filepath.Join(d, "keep.txt")) }
	a := &stepwise{steps: []func(string){breaker, nil, nil}}
	x := &Executor{Fleet: f, Store: st, Adapter: a, WorkRoot: t.TempDir()}
	rec, _ := x.Execute(ctx, "anywhere", "x", Options{NoPR: true})
	if rec.Status != store.Blocked || a.calls != 3 || !strings.Contains(rec.Error, "breaks what passed before") {
		t.Errorf("status=%s calls=%d err=%q", rec.Status, a.calls, rec.Error)
	}
	// --allow-red does not rescue a regression.
	a = &stepwise{steps: []func(string){breaker, nil, nil}}
	x.Adapter = a
	rec, _ = x.Execute(ctx, "anywhere", "x", Options{NoPR: true, AllowRed: true})
	if rec.Status != store.Blocked {
		t.Error("allow-red must not permit a regression")
	}
}

func TestPreflightRequiresNewWorkflowToPass(t *testing.T) {
	ctx := context.Background()
	o := origin(t) // no contract, no workflows: derivation applies after the agent adds CI
	f := fleet(t, o)
	st, _ := store.Open(filepath.Join(t.TempDir(), "m.db"))
	defer st.Close()
	addCI := func(d string) {
		os.MkdirAll(filepath.Join(d, ".github", "workflows"), 0o755)
		os.WriteFile(filepath.Join(d, ".github", "workflows", "ci.yml"), []byte("name: ci\non: [pull_request]\njobs:\n  t:\n    steps:\n      - run: test -f present.txt\n"), 0o644)
	}
	// The agent adds CI that fails, and never fixes it.
	a := &stepwise{steps: []func(string){addCI, nil, nil}}
	x := &Executor{Fleet: f, Store: st, Adapter: a, WorkRoot: t.TempDir()}
	rec, _ := x.Execute(ctx, "anywhere", "x", Options{NoPR: true})
	if rec.Status != store.Blocked || !strings.Contains(rec.Error, "own checks fail") || a.calls != 3 {
		t.Errorf("status=%s calls=%d err=%q", rec.Status, a.calls, rec.Error)
	}
	// Same, but --allow-red: not a regression, so it may proceed.
	a = &stepwise{steps: []func(string){addCI, nil, nil}}
	x.Adapter = a
	rec, _ = x.Execute(ctx, "anywhere", "x", Options{NoPR: true, AllowRed: true})
	if rec.Status != store.Committed {
		t.Errorf("allow-red: %s %q", rec.Status, rec.Error)
	}
	// And the agent that fixes it on the second go gets through.
	a = &stepwise{steps: []func(string){addCI, func(d string) { os.WriteFile(filepath.Join(d, "present.txt"), []byte("x"), 0o644) }}}
	x.Adapter = a
	rec, _ = x.Execute(ctx, "anywhere", "x", Options{NoPR: true})
	if rec.Status != store.Committed || a.calls != 2 {
		t.Errorf("fixed CI: %s calls=%d %q", rec.Status, a.calls, rec.Error)
	}
}

func TestNeverPathsBlockEveryJob(t *testing.T) {
	o := origin(t)
	f := fleet(t, o)
	f.Never.Paths = []string{"infra/**"}
	st, _ := store.Open(filepath.Join(t.TempDir(), "m.db"))
	defer st.Close()
	a := &stepwise{steps: []func(string){func(d string) {
		os.MkdirAll(filepath.Join(d, "infra"), 0o755)
		os.WriteFile(filepath.Join(d, "infra", "main.tf"), []byte("x"), 0o644)
	}}}
	x := &Executor{Fleet: f, Store: st, Adapter: a, WorkRoot: t.TempDir()}
	rec, _ := x.Execute(context.Background(), "anywhere", "x", Options{NoPR: true})
	if rec.Status != store.Blocked || !strings.Contains(rec.Error, "infra/**") {
		t.Errorf("status=%s err=%q", rec.Status, rec.Error)
	}
}
