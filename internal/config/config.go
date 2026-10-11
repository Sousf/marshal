// Package config loads the fleet file: which repositories MARSHAL looks after,
// which jobs run against them, and how much autonomy each class of change has
// earned.
//
// The file is deliberately the one thing the agent can never edit. Every cap,
// allowlist and trust level lives here, read by Go before a run starts, so a
// prompt injection in a changelog cannot talk its way past them.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Level is how much a class of change is allowed to do without a human.
//
// Ordered from most to least supervised so comparisons read naturally:
// a class may act on its own when its level is at least Soak.
type Level string

const (
	// Locked: the agent never proposes this class of change.
	Locked Level = "locked"
	// Review: a PR is opened and waits for the operator.
	Review Level = "review"
	// Soak: the PR merges itself after a quiet window unless someone objects.
	Soak Level = "soak"
	// Auto: the PR merges as soon as the gates pass.
	Auto Level = "auto"
)

var levelOrder = map[Level]int{Locked: 0, Review: 1, Soak: 2, Auto: 3}

// Valid reports whether l is one of the four known levels.
func (l Level) Valid() bool { _, ok := levelOrder[l]; return ok }

// Rank orders levels: Locked < Review < Soak < Auto.
func (l Level) Rank() int { return levelOrder[l] }

// Fleet is the whole file.
type Fleet struct {
	Version  int      `yaml:"version"`
	Defaults Defaults `yaml:"defaults"`
	Repos    []Repo   `yaml:"repos"`
	Jobs     []Job    `yaml:"jobs"`
	Policy   Policy   `yaml:"policy"`
	// GitHubApp, when set, makes MARSHAL act as a GitHub App installation
	// instead of the host user's gh login.
	GitHubApp GitHubApp `yaml:"github_app"`

	// dir is where the file was read from; relative prompt paths resolve
	// against it.
	dir string
}

// GitHubApp identifies an installed GitHub App. All three of app_id,
// installation_id and private_key_file are required when any is set.
type GitHubApp struct {
	AppID          int64  `yaml:"app_id"`
	InstallationID int64  `yaml:"installation_id"`
	PrivateKeyFile string `yaml:"private_key_file"`
	// Slug is the app's URL name, used for the bot commit identity.
	Slug string `yaml:"slug"`
}

// Set reports whether an app is configured at all.
func (g GitHubApp) Set() bool {
	return g.AppID != 0 || g.InstallationID != 0 || g.PrivateKeyFile != ""
}

// Identity is the git author MARSHAL commits as. Left empty, commits inherit
// whatever git resolves on the host, which on a dedicated runner user is the
// right answer anyway.
type Identity struct {
	Name  string `yaml:"name"`
	Email string `yaml:"email"`
}

// Defaults apply to every job unless the job overrides them.
type Defaults struct {
	Model           string   `yaml:"model"`
	Effort          string   `yaml:"effort"`
	MaxTurns        int      `yaml:"max_turns"`
	MaxUSD          float64  `yaml:"max_usd"`
	PermissionMode  string   `yaml:"permission_mode"`
	AllowedTools    []string `yaml:"allowed_tools"`
	DisallowedTools []string `yaml:"disallowed_tools"`
	Identity        Identity `yaml:"identity"`
}

// Repo is one repository in the fleet.
type Repo struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url"`
	// Branch is the base for PRs. Empty means the remote's default branch.
	Branch string `yaml:"branch"`
	// GitHub is the owner/name slug when it cannot be read off the URL, for
	// example when url is a local mirror. Empty means derive it from url.
	GitHub string   `yaml:"github"`
	Jobs   []string `yaml:"jobs"`
	// Trust overrides the policy's starting level for a class, per repo.
	// A brand new repo joins at review regardless of what it has earned
	// elsewhere, and this is how you say so without touching the policy.
	Trust map[string]Level `yaml:"trust"`
}

