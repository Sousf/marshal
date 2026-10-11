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
	if strings.Join(env, " ") != "PATH=/bin HOME=/h GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1" {
		t.Errorf("%v", env)
	}
}

func TestStripHostSetup(t *testing.T) {
	for _, c := range []struct {
		step, kept string
		dropped    int
	}{
		{"go test ./...", "go test ./...", 0},
		{"sudo apt-get update && sudo apt-get install -y libpcap-dev", "", 2},
		{"sudo apt-get install -y libpcap-dev\ngo build ./...", "go build ./...", 1},
		{"if [ -n \"$(gofmt -l .)\" ]; then\n  exit 1\nfi", "if [ -n \"$(gofmt -l .)\" ]; then\n  exit 1\nfi", 0},
		{"if true; then\n  sudo apt-get install x\nfi\ngo test ./...", "", 1},
		{"brew install foo; go test", "", 1},
		{"go build ./... # sudo is fine in a comment", "go build ./... # sudo is fine in a comment", 0},
	} {
		kept, dropped := stripHostSetup(c.step)
		if kept != c.kept || len(dropped) != c.dropped {
			t.Errorf("%q: kept %q dropped %v", c.step, kept, dropped)
		}
	}
}

func TestDeriveLeavesOutRunnerSetup(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755)
	os.WriteFile(filepath.Join(dir, ".github", "workflows", "ci.yml"), []byte(`on: [pull_request]
jobs:
  t:
    steps:
      - run: sudo apt-get update && sudo apt-get install -y libpcap-dev
      - run: go test ./...
`), 0o644)
	c, ok, err := Load(dir)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if c.Verbs["ci"] != "go test ./..." || len(c.HostSetup) != 2 {
		t.Errorf("ci=%q setup=%v", c.Verbs["ci"], c.HostSetup)
	}
}

// A shell that forks leaves its child holding the output pipe; a timeout
// must kill the whole process group, not just the shell.
func TestTimeoutKillsChildren(t *testing.T) {
	c := &Contract{Version: 1, Timeout: time.Second, Verbs: map[string]string{"smoke": "sleep 6; true"}}
	start := time.Now()
	r := c.Run(context.Background(), t.TempDir(), "smoke")
	if r.OK || time.Since(start) > 3*time.Second {
		t.Errorf("took %s: %+v", time.Since(start), r)
	}
}

func TestVerbsRunWithoutHostGitConfig(t *testing.T) {
	t.Setenv("GIT_AUTHOR_NAME", "someone")
	t.Setenv("GIT_CONFIG_GLOBAL", "/home/someone/.gitconfig")
	c := &Contract{Version: 1, Verbs: map[string]string{"test": `test -z "$GIT_AUTHOR_NAME" && test "$GIT_CONFIG_GLOBAL" = /dev/null && test "$GIT_CONFIG_NOSYSTEM" = 1`}}
	if r := c.Run(context.Background(), t.TempDir(), "test"); !r.OK {
		t.Errorf("host git config leaked into a verb: %+v", r)
	}
}

func TestMissingToolSkipsOnlyItsStep(t *testing.T) {
	c := &Contract{Version: 1, Verbs: map[string]string{"ci": "x"}, Steps: []string{"true", "definitely-not-installed-xyz", "echo third; false"}}
	r := c.Run(context.Background(), t.TempDir(), "ci")
	if r.OK || r.Unavailable || !strings.Contains(r.Output, "[skipped, not installed here] definitely-not-installed-xyz") || !strings.Contains(r.Output, "third") {
		t.Errorf("%+v", r)
	}
	c.Steps = []string{"nope-a-xyz", "nope-b-xyz"}
	if r := c.Run(context.Background(), t.TempDir(), "ci"); !r.Unavailable {
		t.Errorf("all missing should be unavailable: %+v", r)
	}
}
