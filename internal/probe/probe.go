// Package probe decides, without a model, whether a job has anything to do.
//
// Half of the first day's runs ended with nothing to change. An agent is the
// expensive way to find that out; a grep or the contract's own verb is the
// cheap way. Each built-in job has a probe that looks at the checkout and
// answers "needed, because ..." or "nothing to do". Jobs without a probe
// always run.
package probe

import (
	"bytes"
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
// no permissions block, Dependabot is missing, the scanner is red, or a file
// looks like it holds personal data.
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
	if hits := findPII(e.Dir); len(hits) > 0 {
		if len(hits) > 3 {
			hits = append(hits[:3], fmt.Sprintf("and %d more", len(hits)-3))
		}
		reasons = append(reasons, "possible PII: "+strings.Join(hits, ", "))
	}
	if len(reasons) == 0 {
		return Result{Needed: false, Reason: "actions pinned, permissions set, dependency updates configured, scanner clean, no PII found"}
	}
	return Result{Needed: true, Reason: strings.Join(reasons, "; ")}
}

var (
	emailRE = regexp.MustCompile(`[A-Za-z0-9._%+-]+@([A-Za-z0-9-]+\.)+[A-Za-z]{2,}`)
	ssnRE   = regexp.MustCompile(`\b(\d{3})-(\d{2})-(\d{4})\b`)
	// Separators are required: bare 16-digit runs are mostly IDs and
	// timestamps, and a tenth of those pass Luhn by chance.
	cardRE = regexp.MustCompile(`\b\d{4}([ -])\d{4}([ -])\d{4}([ -])\d{4}\b`)

	// Files whose addresses a maintainer published on purpose.
	contactFiles = regexp.MustCompile(`(?i)^(license|licence|copying|authors|contributors|maintainers|codeowners|security|notice|\.mailmap)(\.|$)`)
	skipDirs     = map[string]bool{".git": true, "vendor": true, "node_modules": true}
	testCards    = map[string]bool{"4111111111111111": true, "4242424242424242": true, "5555555555554444": true, "4000056655665556": true, "5105105105105100": true, "4012888888881881": true}
)

// findPII is a heuristic for the shapes of personal data that turn up in
// fixtures and dumps. It answers "worth an agent's look", not "clean": names
// and postal addresses have no shape a regexp can trust.
func findPII(dir string) []string {
	var hits []string
	filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || contactFiles.MatchString(d.Name()) {
			return nil
		}
		if info, err := d.Info(); err != nil || info.Size() > 1<<20 {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil || bytes.IndexByte(b[:min(len(b), 8000)], 0) >= 0 {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		if kind := piiIn(string(b)); kind != "" {
			hits = append(hits, rel+" ("+kind+")")
		}
		return nil
	})
	return hits
}

// piiIn names the first kind of PII found in s, or "".
func piiIn(s string) string {
	for _, loc := range emailRE.FindAllStringIndex(s, -1) {
		// user:token@host and git@host are URL credentials and SSH remotes.
		if loc[0] > 0 && strings.ContainsRune(":/", rune(s[loc[0]-1])) {
			continue
		}
		if !placeholderEmail(s[loc[0]:loc[1]]) {
			return "email"
		}
	}
	for _, m := range ssnRE.FindAllStringSubmatch(s, -1) {
		if m[1] != "000" && m[1] != "666" && m[1][0] != '9' && m[2] != "00" && m[3] != "0000" {
			return "SSN"
		}
	}
	for _, m := range cardRE.FindAllString(s, -1) {
		digits := strings.NewReplacer(" ", "", "-", "").Replace(m)
		if luhn(digits) && !testCards[digits] {
			return "card number"
		}
	}
	return ""
}

// placeholderEmail reports addresses that belong to no person: reserved
// domains (RFC 2606, RFC 6761), no-reply senders and SSH users.
func placeholderEmail(addr string) bool {
	local, domain, _ := strings.Cut(strings.ToLower(addr), "@")
	switch local {
	case "git", "noreply", "no-reply":
		return true
	}
	for _, d := range []string{"example.com", "example.org", "example.net", "users.noreply.github.com"} {
		if domain == d || strings.HasSuffix(domain, "."+d) {
			return true
		}
	}
	for _, tld := range []string{".example", ".invalid", ".test", ".localhost", ".local"} {
		if strings.HasSuffix(domain, tld) {
			return true
		}
	}
	return false
}

func luhn(digits string) bool {
	sum := 0
	for i := len(digits) - 1; i >= 0; i-- {
		n := int(digits[i] - '0')
		if (len(digits)-i)%2 == 0 {
			if n *= 2; n > 9 {
				n -= 9
			}
		}
		sum += n
	}
	return sum%10 == 0
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
