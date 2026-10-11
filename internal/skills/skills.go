// Package skills is MARSHAL's procedural memory: things it learned how to
// do, written down as SKILL.md files in a repository the operator reviews.
//
// A skill is the most dangerous thing MARSHAL can remember, because it is an
// instruction to a future run, so every path into the library goes through
// a pull request and every skill is checked for the shapes an attacker would
// want in there before the pull request is even opened. Facts about one
// repository live in that repository's AGENTS.md; skills are what carries
// across repositories.
package skills

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

// Skill is one procedure.
type Skill struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Tags        []string `yaml:"tags,omitempty"`
	// LearnedFrom records the repo and run that proposed it.
	LearnedFrom string `yaml:"learned_from,omitempty"`
	// Body is the procedure, Markdown, after the frontmatter.
	Body string `yaml:"-"`
	// Path is where it was loaded from, relative to the library root.
	Path string `yaml:"-"`
}

// Slug is the directory name for a skill.
func (s Skill) Slug() string { return Slugify(s.Name) }

// Slugify turns a name into a safe directory name.
func Slugify(name string) string {
	var b strings.Builder
	last := '-'
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			last = r
		default:
			if last != '-' {
				b.WriteRune('-')
				last = '-'
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// MaxBody bounds a skill so the library stays a set of procedures, not a
// dumping ground for transcripts.
const MaxBody = 6000

// Parse reads a SKILL.md: YAML frontmatter between --- lines, then Markdown.
func Parse(path string, b []byte) (Skill, error) {
	text := strings.ReplaceAll(string(b), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return Skill{}, fmt.Errorf("%s: no frontmatter", path)
	}
	end := strings.Index(text[4:], "\n---")
	if end < 0 {
		return Skill{}, fmt.Errorf("%s: unterminated frontmatter", path)
	}
	var s Skill
	if err := yaml.Unmarshal([]byte(text[4:4+end]), &s); err != nil {
		return Skill{}, fmt.Errorf("%s: %w", path, err)
	}
	body := text[4+end+4:]
	s.Body = strings.TrimSpace(strings.TrimPrefix(body, "\n"))
	s.Path = path
	if s.Name == "" || s.Description == "" {
		return Skill{}, fmt.Errorf("%s: name and description are required", path)
	}
	if len(s.Body) > MaxBody {
		return Skill{}, fmt.Errorf("%s: body is %d bytes, cap is %d", path, len(s.Body), MaxBody)
	}
	return s, nil
}

// Render writes a skill back as SKILL.md.
func Render(s Skill) []byte {
	fm, _ := yaml.Marshal(struct {
		Name        string   `yaml:"name"`
		Description string   `yaml:"description"`
		Tags        []string `yaml:"tags,omitempty"`
		LearnedFrom string   `yaml:"learned_from,omitempty"`
	}{s.Name, s.Description, s.Tags, s.LearnedFrom})
	return []byte("---\n" + string(fm) + "---\n\n" + strings.TrimSpace(s.Body) + "\n")
}

// Load reads every skills/*/SKILL.md under root. Files that fail to parse
// are skipped and reported, never fatal: one bad file must not disable the
// library.
func Load(root string) ([]Skill, []error) {
	var out []Skill
	var errs []error
	matches, _ := filepath.Glob(filepath.Join(root, "*", "SKILL.md"))
	sort.Strings(matches)
	for _, m := range matches {
		b, err := os.ReadFile(m)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		rel, _ := filepath.Rel(root, m)
		s, err := Parse(rel, b)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, s)
	}
	return out, errs
}

// OperatorFile is the reviewed profile of the person MARSHAL works for.
const OperatorFile = "OPERATOR.md"

// LoadOperator reads the profile; empty if absent.
func LoadOperator(root string) string {
	b, err := os.ReadFile(filepath.Join(root, OperatorFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// forbidden are the shapes that have no place in a procedure MARSHAL will
// follow unattended. Matching one refuses the proposal outright; the
// operator never sees it as a PR, only as a note on the run.
var forbidden = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(curl|wget)\b[^\n]*\|\s*(ba|z|da)?sh\b`),
	regexp.MustCompile(`(?i)--dangerously|--no-verify|GIT_SSL_NO_VERIFY|StrictHostKeyChecking\s*=?\s*no`),
	regexp.MustCompile(`(?i)\b(api[_-]?key|secret|password|token|credential)s?\b[^\n]*(=|:)\s*\S{8,}`),
	regexp.MustCompile(`(?i)\b(ignore|disregard|override)\b[^\n]*\b(previous|prior|above|system|policy|instruction)`),
	regexp.MustCompile(`(?i)\b(skip|disable|bypass)\b[^\n]*\b(tests?|checks?|verif\w*|review|polic\w*|cap)`),
	regexp.MustCompile(`(?i)\b(git\s+push\s+(-f|--force)|force[- ]push)`),
	regexp.MustCompile(`(?i)\b(rm\s+-rf\s+/|chmod\s+777|sudo\s)`),
	regexp.MustCompile(`(?i)\b(always|automatically)\s+(merge|approve|pass)\b`),
	regexp.MustCompile(`(?i)base64\s+(-d|--decode)[^\n]*\|`),
}

// Check returns the reasons a skill may not enter the library.
func Check(s Skill) []string {
	var out []string
	text := s.Name + "\n" + s.Description + "\n" + s.Body
	for _, re := range forbidden {
		if m := re.FindString(text); m != "" {
			out = append(out, fmt.Sprintf("forbidden content: %q", clip(m, 60)))
		}
	}
	if strings.Count(s.Body, "http") > 5 {
		out = append(out, "too many URLs for a procedure")
	}
	if len(s.Body) > MaxBody {
		out = append(out, fmt.Sprintf("body over %d bytes", MaxBody))
	}
	return out
}

// ParseProposals extracts "## Skill: <name>" sections from a run's report
// and returns the report without them. The first non-empty line after the
// heading is the description; the rest is the body.
func ParseProposals(report string, learnedFrom string) (rest string, proposals []Skill) {
	lines := strings.Split(report, "\n")
	var kept []string
	var cur *Skill
	flush := func() {
		if cur != nil {
			cur.Body = strings.TrimSpace(cur.Body)
			if cur.Description != "" && cur.Body != "" {
				proposals = append(proposals, *cur)
			}
			cur = nil
		}
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "## Skill:") || strings.HasPrefix(line, "## Skill :") {
			flush()
			name := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(line, "## Skill:"), "## Skill :"))
			if name == "" {
				continue
			}
			cur = &Skill{Name: name, LearnedFrom: learnedFrom}
			continue
		}
		if cur != nil {
			if strings.HasPrefix(line, "## ") || strings.HasPrefix(line, "# ") {
				flush()
				kept = append(kept, line)
				continue
			}
			t := strings.TrimSpace(line)
			if cur.Description == "" {
				if t != "" {
					cur.Description = clip(t, 200)
				}
				continue
			}
			cur.Body += line + "\n"
			continue
		}
		kept = append(kept, line)
	}
	flush()
	return strings.TrimRight(strings.Join(kept, "\n"), "\n"), proposals
}

// Library is a loaded, searchable set of skills.
type Library struct {
	Skills   []Skill
	Operator string
}

// Open loads a library from root (the skills directory).
func Open(root string) (*Library, []error) {
	sk, errs := Load(root)
	return &Library{Skills: sk, Operator: LoadOperator(root)}, errs
}

// Search ranks skills against a query by term overlap, names and tags
// weighted above bodies. Plain scoring rather than an index: a library is
// tens of files, not thousands, and this stays correct with no database.
func (l *Library) Search(query string, limit int) []Skill {
	terms := tokens(query)
	if len(terms) == 0 || l == nil {
		return nil
	}
	type hit struct {
		s     Skill
		score int
	}
	var hits []hit
	for _, s := range l.Skills {
		name := tokens(s.Name + " " + strings.Join(s.Tags, " "))
		desc := tokens(s.Description)
		body := tokens(s.Body)
		score := 0
		for t := range terms {
			if name[t] {
				score += 5
			}
			if desc[t] {
				score += 3
			}
			if body[t] {
				score++
			}
		}
		if score > 0 {
			hits = append(hits, hit{s, score})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	var out []Skill
	for i := 0; i < len(hits) && i < limit; i++ {
		out = append(out, hits[i].s)
	}
	return out
}

var stop = map[string]bool{"the": true, "and": true, "for": true, "with": true, "that": true, "this": true, "from": true, "are": true, "not": true, "you": true, "your": true, "run": true, "use": true}

func tokens(s string) map[string]bool {
	out := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(s))
	sc.Split(bufio.ScanWords)
	for sc.Scan() {
		w := strings.ToLower(strings.Trim(sc.Text(), ".,;:()[]{}\"'`*_#-/"))
		if len(w) < 3 || stop[w] {
			continue
		}
		out[w] = true
	}
	return out
}

// Brief renders the memory section of a prompt: the operator profile, the
// skills selected for this run in full, and the rest of the library by
// name so the agent can read one from the mounted directory.
func (l *Library) Brief(selected []Skill, mount string) string {
	if l == nil || (len(l.Skills) == 0 && l.Operator == "") {
		return ""
	}
	var b strings.Builder
	if l.Operator != "" {
		b.WriteString("## About the operator\n\nReviewed notes on how the person you work for likes things done. Follow them where they apply.\n\n")
		b.WriteString(l.Operator + "\n\n")
	}
	if len(l.Skills) > 0 {
		b.WriteString("## Skills\n\nProcedures MARSHAL learned on earlier runs, reviewed by the operator. Use the ones that apply. If one is wrong or incomplete for what you find, propose the corrected version under the same name at the end of your report.\n\n")
		sel := map[string]bool{}
		for _, s := range selected {
			sel[s.Name] = true
			fmt.Fprintf(&b, "### %s\n\n%s\n\n%s\n\n", s.Name, s.Description, s.Body)
		}
		var others []string
		for _, s := range l.Skills {
			if !sel[s.Name] {
				others = append(others, fmt.Sprintf("- %s: %s (%s)", s.Name, s.Description, filepath.Join(mount, s.Path)))
			}
		}
		if len(others) > 0 {
			b.WriteString("Other skills in the library, readable at the paths given:\n\n" + strings.Join(others, "\n") + "\n\n")
		}
	}
	return b.String()
}

// Contract is appended to the brief: how to propose a skill.
const Contract = `
- If this run taught you a procedure that would save a future run real effort, on this repository or
  any other, end your report with a section headed exactly "## Skill: <short name>". The first line
  after the heading is a one-sentence description; the rest is the procedure: numbered steps, the
  commands, what to check. A skill is how to do something, not a fact about one repository (facts go
  under "## Lessons"). Keep it under a page. It becomes a pull request the operator reviews; never
  include credentials, downloads piped to a shell, or anything that weakens a check.
`

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
