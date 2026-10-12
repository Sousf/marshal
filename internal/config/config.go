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
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
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
	// Notify is where the operator hears about events.
	Notify Notify `yaml:"notify"`
	// Memory names the repository that holds the skills library and the
	// operator profile. Empty disables skills.
	Memory Memory `yaml:"memory"`
	// Tasks are scheduled free-form instructions, with or without a repo.
	Tasks []Task `yaml:"tasks"`
	// Never is the deny floor. Nothing a job, task, chat, contract or trust
	// level says can lift it.
	Never Never `yaml:"never"`
	// Homebrew, when set, makes onboarding check that each Go repo
	// releases to this tap, and propose the wiring when it does not.
	Homebrew Homebrew `yaml:"homebrew"`
	// Review, when enabled, makes MARSHAL review pull requests people open
	// on the fleet's repos.
	Review PRReview `yaml:"review"`

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

// Memory points at the skills library: a fleet repo and a directory in it.
type Memory struct {
	Repo string `yaml:"repo"`
	// Dir defaults to "skills".
	Dir string `yaml:"dir"`
	// NudgeTurns is how long a run must be before a cheap model is asked
	// whether it taught a skill. Zero means 20; negative turns it off.
	NudgeTurns int `yaml:"nudge_turns"`
	// NudgeModel answers the nudge. Empty means claude-sonnet-5-5.
	NudgeModel string `yaml:"nudge_model"`
}

// Nudge resolves the skill nudge: the turn threshold, zero when off, and
// the model.
func (m Memory) Nudge() (int, string) {
	turns, model := m.NudgeTurns, m.NudgeModel
	switch {
	case turns < 0:
		turns = 0
	case turns == 0:
		turns = 20
	}
	if model == "" {
		model = "claude-sonnet-5-5"
	}
	return turns, model
}

// Task is a free-form instruction. With Repo it ends in a pull request like
// a job; without, in a report.
type Task struct {
	Name     string `yaml:"name"`
	Schedule string `yaml:"schedule"`
	Prompt   string `yaml:"prompt"`
	Repo     string `yaml:"repo"`
	// Class is the policy class for a repo task's changes. Default "code".
	Class string `yaml:"class"`
}

// MemoryRepo resolves the memory repo and directory; ok is false when none
// is configured.
func (f *Fleet) MemoryRepo() (Repo, string, bool) {
	if f.Memory.Repo == "" {
		return Repo{}, "", false
	}
	r, ok := f.Repo(f.Memory.Repo)
	if !ok {
		return Repo{}, "", false
	}
	dir := f.Memory.Dir
	if dir == "" {
		dir = "skills"
	}
	return r, dir, true
}

// PRReview configures reviewing pull requests that people open. MARSHAL
// verifies each new head and posts the verdict; it never pushes to, merges
// or closes such a PR on its own.
type PRReview struct {
	Enabled bool `yaml:"enabled"`
	// ApproveAuthors are the GitHub logins whose pull requests MARSHAL may
	// approve, on a verifier PASS with green checks. An approval satisfies
	// a one-approval ruleset, so anyone not listed gets the review and
	// never the approval.
	ApproveAuthors []string `yaml:"approve_authors"`
}

// MayApprove reports whether login is one of ApproveAuthors.
func (r PRReview) MayApprove(login string) bool {
	for _, a := range r.ApproveAuthors {
		if login != "" && strings.EqualFold(a, login) {
			return true
		}
	}
	return false
}

// Homebrew names the tap every tool in the fleet publishes to.
type Homebrew struct {
	// Tap is owner/name of the tap repository, e.g. Sousf/homebrew-tap.
	Tap string `yaml:"tap"`
	// Branch is the tap's branch. Empty means main.
	Branch string `yaml:"branch"`
	// UpgradeLocal makes the daemon upgrade a tool on its own host as soon
	// as a release it saw merged reaches the tap, if the tool is installed
	// there through brew. For a daemon on the operator's workstation.
	UpgradeLocal bool `yaml:"upgrade_local"`
	// Brew is the brew executable. Empty means Linuxbrew's.
	Brew string `yaml:"brew"`
}

