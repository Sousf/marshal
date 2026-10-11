package run

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Sousf/marshal/internal/config"
	"github.com/Sousf/marshal/internal/store"
)

// The release job is deterministic: no agent, no model, no spend. It keeps
// one pull request per repository open that, when the operator merges it,
// ships what has merged since the last release. Merging is the only way
// anything is released; the repository's release workflow tags the version
// in VERSION and publishes it.

// ReleaseJob is the job name that runs the release preparer instead of an
// agent.
const ReleaseJob = "release"

// ReleaseBranch is the one branch MARSHAL rebuilds for a repo's release PR.
const ReleaseBranch = "marshal/release"

// releaseWorkflow is what merging the PR relies on; without it a merge
// would change VERSION and ship nothing, so no PR is opened.
const releaseWorkflow = ".github/workflows/release.yml"

// maxChangelog bounds the entries in one release's notes.
const maxChangelog = 50

// Version is a plain major.minor.patch.
type Version struct{ Major, Minor, Patch int }

// ParseVersion reads "1.2.3" or "v1.2.3". Anything else, including a
// pre-release suffix, is not a version MARSHAL manages.
func ParseVersion(s string) (Version, bool) {
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(s), "v"), ".")
	if len(parts) != 3 {
		return Version{}, false
	}
	var n [3]int
	for i, p := range parts {
		v, err := strconv.Atoi(p)
		if err != nil || v < 0 || (len(p) > 1 && p[0] == '0') {
			return Version{}, false
		}
		n[i] = v
	}
	return Version{n[0], n[1], n[2]}, true
}

func (v Version) String() string { return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch) }

// Less orders versions.
func (v Version) Less(o Version) bool {
	if v.Major != o.Major {
		return v.Major < o.Major
	}
	if v.Minor != o.Minor {
		return v.Minor < o.Minor
	}
	return v.Patch < o.Patch
}

// NextVersion is the version a release PR proposes: a patch above the
// last release, or 0.1.0 for the first, unless the operator already set a
// higher one on the release branch, which is how a minor or major bump is
// asked for.
func NextVersion(last Version, released bool, onBranch string) Version {
	next := Version{0, 1, 0}
	if released {
		next = Version{last.Major, last.Minor, last.Patch + 1}
	}
	if b, ok := ParseVersion(onBranch); ok && next.Less(b) {
		next = b
	}
	return next
}

// Change is one line of release notes.
type Change struct {
	Title string
	PR    string // "#12", when the commit is a pull request merge
}

// ParseLog reads `git log --format=%s%x1f%b%x1e` output. A pull request
// merge contributes its title (the body's first line) and number; the
// release merges themselves are left out.
func ParseLog(out string) []Change {
	var cs []Change
	for _, rec := range strings.Split(out, "\x1e") {
		rec = strings.TrimLeft(rec, "\n")
		if strings.TrimSpace(rec) == "" {
			continue
		}
		subject, body, _ := strings.Cut(rec, "\x1f")
		c := Change{Title: strings.TrimSpace(subject)}
		if rest, ok := strings.CutPrefix(c.Title, "Merge pull request #"); ok {
			num, from, _ := strings.Cut(rest, " ")
			c.PR = "#" + num
			c.Title = firstLine(strings.TrimSpace(body))
			if c.Title == "" {
				c.Title = strings.TrimPrefix(from, "from ")
			}
			if strings.Contains(from, ReleaseBranch) {
				continue
			}
		}
		if strings.HasPrefix(c.Title, "Release v") {
			continue
		}
		cs = append(cs, c)
	}
	return cs
}

// Notes renders a release's entries.
func Notes(v Version, date time.Time, cs []Change) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## v%s (%s)\n\n", v, date.Format("2006-01-02"))
	for i, c := range cs {
		if i == maxChangelog {
			fmt.Fprintf(&b, "- and %d more\n", len(cs)-maxChangelog)
			break
		}
		if c.PR != "" {
			fmt.Fprintf(&b, "- %s (%s)\n", c.Title, c.PR)
		} else {
			fmt.Fprintf(&b, "- %s\n", c.Title)
		}
	}
	return b.String()
}

// PrependNotes puts a release's notes at the top of a changelog, under its
// heading, creating the file's heading when there is none.
func PrependNotes(changelog, notes string) string {
	const head = "# Changelog\n\n"
	rest := strings.TrimLeft(changelog, "\n")
	if r, ok := strings.CutPrefix(rest, "# Changelog"); ok {
		rest = strings.TrimLeft(r, "\n")
	}
	if rest == "" {
		return head + notes
	}
	return head + notes + "\n" + rest
}

