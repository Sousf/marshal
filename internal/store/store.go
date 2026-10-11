// Package store persists runs, their event logs, and the trust ladder in one
// SQLite file.
//
// Pure Go driver, so the binary cross-compiles for hermes1 without a C
// toolchain. One writer at a time is plenty: runs are serialised per repo and
// the TUI only reads.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Status of a run.
type Status string

const (
	Running   Status = "running"
	NoChange  Status = "no-change" // agent finished, nothing to propose
	Committed Status = "committed" // committed on a local branch, no PR asked for
	Pushed    Status = "pushed"    // follow-up pushed to an existing PR
	Proposed  Status = "proposed"  // PR opened, awaiting review or soak
	Merged    Status = "merged"
	Rejected  Status = "rejected" // operator closed the PR
	Blocked   Status = "blocked"  // policy refused the diff
	Failed    Status = "failed"   // agent or harness error
	Cancelled Status = "cancelled"
)

// Run is one execution of a job against a repo.
type Run struct {
	ID         string
	Job        string
	Repo       string
	Class      string
	Adapter    string
	Model      string
	Status     Status
	StartedAt  time.Time
	FinishedAt time.Time
	Turns      int
	CostUSD    float64
	Branch     string
	PRURL      string
	// MergedBy records who merged a Proposed run: "human", "auto" or "soak".
	// Only human merges count as approvals on the trust ladder.
	MergedBy string
	// Parent is the run this one followed up, for a fix pushed to an
	// existing PR. BaseHead is the PR head the follow-up started from, so
	// the same failing head is never attempted twice.
	Parent   string
	BaseHead string
	// Feedback is what humans said on the PR, captured when it closed, and
	// fed into later briefs for the same job.
	Feedback string
	// Summary is the agent's final report; the PR body.
	Summary string
	// Error explains Failed or Blocked.
	Error string
}

// Event is one logged thing during a run.
type Event struct {
	Seq  int64
	At   time.Time
	Kind string
	Text string
}

// Verdict is what the verifier said about one head of a proposal.
type Verdict struct {
	RunID string
	// Head is the commit the verdict is about. A new push invalidates it.
	Head string
	// Verdict is PASS, FAIL or UNSURE. Only PASS allows an autonomous merge.
	Verdict string
	Summary string
	// Findings is the verifier's structured findings as JSON, kept verbatim.
	Findings string
	Turns    int
	CostUSD  float64
	At       time.Time
}

// Trust is where a class sits on the ladder for one repo.
type Trust struct {
	Repo, Class string
	Level       string
	// Streak counts consecutive approvals since the last demotion.
	Streak     int
	Approvals  int
	Rejections int
	Reverts    int
	UpdatedAt  time.Time
}

// Store is an open database.
type Store struct {
	db *sql.DB
}

// Open opens or creates the database at path, applying the schema.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(wal)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

const schema = `
CREATE TABLE IF NOT EXISTS runs (
	id TEXT PRIMARY KEY,
	job TEXT NOT NULL,
	repo TEXT NOT NULL,
	class TEXT NOT NULL DEFAULT '',
	adapter TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL,
	started_at INTEGER NOT NULL,
	finished_at INTEGER,
	turns INTEGER NOT NULL DEFAULT 0,
	cost_usd REAL NOT NULL DEFAULT 0,
	branch TEXT NOT NULL DEFAULT '',
	pr_url TEXT NOT NULL DEFAULT '',
	summary TEXT NOT NULL DEFAULT '',
	error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS runs_repo ON runs(repo, started_at);
CREATE TABLE IF NOT EXISTS verdicts (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
	head TEXT NOT NULL,
	verdict TEXT NOT NULL,
	summary TEXT NOT NULL DEFAULT '',
	findings TEXT NOT NULL DEFAULT '',
	turns INTEGER NOT NULL DEFAULT 0,
	cost_usd REAL NOT NULL DEFAULT 0,
	at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS verdicts_run ON verdicts(run_id, id);
CREATE TABLE IF NOT EXISTS settings (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS events (
	seq INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
	at INTEGER NOT NULL,
	kind TEXT NOT NULL,
	text TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS events_run ON events(run_id, seq);
CREATE TABLE IF NOT EXISTS trust (
	repo TEXT NOT NULL,
	class TEXT NOT NULL,
	level TEXT NOT NULL,
	streak INTEGER NOT NULL DEFAULT 0,
	approvals INTEGER NOT NULL DEFAULT 0,
	rejections INTEGER NOT NULL DEFAULT 0,
	reverts INTEGER NOT NULL DEFAULT 0,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY (repo, class)
);
`