// BrewPath resolves the brew executable.
func (h Homebrew) BrewPath() string {
	if h.Brew != "" {
		return h.Brew
	}
	return "/home/linuxbrew/.linuxbrew/bin/brew"
}

// Formula is the full brew name of a tool in the tap: owner/tap/name.
func (h Homebrew) Formula(name string) string {
	owner, tap, _, _ := h.TapParts()
	return strings.ToLower(owner + "/" + strings.TrimPrefix(tap, "homebrew-") + "/" + name)
}

// TapParts splits the tap into owner, repository and branch.
func (h Homebrew) TapParts() (owner, name, branch string, ok bool) {
	owner, name, ok = strings.Cut(h.Tap, "/")
	branch = h.Branch
	if branch == "" {
		branch = "main"
	}
	return owner, name, branch, ok && owner != "" && name != "" && !strings.Contains(name, "/")
}

// Never lists what no run may do, whatever else is configured. It only adds
// restrictions: paths join the locked set, tools join every agent's
// disallowed list (jobs, tasks, follow-ups, the verifier and chat), and
// auto_merge patterns hold matching proposals for a human at any level.
type Never struct {
	// Paths are locked globs, the same syntax as a job's paths.
	Paths []string `yaml:"paths"`
	// Tools use the harness's pattern syntax, for example "Bash(curl:*)".
	Tools []string `yaml:"tools"`
	// AutoMerge holds "repo/class" globs, for example "marshal/*" or
	// "*/deps", that never merge without a human.
	AutoMerge []string `yaml:"auto_merge"`
}

// HoldsMerge reports whether the floor forbids merging a repo's class
// without a human.
func (n Never) HoldsMerge(repo, class string) bool {
	for _, p := range n.AutoMerge {
		if ok, _ := path.Match(p, repo+"/"+class); ok {
			return true
		}
	}
	return false
}

// Notify configures notification channels. Any subset may be set.
type Notify struct {
	Ntfy     Ntfy     `yaml:"ntfy"`
	Telegram Telegram `yaml:"telegram"`
	// Webhook receives every event as a JSON POST.
	Webhook string `yaml:"webhook"`
	// Events lists the kinds to send: proposed, merged, needs-human, failed,
	// reverted, digest. Empty means all but merged.
	Events []string `yaml:"events"`
	// Digest is the local time of day for the daily summary, "HH:MM".
	// Empty means 08:00.
	Digest string `yaml:"digest"`
}

// Ntfy is an ntfy.sh topic, self-hosted or not.
type Ntfy struct {
	URL   string `yaml:"url"`
	Token string `yaml:"token"`
}

// Telegram is a bot and the chat it posts to.
type Telegram struct {
	BotToken string `yaml:"bot_token"`
	ChatID   string `yaml:"chat_id"`
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
	// Reviewers are requested on every pull request MARSHAL opens: GitHub
	// logins, or org/team. A repo's own list replaces them.
	Reviewers       []string `yaml:"reviewers"`
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
	// Reviewers replaces defaults.reviewers for this repo.
	Reviewers []string `yaml:"reviewers"`
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
	// CheckFixAttempts is how many times MARSHAL may push a fix to one of
	// its own PRs whose checks failed. Zero means the default of 2; -1
	// disables follow-ups.
	CheckFixAttempts int `yaml:"check_fix_attempts"`
}

