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

func TestMemoryRepo(t *testing.T) {
	f, _ := Parse([]byte(good))
	if _, _, ok := f.MemoryRepo(); ok {
		t.Error("no memory by default")
	}
	f.Memory.Repo = "a"
	r, dir, ok := f.MemoryRepo()
	if !ok || r.Name != "a" || dir != "skills" {
		t.Errorf("%+v %q %v", r, dir, ok)
	}
}

func TestResolveVerifier(t *testing.T) {
	f, _ := Parse([]byte(good))
	v := f.ResolveVerifier()
	if v.Model != "m" || v.MaxTurns != 25 || v.MaxUSD != 2 || v.MaxDiffBytes == 0 || v.Always {
		t.Errorf("%+v", v)
	}
	f.Policy.Verifier.Model = "other"
	f.Policy.Verifier.Always = true
	if v := f.ResolveVerifier(); v.Model != "other" || !v.Always {
		t.Errorf("%+v", v)
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
		"partial app":     good + "\ngithub_app: {app_id: 1}\n",
		"bad memory repo": good + "\nmemory: {repo: nope}\n",
		"task no prompt":  good + "\ntasks: [{name: t}]\n",
		"task bad repo":   good + "\ntasks: [{name: t, prompt: p, repo: nope}]\n",
	}
	for name, src := range cases {
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestSetGitHubApp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fleet.yaml")
	os.WriteFile(path, []byte(good+"\n# github_app:\n#   app_id: 1\n"), 0o600)
	if err := SetGitHubApp(path, GitHubApp{AppID: 7, PrivateKeyFile: "~/k.pem", Slug: "m"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "github_app:\n  app_id: 7\n  installation_id: 0\n  private_key_file: ~/k.pem\n  slug: m\n") {
		t.Errorf("block not written:\n%s", b)
	}
	if !strings.Contains(string(b), "# github_app:") || !strings.Contains(string(b), "version: 1") {
		t.Error("comments and the rest of the file must survive")
	}
	// Second write replaces the block rather than appending another.
	if err := SetGitHubApp(path, GitHubApp{AppID: 7, InstallationID: 9, PrivateKeyFile: "~/k.pem", Slug: "m"}); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(path)
	if strings.Count(string(b), "\ngithub_app:") != 1 || !strings.Contains(string(b), "installation_id: 9") {
		t.Errorf("block not replaced:\n%s", b)
	}
	if _, err := Load(path); err != nil {
		t.Errorf("file must load: %v", err)
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

func TestNeverFloor(t *testing.T) {
	f, err := Parse([]byte(good + `
never:
  paths: ["infra/**"]
  tools: ["Bash(go:*)", "WebFetch"]
  auto_merge: ["a/*", "*/deps"]
`))
	if err != nil {
		t.Fatal(err)
	}
	j, _ := f.Job("j1")
	r := f.Resolve(j)
	if strings.Contains(strings.Join(r.AllowedTools, ","), "Bash(go:*)") {
		t.Errorf("a never tool stayed allowed: %v", r.AllowedTools)
	}
	if got := strings.Join(r.DisallowedTools, ","); got != "Bash(git push:*),Bash(go:*),WebFetch" {
		t.Errorf("disallowed = %q", got)
	}
	for _, c := range []struct {
		repo, class string
		hold        bool
	}{{"a", "c1", true}, {"b", "deps", true}, {"b", "c1", false}} {
		if got := f.Never.HoldsMerge(c.repo, c.class); got != c.hold {
			t.Errorf("HoldsMerge(%s,%s) = %v", c.repo, c.class, got)
		}
	}
	if _, err := Parse([]byte(good + "never:\n  auto_merge: [\"[\"]\n")); err == nil {
		t.Error("a malformed pattern must not load")
	}
}

func TestIsolation(t *testing.T) {
	f, err := Parse([]byte(good + "isolation:\n  mode: bwrap\n  read_only: [\"~/sdk\"]\n  writable: [/var/cache/x]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !f.Isolation.On() {
		t.Error("mode bwrap should be on")
	}
	for _, bad := range []string{"isolation:\n  mode: docker\n", "isolation:\n  mode: bwrap\n  writable: [cache]\n"} {
		if _, err := Parse([]byte(good + bad)); err == nil {
			t.Errorf("should not load: %s", bad)
		}
	}
	if g, _ := Parse([]byte(good)); g.Isolation.On() {
		t.Error("isolation should be off unless configured")
	}
}

func TestReviewers(t *testing.T) {
	src := strings.Replace(good, "  model: m\n", "  model: m\n  reviewers: [Sousf]\n", 1)
	src = strings.Replace(src, "repos:\n", "repos:\n  - {name: b, url: u2, jobs: [j1], reviewers: [someone-else, Sousf/maintainers]}\n", 1)
	f, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if a, _ := f.Repo("a"); strings.Join(a.Reviewers, ",") != "Sousf" {
		t.Errorf("a inherits the default reviewers: %v", a.Reviewers)
	}
	if b, _ := f.Repo("b"); strings.Join(b.Reviewers, ",") != "someone-else,Sousf/maintainers" {
		t.Errorf("b overrides them: %v", b.Reviewers)
	}
	for _, bad := range []string{"--admin", "a b", "x;y", "-lead", "o/t/z"} {
		if _, err := Parse([]byte(strings.Replace(good, "  model: m\n", "  model: m\n  reviewers: [\""+bad+"\"]\n", 1))); err == nil {
			t.Errorf("reviewer %q should be refused", bad)
		}
	}
}

// What the daemon hands a remote client: enough to render repos, jobs and
// the ladder, nothing that authenticates anything.
func TestShareable(t *testing.T) {
	src := good + `
github_app: {app_id: 1, installation_id: 2, private_key_file: /secret/app.pem}
notify:
  ntfy: {url: "https://ntfy.sh/secret-topic", token: tk_secret}
  telegram: {bot_token: "123:secretbot", chat_id: "42"}
  webhook: https://hooks.example/secret
`
	src = strings.Replace(src, "{name: a, url: u,", "{name: a, url: \"https://x-access-token:ghs_secret@github.com/o/a.git\",", 1)
	f, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.Shareable()
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"secret", "ghs_", "tk_", ".pem"} {
		if strings.Contains(string(b), secret) {
			t.Errorf("shared fleet leaks %q:\n%s", secret, b)
		}
	}
	got, err := ParseShared(append(b, []byte("\nfield_from_a_newer_daemon: 1\n")...))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Repos) != 1 || len(got.Jobs) != 2 || got.Repos[0].URL != "https://github.com/o/a.git" || got.Policy.Classes["c1"].Start != "review" {
		t.Errorf("shared fleet lost what clients render: %+v", got)
	}
	if f.Notify.Telegram.BotToken == "" {
		t.Error("Shareable must not modify the daemon's own fleet")
	}
}
