package run

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Sousf/marshal/internal/config"
	"github.com/Sousf/marshal/internal/store"
)

func write(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for p, s := range files {
		os.MkdirAll(filepath.Join(dir, filepath.Dir(p)), 0o755)
		if err := os.WriteFile(filepath.Join(dir, p), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReadBrewFacts(t *testing.T) {
	root := t.TempDir()
	write(t, root, map[string]string{
		"go.mod":    "module x\n",
		"main.go":   "package main\n\nvar version = \"dev\"\n\nfunc main() { if a == \"version\" {} }\n",
		"LICENSE":   "MIT License\n",
		"README.md": "# tool\n\n![demo](x.gif)\n\nA **small** tool that \"quotes\"\nthings. It wraps lines.\n",
	})
	f, err := ReadBrewFacts(root, "tool")
	if err != nil {
		t.Fatal(err)
	}
	if f.Main != "." || f.Bin != "tool" || !f.Version || !f.VersionCmd || f.License != "MIT" || len(f.Files) != 2 {
		t.Errorf("root: %+v", f)
	}
	if f.Desc != `"A small tool that \"quotes\" things."` {
		t.Errorf("desc = %s", f.Desc)
	}

	cmd := t.TempDir()
	write(t, cmd, map[string]string{"go.mod": "module x\n", "cmd/hmon/main.go": "package main\nfunc main(){}\n", "internal/a/a.go": "package a\n"})
	if f, err := ReadBrewFacts(cmd, "hmon"); err != nil || f.Main != "./cmd/hmon" || f.Version || f.VersionCmd {
		t.Errorf("cmd: %+v %v", f, err)
	}
	write(t, cmd, map[string]string{"cmd/other/main.go": "package main\nfunc main(){}\n"})
	if f, _ := ReadBrewFacts(cmd, "hmon"); f.Main != "./cmd/hmon" {
		t.Errorf("several commands should pick the repo's name: %+v", f)
	}
	if _, err := ReadBrewFacts(cmd, "nope"); err == nil {
		t.Error("ambiguous commands must not be guessed")
	}
	if _, err := ReadBrewFacts(t.TempDir(), "x"); err == nil {
		t.Error("not a Go module")
	}
}

func TestBrewTemplatesAreValidYAML(t *testing.T) {
	f := BrewFacts{Repo: "x", Name: "x", Bin: "x", Main: ".", Version: true, Files: []string{"README.md"},
		Desc: `"a \"b\""`, Homepage: "https://github.com/o/x", Tap: "o/homebrew-tap", TapOwner: "o", TapName: "homebrew-tap", TapBranch: "main", Branch: "master"}
	base, err := render("goreleaser.yaml", f)
	if err != nil {
		t.Fatal(err)
	}
	brews, err := render("brews.yaml", f)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(base+brews), &doc); err != nil {
		t.Fatalf("%v\n%s", err, base+brews)
	}
	if doc["brews"] == nil || doc["builds"] == nil || !strings.Contains(base, "-X main.version={{ .Version }}") {
		t.Errorf("rendered:\n%s", base+brews)
	}
	wf, err := render("release.yml", f)
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal([]byte(wf), &doc); err != nil || !strings.Contains(wf, "branches: [master]") || !strings.Contains(wf, "${{ secrets.HOMEBREW_TAP_DEPLOY_KEY }}") || !strings.Contains(wf, "paths: [VERSION]") {
		t.Errorf("workflow: %v\n%s", err, wf)
	}
}

// The template is the source of this repository's own release workflow;
// the two must not drift.
func TestOwnReleaseWorkflowMatchesTemplate(t *testing.T) {
	own, err := os.ReadFile("../../.github/workflows/release.yml")
	if err != nil {
		t.Skip(err)
	}
	wf, _ := render("release.yml", BrewFacts{Branch: "master", Tap: "Sousf/homebrew-tap"})
	if string(own) != wf {
		t.Error(".github/workflows/release.yml differs from internal/run/templates/release.yml rendered for master")
	}
}

func TestOnboardProposesBrewSetup(t *testing.T) {
	ctx := context.Background()
	o := origin(t) // has a placeholder release.yml without the VERSION trigger
	write(t, o, map[string]string{"main.go": "package main\n\nfunc main() {}\n", "README.md": "# x\n\nDoes x.\n"})
	for _, args := range [][]string{{"add", "."}, {"commit", "--quiet", "-m", "code"}} {
		c := exec.Command("git", args...)
		c.Dir = o
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
	f, err := config.Parse([]byte(`
version: 1
defaults: {model: m}
repos:
  - {name: x, url: ` + o + `, github: o/x, jobs: [onboard]}
jobs:
  - {name: onboard, class: code, prompt: p}
policy:
  classes: {code: {start: review}, release: {start: review, promote: 0}}
homebrew: {tap: o/homebrew-tap}
`))
	if err != nil {
		t.Fatal(err)
	}
	st, _ := store.Open(filepath.Join(t.TempDir(), "m.db"))
	defer st.Close()
	work := t.TempDir()
	x := &Executor{Fleet: f, Store: st, Adapter: &stepwise{}, WorkRoot: work}
	repo, _ := f.Repo("x")
	var log bytes.Buffer
	rec, err := x.brewSetup(ctx, repo, Options{NoPR: true, Keep: true, Log: &log})
	if err != nil || rec.Status != store.Committed || rec.Class != "release" || rec.Adapter != "marshal" {
		t.Fatalf("%+v %v\n%s", rec, err, log.String())
	}
	for _, want := range []string{"marshal tap-key x", "`.goreleaser.yaml`", "go build .` for linux/amd64 succeeds", "brew install o/tap/x"} {
		if !strings.Contains(rec.Summary, want) {
			t.Errorf("summary missing %q:\n%s", want, rec.Summary)
		}
	}
	dirs, _ := filepath.Glob(filepath.Join(work, "run-"+rec.ID+"-*"))
	gr, _ := os.ReadFile(filepath.Join(dirs[0], ".goreleaser.yaml"))
	wf, _ := os.ReadFile(filepath.Join(dirs[0], ".github/workflows/release.yml"))
	if !strings.Contains(string(gr), "owner: o") || !strings.Contains(string(gr), `description: "Does x."`) || !strings.Contains(string(wf), "branches: [main]") {
		t.Errorf("goreleaser:\n%s\nworkflow:\n%s", gr, wf)
	}
	// A repo already wired is left alone.
	write(t, o, map[string]string{".goreleaser.yaml": "version: 2\nbrews:\n  - name: x\n", ".github/workflows/release.yml": string(wf)})
	for _, args := range [][]string{{"add", "."}, {"commit", "--quiet", "-m", "wired"}} {
		c := exec.Command("git", args...)
		c.Dir = o
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		c.CombinedOutput()
	}
	if rec, _ := x.brewSetup(ctx, repo, Options{NoPR: true}); rec.Status != store.NoChange {
		t.Errorf("wired repo: %s %q", rec.Status, rec.Error)
	}
}

func TestBrewSetupRefusesWhatDoesNotBuild(t *testing.T) {
	o := origin(t)
	write(t, o, map[string]string{"main.go": "package main\n\n// #include <stdio.h>\nimport \"C\"\n\nfunc main() {}\n"})
	for _, args := range [][]string{{"add", "."}, {"commit", "--quiet", "-m", "cgo"}} {
		c := exec.Command("git", args...)
		c.Dir = o
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		c.CombinedOutput()
	}
	f, _ := config.Parse([]byte("version: 1\nrepos:\n  - {name: x, url: " + o + ", github: o/x}\npolicy:\n  classes: {release: {start: review}}\nhomebrew: {tap: o/homebrew-tap}\n"))
	st, _ := store.Open(filepath.Join(t.TempDir(), "m.db"))
	defer st.Close()
	x := &Executor{Fleet: f, Store: st, Adapter: &stepwise{}, WorkRoot: t.TempDir()}
	repo, _ := f.Repo("x")
	rec, _ := x.brewSetup(context.Background(), repo, Options{NoPR: true})
	if rec.Status != store.Skipped || !strings.Contains(rec.Error, "CGO_ENABLED=0") {
		t.Errorf("cgo repo: %s %q", rec.Status, rec.Error)
	}
}

func TestBrewSetupIsBuiltIn(t *testing.T) {
	o := origin(t)
	write(t, o, map[string]string{"main.go": "package main\n\nfunc main() {}\n"})
	for _, args := range [][]string{{"add", "."}, {"commit", "--quiet", "-m", "code"}} {
		c := exec.Command("git", args...)
		c.Dir = o
		c.CombinedOutput()
	}
	f, _ := config.Parse([]byte("version: 1\nrepos:\n  - {name: x, url: " + o + ", github: o/x}\npolicy:\n  classes: {release: {start: review}}\nhomebrew: {tap: o/homebrew-tap}\n"))
	st, _ := store.Open(filepath.Join(t.TempDir(), "m.db"))
	defer st.Close()
	x := &Executor{Fleet: f, Store: st, Adapter: &stepwise{}, WorkRoot: t.TempDir()}
	rec, err := x.Execute(context.Background(), BrewSetupJob, "x", Options{ID: "abcd1234", NoPR: true})
	if err != nil || rec.ID != "abcd1234" || rec.Status != store.Committed {
		t.Errorf("%+v %v", rec, err)
	}
}
