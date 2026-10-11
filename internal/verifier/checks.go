package verifier

import (
	"bufio"
	"fmt"
	"regexp"
	"strings"
)

// Finding is one thing the verifier, or the deterministic checks, noticed.
type Finding struct {
	Category string `json:"category"` // scope, safety, injection, verification, other
	Severity string `json:"severity"` // high, medium, low
	Detail   string `json:"detail"`
	// Source is "check" for the deterministic pass, "agent" for the model.
	Source string `json:"source,omitempty"`
}

// Checks are what the harness can establish about a diff without a model.
// They are put in front of the verifier as facts, and a high-severity one
// fails the verdict on its own, because these are exactly the things an
// injected proposer would try to talk a reader past.
type Checks struct {
	// NewModules lists require lines added to go.mod.
	NewModules []string
	// WorkflowUses lists added or changed `uses:` lines in workflows.
	WorkflowUses []string
	// Suspicious lists lines that look like encoded blobs or raw URLs added
	// to source files.
	Suspicious []string
}

// Findings converts the checks into findings.
func (c Checks) Findings() []Finding {
	var out []Finding
	for _, m := range c.NewModules {
		out = append(out, Finding{Category: "safety", Severity: "high", Source: "check",
			Detail: "go.mod gains a new module: " + m})
	}
	for _, u := range c.WorkflowUses {
		out = append(out, Finding{Category: "safety", Severity: "high", Source: "check",
			Detail: "workflow action added or changed: " + u})
	}
	for _, s := range c.Suspicious {
		out = append(out, Finding{Category: "safety", Severity: "medium", Source: "check",
			Detail: "added line looks like an encoded blob or a raw URL: " + s})
	}
	return out
}

// Fatal reports whether any check alone should fail the verdict.
func (c Checks) Fatal() bool {
	return len(c.NewModules) > 0 || len(c.WorkflowUses) > 0
}

// Run computes the checks from the base and head go.mod and the patch.
func Run(baseGoMod, headGoMod, patch string, class string) Checks {
	var c Checks
	// New modules. A dependency job may bump versions; it may not add
	// modules. Every class is held to that.
	before := requires(baseGoMod)
	for mod := range requires(headGoMod) {
		if _, ok := before[mod]; !ok {
			c.NewModules = append(c.NewModules, mod)
		}
	}
	c.WorkflowUses, c.Suspicious = scanPatch(patch)
	return c
}

var requireRE = regexp.MustCompile(`^\s*(?:require\s+)?([A-Za-z0-9._~/-]+\.[A-Za-z0-9._~/-]+)\s+v[0-9]`)

// requires returns the module paths a go.mod requires, direct and indirect.
func requires(gomod string) map[string]bool {
	out := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(gomod))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "//") || strings.HasPrefix(line, "module ") || strings.HasPrefix(line, "go ") || strings.HasPrefix(line, "replace ") || strings.HasPrefix(line, "exclude ") || strings.HasPrefix(line, "retract ") || strings.HasPrefix(line, "toolchain ") {
			continue
		}
		if m := requireRE.FindStringSubmatch(line); m != nil {
			out[m[1]] = true
		}
	}
	return out
}

var (
	usesRE   = regexp.MustCompile(`^\+\s*-?\s*uses:\s*(\S+)`)
	base64RE = regexp.MustCompile(`[A-Za-z0-9+/]{80,}={0,2}`)
	urlRE    = regexp.MustCompile(`https?://[^\s"'` + "`" + `)]+`)
)

// scanPatch walks added lines. Workflow `uses:` lines are reported whatever
// they are; the verifier decides if the pin is right. Encoded blobs and
// URLs are reported from source files only, since lock files and vendored
// data are full of both.
func scanPatch(patch string) (uses, suspicious []string) {
	file := ""
	sc := bufio.NewScanner(strings.NewReader(patch))
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "+++ b/") {
			file = strings.TrimPrefix(line, "+++ b/")
			continue
		}
		if !strings.HasPrefix(line, "+") || strings.HasPrefix(line, "+++") {
			continue
		}
		if strings.HasPrefix(file, ".github/workflows/") {
			if m := usesRE.FindStringSubmatch(line); m != nil {
				uses = append(uses, file+": "+m[1])
			}
			continue
		}
		if !sourceFile(file) {
			continue
		}
		body := line[1:]
		if base64RE.MatchString(body) {
			suspicious = append(suspicious, fmt.Sprintf("%s: %s", file, clip(body, 80)))
			continue
		}
		if u := urlRE.FindString(body); u != "" && !strings.Contains(u, "github.com") && !strings.Contains(u, "golang.org") && !strings.Contains(u, "pkg.go.dev") {
			suspicious = append(suspicious, fmt.Sprintf("%s: %s", file, u))
		}
	}
	return uses, suspicious
}

func sourceFile(f string) bool {
	for _, ext := range []string{".go", ".py", ".js", ".ts", ".rs", ".sh", ".rb", ".c", ".h", ".zig", ".yml", ".yaml", ".toml"} {
		if strings.HasSuffix(f, ext) {
			return !strings.HasSuffix(f, "go.sum") && !strings.Contains(f, "testdata/")
		}
	}
	return false
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
