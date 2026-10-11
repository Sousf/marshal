package policy

import (
	"testing"

	"github.com/Sousf/marshal/internal/config"
	"github.com/Sousf/marshal/internal/store"
)

func TestCheck(t *testing.T) {
	job := config.Job{Paths: []string{".github/workflows/*.yml", "go.mod", "go.sum"}, MaxDiffLines: 50}
	changes := []Change{
		{Path: ".github/workflows/ci.yml", Added: 3, Removed: 3},
		{Path: "go.sum", Added: 2, Removed: 2},
		{Path: "internal/x.go", Added: 1},
		{Path: ".github/workflows/release.yml", Added: 1},
		{Path: "deploy/prod.key", Added: 1},
	}
	v := Check(job, changes)
	rules := map[string]int{}
	for _, x := range v {
		rules[x.Rule]++
	}
	if rules["paths"] != 1 || rules["locked"] != 2 || rules["size"] != 0 {
		t.Errorf("violations: %v", v)
	}
	big := []Change{{Path: "go.mod", Added: 40, Removed: 20}}
	if v := Check(job, big); len(v) != 1 || v[0].Rule != "size" {
		t.Errorf("size cap not enforced: %v", v)
	}
	if v := Check(config.Job{}, []Change{{Path: "anything/at/all.go"}}); len(v) != 0 {
		t.Errorf("job with no paths should allow anything unlocked: %v", v)
	}
	if v := Check(job, []Change{{Path: "AGENTS.md", Added: 2}}); len(v) != 0 {
		t.Errorf("AGENTS.md is allowed for every job: %v", v)
	}
	if v := CheckWith(config.Job{}, []string{"deploy/**"}, []Change{{Path: "deploy/x.sh"}}); len(v) != 1 || v[0].Rule != "locked" {
		t.Errorf("contract locks apply: %v", v)
	}
	if got := Touches([]string{"internal/auth/**", "*.pem"}, []Change{{Path: "internal/auth/x.go"}, {Path: "main.go"}}); len(got) != 1 || got[0] != "internal/auth/x.go" {
		t.Errorf("touches = %v", got)
	}
}

func TestMatch(t *testing.T) {
	cases := []struct {
		pat, p string
		want   bool
	}{
		{"**/*.key", "a/b/c.key", true},
		{"**/*.key", "c.key", true},
		{"**/*.key", "c.keyring", false},
		{".marshal/**", ".marshal/notes.md", true},
		{".marshal/**", ".marshalx", false},
		{".github/workflows/release*.yml", ".github/workflows/release-nightly.yml", true},
		{"go.mod", "go.mod", true},
		{"go.mod", "sub/go.mod", false},
	}
	for _, c := range cases {
		if got := match(c.pat, c.p); got != c.want {
			t.Errorf("match(%q,%q)=%v want %v", c.pat, c.p, got, c.want)
		}
	}
}

func TestParseNumstat(t *testing.T) {
	ch := ParseNumstat("3\t1\tgo.mod\n-\t-\timg.png\n10\t0\tdir with space/f.go\n")
	if len(ch) != 3 || ch[0].Added != 3 || ch[1].Added != 0 || ch[2].Path != "dir with space/f.go" {
		t.Errorf("%+v", ch)
	}
}

func TestLadder(t *testing.T) {
	f := &config.Fleet{Policy: config.Policy{Classes: map[string]config.Class{
		"fix":   {Start: config.Review, Promote: 3, DemoteOnReject: 1, DemoteOnRevert: 2},
		"never": {Start: config.Locked, Promote: 1},
	}}}
	l := Ladder{Fleet: f}
	repo := config.Repo{Name: "r"}

	tr := l.Current(repo, "fix", store.Trust{}, false)
	if tr.Level != "review" {
		t.Fatalf("seed = %q", tr.Level)
	}
	for i := 0; i < 3; i++ {
		tr = l.Apply(tr, Approved)
	}
	if tr.Level != "soak" || tr.Streak != 0 || tr.Approvals != 3 {
		t.Errorf("after 3 approvals: %+v", tr)
	}
	for i := 0; i < 3; i++ {
		tr = l.Apply(tr, Approved)
	}
	if tr.Level != "auto" {
		t.Errorf("after 6 approvals: %+v", tr)
	}
	tr = l.Apply(tr, Approved)
	if tr.Level != "auto" {
		t.Errorf("auto is the ceiling: %+v", tr)
	}
	tr = l.Apply(tr, Reverted)
	if tr.Level != "review" || tr.Streak != 0 || tr.Reverts != 1 {
		t.Errorf("revert from auto: %+v", tr)
	}
	tr = l.Apply(tr, Rejected)
	if tr.Level != "review" {
		t.Errorf("review is the demotion floor: %+v", tr)
	}

	locked := l.Current(repo, "never", store.Trust{}, false)
	if got := l.Apply(locked, Approved); got.Level != "locked" {
		t.Errorf("locked must not move: %+v", got)
	}

	override := config.Repo{Name: "new", Trust: map[string]config.Level{"fix": config.Locked}}
	if got := l.Current(override, "fix", store.Trust{}, false); got.Level != "locked" {
		t.Errorf("repo override ignored: %+v", got)
	}
}
