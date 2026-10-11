// Package probe decides, without a model, whether a job has anything to do.
//
// Half of the first day's runs ended with nothing to change. An agent is the
// expensive way to find that out; a grep or the contract's own verb is the
// cheap way. Each built-in job has a probe that looks at the checkout and
// answers "needed, because ..." or "nothing to do". Jobs without a probe
// always run.
package probe

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Sousf/marshal/internal/contract"
)

// Env is what a probe may look at.
type Env struct {
	Dir      string
	Contract *contract.Contract
	// LastRun is when this job last ran on this repo; zero if never.
	LastRun time.Time
	// FeedbackSince reports whether reviewer feedback arrived after t. Set
	// by the caller that has the store; nil means unknown.
	FeedbackSince func(t time.Time) bool
}

// Result of a probe.
type Result struct {
	Needed bool
	Reason string
}

// Func is a probe.
type Func func(ctx context.Context, e Env) Result

// For returns the probe for a built-in job, or nil for a job that should
// simply run.
func For(job string) Func {
	switch job {
	case "vuln-fix":
		return vulnFix
	case "posture-audit":
		return postureAudit
	case "onboard":
		return onboard
	case "reflect":
		return reflect
	}
	return nil
}

// Run applies the job's probe. Jobs without one are always needed.
func Run(ctx context.Context, job string, e Env) Result {
	f := For(job)
	if f == nil {
		return Result{Needed: true, Reason: "no probe for this job"}
	}
	return f(ctx, e)
}

// vulnFix needs the agent only when a scanner reports something. The
// contract's vuln verb is used when present, govulncheck for a Go module
// otherwise; with no scanner at all there is nothing to act on.
func vulnFix(ctx context.Context, e Env) Result {
	if e.Contract.Has("vuln") {
		r := e.Contract.Run(ctx, e.Dir, "vuln")
		switch {
		case r.Unavailable:
			return Result{Needed: false, Reason: "vuln verb's scanner is not installed here"}
		case r.OK:
			return Result{Needed: false, Reason: "vuln verb passes"}
		}
		return Result{Needed: true, Reason: "vuln verb fails: " + firstLine(r.Output)}
	}
	if _, err := os.Stat(filepath.Join(e.Dir, "go.mod")); err == nil {
		if _, err := exec.LookPath("govulncheck"); err != nil {
			return Result{Needed: false, Reason: "govulncheck is not installed here"}
		}
		cmd := exec.CommandContext(ctx, "govulncheck", "./...")
		cmd.Dir = e.Dir
		out, err := cmd.CombinedOutput()
		if err == nil {
			return Result{Needed: false, Reason: "govulncheck is clean"}
		}
		return Result{Needed: true, Reason: "govulncheck reports findings: " + lastInteresting(string(out))}
	}
	return Result{Needed: false, Reason: "no scanner for this repository"}
}

var (
	usesRE = regexp.MustCompile(`(?m)^\s*-?\s*uses:\s*([^\s#]+)`)
	shaRE  = regexp.MustCompile(`@[0-9a-f]{40}$`)
	permRE = regexp.MustCompile(`(?m)^permissions:`)
	prRE   = regexp.MustCompile(`(?m)^\s*pull_request\s*:`)
)

// postureAudit needs the agent when a workflow has an unpinned action or
// no permissions block, Dependabot is missing, or the scanner is red.
func postureAudit(ctx context.Context, e Env) Result {
	var reasons []string
	workflows, _ := filepath.Glob(filepath.Join(e.Dir, ".github", "workflows", "*.yml"))
	more, _ := filepath.Glob(filepath.Join(e.Dir, ".github", "workflows", "*.yaml"))
	workflows = append(workflows, more...)
	for _, w := range workflows {
		b, err := os.ReadFile(w)
		if err != nil {
			continue
		}
		name := filepath.Base(w)
		unpinned := 0
		for _, m := range usesRE.FindAllStringSubmatch(string(b), -1) {
			ref := m[1]
			if strings.HasPrefix(ref, "./") || strings.HasPrefix(ref, "docker://") {
				continue
			}
			if !shaRE.MatchString(ref) {
				unpinned++
			}
		}
		if unpinned > 0 {
			reasons = append(reasons, fmt.Sprintf("%s: %d action(s) not pinned to a SHA", name, unpinned))
		}
		if !permRE.Match(b) {
			reasons = append(reasons, name+": no top-level permissions block")
		}
	}
	if _, err := os.Stat(filepath.Join(e.Dir, ".github", "dependabot.yml")); err != nil {
		if _, err := os.Stat(filepath.Join(e.Dir, "renovate.json")); err != nil {
			reasons = append(reasons, "no Dependabot or Renovate configuration")
		}
	}
	if v := vulnFix(ctx, e); v.Needed {
		reasons = append(reasons, v.Reason)
	}
	if len(reasons) == 0 {
		return Result{Needed: false, Reason: "actions pinned, permissions set, dependency updates configured, scanner clean"}
	}
	return Result{Needed: true, Reason: strings.Join(reasons, "; ")}
}

// onboard needs the agent when the baseline is incomplete: CI on pull
// requests, Dependabot, AGENTS.md, a contract.
func onboard(_ context.Context, e Env) Result {
	var missing []string
	ci := false
	workflows, _ := filepath.Glob(filepath.Join(e.Dir, ".github", "workflows", "*.y*ml"))
	for _, w := range workflows {
		if b, err := os.ReadFile(w); err == nil && prRE.Match(b) {
			ci = true
			break
		}
	}
	if !ci {
		missing = append(missing, "a workflow that runs on pull requests")
	}
	if _, err := os.Stat(filepath.Join(e.Dir, ".github", "dependabot.yml")); err != nil {
		missing = append(missing, "Dependabot")
	}
	if _, err := os.Stat(filepath.Join(e.Dir, "AGENTS.md")); err != nil {
		missing = append(missing, "AGENTS.md")
	}
	if _, err := os.Stat(filepath.Join(e.Dir, contract.File)); err != nil {
		missing = append(missing, contract.File)
	}
	if len(missing) == 0 {
		return Result{Needed: false, Reason: "CI, Dependabot, AGENTS.md and a contract are all present"}
	}
	return Result{Needed: true, Reason: "missing " + strings.Join(missing, ", ")}
}

// reflect needs the agent only when there is new feedback to reflect on.
func reflect(_ context.Context, e Env) Result {
	if e.FeedbackSince == nil {
		return Result{Needed: true, Reason: "feedback state unknown"}
	}
	if e.FeedbackSince(e.LastRun) {
		return Result{Needed: true, Reason: "new reviewer feedback since the last reflection"}
	}
	return Result{Needed: false, Reason: "no new reviewer feedback since the last reflection"}
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}

// lastInteresting pulls the summary line govulncheck prints, if any.
func lastInteresting(out string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "vulnerabilit") && strings.Contains(l, "affect") {
			return strings.TrimSpace(l)
		}
	}
	return firstLine(out)
}
