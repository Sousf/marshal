package run

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"text/template"
	"time"

	"github.com/Sousf/marshal/internal/config"
	"github.com/Sousf/marshal/internal/ghapp"
	"github.com/Sousf/marshal/internal/github"
	"github.com/Sousf/marshal/internal/store"
	"github.com/Sousf/marshal/internal/workspace"
)

// Brew setup runs during onboarding, in Go, never by the agent: the files
// it writes are the release pipeline, which agents may not touch. It fills
// fixed templates from facts read off the repository and proposes them as
// a pull request of their own.

// BrewSetupJob names the run records of the brew check.
const BrewSetupJob = "brew-setup"

//go:embed templates/*
var templates embed.FS

func render(name string, data any) (string, error) {
	t, err := template.New(name).Delims("<<", ">>").ParseFS(templates, "templates/"+name)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	if err := t.Execute(&b, data); err != nil {
		return "", err
	}
	return b.String(), nil
}

// BrewFacts is what the templates need, read from a checkout.
type BrewFacts struct {
	Repo, Name, Bin, Main string
	Version, VersionCmd   bool
	Files                 []string
	License, Desc         string
	Homepage              string
	Tap, TapOwner         string
	TapName, TapBranch    string
	Branch                string
}

var (
	reVersionVar = regexp.MustCompile(`(?m)^\s*(var\s+version\b|version\s*=)`)
	reVersionCmd = regexp.MustCompile(`"(version|--version)"`)
)

// ReadBrewFacts inspects a Go repository for what a release build needs.
// It refuses rather than guesses when the main package is ambiguous.
func ReadBrewFacts(dir, repoName string) (BrewFacts, error) {
	f := BrewFacts{Repo: repoName, Name: repoName}
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		return f, fmt.Errorf("not a Go module")
	}
	mainDir := ""
	switch {
	case isMainPackage(dir):
		f.Main, f.Bin, mainDir = ".", repoName, dir
	default:
		cmds, _ := filepath.Glob(filepath.Join(dir, "cmd", "*"))
		var found []string
		for _, c := range cmds {
			if isMainPackage(c) {
				found = append(found, filepath.Base(c))
			}
		}
		pick := ""
		switch {
		case len(found) == 1:
			pick = found[0]
		case slices.Contains(found, repoName):
			pick = repoName
		case len(found) == 0:
			return f, fmt.Errorf("no main package at the root or under cmd/")
		default:
			return f, fmt.Errorf("several commands under cmd/ (%s) and none named %s; set up by hand", strings.Join(found, ", "), repoName)
		}
		f.Main, f.Bin, mainDir = "./cmd/"+pick, pick, filepath.Join(dir, "cmd", pick)
	}
	src := mainSource(mainDir)
	f.Version = reVersionVar.MatchString(src)
	f.VersionCmd = f.Version && reVersionCmd.MatchString(src)
	for _, name := range []string{"README.md", "LICENSE"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			f.Files = append(f.Files, name)
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, "LICENSE")); err == nil {
		first := strings.TrimSpace(firstLine(string(b)))
		switch {
		case strings.HasPrefix(first, "MIT"):
			f.License = "MIT"
		case strings.Contains(first, "Apache License"):
			f.License = "Apache-2.0"
		}
	}
	f.Desc = yamlString(readmeSummary(dir, repoName))
	return f, nil
}

func isMainPackage(dir string) bool {
	files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
	for _, p := range files {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		b, err := os.ReadFile(p)
		if err == nil && regexp.MustCompile(`(?m)^package main\b`).Match(b) {
			return true
		}
	}
	return false
}

// mainSource concatenates the main package's files, bounded: it is only
// searched for a version variable and subcommand.
func mainSource(dir string) string {
	var b strings.Builder
	files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
	for _, p := range files {
		if strings.HasSuffix(p, "_test.go") || b.Len() > 1<<20 {
			continue
		}
		if s, err := os.ReadFile(p); err == nil {
			b.Write(s)
		}
	}
	return b.String()
}

