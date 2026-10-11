// Package policy decides what a run is allowed to have done and how much
// supervision its result needs.
//
// Two halves. Check inspects a finished diff against the job's limits before
// anything is committed: files outside the allowed paths, too many lines, new
// modules. The ladder half turns review outcomes into trust levels, so a
// class of change that keeps getting approved stops needing approval.
package policy

import (
	"fmt"
	"path"
	"strings"

	"github.com/Sousf/marshal/internal/config"
	"github.com/Sousf/marshal/internal/store"
)

// Change is one file in a diff, from `git diff --numstat`.
type Change struct {
	Path    string
	Added   int
	Removed int
}

// Violation is one reason a diff was refused.
type Violation struct {
	Rule string
	Path string
	Text string
}

func (v Violation) String() string {
	if v.Path != "" {
		return v.Rule + ": " + v.Path + ": " + v.Text
	}
	return v.Rule + ": " + v.Text
}

// alwaysLocked lists paths no job may touch, however it is configured. The
// fleet file itself is read from outside the repo so it is not here, but the
// things that could mint credentials or ship a release are.
var alwaysLocked = []string{
	".github/workflows/release*.yml",
	".github/workflows/release*.yaml",
	".goreleaser.yml",
	".goreleaser.yaml",
	"**/*.pem",
	"**/*.key",
	".env",
	"**/.env",
	".marshal/**",
	// The release job writes these, deterministically, in a PR of its own.
	// An agent that edits them could choose what ships.
	"VERSION",
	"CHANGELOG.md",
}

// alwaysAllowed may be touched by any job whatever its path list: the
// conventions file is where MARSHAL records what it learned, in the same
// pull request as the change that taught it.
var alwaysAllowed = []string{"AGENTS.md"}

// Check returns every violation in the diff. An empty result means the diff
// may be committed.
func Check(job config.Job, changes []Change) []Violation {
	return CheckWith(job, nil, changes)
}

// CheckWith is Check with extra locked patterns from the repository's own
// contract. Contract locks only ever add to MARSHAL's set.
func CheckWith(job config.Job, extraLocked []string, changes []Change) []Violation {
	var out []Violation
	total := 0
	locked := append(append([]string{}, alwaysLocked...), extraLocked...)
	for _, c := range changes {
		total += c.Added + c.Removed
		if m := matchAny(locked, c.Path); m != "" {
			out = append(out, Violation{Rule: "locked", Path: c.Path, Text: "matches locked pattern " + m})
			continue
		}
		if matchAny(alwaysAllowed, c.Path) != "" {
			continue
		}
		if len(job.Paths) > 0 && matchAny(job.Paths, c.Path) == "" {
			out = append(out, Violation{Rule: "paths", Path: c.Path, Text: "outside the job's allowed paths"})
		}
	}
	if job.MaxDiffLines > 0 && total > job.MaxDiffLines {
		out = append(out, Violation{Rule: "size", Text: fmt.Sprintf("%d changed lines, cap is %d", total, job.MaxDiffLines)})
	}
	return out
}

// Touches reports the changed paths matching any of the patterns, for the
// contract's sensitive list.
func Touches(patterns []string, changes []Change) []string {
	var out []string
	for _, c := range changes {
		if matchAny(patterns, c.Path) != "" {
			out = append(out, c.Path)
		}
	}
	return out
}

// matchAny returns the first pattern matching p. Patterns use path.Match
// syntax with one extension: a leading "**/" matches at any depth, so
// "**/*.key" catches a key file anywhere in the tree.
func matchAny(patterns []string, p string) string {
	p = strings.TrimPrefix(p, "./")
	for _, pat := range patterns {
		if match(pat, p) {
			return pat
		}
	}
	return ""
}

func match(pat, p string) bool {
	if strings.HasPrefix(pat, "**/") {
		rest := pat[3:]
		if ok, _ := path.Match(rest, p); ok {
			return true
		}
		for i := 0; i < len(p); i++ {
			if p[i] == '/' {
				if ok, _ := path.Match(rest, p[i+1:]); ok {
					return true
				}
			}
		}
		return false
	}
	if strings.HasSuffix(pat, "/**") {
		dir := strings.TrimSuffix(pat, "/**")
		return p == dir || strings.HasPrefix(p, dir+"/")
	}
	ok, _ := path.Match(pat, p)
	return ok
}

// ParseNumstat reads `git diff --numstat` output.
func ParseNumstat(s string) []Change {
	var out []Change
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		c := Change{Path: strings.Join(f[2:], " ")}
		// Binary files report "-" for both counts.
		fmt.Sscan(f[0], &c.Added)
		fmt.Sscan(f[1], &c.Removed)
		out = append(out, c)
	}
	return out
}

// Outcome is what the operator, or CI, said about a proposal.
type Outcome int

const (
	Approved Outcome = iota
	Rejected
	Reverted
)

// Ladder applies trust rules to a repo and class.
type Ladder struct {
	Fleet *config.Fleet
}

// Current returns the trust record for a repo and class, seeding it from the
// policy's start level when nothing has been recorded.
func (l Ladder) Current(repo config.Repo, class string, t store.Trust, found bool) store.Trust {
	if found {
		return t
	}
	return store.Trust{Repo: repo.Name, Class: class, Level: string(l.Fleet.StartLevel(repo, class))}
}

// Apply moves a trust record in response to an outcome and returns the new
// record. Approvals extend a streak and promote one step when the streak
// reaches the class's threshold. Rejections and reverts demote by the
// configured number of steps and reset the streak. Locked is a floor the
// ladder never crosses: a class can only be unlocked by editing the policy.
func (l Ladder) Apply(t store.Trust, o Outcome) store.Trust {
	c := l.Fleet.Policy.Classes[t.Class]
	lvl := config.Level(t.Level)
	if lvl == config.Locked {
		return t
	}
	switch o {
	case Approved:
		t.Approvals++
		t.Streak++
		if c.Promote > 0 && t.Streak >= c.Promote && lvl != config.Auto {
			lvl = step(lvl, +1)
			t.Streak = 0
		}
	case Rejected:
		t.Rejections++
		t.Streak = 0
		lvl = step(lvl, -max(c.DemoteOnReject, 1))
	case Reverted:
		t.Reverts++
		t.Streak = 0
		lvl = step(lvl, -max(c.DemoteOnRevert, 2))
	}
	// Review is the floor for demotion. Locking is a human decision.
	if lvl == config.Locked {
		lvl = config.Review
	}
	t.Level = string(lvl)
	return t
}

var ranks = []config.Level{config.Locked, config.Review, config.Soak, config.Auto}

func step(l config.Level, by int) config.Level {
	i := l.Rank() + by
	if i < 0 {
		i = 0
	}
	if i >= len(ranks) {
		i = len(ranks) - 1
	}
	return ranks[i]
}

// MayMerge reports whether a proposal at this level may merge without a
// human, and whether it must wait for the soak window first.
func MayMerge(l config.Level) (auto, soak bool) {
	switch l {
	case config.Auto:
		return true, false
	case config.Soak:
		return true, true
	}
	return false, false
}