// Job is one kind of work the agent does against a repo.
type Job struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Schedule    string `yaml:"schedule"`
	// Class names the policy class the job's changes belong to, which
	// decides how autonomously the result may merge.
	Class string `yaml:"class"`
	// Prompt is the brief, inline. PromptFile points at one on disk instead.
	// With neither set, the embedded default for a job of this name is used.
	Prompt     string `yaml:"prompt"`
	PromptFile string `yaml:"prompt_file"`

	Model           string   `yaml:"model"`
	Effort          string   `yaml:"effort"`
	MaxTurns        int      `yaml:"max_turns"`
	MaxUSD          float64  `yaml:"max_usd"`
	PermissionMode  string   `yaml:"permission_mode"`
	AllowedTools    []string `yaml:"allowed_tools"`
	DisallowedTools []string `yaml:"disallowed_tools"`

	// Paths restricts which files the run may touch, as path.Match globs
	// relative to the repo root. Empty means anywhere. A run that changes a
	// file outside the list is rejected before anything is committed.
	Paths []string `yaml:"paths"`
	// MaxDiffLines caps added plus removed lines. Zero means no cap.
	MaxDiffLines int `yaml:"max_diff_lines"`
}

// Policy is the trust ladder and the verifier.
type Policy struct {
	Classes  map[string]Class `yaml:"classes"`
	Caps     Caps             `yaml:"caps"`
	Verifier Verifier         `yaml:"verifier"`
}

// Verifier configures the second agent that reads a proposal's diff before
// MARSHAL merges it on its own.
type Verifier struct {
	// Model defaults to the fleet default; a different family from the
	// proposer is a reasonable choice and costs nothing to configure.
	Model    string  `yaml:"model"`
	Effort   string  `yaml:"effort"`
	MaxTurns int     `yaml:"max_turns"`
	MaxUSD   float64 `yaml:"max_usd"`
	// Always runs the verifier on every proposal, not only the ones about to
	// merge autonomously, and posts its findings as a PR comment to make the
	// human review faster. Costs a run per PR.
	Always bool `yaml:"always"`
	// MaxDiffBytes caps how much patch text is put in front of the verifier.
	// Beyond it the diff is summarised by file and the verdict is UNSURE
	// unless the agent reads the files itself. Zero means 200 KB.
	MaxDiffBytes int `yaml:"max_diff_bytes"`
}

// ResolveVerifier folds defaults into the verifier settings.
func (f *Fleet) ResolveVerifier() Verifier {
	v := f.Policy.Verifier
	if v.Model == "" {
		v.Model = f.Defaults.Model
	}
	if v.Effort == "" {
		v.Effort = f.Defaults.Effort
	}
	if v.MaxTurns == 0 {
		v.MaxTurns = 25
	}
	if v.MaxUSD == 0 {
		v.MaxUSD = 2
	}
	if v.MaxDiffBytes == 0 {
		v.MaxDiffBytes = 200 * 1024
	}
	return v
}

// Class says where a kind of change starts on the ladder and how it moves.
type Class struct {
	Start Level `yaml:"start"`
	// Promote is how many consecutive approvals with no revert earn one step
	// up. Zero means the class never moves.
	Promote int `yaml:"promote"`
	// DemoteOnReject is how many steps a rejected PR costs.
	DemoteOnReject int `yaml:"demote_on_reject"`
	// DemoteOnRevert is how many steps a post-merge revert costs.
	DemoteOnRevert int `yaml:"demote_on_revert"`
	// SoakHours is the quiet window for the soak level.
	SoakHours int `yaml:"soak_hours"`
}

// Caps bound autonomy fleet-wide regardless of trust.
type Caps struct {
	AutoMergesPerRepoPerDay int     `yaml:"auto_merges_per_repo_per_day"`
	USDPerDay               float64 `yaml:"usd_per_day"`
	PRsPerRepoPerDay        int     `yaml:"prs_per_repo_per_day"`
}