// readmeSummary is the README's first line of prose, which is untrusted
// text: it is clipped, stripped to printable characters, and only ever
// emitted as a quoted YAML string.
func readmeSummary(dir, name string) string {
	b, _ := os.ReadFile(filepath.Join(dir, "README.md"))
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") || strings.HasPrefix(l, "!") || strings.HasPrefix(l, "[") || strings.HasPrefix(l, "<") || strings.HasPrefix(l, "```") {
			continue
		}
		l = strings.Map(func(r rune) rune {
			if r < 0x20 || r == 0x7f {
				return -1
			}
			return r
		}, strings.NewReplacer("**", "", "`", "", "*", "").Replace(l))
		if len(l) > 80 {
			l = strings.TrimSpace(l[:77]) + "..."
		}
		return l
	}
	return name
}

// yamlString quotes s for YAML. A JSON string is a valid YAML
// double-quoted scalar, escapes included.
func yamlString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// brewReady reports which halves of the pipeline already exist.
func brewReady(dir string) (goreleaserPath string, hasBrew, hasWorkflow bool) {
	for _, n := range []string{".goreleaser.yaml", ".goreleaser.yml"} {
		if b, err := os.ReadFile(filepath.Join(dir, n)); err == nil {
			goreleaserPath = n
			hasBrew = regexp.MustCompile(`(?m)^(brews|homebrew_casks):`).Match(b)
			break
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, releaseWorkflow)); err == nil {
		hasWorkflow = bytes.Contains(b, []byte("paths: [VERSION]"))
	}
	return
}

func (x *Executor) brewSetup(ctx context.Context, repo config.Repo, opts Options) (store.Run, error) {
	owner, tapName, tapBranch, ok := x.Fleet.Homebrew.TapParts()
	if !ok {
		return store.Run{}, nil
	}
	class := "release"
	if _, ok := x.Fleet.Policy.Classes[class]; !ok {
		class = "code"
	}
	if open, err := x.Store.ListByStatus(ctx, store.Proposed); err == nil {
		for _, r := range open {
			if r.Job == BrewSetupJob && r.Repo == repo.Name {
				return r, nil // already proposed; the operator has it
			}
		}
	}
	rec := &store.Run{Job: BrewSetupJob, Repo: repo.Name, Class: class, Adapter: "marshal", StartedAt: time.Now()}
	if err := x.Store.CreateRun(ctx, rec); err != nil {
		return store.Run{}, err
	}
	logf := x.logger(ctx, rec.ID, opts.Log)
	finish := func(st store.Status, errText string) (store.Run, error) {
		rec.Status = st
		rec.Error = errText
		rec.FinishedAt = time.Now()
		return *rec, x.Store.UpdateRun(ctx, rec)
	}
	logf("note", "run %s: brew check on %s (deterministic, no agent)", rec.ID, repo.Name)
	ws, err := x.harnessClone(ctx, repo, BrewSetupJob, rec.ID, opts.Env)
	if err != nil {
		return finish(store.Failed, err.Error())
	}
	if !opts.Keep {
		defer ws.Remove()
	}
	rec.Branch = ws.Branch
	grPath, hasBrew, hasWorkflow := brewReady(ws.Dir)
	if hasBrew && hasWorkflow {
		logf("note", "already releases through Homebrew")
		return finish(store.NoChange, "")
	}
	facts, err := ReadBrewFacts(ws.Dir, repo.Name)
	if err != nil {
		logf("note", "brew: %v", err)
		return finish(store.Skipped, "brew: "+err.Error())
	}
	slug := repo.GitHub
	if slug == "" {
		slug, _ = github.Slug(repo.URL)
	}
	facts.Homepage = "https://github.com/" + slug
	facts.Tap, facts.TapOwner, facts.TapName, facts.TapBranch = x.Fleet.Homebrew.Tap, owner, tapName, tapBranch
	facts.Branch = ws.Repo.Branch

	var did []string
	if !hasBrew {
		brews, err := render("brews.yaml", facts)
		if err != nil {
			return finish(store.Failed, err.Error())
		}
		if grPath == "" {
			grPath = ".goreleaser.yaml"
			base, err := render("goreleaser.yaml", facts)
			if err != nil {
				return finish(store.Failed, err.Error())
			}
			brews = base + brews
			did = append(did, "`"+grPath+"`: a GoReleaser config building `"+facts.Bin+"` from `"+facts.Main+"` for Linux and macOS, with a Homebrew formula")
		} else {
			old, _ := os.ReadFile(filepath.Join(ws.Dir, grPath))
			brews = strings.TrimRight(string(old), "\n") + "\n" + brews
			did = append(did, "`"+grPath+"`: a `brews` section publishing a formula to "+facts.Tap)
		}
		if err := os.WriteFile(filepath.Join(ws.Dir, grPath), []byte(brews), 0o644); err != nil {
			return finish(store.Failed, err.Error())
		}
	}
	if !hasWorkflow {
		wf, err := render("release.yml", facts)
		if err != nil {
			return finish(store.Failed, err.Error())
		}
		os.MkdirAll(filepath.Join(ws.Dir, ".github", "workflows"), 0o755)
		if err := os.WriteFile(filepath.Join(ws.Dir, releaseWorkflow), []byte(wf), 0o644); err != nil {
			return finish(store.Failed, err.Error())
		}
		did = append(did, "`"+releaseWorkflow+"`: tags `VERSION` when a release PR merges and runs GoReleaser")
	}
	build := buildCheck(ctx, ws.Dir, facts.Main)
	if build != "" {
		// Same rule as every other PR: nothing opens with a failing check.
		// A cgo tool needs a native toolchain per platform, which the
		// template does not set up.
		logf("blocked", "CGO_ENABLED=0 build fails; set up its release by hand:\n%s", build)
		return finish(store.Skipped, "brew: does not build with CGO_ENABLED=0 (cgo?); set up by hand")
	}
	title := "Release through Homebrew"
	body := releaseSetupBody(facts, did)
	if err := ws.Commit(ctx, title+"\n\n"+body); err != nil {
		return finish(store.Failed, err.Error())
	}
	rec.Summary = title + "\n\n" + body
	if opts.NoPR {
		logf("note", "brew setup committed on %s (no PR requested)", ws.Branch)
		return finish(store.Committed, "")
	}
	if err := ws.Push(ctx); err != nil {
		return finish(store.Failed, err.Error())
	}
	url, err := ws.OpenPR(ctx, title, body, []string{Label, Label + ":release"})
	if err != nil {
		return finish(store.Failed, err.Error())
	}
	rec.PRURL = url
	logf("note", "brew setup proposed: %s", url)
	return finish(store.Proposed, "")
}