func (s *Store) migrate() error {
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}
	// Columns added after the first release. SQLite has no ADD COLUMN IF NOT
	// EXISTS, so the error for an existing column is the expected outcome.
	for _, stmt := range []string{
		`ALTER TABLE runs ADD COLUMN merged_by TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE runs ADD COLUMN parent TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE runs ADD COLUMN base_head TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE runs ADD COLUMN feedback TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := s.db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return err
		}
	}
	return nil
}

// NewID returns a short random identifier: 8 hex characters is enough for
// one operator's fleet and short enough to type.
func NewID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// CreateRun records the start of a run. r.ID is assigned if empty.
func (s *Store) CreateRun(ctx context.Context, r *Run) error {
	if r.ID == "" {
		r.ID = NewID()
	}
	if r.StartedAt.IsZero() {
		r.StartedAt = time.Now()
	}
	if r.Status == "" {
		r.Status = Running
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO runs
		(id, job, repo, class, adapter, model, status, started_at, branch, parent, base_head)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.Job, r.Repo, r.Class, r.Adapter, r.Model, r.Status, r.StartedAt.Unix(), r.Branch, r.Parent, r.BaseHead)
	return err
}

// UpdateRun writes every mutable field of r.
func (s *Store) UpdateRun(ctx context.Context, r *Run) error {
	var fin any
	if !r.FinishedAt.IsZero() {
		fin = r.FinishedAt.Unix()
	}
	res, err := s.db.ExecContext(ctx, `UPDATE runs SET status=?, finished_at=?, turns=?,
		cost_usd=?, branch=?, pr_url=?, summary=?, error=?, merged_by=?, feedback=? WHERE id=?`,
		r.Status, fin, r.Turns, r.CostUSD, r.Branch, r.PRURL, r.Summary, r.Error, r.MergedBy, r.Feedback, r.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("run %s: not found", r.ID)
	}
	return nil
}

// AppendEvent logs one event against a run.
func (s *Store) AppendEvent(ctx context.Context, runID string, e Event) error {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO events (run_id, at, kind, text) VALUES (?,?,?,?)`,
		runID, e.At.UnixMilli(), e.Kind, e.Text)
	return err
}

// ErrNotFound is returned for an unknown run.
var ErrNotFound = errors.New("not found")

const runCols = `id, job, repo, class, adapter, model, status, started_at, finished_at,
	turns, cost_usd, branch, pr_url, summary, error, merged_by, parent, base_head, feedback`

func scanRun(sc interface{ Scan(...any) error }) (Run, error) {
	var r Run
	var started int64
	var finished sql.NullInt64
	err := sc.Scan(&r.ID, &r.Job, &r.Repo, &r.Class, &r.Adapter, &r.Model, &r.Status,
		&started, &finished, &r.Turns, &r.CostUSD, &r.Branch, &r.PRURL, &r.Summary, &r.Error, &r.MergedBy, &r.Parent, &r.BaseHead, &r.Feedback)
	if err != nil {
		return r, err
	}
	r.StartedAt = time.Unix(started, 0)
	if finished.Valid {
		r.FinishedAt = time.Unix(finished.Int64, 0)
	}
	return r, nil
}

// GetRun returns one run. A unique prefix of the ID is accepted.
func (s *Store) GetRun(ctx context.Context, id string) (Run, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+runCols+` FROM runs WHERE id LIKE ? || '%' LIMIT 2`, id)
	if err != nil {
		return Run{}, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return Run{}, err
		}
		out = append(out, r)
	}
	switch len(out) {
	case 0:
		return Run{}, fmt.Errorf("run %q: %w", id, ErrNotFound)
	case 1:
		return out[0], rows.Err()
	}
	return Run{}, fmt.Errorf("run %q: ambiguous prefix", id)
}