// SetGitHubApp writes the github_app block into the fleet file at path,
// replacing any existing uncommented block, and validates the result before
// it replaces the file. Text-level on purpose: a YAML round trip would drop
// the operator's comments.
func SetGitHubApp(path string, g GitHubApp) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(string(b), "\n")
	var kept []string
	skipping := false
	for _, l := range lines {
		if strings.HasPrefix(l, "github_app:") {
			skipping = true
			continue
		}
		if skipping {
			if l == "" || strings.HasPrefix(l, " ") || strings.HasPrefix(l, "\t") {
				continue
			}
			skipping = false
		}
		kept = append(kept, l)
	}
	out := strings.TrimRight(strings.Join(kept, "\n"), "\n") + "\n\n"
	out += "github_app:\n"
	out += fmt.Sprintf("  app_id: %d\n", g.AppID)
	out += fmt.Sprintf("  installation_id: %d\n", g.InstallationID)
	out += fmt.Sprintf("  private_key_file: %s\n", g.PrivateKeyFile)
	if g.Slug != "" {
		out += fmt.Sprintf("  slug: %s\n", g.Slug)
	}
	// Validate everything but the all-or-nothing rule, since the
	// installation id arrives in a second step.
	var f Fleet
	dec := yaml.NewDecoder(strings.NewReader(out))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return fmt.Errorf("fleet file would not parse after edit: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(out), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Path returns the fleet file location: $MARSHAL_CONFIG, else
// $XDG_CONFIG_HOME/marshal/fleet.yaml.
func Path() string {
	if p := os.Getenv("MARSHAL_CONFIG"); p != "" {
		return p
	}
	base, err := os.UserConfigDir()
	if err != nil {
		base = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(base, "marshal", "fleet.yaml")
}

// StateDir returns where the database and work trees live: $MARSHAL_STATE,
// else $XDG_DATA_HOME/marshal.
func StateDir() string {
	if p := os.Getenv("MARSHAL_STATE"); p != "" {
		return p
	}
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		base = filepath.Join(os.Getenv("HOME"), ".local", "share")
	}
	return filepath.Join(base, "marshal")
}

// Load reads and validates the fleet file at path.
func Load(path string) (*Fleet, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	f, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	f.dir = filepath.Dir(path)
	return f, nil
}

// Parse decodes and validates fleet YAML.
func Parse(b []byte) (*Fleet, error) {
	var f Fleet
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, err
	}
	if err := f.validate(); err != nil {
		return nil, err
	}
	return &f, nil
}

func (f *Fleet) validate() error {
	var errs []error
	if f.Version != 1 {
		errs = append(errs, fmt.Errorf("version: want 1, got %d", f.Version))
	}
	jobs := map[string]bool{}
	for i, j := range f.Jobs {
		switch {
		case j.Name == "":
			errs = append(errs, fmt.Errorf("jobs[%d]: name is required", i))
		case jobs[j.Name]:
			errs = append(errs, fmt.Errorf("jobs[%d]: duplicate name %q", i, j.Name))
		}
		jobs[j.Name] = true
		if j.Class == "" {
			errs = append(errs, fmt.Errorf("job %q: class is required", j.Name))
		} else if _, ok := f.Policy.Classes[j.Class]; !ok {
			errs = append(errs, fmt.Errorf("job %q: class %q is not in policy.classes", j.Name, j.Class))
		}
		if j.Prompt != "" && j.PromptFile != "" {
			errs = append(errs, fmt.Errorf("job %q: prompt and prompt_file are exclusive", j.Name))
		}
	}
	repos := map[string]bool{}
	for i, r := range f.Repos {
		switch {
		case r.Name == "":
			errs = append(errs, fmt.Errorf("repos[%d]: name is required", i))
		case repos[r.Name]:
			errs = append(errs, fmt.Errorf("repos[%d]: duplicate name %q", i, r.Name))
		}
		repos[r.Name] = true
		if r.URL == "" {
			errs = append(errs, fmt.Errorf("repo %q: url is required", r.Name))
		}
		for _, j := range r.Jobs {
			if !jobs[j] {
				errs = append(errs, fmt.Errorf("repo %q: unknown job %q", r.Name, j))
			}
		}
		for c, l := range r.Trust {
			if _, ok := f.Policy.Classes[c]; !ok {
				errs = append(errs, fmt.Errorf("repo %q: trust names unknown class %q", r.Name, c))
			}
			if !l.Valid() {
				errs = append(errs, fmt.Errorf("repo %q: trust.%s: unknown level %q", r.Name, c, l))
			}
		}
	}
	for name, c := range f.Policy.Classes {
		if !c.Start.Valid() {
			errs = append(errs, fmt.Errorf("policy.classes.%s: start: unknown level %q", name, c.Start))
		}
	}
	if g := f.GitHubApp; g.Set() && (g.AppID == 0 || g.InstallationID == 0 || g.PrivateKeyFile == "") {
		errs = append(errs, errors.New("github_app: app_id, installation_id and private_key_file are all required"))
	}
	return errors.Join(errs...)
}

