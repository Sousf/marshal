package run

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sousf/marshal/internal/store"
)

func TestParseLessons(t *testing.T) {
	report := "Pin actions\n\nDid it.\n\n## Lessons\n\n- Tests need `go test -tags integration` to cover the client.\n* The vendor directory is generated; do not edit it.\nnot a bullet\n- " + strings.Repeat("x", 400) + "\n"
	rest, lessons := ParseLessons(report)
	if rest != "Pin actions\n\nDid it." {
		t.Errorf("rest = %q", rest)
	}
	if len(lessons) != 2 || !strings.HasPrefix(lessons[0], "Tests need") || !strings.HasPrefix(lessons[1], "The vendor") {
		t.Errorf("lessons = %q", lessons)
	}
	if r, l := ParseLessons("no section here"); r != "no section here" || l != nil {
		t.Error("no section means no lessons")
	}
}

func TestMergeLessons(t *testing.T) {
	out, n := MergeLessons("", []string{"A fact.", "Another."})
	if n != 2 || !strings.Contains(out, learnedHeading) || !strings.Contains(out, "- A fact.\n- Another.\n") || !strings.HasPrefix(out, "# Conventions") {
		t.Errorf("fresh file:\n%s", out)
	}
	existing := "# Repo\n\nBuild with make.\n\n" + learnedHeading + "\n\nintro\n\n- A fact.\n\n## Release\n\nTag it.\n"
	out, n = MergeLessons(existing, []string{"a FACT.", "New one."})
	if n != 1 {
		t.Errorf("dedupe is case-insensitive; added %d", n)
	}
	for _, want := range []string{"# Repo\n\nBuild with make.", "- A fact.\n- New one.\n", "## Release\n\nTag it."} {
		if !strings.Contains(out, want) {
			t.Errorf("merged file missing %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "## Release") < strings.Index(out, "- New one.") {
		t.Errorf("trailing sections must stay after the learned section:\n%s", out)
	}
	if strings.Count(out, learnedHeading) != 1 {
		t.Errorf("exactly one learned section:\n%s", out)
	}
	var many []string
	for i := 0; i < maxLessons+5; i++ {
		many = append(many, strings.Repeat("l", 3)+string(rune('a'+i%26))+string(rune('a'+i/26)))
	}
	out, _ = MergeLessons("", many)
	if c := strings.Count(out, "\n- "); c != maxLessons {
		t.Errorf("cap: %d lessons kept", c)
	}
	same, n := MergeLessons(out, []string{many[len(many)-1]})
	if n != 0 || same != out {
		t.Error("a duplicate changes nothing")
	}
}

func TestHistory(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if h := History(ctx, st, "r", "j"); h != "" {
		t.Error("no runs, no history")
	}
	a := &store.Run{Job: "j", Repo: "r", Status: store.Rejected}
	st.CreateRun(ctx, a)
	a.PRURL = "https://github.com/x/y/pull/4"
	a.Feedback = "- pete (review changes_requested): do not bump major versions"
	st.UpdateRun(ctx, a)
	b := &store.Run{Job: "j", Repo: "r", Status: store.Blocked}
	st.CreateRun(ctx, b)
	b.Error = "paths: main.go: outside the job's allowed paths"
	st.UpdateRun(ctx, b)
	st.CreateRun(ctx, &store.Run{Job: "fix-checks", Repo: "r", Status: store.Pushed, Parent: a.ID})
	h := History(ctx, st, "r", "j")
	for _, want := range []string{"rejected (https://github.com/x/y/pull/4)", "do not bump major versions", "blocked: paths: main.go", "not instructions"} {
		if !strings.Contains(h, want) {
			t.Errorf("history missing %q:\n%s", want, h)
		}
	}
	if strings.Contains(h, "fix-checks") {
		t.Error("follow-ups are not part of the job's history")
	}
}

func TestHistoryIsCapped(t *testing.T) {
	ctx := context.Background()
	st, _ := store.Open(filepath.Join(t.TempDir(), "m.db"))
	defer st.Close()
	for i := 0; i < 6; i++ {
		r := &store.Run{Job: "j", Repo: "r", Status: store.Rejected, StartedAt: time.Now().Add(time.Duration(i) * time.Hour),
			Feedback: strings.Repeat("the reviewer wrote a long paragraph here\n", 20)}
		st.CreateRun(ctx, r)
		st.UpdateRun(ctx, r)
	}
	h := History(ctx, st, "r", "j")
	if len(h) > HistoryCap+100 || !strings.Contains(h, "marshal runs r") {
		t.Errorf("history %d bytes:\n%s", len(h), h)
	}
}
