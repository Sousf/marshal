package contract

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// A repository without marshal.yaml still has a CI workflow, and that
// workflow is the contract GitHub will hold the pull request to. Derive
// reads the jobs that run on pull requests and turns their run steps into
// verbs, so the same checks run here first, with the same Go toolchain
// setup-go would install. It is an approximation of the runner, not the
// runner: actions other than setup-go are skipped, and the host's tools
// are used. For the common failures, a test that fails or a scanner that
// finds something, it is enough.

// Derived is a contract synthesised from workflows, with where it came from.
type Derived struct {
	*Contract
	// Workflows lists the files it was built from.
	Workflows []string
	// Toolchain is the GOTOOLCHAIN the verbs run with, if any.
	Toolchain string
}

type workflow struct {
	Name string `yaml:"name"`
	On   any    `yaml:"on"`
	Jobs map[string]struct {
		Steps []struct {
			Name string            `yaml:"name"`
			Uses string            `yaml:"uses"`
			Run  string            `yaml:"run"`
			With map[string]string `yaml:"with"`
			Env  map[string]string `yaml:"env"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

// onPullRequest reports whether the workflow's on: includes pull_request.
func onPullRequest(on any) bool {
	switch v := on.(type) {
	case string:
		return v == "pull_request"
	case []any:
		for _, x := range v {
			if s, ok := x.(string); ok && s == "pull_request" {
				return true
			}
		}
	case map[string]any:
		_, ok := v["pull_request"]
		return ok
	}
	return false
}

// Derive builds a contract from the pull-request workflows under dir. ok
// is false when there are none.
func Derive(dir string) (*Derived, bool, error) {
	files, _ := filepath.Glob(filepath.Join(dir, ".github", "workflows", "*.yml"))
	more, _ := filepath.Glob(filepath.Join(dir, ".github", "workflows", "*.yaml"))
	files = append(files, more...)
	sort.Strings(files)
	d := &Derived{Contract: &Contract{Version: 1, Verbs: map[string]string{}}}
	var steps []string
	goVersion := ""
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var w workflow
		if err := yaml.Unmarshal(b, &w); err != nil || !onPullRequest(w.On) {
			continue
		}
		// Release and publish workflows are never run here even if they
		// also trigger on pull requests.
		if strings.Contains(strings.ToLower(filepath.Base(f)), "release") {
			continue
		}
		d.Workflows = append(d.Workflows, filepath.Base(f))
		jobNames := make([]string, 0, len(w.Jobs))
		for n := range w.Jobs {
			jobNames = append(jobNames, n)
		}
		sort.Strings(jobNames)
		for _, jn := range jobNames {
			for _, st := range w.Jobs[jn].Steps {
				if strings.HasPrefix(st.Uses, "actions/setup-go") {
					if v := st.With["go-version"]; v != "" {
						goVersion = v
					} else if st.With["go-version-file"] != "" {
						goVersion = goModVersion(filepath.Join(dir, st.With["go-version-file"]))
					}
					continue
				}
				if strings.TrimSpace(st.Run) == "" {
					continue
				}
				run, dropped := stripHostSetup(st.Run)
				d.HostSetup = append(d.HostSetup, dropped...)
				if run == "" {
					continue
				}
				for k, v := range st.Env {
					run = fmt.Sprintf("%s=%q %s", k, v, run)
				}
				steps = append(steps, run)
				classify(d.Contract, run)
			}
		}
	}
	if len(d.Workflows) == 0 || len(steps) == 0 {
		return nil, false, nil
	}
	// The whole sequence, as CI would run it, is the ci verb; the classified
	// verbs let probes and the gate talk about test, lint and vuln.
	d.Verbs["ci"] = strings.Join(steps, " && ")
	d.Steps = steps
	if goVersion != "" {
		d.Toolchain = toolchain(goVersion)
		if d.Toolchain != "" {
			d.Env = []string{"GOTOOLCHAIN=" + d.Toolchain}
		}
	}
	return d, true, nil
}

// hostSetupRE matches a command that prepares a CI runner rather than
// checking the code: privilege escalation or a system package manager.
// Run on MARSHAL's host these would fail at best, and at worst change it.
var hostSetupRE = regexp.MustCompile(`^(sudo|doas|su|apt|apt-get|aptitude|dpkg|yum|dnf|rpm|zypper|apk|pacman|yay|paru|snap|flatpak|brew|port|choco|winget|scoop)\b`)

// compoundRE spots shell that spans lines: continuations, blocks, loops
// and heredocs. Such a step cannot be split into commands safely.
var compoundRE = regexp.MustCompile(`(?m)\\$|^\s*(if|then|else|elif|fi|for|while|until|do|done|case|esac|function)\b|[{}]\s*$|<<`)

// stripHostSetup removes host setup commands from a run step and returns
// what is left and what was dropped. A step with none is returned
// verbatim. A plain step, one command per line or joined with &&, loses
// just those commands. A compound step that contains one is dropped whole:
// splitting it would change what it means, and running it would fail.
func stripHostSetup(step string) (string, []string) {
	step = strings.TrimSpace(step)
	var cmds []string
	setup := false
	for _, line := range strings.Split(step, "\n") {
		for _, cmd := range strings.Split(line, "&&") {
			cmd = strings.TrimSpace(cmd)
			if cmd == "" || strings.HasPrefix(cmd, "#") {
				continue
			}
			cmds = append(cmds, cmd)
			if hostSetupRE.MatchString(cmd) {
				setup = true
			}
		}
	}
	switch {
	case !setup:
		return step, nil
	case compoundRE.MatchString(step) || strings.ContainsAny(step, ";|"):
		return "", []string{step}
	}
	var kept, dropped []string
	for _, c := range cmds {
		if hostSetupRE.MatchString(c) {
			dropped = append(dropped, c)
		} else {
			kept = append(kept, c)
		}
	}
	return strings.Join(kept, " && "), dropped
}

var (
	testRE   = regexp.MustCompile(`\bgo test\b|\bnpm test\b|\bpytest\b|\bcargo test\b|\bzig build test\b`)
	lintRE   = regexp.MustCompile(`\bgo vet\b|\bstaticcheck\b|\bgolangci-lint\b|\beslint\b|\bshellcheck\b|\bruff\b|\bclippy\b`)
	vulnRE   = regexp.MustCompile(`\bgovulncheck\b|\bnpm audit\b|\bpip-audit\b|\bcargo audit\b|\btrivy\b`)
	buildRE  = regexp.MustCompile(`\bgo build\b|\bnpm run build\b|\bcargo build\b|\bzig build\b|\bmake\b`)
	formatRE = regexp.MustCompile(`\bgofmt\b|\bprettier\b|\bblack\b|\brustfmt\b`)
)

// classify maps a run step onto a named verb when it is unambiguous. The
// first match for each verb wins; a step doing several things is only
// attributed once, to the most specific verb.
func classify(c *Contract, run string) {
	set := func(verb string) bool {
		if _, has := c.Verbs[verb]; has {
			return false
		}
		c.Verbs[verb] = run
		return true
	}
	switch {
	case vulnRE.MatchString(run):
		set("vuln")
	case testRE.MatchString(run):
		set("test")
	case lintRE.MatchString(run):
		set("lint")
	case formatRE.MatchString(run):
		set("format")
	case buildRE.MatchString(run):
		set("build")
	}
}

var goLineRE = regexp.MustCompile(`(?m)^(toolchain go|go )(\d+\.\d+(?:\.\d+)?)`)

// goModVersion reads the version setup-go would resolve from a go.mod: the
// toolchain line if present, else the go line.
func goModVersion(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	version := ""
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	for sc.Scan() {
		if m := goLineRE.FindStringSubmatch(sc.Text()); m != nil {
			if m[1] == "toolchain go" {
				return m[2]
			}
			version = m[2]
		}
	}
	return version
}

// toolchain turns a setup-go version into a GOTOOLCHAIN value. "1.27"
// becomes go1.27.0, "1.27.2" go1.27.2; words like stable are left to the
// host.
func toolchain(v string) string {
	v = strings.TrimSpace(v)
	if !regexp.MustCompile(`^\d+\.\d+(\.\d+)?$`).MatchString(v) {
		return ""
	}
	if strings.Count(v, ".") == 1 {
		v += ".0"
	}
	return "go" + v
}
