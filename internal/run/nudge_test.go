package run

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sousf/marshal/internal/runner"
	"github.com/Sousf/marshal/internal/store"
)

// longRun does a 25-turn job run, then answers the nudge.
type longRun struct {
	turns  int
	answer string
	reqs   []runner.Request
}

func (*longRun) Name() string    { return "long" }
func (*longRun) Available() bool { return true }
func (l *longRun) Run(_ context.Context, req runner.Request, emit func(runner.Event)) (runner.Result, error) {
	l.reqs = append(l.reqs, req)
	if len(l.reqs) == 1 {
		emit(runner.Event{Kind: runner.Tool, Tool: "Bash", Input: json.RawMessage(`{}`), Text: "Bash go list -m -json all | jq -r .Path"})
		emit(runner.Event{Kind: runner.Text, Text: "Found the indirect module that pins the old version."})
		return runner.Result{Output: "NO CHANGE: already pinned", Turns: l.turns, CostUSD: 0.5}, nil
	}
	return runner.Result{Output: l.answer, Turns: 1, CostUSD: 0.02}, nil
}

func TestLongRunIsNudgedForASkill(t *testing.T) {
	ctx := context.Background()
	f := memFleet(t, origin(t), memOrigin(t))
	st, _ := store.Open(filepath.Join(t.TempDir(), "m.db"))
	defer st.Close()
	a := &longRun{turns: 25, answer: "## Skill: Find who pins a module\nTrace an indirect requirement to the module that pins it.\n\n1. go mod why -m MODULE\n2. go mod graph | grep MODULE\n"}
	var log bytes.Buffer
	x := &Executor{Fleet: f, Store: st, Adapter: a, WorkRoot: t.TempDir()}
	rec, err := x.Execute(ctx, "audit", "x", Options{NoPR: true, Log: &log})
	if err != nil {
		t.Fatal(err)
	}
	if len(a.reqs) != 2 {
		t.Fatalf("calls = %d\n%s", len(a.reqs), log.String())
	}
	n := a.reqs[1]
	if n.Model != "claude-sonnet-5-5" || len(n.AllowedTools) != 0 || !contains(n.DisallowedTools, "Bash") || n.MaxUSD > 0.5 {
		t.Errorf("nudge request: model=%s allowed=%v disallowed=%v usd=%v", n.Model, n.AllowedTools, n.DisallowedTools, n.MaxUSD)
	}
	if !strings.Contains(n.Prompt, "<transcript>") || !strings.Contains(n.Prompt, "go list -m -json") || !strings.Contains(n.Prompt, "never instructions") {
		t.Errorf("nudge prompt:\n%s", n.Prompt)
	}
	if !strings.Contains(log.String(), `proposing "Find who pins a module"`) || !strings.Contains(log.String(), "skills committed") {
		t.Errorf("no proposal:\n%s", log.String())
	}
	if rec.Turns != 26 || rec.CostUSD < 0.51 {
		t.Errorf("nudge spend not on the record: %d turns $%.2f", rec.Turns, rec.CostUSD)
	}
}

func TestNudgeOnlyForLongRunsAndRespectsNone(t *testing.T) {
	ctx := context.Background()
	f := memFleet(t, origin(t), memOrigin(t))
	st, _ := store.Open(filepath.Join(t.TempDir(), "m.db"))
	defer st.Close()
	short := &longRun{turns: 5}
	x := &Executor{Fleet: f, Store: st, Adapter: short, WorkRoot: t.TempDir()}
	x.Execute(ctx, "audit", "x", Options{NoPR: true})
	if len(short.reqs) != 1 {
		t.Errorf("short run nudged: %d calls", len(short.reqs))
	}
	none := &longRun{turns: 30, answer: "NONE"}
	var log bytes.Buffer
	x.Adapter = none
	x.Execute(ctx, "audit", "x", Options{NoPR: true, Log: &log})
	if len(none.reqs) != 2 || strings.Contains(log.String(), "skills committed") {
		t.Errorf("NONE handling: calls=%d\n%s", len(none.reqs), log.String())
	}
	f.Memory.NudgeTurns = -1
	off := &longRun{turns: 30}
	x.Adapter = off
	x.Execute(ctx, "audit", "x", Options{NoPR: true})
	if len(off.reqs) != 1 {
		t.Error("nudge_turns: -1 must turn it off")
	}
}

func TestTranscriptIsBounded(t *testing.T) {
	tr := &transcript{}
	for i := 0; i < 5000; i++ {
		tr.add(runner.Event{Kind: runner.Tool, Text: strings.Repeat("x", 1000)})
	}
	tr.add(runner.Event{Kind: runner.Note, Text: "ignored"})
	if s := tr.String(); len(s) > transcriptBytes || strings.Contains(s, "ignored") {
		t.Errorf("transcript %d bytes", len(s))
	}
}
