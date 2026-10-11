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

func TestDerive(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755)
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n\ngo 1.27.0\n"), 0o644)
	os.WriteFile(filepath.Join(dir, ".github", "workflows", "ci.yml"), []byte(`name: ci
on:
  push:
    branches: [main]
  pull_request:
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
      - uses: actions/setup-go@v6
        with:
          go-version-file: go.mod
      - run: go vet ./...
      - run: go test ./...
      - name: govulncheck
        run: |
          go install golang.org/x/vuln/cmd/govulncheck@latest
          govulncheck ./...
`), 0o644)
	os.WriteFile(filepath.Join(dir, ".github", "workflows", "release.yml"), []byte("name: release\non: [pull_request]\njobs:\n  r:\n    steps:\n      - run: goreleaser release\n"), 0o644)
	os.WriteFile(filepath.Join(dir, ".github", "workflows", "nightly.yml"), []byte("name: n\non:\n  schedule: [{cron: '0 0 * * *'}]\njobs:\n  r:\n    steps:\n      - run: echo nightly\n"), 0o644)

	c, ok, err := Load(dir)
	if err != nil || !ok || !c.Derived {
		t.Fatalf("%v %v %+v", ok, err, c)
	}
	if c.Verbs["lint"] != "go vet ./..." || c.Verbs["test"] != "go test ./..." || !strings.Contains(c.Verbs["vuln"], "govulncheck ./...") {
		t.Errorf("verbs = %v", c.Verbs)
	}
	if !strings.HasPrefix(c.Verbs["ci"], "go vet ./... && go test ./... && go install") || strings.Contains(c.Verbs["ci"], "goreleaser") || strings.Contains(c.Verbs["ci"], "nightly") {
		t.Errorf("ci = %q", c.Verbs["ci"])
	}
	if len(c.Env) != 1 || c.Env[0] != "GOTOOLCHAIN=go1.27.0" {
		t.Errorf("env = %v", c.Env)
	}
	if !strings.Contains(c.Brief(nil), "derived from its pull-request workflow") || !strings.Contains(c.Brief(nil), "go1.27.0") {
		t.Errorf("brief:\n%s", c.Brief(nil))
	}
	// marshal.yaml wins over derivation.
	os.WriteFile(filepath.Join(dir, File), []byte("contract: 1\nverbs: {test: 'true'}\n"), 0o644)
	c2, _, _ := Load(dir)
	if c2.Derived || c2.Verbs["test"] != "true" {
		t.Error("explicit contract should win")
	}
	for in, want := range map[string]string{"1.27": "go1.27.0", "1.27.2": "go1.27.2", "stable": "", "": ""} {
		if got := toolchain(in); got != want {
			t.Errorf("toolchain(%q) = %q want %q", in, got, want)
		}
	}
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n\ngo 1.27.0\n\ntoolchain go1.27.2\n"), 0o644)
	if v := goModVersion(filepath.Join(dir, "go.mod")); v != "1.27.2" {
		t.Errorf("toolchain line should win: %q", v)
	}
}

func TestScrub(t *testing.T) {
	env := scrub([]string{"PATH=/bin", "GH_TOKEN=x", "GIT_CONFIG_COUNT=1", "ANTHROPIC_API_KEY=y", "HOME=/h"})
	if strings.Join(env, " ") != "PATH=/bin HOME=/h" {
		t.Errorf("%v", env)
	}
}
