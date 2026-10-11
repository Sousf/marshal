package run

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sousf/marshal/internal/config"
	"github.com/Sousf/marshal/internal/store"
)

func TestVersions(t *testing.T) {
	for s, ok := range map[string]bool{"1.2.3": true, "v0.9.0\n": true, "1.2": false, "1.2.3-rc1": false, "01.2.3": false, "x": false} {
		if _, got := ParseVersion(s); got != ok {
			t.Errorf("ParseVersion(%q) = %v", s, got)
		}
	}
	v := Version{0, 9, 0}
	for _, c := range []struct {
		last     Version
		released bool
		branch   string
		want     string
	}{
		{v, true, "", "0.9.1"},
		{v, false, "", "0.1.0"},
		{v, true, "0.10.0\n", "0.10.0"}, // the operator's minor bump survives
		{v, true, "0.9.0", "0.9.1"},     // a stale branch never goes backwards
		{v, true, "garbage", "0.9.1"},
	} {
		if got := NextVersion(c.last, c.released, c.branch).String(); got != c.want {
			t.Errorf("NextVersion(%v, %v, %q) = %s, want %s", c.last, c.released, c.branch, got, c.want)
		}
	}
}

func TestParseLogAndNotes(t *testing.T) {
	log := "Merge pull request #12 from Sousf/marshal/vuln-fix/abc\x1fBump x/net for GO-2026-1\n\nbody\x1e\n" +
		"Fix the tab label\x1f\x1e\n" +
		"Merge pull request #11 from Sousf/marshal/release\x1fRelease v0.9.0\x1e\n" +
		"Merge pull request #10 from Sousf/feature\x1f\x1e\n"
	cs := ParseLog(log)
	if len(cs) != 3 || cs[0] != (Change{"Bump x/net for GO-2026-1", "#12"}) || cs[1].Title != "Fix the tab label" || cs[2].Title != "Sousf/feature" {
		t.Fatalf("changes = %+v", cs)
	}
	notes := Notes(Version{1, 0, 0}, time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC), cs)
	if !strings.HasPrefix(notes, "## v1.0.0 (2026-10-11)\n\n- Bump x/net for GO-2026-1 (#12)\n") {
		t.Errorf("notes:\n%s", notes)
	}
	if got := PrependNotes("", notes); !strings.HasPrefix(got, "# Changelog\n\n## v1.0.0") {
		t.Errorf("new changelog:\n%s", got)
	}
	if got := PrependNotes("# Changelog\n\n## v0.9.0\n- old\n", notes); !strings.Contains(got, "(#12)\n- Fix the tab label\n- Sousf/feature (#10)\n\n## v0.9.0\n- old") || strings.Count(got, "# Changelog") != 1 {
		t.Errorf("prepended:\n%s", got)
	}
}

func releaseFleet(t *testing.T, url string) *config.Fleet {
	t.Helper()
	f, err := config.Parse([]byte(`
version: 1
defaults: {model: m}
repos:
  - {name: x, url: ` + url + `, jobs: [release]}
jobs:
  - {name: release, class: release, schedule: daily}
policy:
  classes: {release: {start: review, promote: 0}}
`))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestReleaseJob(t *testing.T) {
	ctx := context.Background()
	o := origin(t)
	git := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = o
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	st, _ := store.Open(filepath.Join(t.TempDir(), "m.db"))
	defer st.Close()
	work := t.TempDir()
	x := &Executor{Fleet: releaseFleet(t, o), Store: st, Adapter: &stepwise{}, WorkRoot: work}
	run := func() (store.Run, string) {
		var log bytes.Buffer
		rec, err := x.Execute(ctx, "release", "x", Options{NoPR: true, Keep: true, Log: &log})
		if err != nil {
			t.Fatal(err)
		}
		return rec, log.String()
	}

	git("tag", "v0.9.0")
	if rec, log := run(); rec.Status != store.Skipped || !strings.Contains(rec.Error, "nothing since v0.9.0") {
		t.Fatalf("nothing merged: %s %q\n%s", rec.Status, rec.Error, log)
	}
	os.WriteFile(filepath.Join(o, "a.go"), []byte("package x\n"), 0o644)
	git("add", ".")
	git("commit", "--quiet", "-m", "Add the a feature")
	rec, log := run()
	if rec.Status != store.Committed || !strings.Contains(rec.Summary, "Release v0.9.1") || rec.Adapter != "marshal" || rec.CostUSD != 0 {
		t.Fatalf("release: %s %q %+v\n%s", rec.Status, rec.Error, rec, log)
	}
	dirs, _ := filepath.Glob(filepath.Join(work, "run-"+rec.ID+"-*"))
	if len(dirs) != 1 {
		t.Fatalf("kept tree: %v", dirs)
	}
	v, _ := os.ReadFile(filepath.Join(dirs[0], "VERSION"))
	cl, _ := os.ReadFile(filepath.Join(dirs[0], "CHANGELOG.md"))
	if string(v) != "0.9.1\n" || !strings.Contains(string(cl), "- Add the a feature") {
		t.Errorf("VERSION=%q CHANGELOG:\n%s", v, cl)
	}

	// The operator bumped the minor on the release branch: kept.
	git("checkout", "--quiet", "-b", "marshal/release")
	os.WriteFile(filepath.Join(o, "VERSION"), []byte("0.10.0\n"), 0o644)
	git("add", ".")
	git("commit", "--quiet", "-m", "minor please")
	git("checkout", "--quiet", "main")
	if rec, log := run(); !strings.Contains(rec.Summary, "Release v0.10.0") {
		t.Errorf("operator's version lost: %q\n%s", firstLine(rec.Summary), log)
	}
	os.Remove(filepath.Join(o, "VERSION"))

	// Merged but not yet tagged: wait for the workflow, open nothing.
	os.WriteFile(filepath.Join(o, "VERSION"), []byte("0.9.1\n"), 0o644)
	git("add", ".")
	git("commit", "--quiet", "-m", "Release v0.9.1")
	if rec, _ := run(); rec.Status != store.Skipped || !strings.Contains(rec.Error, "not yet tagged") {
		t.Errorf("pending release: %s %q", rec.Status, rec.Error)
	}

	// No release workflow, no PR.
	os.Remove(filepath.Join(o, ".github/workflows/release.yml"))
	git("commit", "--quiet", "-am", "drop release")
	if rec, _ := run(); rec.Status != store.Skipped || !strings.Contains(rec.Error, "no .github/workflows/release.yml") {
		t.Errorf("no workflow: %s %q", rec.Status, rec.Error)
	}
}