func (x *Executor) release(ctx context.Context, job config.Job, repo config.Repo, opts Options) (store.Run, error) {
	rec := &store.Run{ID: opts.ID, Job: job.Name, Repo: repo.Name, Class: job.Class, Adapter: "marshal", StartedAt: time.Now()}
	if err := x.Store.CreateRun(ctx, rec); err != nil {
		return store.Run{}, err
	}
	rec.Branch = ReleaseBranch
	logf := x.logger(ctx, rec.ID, opts.Log)
	finish := func(st store.Status, errText string) (store.Run, error) {
		rec.Status = st
		rec.Error = errText
		rec.FinishedAt = time.Now()
		return *rec, x.Store.UpdateRun(ctx, rec)
	}
	logf("note", "run %s: release on %s (deterministic, no agent)", rec.ID, repo.Name)

	ws, err := x.harnessClone(ctx, repo, job.Name, rec.ID, opts.Env)
	if err != nil {
		return finish(store.Failed, err.Error())
	}
	if !opts.Keep {
		defer ws.Remove()
	}
	git := func(args ...string) (string, error) { return ws.Git(ctx, args...) }
	if head, err := ws.HeadSHA(ctx); err == nil {
		rec.Head = head
	}

	if _, err := os.Stat(filepath.Join(ws.Dir, releaseWorkflow)); err != nil {
		logf("note", "no %s; a release PR would ship nothing", releaseWorkflow)
		return finish(store.Skipped, "release: no "+releaseWorkflow)
	}
	var last Version
	released := false
	tags, _ := git("tag", "--list", "v*", "--sort=-v:refname")
	lastTag := ""
	for _, t := range strings.Fields(tags) {
		if v, ok := ParseVersion(t); ok {
			last, released, lastTag = v, true, t
			break
		}
	}
	if b, err := os.ReadFile(filepath.Join(ws.Dir, "VERSION")); err == nil {
		if v, ok := ParseVersion(string(b)); ok && (!released || last.Less(v)) {
			// Merged but not yet tagged: the release workflow is running,
			// or failed. Either way a second PR would race it.
			logf("note", "VERSION on %s is %s, ahead of the last tag; waiting for the release workflow", ws.Repo.Branch, v)
			return finish(store.Skipped, "release: v"+v.String()+" is merged and not yet tagged")
		}
	}
	rng := ws.Base()
	if released {
		rng = lastTag + ".." + ws.Base()
	}
	log, err := git("log", "--first-parent", "--format=%s%x1f%b%x1e", rng)
	if err != nil {
		return finish(store.Failed, err.Error())
	}
	changes := ParseLog(log)
	if len(changes) == 0 {
		logf("note", "nothing merged since %s", lastTag)
		return finish(store.Skipped, "release: nothing since "+lastTag)
	}

	onBranch := ""
	if _, err := git("fetch", "--quiet", "origin", ReleaseBranch); err == nil {
		onBranch, _ = git("show", "FETCH_HEAD:VERSION")
	}
	next := NextVersion(last, released, onBranch)
	notes := Notes(next, time.Now(), changes)
	cl, _ := os.ReadFile(filepath.Join(ws.Dir, "CHANGELOG.md"))
	if err := os.WriteFile(filepath.Join(ws.Dir, "CHANGELOG.md"), []byte(PrependNotes(string(cl), notes)), 0o644); err != nil {
		return finish(store.Failed, err.Error())
	}
	if err := os.WriteFile(filepath.Join(ws.Dir, "VERSION"), []byte(next.String()+"\n"), 0o644); err != nil {
		return finish(store.Failed, err.Error())
	}
	title := "Release v" + next.String()
	if err := ws.Commit(ctx, title+"\n\n"+notes); err != nil {
		return finish(store.Failed, err.Error())
	}
	if onBranch != "" {
		// Same tree as the open PR's branch: nothing to update. The commit
		// itself always differs (time), so compare content.
		if _, err := git("diff", "--quiet", "FETCH_HEAD", "HEAD", "--"); err == nil {
			logf("note", "release PR already up to date at v%s", next)
			return finish(store.NoChange, "")
		}
	}
	rec.Summary = title + "\n\n" + releaseBody(next, changes, notes)
	if opts.NoPR {
		logf("note", "release committed locally as v%s (no PR requested)", next)
		return finish(store.Committed, "")
	}
	if err := ws.ForcePushTo(ctx, ReleaseBranch); err != nil {
		return finish(store.Failed, err.Error())
	}
	_, body, _ := strings.Cut(rec.Summary, "\n\n")
	url, err := ws.FindPR(ctx, ReleaseBranch)
	if err != nil {
		return finish(store.Failed, err.Error())
	}
	if url != "" {
		if err := ws.EditPR(ctx, url, title, body); err != nil {
			return finish(store.Failed, err.Error())
		}
		rec.PRURL = url
		// The open PR already has a proposal on record; this run is an
		// update to it, so the ladder counts the eventual merge once.
		if prev, err := x.Store.ListByStatus(ctx, store.Proposed); err == nil {
			for _, p := range prev {
				if p.PRURL == url {
					rec.Parent = p.ID
				}
			}
		}
		logf("note", "release PR updated to v%s: %s", next, url)
		return finish(store.Pushed, "")
	}
	ws.Branch = ReleaseBranch
	url, err = ws.OpenPR(ctx, title, body, []string{Label, Label + ":release"})
	if err != nil {
		return finish(store.Failed, err.Error())
	}
	rec.PRURL = url
	logf("note", "release PR opened for v%s: %s", next, url)
	return finish(store.Proposed, "")
}

func releaseBody(v Version, cs []Change, notes string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Merging this releases **v%s**: %d change(s) since the last release. ", v, len(cs))
	b.WriteString("The release workflow tags the version in `VERSION` and publishes it; nothing ships until you merge.\n\n")
	b.WriteString("For a minor or major release, edit `VERSION` on this branch. MARSHAL keeps a higher version you set when it updates the notes.\n\n")
	b.WriteString(strings.Replace(notes, "## ", "### ", 1))
	b.WriteString("\nPrepared by MARSHAL without an agent.\n")
	return b.String()
}