// Repo finds a repository by name.
func (f *Fleet) Repo(name string) (Repo, bool) {
	for _, r := range f.Repos {
		if r.Name == name {
			r = f.fillRepo(r)
			return r, true
		}
	}
	return Repo{}, false
}

// fillRepo is where repo defaults would go. Branch is deliberately left
// empty: the workspace asks the remote for its default branch, which is
// right for every repo, where guessing "main" was wrong for the first one
// tried.
func (f *Fleet) fillRepo(r Repo) Repo { return r }

// Job finds a job by name.
func (f *Fleet) Job(name string) (Job, bool) {
	for _, j := range f.Jobs {
		if j.Name == name {
			return j, true
		}
	}
	return Job{}, false
}

// Resolved is a job with the defaults folded in, ready to run.
type Resolved struct {
	Job
	Identity Identity
}

// Resolve folds defaults into a job. Lists concatenate rather than replace:
// a job that needs one extra tool should not have to restate the base set,
// and a job can never remove a default disallow.
func (f *Fleet) Resolve(j Job) Resolved {
	d := f.Defaults
	r := Resolved{Job: j, Identity: d.Identity}
	if r.Model == "" {
		r.Model = d.Model
	}
	if r.Effort == "" {
		r.Effort = d.Effort
	}
	if r.MaxTurns == 0 {
		r.MaxTurns = d.MaxTurns
	}
	if r.MaxUSD == 0 {
		r.MaxUSD = d.MaxUSD
	}
	if r.PermissionMode == "" {
		r.PermissionMode = d.PermissionMode
	}
	r.AllowedTools = dedupe(append(append([]string{}, d.AllowedTools...), j.AllowedTools...))
	r.DisallowedTools = dedupe(append(append([]string{}, d.DisallowedTools...), j.DisallowedTools...))
	return r
}

// PromptText returns the job's brief: inline, from file, or the embedded
// default supplied by the caller for jobs that set neither.
func (f *Fleet) PromptText(j Job, embedded func(name string) (string, bool)) (string, error) {
	switch {
	case j.Prompt != "":
		return j.Prompt, nil
	case j.PromptFile != "":
		p := j.PromptFile
		if !filepath.IsAbs(p) {
			p = filepath.Join(f.dir, p)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return "", fmt.Errorf("job %q: %w", j.Name, err)
		}
		return string(b), nil
	}
	if embedded != nil {
		if s, ok := embedded(j.Name); ok {
			return s, nil
		}
	}
	return "", fmt.Errorf("job %q: no prompt, prompt_file, or embedded default", j.Name)
}

// StartLevel is where a class begins for a repo: the repo's override if it
// has one, else the class's start.
func (f *Fleet) StartLevel(repo Repo, class string) Level {
	if l, ok := repo.Trust[class]; ok {
		return l
	}
	if c, ok := f.Policy.Classes[class]; ok {
		return c.Start
	}
	return Review
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