// FixAttempts resolves the follow-up cap.
func (c Caps) FixAttempts() int {
	switch {
	case c.CheckFixAttempts < 0:
		return 0
	case c.CheckFixAttempts == 0:
		return 2
	}
	return c.CheckFixAttempts
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

// Shareable renders the fleet for a client on another machine: the repos,
// jobs and policy it shows, without the notification credentials, the app
// key's location, or credentials in a repo URL.
func (f *Fleet) Shareable() ([]byte, error) {
	c := *f
	c.Notify = Notify{}
	c.GitHubApp.PrivateKeyFile = ""
	c.Repos = make([]Repo, len(f.Repos))
	for i, r := range f.Repos {
		if u, err := url.Parse(r.URL); err == nil && u.User != nil {
			u.User = nil
			r.URL = u.String()
		}
		c.Repos[i] = r
	}
	return yaml.Marshal(&c)
}

// ParseShared reads what Shareable wrote. The daemon validated it; a
// client that is older or newer than the daemon still reads what it
// knows, rather than refusing the whole fleet over one new field.
func ParseShared(b []byte) (*Fleet, error) {
	var f Fleet
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	return &f, nil
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
	for _, l := range f.Defaults.Reviewers {
		if !reviewerRE.MatchString(l) {
			errs = append(errs, fmt.Errorf("defaults.reviewers: %q is not a GitHub login or org/team", l))
		}
	}
	repos := map[string]bool{}
	for i, r := range f.Repos {
		for _, l := range r.Reviewers {
			if !reviewerRE.MatchString(l) {
				errs = append(errs, fmt.Errorf("repo %q: reviewers: %q is not a GitHub login or org/team", r.Name, l))
			}
		}
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
	if f.Memory.Repo != "" && !repos[f.Memory.Repo] {
		errs = append(errs, fmt.Errorf("memory.repo: unknown repo %q", f.Memory.Repo))
	}
	tasks := map[string]bool{}
	for i, t := range f.Tasks {
		switch {
		case t.Name == "":
			errs = append(errs, fmt.Errorf("tasks[%d]: name is required", i))
		case tasks[t.Name]:
			errs = append(errs, fmt.Errorf("tasks[%d]: duplicate name %q", i, t.Name))
		}
		tasks[t.Name] = true
		if strings.TrimSpace(t.Prompt) == "" {
			errs = append(errs, fmt.Errorf("task %q: prompt is required", t.Name))
		}
		if t.Repo != "" && !repos[t.Repo] {
			errs = append(errs, fmt.Errorf("task %q: unknown repo %q", t.Name, t.Repo))
		}
		if t.Class != "" {
			if _, ok := f.Policy.Classes[t.Class]; !ok {
				errs = append(errs, fmt.Errorf("task %q: class %q is not in policy.classes", t.Name, t.Class))
			}
		}
	}
	for _, p := range append(append([]string{}, f.Never.Paths...), f.Never.AutoMerge...) {
		if _, err := path.Match(strings.TrimPrefix(p, "**/"), "x"); err != nil {
			errs = append(errs, fmt.Errorf("never: bad pattern %q: %w", p, err))
		}
	}
	if f.Homebrew.Tap != "" {
		if _, _, _, ok := f.Homebrew.TapParts(); !ok {
			errs = append(errs, fmt.Errorf("homebrew.tap: want owner/name, got %q", f.Homebrew.Tap))
		}
	}
	for _, t := range f.Never.Tools {
		if strings.TrimSpace(t) == "" {
			errs = append(errs, errors.New("never.tools: empty entry"))
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
func (f *Fleet) fillRepo(r Repo) Repo {
	if len(r.Reviewers) == 0 {
		r.Reviewers = f.Defaults.Reviewers
	}
	return r
}

// reviewerRE is a GitHub login or an org/team slug. They become gh
// arguments, so nothing that reads as a flag or holds a separator passes.
var reviewerRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}(/[A-Za-z0-9][A-Za-z0-9._-]*)?$`)

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
	r.DisallowedTools = dedupe(append(append(append([]string{}, d.DisallowedTools...), j.DisallowedTools...), f.Never.Tools...))
	r.AllowedTools = Without(r.AllowedTools, f.Never.Tools)
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

// Without drops every entry of deny from tools, so an allow never sits
// beside the deny it contradicts, whatever the harness does with the pair.
func Without(tools, deny []string) []string {
	if len(deny) == 0 {
		return tools
	}
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		denied := false
		for _, d := range deny {
			if t == d {
				denied = true
				break
			}
		}
		if !denied {
			out = append(out, t)
		}
	}
	return out
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
