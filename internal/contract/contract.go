// Package contract reads marshal.yaml, the small file by which a repository
// tells MARSHAL how to build, test and check it, and runs those verbs.
//
// The point is to take discovery and trust out of the loop. With the verbs
// known, the harness measures the tree before the agent touches it, refuses
// a change that breaks what worked, and the verifier runs the same commands
// instead of believing a claim. None of that involves a model.
package contract

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// File is the contract's name at the repository root.
const File = "marshal.yaml"

// Verbs in the order they are run and reported. A repository may define any
// subset; unknown names are rejected so a typo cannot silently skip a gate.
var Verbs = []string{"build", "test", "lint", "vuln", "format", "smoke"}

// Gate lists the verbs the harness refuses to regress.
var Gate = []string{"build", "test", "lint", "format"}

// Contract is a parsed marshal.yaml.
type Contract struct {
	Version int               `yaml:"contract"`
	Verbs   map[string]string `yaml:"verbs"`
	// Locked are paths no job may change, in addition to MARSHAL's own set.
	Locked []string `yaml:"locked"`
	// Sensitive are paths whose change always needs a human, whatever the
	// trust level says.
	Sensitive []string `yaml:"sensitive"`
	// Timeout per verb; zero means ten minutes.
	Timeout time.Duration `yaml:"timeout"`
}

// Load reads the contract from a checkout. ok is false when the file is
// absent, which is not an error: the repository simply has no contract yet.
func Load(dir string) (*Contract, bool, error) {
	b, err := os.ReadFile(filepath.Join(dir, File))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	c, err := Parse(b)
	if err != nil {
		return nil, true, fmt.Errorf("%s: %w", File, err)
	}
	return c, true, nil
}

// Parse decodes and validates contract YAML.
func Parse(b []byte) (*Contract, error) {
	var c Contract
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, err
	}
	if c.Version != 1 {
		return nil, fmt.Errorf("contract: want 1, got %d", c.Version)
	}
	known := map[string]bool{}
	for _, v := range Verbs {
		known[v] = true
	}
	for name, cmd := range c.Verbs {
		if !known[name] {
			return nil, fmt.Errorf("verbs.%s: unknown verb; known: %s", name, strings.Join(Verbs, ", "))
		}
		if strings.TrimSpace(cmd) == "" {
			return nil, fmt.Errorf("verbs.%s: empty command", name)
		}
	}
	return &c, nil
}

// Has reports whether a verb is defined.
func (c *Contract) Has(verb string) bool {
	return c != nil && strings.TrimSpace(c.Verbs[verb]) != ""
}

// Result is one verb's outcome.
type Result struct {
	Verb    string
	Command string
	OK      bool
	// Unavailable means the command's program is not installed here (exit
	// 127), which is reported, not held against the change.
	Unavailable bool
	Output      string // last few KB
	Duration    time.Duration
}

func (r Result) String() string {
	switch {
	case r.Unavailable:
		return r.Verb + ": unavailable (command not found)"
	case r.OK:
		return fmt.Sprintf("%s: pass (%s)", r.Verb, r.Duration.Round(time.Millisecond))
	}
	return fmt.Sprintf("%s: FAIL (%s)", r.Verb, r.Duration.Round(time.Millisecond))
}

// Run executes one verb in dir with a bounded output tail. The command runs
// through sh so a contract can use pipes and &&, as a developer would.
func (c *Contract) Run(ctx context.Context, dir, verb string) Result {
	cmd := c.Verbs[verb]
	res := Result{Verb: verb, Command: cmd}
	if strings.TrimSpace(cmd) == "" {
		res.Unavailable = true
		return res
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	sh := exec.CommandContext(ctx, "sh", "-c", cmd)
	sh.Dir = dir
	// No credentials reach a verb: it is the repository's own command line.
	sh.Env = scrub(os.Environ())
	sh.WaitDelay = 5 * time.Second
	var out tail
	sh.Stdout = &out
	sh.Stderr = &out
	start := time.Now()
	err := sh.Run()
	res.Duration = time.Since(start)
	res.Output = strings.TrimSpace(out.String())
	if err == nil {
		res.OK = true
		return res
	}
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 127 {
		res.Unavailable = true
		return res
	}
	if ctx.Err() == context.DeadlineExceeded {
		res.Output += fmt.Sprintf("\n[timed out after %s]", timeout)
	}
	return res
}

// RunAll runs the given verbs that the contract defines, in order.
func (c *Contract) RunAll(ctx context.Context, dir string, verbs ...string) []Result {
	var out []Result
	for _, v := range verbs {
		if c.Has(v) {
			out = append(out, c.Run(ctx, dir, v))
		}
	}
	return out
}

// Regressions compares a baseline with a later run and returns the verbs
// that passed before and fail now. Those are the harness's to refuse; a
// verb that was already failing is reported, not blamed on the change.
func Regressions(before, after []Result) []Result {
	passed := map[string]bool{}
	for _, r := range before {
		if r.OK {
			passed[r.Verb] = true
		}
	}
	var out []Result
	for _, r := range after {
		if passed[r.Verb] && !r.OK && !r.Unavailable {
			out = append(out, r)
		}
	}
	return out
}

// Summary renders results for a brief, a PR body or a log.
func Summary(results []Result) string {
	var b strings.Builder
	for _, r := range results {
		b.WriteString("- " + r.String() + "\n")
	}
	return b.String()
}

// Failures renders the output of failed verbs, bounded, for a brief.
func Failures(results []Result, limit int) string {
	var b strings.Builder
	for _, r := range results {
		if r.OK || r.Unavailable {
			continue
		}
		out := r.Output
		if len(out) > limit {
			out = "[... trimmed ...]\n" + out[len(out)-limit:]
		}
		fmt.Fprintf(&b, "### %s: `%s`\n\n```\n%s\n```\n\n", r.Verb, r.Command, out)
	}
	return b.String()
}

// Brief renders the contract section of a prompt.
func (c *Contract) Brief(baseline []Result) string {
	if c == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("## The repository's contract (marshal.yaml)\n\nThese are the exact commands this repository is built and checked with. Use them rather than guessing; MARSHAL runs them itself after you finish and refuses a change that makes one of them fail that passed before.\n\n")
	for _, v := range Verbs {
		if c.Has(v) {
			fmt.Fprintf(&b, "- %s: `%s`\n", v, c.Verbs[v])
		}
	}
	if len(baseline) > 0 {
		b.WriteString("\nState of the tree before your run:\n\n" + Summary(baseline))
		if f := Failures(baseline, 3000); f != "" {
			b.WriteString("\nOutput of what already fails (pre-existing, not yours, fix only if it is your job):\n\n" + f)
		}
	}
	if len(c.Sensitive) > 0 {
		b.WriteString("\nSensitive paths, which a human always reviews: " + strings.Join(c.Sensitive, ", ") + "\n")
	}
	return b.String()
}

func scrub(env []string) []string {
	var out []string
	for _, kv := range env {
		k := strings.SplitN(kv, "=", 2)[0]
		switch {
		case k == "GH_TOKEN", k == "GITHUB_TOKEN", strings.HasPrefix(k, "GIT_CONFIG_"), strings.HasPrefix(k, "ANTHROPIC_"), strings.HasPrefix(k, "AWS_"):
			continue
		}
		out = append(out, kv)
	}
	return out
}

type tail struct{ buf []byte }

func (t *tail) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > 16*1024 {
		t.buf = t.buf[len(t.buf)-16*1024:]
	}
	return len(p), nil
}

func (t *tail) String() string { return string(t.buf) }
