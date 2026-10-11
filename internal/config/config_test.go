package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const good = `
version: 1
defaults:
  model: m
  max_turns: 10
  allowed_tools: [Read, "Bash(go:*)"]
  disallowed_tools: ["Bash(git push:*)"]
repos:
  - {name: a, url: u, jobs: [j1], trust: {c1: locked}}
jobs:
  - {name: j1, class: c1, prompt: hi, allowed_tools: [Grep, Read], max_turns: 3}
  - {name: j2, class: c1, prompt_file: brief.md}
policy:
  classes: {c1: {start: review, promote: 3}}
`

func TestParseAndResolve(t *testing.T) {
	f, err := Parse([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	j, _ := f.Job("j1")
	r := f.Resolve(j)
	if r.Model != "m" || r.MaxTurns != 3 {
		t.Errorf("resolve: %+v", r)
	}
	if got := strings.Join(r.AllowedTools, ","); got != "Read,Bash(go:*),Grep" {
		t.Errorf("allowed = %q (defaults first, job appended, deduped)", got)
	}
	if len(r.DisallowedTools) != 1 {
		t.Errorf("disallowed = %v", r.DisallowedTools)
	}
	repo, ok := f.Repo("a")
	if !ok || repo.Branch != "" {
		t.Errorf("repo = %+v (branch stays empty until the remote is asked)", repo)
	}
	if f.StartLevel(repo, "c1") != Locked {
		t.Error("repo trust override should win")
	}
	if f.StartLevel(Repo{}, "c1") != Review {
		t.Error("class start should apply without override")
	}
	if f.StartLevel(Repo{}, "unknown") != Review {
		t.Error("unknown class should default to review, the safe level")
	}
}

func TestPromptSources(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "brief.md"), []byte("from file"), 0o644)
	os.WriteFile(filepath.Join(dir, "fleet.yaml"), []byte(good), 0o644)
	f, err := Load(filepath.Join(dir, "fleet.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	j1, _ := f.Job("j1")
	if s, _ := f.PromptText(j1, nil); s != "hi" {
		t.Errorf("inline = %q", s)
	}
	j2, _ := f.Job("j2")
	if s, err := f.PromptText(j2, nil); s != "from file" || err != nil {
		t.Errorf("file = %q %v", s, err)
	}
	j3 := Job{Name: "j3"}
	if s, _ := f.PromptText(j3, func(n string) (string, bool) { return "embedded " + n, true }); s != "embedded j3" {
		t.Errorf("embedded = %q", s)
	}
	if _, err := f.PromptText(j3, nil); err == nil {
		t.Error("no source at all should be an error")
	}
}

func TestValidation(t *testing.T) {
	cases := map[string]string{
		"version":         strings.Replace(good, "version: 1", "version: 2", 1),
		"unknown class":   strings.Replace(good, "class: c1, prompt: hi", "class: nope, prompt: hi", 1),
		"unknown job":     strings.Replace(good, "jobs: [j1]", "jobs: [zz]", 1),
		"bad level":       strings.Replace(good, "trust: {c1: locked}", "trust: {c1: yolo}", 1),
		"both prompts":    strings.Replace(good, "prompt: hi", "prompt: hi, prompt_file: x", 1),
		"unknown field":   good + "\nextra: 1\n",
		"dup job":         strings.Replace(good, "name: j2", "name: j1", 1),
		"missing url":     strings.Replace(good, "url: u, ", "", 1),
		"bad class start": strings.Replace(good, "start: review", "start: maybe", 1),
	}
	for name, src := range cases {
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestPaths(t *testing.T) {
	t.Setenv("MARSHAL_CONFIG", "/x/fleet.yaml")
	t.Setenv("MARSHAL_STATE", "/y")
	if Path() != "/x/fleet.yaml" || StateDir() != "/y" {
		t.Error("env overrides ignored")
	}
	t.Setenv("MARSHAL_CONFIG", "")
	t.Setenv("MARSHAL_STATE", "")
	t.Setenv("XDG_DATA_HOME", "/data")
	if StateDir() != "/data/marshal" {
		t.Errorf("state = %q", StateDir())
	}
	if !strings.HasSuffix(Path(), "marshal/fleet.yaml") {
		t.Errorf("path = %q", Path())
	}
}
