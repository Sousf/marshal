package run

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sousf/marshal/internal/config"
	"github.com/Sousf/marshal/internal/store"
)

func TestProbeSkipsAndForceRuns(t *testing.T) {
	ctx := context.Background()
	o := origin(t)
	// A contract whose vuln verb is clean: vuln-fix has nothing to do.
	os.WriteFile(filepath.Join(o, "marshal.yaml"), []byte("contract: 1\nverbs: {vuln: 'true', test: 'true'}\n"), 0o644)
	if out, err := execCommand(o, "add", ".").CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	if out, err := execCommand(o, "commit", "-q", "-m", "contract").CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	f, err := config.Parse([]byte(`
version: 1
defaults: {model: m, max_turns: 5, max_usd: 1}
repos: [{name: x, url: ` + o + `, jobs: [vuln-fix]}]
jobs: [{name: vuln-fix, class: dependency, prompt: "fix vulns"}]
policy: {classes: {dependency: {start: review}}}
`))
	if err != nil {
		t.Fatal(err)
	}
	st, _ := store.Open(filepath.Join(t.TempDir(), "m.db"))
	defer st.Close()
	a := &recording{output: "NO CHANGE: clean"}
	var log bytes.Buffer
	x := &Executor{Fleet: f, Store: st, Adapter: a, WorkRoot: t.TempDir()}
	rec, err := x.Execute(ctx, "vuln-fix", "x", Options{NoPR: true, Log: &log})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != store.Skipped || !strings.Contains(rec.Error, "vuln verb passes") || len(rec.Head) != 40 {
		t.Errorf("%+v\n%s", rec, log.String())
	}
	if a.req.Prompt != "" {
		t.Error("no agent should have run")
	}
	forced, _ := x.Execute(ctx, "vuln-fix", "x", Options{NoPR: true, Force: true})
	if forced.Status != store.NoChange || a.req.Prompt == "" {
		t.Errorf("force should run the agent: %+v", forced)
	}
}