// ListRuns returns the most recent runs, newest first. repo filters when
// non-empty; limit 0 means 50.
func (s *Store) ListRuns(ctx context.Context, repo string, limit int) ([]Run, error) {
	if limit <= 0 {
		limit = 50
	}
	q := `SELECT ` + runCols + ` FROM runs`
	var args []any
	if repo != "" {
		q += ` WHERE repo = ?`
		args = append(args, repo)
	}
	q += ` ORDER BY started_at DESC, rowid DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListByStatus returns every run in one of the given statuses, oldest first.
func (s *Store) ListByStatus(ctx context.Context, statuses ...Status) ([]Run, error) {
	if len(statuses) == 0 {
		return nil, nil
	}
	q := `SELECT ` + runCols + ` FROM runs WHERE status IN (`
	var args []any
	for i, st := range statuses {
		if i > 0 {
			q += ","
		}
		q += "?"
		args = append(args, st)
	}
	q += `) ORDER BY started_at`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListByRepoJob returns the latest runs of one job on one repo, newest
// first, excluding follow-ups.
func (s *Store) ListByRepoJob(ctx context.Context, repo, job string, limit int) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+runCols+` FROM runs WHERE repo=? AND job=? AND parent=''
		ORDER BY started_at DESC, rowid DESC LIMIT ?`, repo, job, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Followups returns the follow-up runs of a parent, oldest first.
func (s *Store) Followups(ctx context.Context, parent string) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+runCols+` FROM runs WHERE parent=? ORDER BY started_at, rowid`, parent)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LastStarted returns when a job last started against a repo, or zero.
func (s *Store) LastStarted(ctx context.Context, job, repo string) (time.Time, error) {
	var ts sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT MAX(started_at) FROM runs WHERE job=? AND repo=?`, job, repo).Scan(&ts)
	if err != nil || !ts.Valid {
		return time.Time{}, err
	}
	return time.Unix(ts.Int64, 0), nil
}

// CountMergedBy counts runs on a repo merged by a given actor after t.
func (s *Store) CountMergedBy(ctx context.Context, repo, by string, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE repo=? AND merged_by=? AND finished_at>=?`,
		repo, by, since.Unix()).Scan(&n)
	return n, err
}

// EventsAfter returns a run's events with seq greater than after, for
// tailing a live run.
func (s *Store) EventsAfter(ctx context.Context, runID string, after int64) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq, at, kind, text FROM events WHERE run_id=? AND seq>? ORDER BY seq`, runID, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var at int64
		if err := rows.Scan(&e.Seq, &at, &e.Kind, &e.Text); err != nil {
			return nil, err
		}
		e.At = time.UnixMilli(at)
		out = append(out, e)
	}
	return out, rows.Err()
}

// PutVerdict records a verdict.
func (s *Store) PutVerdict(ctx context.Context, v Verdict) error {
	if v.At.IsZero() {
		v.At = time.Now()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO verdicts (run_id, head, verdict, summary, findings, turns, cost_usd, at)
		VALUES (?,?,?,?,?,?,?,?)`, v.RunID, v.Head, v.Verdict, v.Summary, v.Findings, v.Turns, v.CostUSD, v.At.Unix())
	return err
}

// LatestVerdict returns the most recent verdict for a run, and whether one
// exists.
func (s *Store) LatestVerdict(ctx context.Context, runID string) (Verdict, bool, error) {
	var v Verdict
	var at int64
	err := s.db.QueryRowContext(ctx, `SELECT run_id, head, verdict, summary, findings, turns, cost_usd, at
		FROM verdicts WHERE run_id=? ORDER BY id DESC LIMIT 1`, runID).
		Scan(&v.RunID, &v.Head, &v.Verdict, &v.Summary, &v.Findings, &v.Turns, &v.CostUSD, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return Verdict{}, false, nil
	}
	if err != nil {
		return Verdict{}, false, err
	}
	v.At = time.Unix(at, 0)
	return v, true, nil
}

// VerdictSpendSince totals verifier cost after t, for the daily cap.
func (s *Store) VerdictSpendSince(ctx context.Context, since time.Time) (float64, error) {
	var usd sql.NullFloat64
	err := s.db.QueryRowContext(ctx, `SELECT SUM(cost_usd) FROM verdicts WHERE at>=?`, since.Unix()).Scan(&usd)
	return usd.Float64, err
}

// Setting reads a key from the settings table; def when unset.
func (s *Store) Setting(ctx context.Context, key, def string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return def, nil
	}
	return v, err
}

// SetSetting writes a key.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// Events returns a run's log in order.
func (s *Store) Events(ctx context.Context, runID string) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq, at, kind, text FROM events WHERE run_id=? ORDER BY seq`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var at int64
		if err := rows.Scan(&e.Seq, &at, &e.Kind, &e.Text); err != nil {
			return nil, err
		}
		e.At = time.UnixMilli(at)
		out = append(out, e)
	}
	return out, rows.Err()
}

