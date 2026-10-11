package contract

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	c, err := Parse([]byte("contract: 1\nverbs:\n  build: go build ./...\n  test: go test ./...\nlocked: [a]\nsensitive: [b/]\n"))
	if err != nil || !c.Has("build") || c.Has("vuln") || len(c.Sensitive) != 1 {
		t.Fatalf("%+v %v", c, err)
	}
	for _, bad := range []string{"contract: 2\n", "contract: 1\nverbs: {deploy: x}\n", "contract: 1\nverbs: {test: ''}\n", "contract: 1\nnope: 1\n"} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("should fail: %q", bad)
		}
	}
}

func TestLoadRunRegressions(t *testing.T) {
	dir := t.TempDir()
	if _, ok, err := Load(dir); ok || err != nil {
		t.Fatal("absent file is not an error")
	}
	os.WriteFile(filepath.Join(dir, File), []byte("contract: 1\ntimeout: 2s\nverbs:\n  build: 'true'\n  test: 'test -f ok.txt'\n  lint: 'echo lint output; false'\n  vuln: 'definitely-not-a-program-xyz'\n  smoke: 'sleep 5'\n"), 0o644)
	c, ok, err := Load(dir)
	if err != nil || !ok {
		t.Fatal(err)
	}
	ctx := context.Background()
	before := c.RunAll(ctx, dir, "build", "test", "lint", "vuln", "format")
	if len(before) != 4 {
		t.Fatalf("%d results, want 4 (format undefined)", len(before))
	}
	byVerb := map[string]Result{}
	for _, r := range before {
		byVerb[r.Verb] = r
	}
	if !byVerb["build"].OK || byVerb["test"].OK || byVerb["lint"].OK || !byVerb["vuln"].Unavailable || byVerb["lint"].Output != "lint output" {
		t.Errorf("%+v", before)
	}
	// Fix the test, break the build: one regression, one improvement.
	os.WriteFile(filepath.Join(dir, "ok.txt"), []byte("x"), 0o644)
	c.Verbs["build"] = "false"
	after := c.RunAll(ctx, dir, "build", "test", "lint")
	reg := Regressions(before, after)
	if len(reg) != 1 || reg[0].Verb != "build" {
		t.Errorf("regressions = %+v", reg)
	}
	s := Summary(after)
	if !strings.Contains(s, "build: FAIL") || !strings.Contains(s, "test: pass") {
		t.Errorf("summary:\n%s", s)
	}
	brief := c.Brief(before)
	if !strings.Contains(brief, "- test: `test -f ok.txt`") || !strings.Contains(brief, "pre-existing") || !strings.Contains(brief, "lint output") {
		t.Errorf("brief:\n%s", brief)
	}
	start := time.Now()
	if r := c.Run(ctx, dir, "smoke"); r.OK || !strings.Contains(r.Output, "timed out") || time.Since(start) > 4*time.Second {
		t.Errorf("timeout: %+v after %s", r, time.Since(start))
	}
}

func TestScrub(t *testing.T) {
	env := scrub([]string{"PATH=/bin", "GH_TOKEN=x", "GIT_CONFIG_COUNT=1", "ANTHROPIC_API_KEY=y", "HOME=/h"})
	if strings.Join(env, " ") != "PATH=/bin HOME=/h" {
		t.Errorf("%v", env)
	}
}
