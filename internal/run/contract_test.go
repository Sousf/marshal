package run

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sousf/marshal/internal/store"
)

func execCommand(dir string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	return cmd
}

// withContract adds a marshal.yaml to an origin whose test verb checks a
// marker file and whose lint verb already fails.
func withContract(t *testing.T, dir string) {
	t.Helper()
	os.WriteFile(filepath.Join(dir, "marshal.yaml"), []byte("contract: 1\nverbs:\n  build: 'true'\n  test: 'test -f keep.txt'\n  lint: 'false'\nlocked: ['deploy/**']\nsensitive: ['auth/**']\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("x"), 0o644)
	git := func(args ...string) {
		cmd := execCommand(dir, args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("add", ".")
	git("commit", "-q", "-m", "contract")
}

func TestContractBaselineAndGate(t *testing.T) {
	ctx := context.Background()
	o := origin(t)
	withContract(t, o)
	f := fleet(t, o)
	st, _ := store.Open(filepath.Join(t.TempDir(), "m.db"))
	defer st.Close()

	// The agent breaks the test by deleting the marker: a regression.
	breaker := fake{edit: func(d string) { os.Remove(filepath.Join(d, "keep.txt")) }, output: "Remove marker\n\nbody"}
	var log bytes.Buffer
	x := &Executor{Fleet: f, Store: st, Adapter: breaker, WorkRoot: t.TempDir()}
	rec, err := x.Execute(ctx, "anywhere", "x", Options{NoPR: true, Log: &log})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != store.Blocked || !strings.Contains(rec.Error, "gate") || !strings.Contains(rec.Error, "test: FAIL") {
		t.Errorf("%+v\n%s", rec, log.String())
	}
	if !strings.Contains(log.String(), "baseline test: pass") || !strings.Contains(log.String(), "baseline lint: FAIL") {
		t.Errorf("baseline not logged:\n%s", log.String())
	}

	// A harmless change passes the gate even though lint was already red,
	// and the brief carried the contract and the pre-existing failure.
	rec2 := &recording{output: "Add file\n\nbody", write: func(d string) { os.WriteFile(filepath.Join(d, "new.txt"), []byte("y"), 0o644) }}
	x.Adapter = rec2
	log.Reset()
	r2, err := x.Execute(ctx, "anywhere", "x", Options{NoPR: true, Log: &log})
	if err != nil {
		t.Fatal(err)
	}
	if r2.Status != store.Committed {
		t.Errorf("%+v\n%s", r2, log.String())
	}
	if !strings.Contains(rec2.req.Prompt, "- test: `test -f keep.txt`") || !strings.Contains(rec2.req.Prompt, "pre-existing") {
		t.Error("brief should carry the contract and the baseline")
	}

	// Contract locks apply; sensitive paths flag the run.
	x.Adapter = fake{edit: func(d string) {
		os.MkdirAll(filepath.Join(d, "deploy"), 0o755)
		os.WriteFile(filepath.Join(d, "deploy", "x.sh"), []byte("x"), 0o644)
	}, output: "Deploy\n\nbody"}
	r3, _ := x.Execute(ctx, "anywhere", "x", Options{NoPR: true})
	if r3.Status != store.Blocked || !strings.Contains(r3.Error, "locked") {
		t.Errorf("contract lock: %+v", r3)
	}
	x.Adapter = fake{edit: func(d string) {
		os.MkdirAll(filepath.Join(d, "auth"), 0o755)
		os.WriteFile(filepath.Join(d, "auth", "x.go"), []byte("package auth\n"), 0o644)
	}, output: "Auth change\n\nbody"}
	r4, _ := x.Execute(ctx, "anywhere", "x", Options{NoPR: true})
	if r4.Status != store.Committed || !r4.Sensitive {
		t.Errorf("sensitive: %+v", r4)
	}
	got, _ := st.GetRun(ctx, r4.ID)
	if !got.Sensitive {
		t.Error("sensitive flag must persist")
	}
}