// buildCheck cross-compiles the main package the way the release will,
// with nothing but the host's own environment: no tokens. Building does
// not run the repository's code. Empty means it built.
func buildCheck(ctx context.Context, dir, main string) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", os.DevNull, main)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64", "GOFLAGS=-mod=mod")
	out, err := cmd.CombinedOutput()
	if err == nil {
		return ""
	}
	s := strings.TrimSpace(string(out))
	if len(s) > 1500 {
		s = s[len(s)-1500:] // the end of a build log is the error
	}
	return s
}

func releaseSetupBody(f BrewFacts, did []string) string {
	var b strings.Builder
	b.WriteString("MARSHAL onboarding found this repository is not released through Homebrew yet. This adds, from fixed templates and without an agent:\n\n")
	for _, d := range did {
		b.WriteString("- " + d + "\n")
	}
	b.WriteString("\n`CGO_ENABLED=0 go build " + f.Main + "` for linux/amd64 succeeds.\n")
	b.WriteString("\n## One step only you can do\n\nThe workflow pushes the formula with a deploy key that can write to " + f.Tap + " and nothing else. MARSHAL cannot create it: that needs admin rights it deliberately does not have. Before the first release, run:\n\n```sh\nmarshal tap-key " + f.Repo + "\n```\n")
	b.WriteString("\nAfter that, merging MARSHAL's release PR publishes, and `brew install " + strings.Replace(f.Tap, "/homebrew-", "/", 1) + "/" + f.Bin + "` installs.\n")
	return b.String()
}

// harnessClone clones a repo for one of MARSHAL's own deterministic jobs,
// acting with the full token when an app is configured. The agent never
// sees this workspace.
func (x *Executor) harnessClone(ctx context.Context, repo config.Repo, job, runID string, env []string) (*workspace.Workspace, error) {
	identity := x.Fleet.Defaults.Identity
	if x.Auth != nil {
		full, err := x.Auth.Env(ctx, ghapp.Full)
		if err != nil {
			return nil, err
		}
		env = append(append([]string{}, env...), full...)
		repo.URL = ghapp.CloneURL(repo)
		identity = x.Auth.Identity()
	}
	return workspace.Prepare(ctx, x.workRoot(), repo, job, runID, identity, env)
}