// CountSince counts runs for a repo that reached any of the given statuses
// after t. It backs the per-day caps.
func (s *Store) CountSince(ctx context.Context, repo string, since time.Time, statuses ...Status) (int, error) {
	q := `SELECT COUNT(*) FROM runs WHERE repo=? AND started_at>=?`
	args := []any{repo, since.Unix()}
	if len(statuses) > 0 {
		q += ` AND status IN (`
		for i, st := range statuses {
			if i > 0 {
				q += ","
			}
			q += "?"
			args = append(args, st)
		}
		q += `)`
	}
	var n int
	err := s.db.QueryRowContext(ctx, q, args...).Scan(&n)
	return n, err
}

// SpendSince totals cost across the fleet after t.
func (s *Store) SpendSince(ctx context.Context, since time.Time) (float64, error) {
	var usd sql.NullFloat64
	err := s.db.QueryRowContext(ctx, `SELECT SUM(cost_usd) FROM runs WHERE started_at>=?`, since.Unix()).Scan(&usd)
	return usd.Float64, err
}

// GetTrust returns the ladder position for a repo and class, or ok=false if
// the pair has never been recorded.
func (s *Store) GetTrust(ctx context.Context, repo, class string) (Trust, bool, error) {
	var t Trust
	var upd int64
	err := s.db.QueryRowContext(ctx, `SELECT repo, class, level, streak, approvals, rejections, reverts, updated_at
		FROM trust WHERE repo=? AND class=?`, repo, class).
		Scan(&t.Repo, &t.Class, &t.Level, &t.Streak, &t.Approvals, &t.Rejections, &t.Reverts, &upd)
	if errors.Is(err, sql.ErrNoRows) {
		return Trust{}, false, nil
	}
	if err != nil {
		return Trust{}, false, err
	}
	t.UpdatedAt = time.Unix(upd, 0)
	return t, true, nil
}

// PutTrust writes a ladder position.
func (s *Store) PutTrust(ctx context.Context, t Trust) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO trust (repo, class, level, streak, approvals, rejections, reverts, updated_at)
		VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT(repo, class) DO UPDATE SET level=excluded.level, streak=excluded.streak,
		approvals=excluded.approvals, rejections=excluded.rejections, reverts=excluded.reverts,
		updated_at=excluded.updated_at`,
		t.Repo, t.Class, t.Level, t.Streak, t.Approvals, t.Rejections, t.Reverts, time.Now().Unix())
	return err
}

// ListTrust returns every recorded ladder position.
func (s *Store) ListTrust(ctx context.Context) ([]Trust, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT repo, class, level, streak, approvals, rejections, reverts, updated_at FROM trust ORDER BY repo, class`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Trust
	for rows.Next() {
		var t Trust
		var upd int64
		if err := rows.Scan(&t.Repo, &t.Class, &t.Level, &t.Streak, &t.Approvals, &t.Rejections, &t.Reverts, &upd); err != nil {
			return nil, err
		}
		t.UpdatedAt = time.Unix(upd, 0)
		out = append(out, t)
	}
	return out, rows.Err()
}
