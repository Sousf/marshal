package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Incident is a failure that keeps happening: one row per signature (repo,
// job and the error with its variable parts stripped), however many runs
// hit it. The operator hears about it once, then once a day until they ack
// it, and never again after the job next succeeds unless it comes back.
type Incident struct {
	Signature  string
	Repo       string
	Job        string
	Sample     string // the normalised error, which is what the signature hashes
	Count      int
	FirstAt    time.Time
	LastAt     time.Time
	LastRun    string
	NotifiedAt time.Time
	AckedAt    time.Time // zero when not acked
	ResolvedAt time.Time // zero while open
}

// Open reports whether the incident is still happening.
func (i Incident) Open() bool { return i.ResolvedAt.IsZero() }

// RemindAfter is how long an unacked incident stays quiet between
// reminders.
const RemindAfter = 24 * time.Hour

// keepResolved bounds the table: resolved incidents older than this go.
const keepResolved = 30 * 24 * time.Hour

const incidentSchema = `
CREATE TABLE IF NOT EXISTS incidents (
	signature TEXT PRIMARY KEY,
	repo TEXT NOT NULL,
	job TEXT NOT NULL,
	sample TEXT NOT NULL,
	count INTEGER NOT NULL,
	first_at INTEGER NOT NULL,
	last_at INTEGER NOT NULL,
	last_run TEXT NOT NULL,
	notified_at INTEGER NOT NULL DEFAULT 0,
	acked_at INTEGER NOT NULL DEFAULT 0,
	resolved_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS incidents_repo_job ON incidents(repo, job, resolved_at);
`

var (
	// Order matters: a UUID or a long hex run would otherwise be half
	// eaten by the number rule.
	reUUID   = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	reHex    = regexp.MustCompile(`\b[0-9a-f]{7,64}\b`)
	reTmp    = regexp.MustCompile(`/(?:tmp|var/folders|private/var)/\S*`)
	reTime   = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}(?::\d{2})?(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})?`)
	reNumber = regexp.MustCompile(`\d+(?:\.\d+)?`)
	reSpace  = regexp.MustCompile(`\s+`)
)

// Normalise strips what differs between two runs hitting the same fault:
// run ids, commit shas, temp paths, timestamps, counts and durations. It
// keeps the first line, which is the failure; the rest is usually log.
func Normalise(errText string) string {
	s := strings.TrimSpace(errText)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = reUUID.ReplaceAllString(s, "<id>")
	s = reTime.ReplaceAllString(s, "<time>")
	s = reTmp.ReplaceAllString(s, "<tmp>")
	s = reHex.ReplaceAllString(s, "<sha>")
	s = reNumber.ReplaceAllString(s, "<n>")
	s = strings.ToLower(reSpace.ReplaceAllString(s, " "))
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// Signature is the dedup key for a failure.
func Signature(repo, job, errText string) string {
	h := sha256.Sum256([]byte(repo + "\x00" + job + "\x00" + Normalise(errText)))
	return hex.EncodeToString(h[:6])
}

// RecordIncident counts a failure against its signature and reports whether
// the operator should be told: on a new incident, on one that came back
// after it resolved, and as a daily reminder while it stays unacked.
func (s *Store) RecordIncident(ctx context.Context, repo, job, errText, runID string, now time.Time) (Incident, bool, error) {
	sig := Signature(repo, job, errText)
	inc, found, err := s.incident(ctx, sig)
	if err != nil {
		return Incident{}, false, err
	}
	if !found || !inc.Open() {
		inc = Incident{Signature: sig, Repo: repo, Job: job, Sample: Normalise(errText),
			Count: 1, FirstAt: now, LastAt: now, LastRun: runID, NotifiedAt: now}
		_, err := s.db.ExecContext(ctx, `INSERT INTO incidents
			(signature, repo, job, sample, count, first_at, last_at, last_run, notified_at, acked_at, resolved_at)
			VALUES (?,?,?,?,?,?,?,?,?,0,0)
			ON CONFLICT(signature) DO UPDATE SET sample=excluded.sample, count=1,
				first_at=excluded.first_at, last_at=excluded.last_at, last_run=excluded.last_run,
				notified_at=excluded.notified_at, acked_at=0, resolved_at=0`,
			sig, repo, job, inc.Sample, 1, now.Unix(), now.Unix(), runID, now.Unix())
		return inc, err == nil, err
	}
	inc.Count++
	inc.LastAt = now
	inc.LastRun = runID
	notify := inc.AckedAt.IsZero() && now.Sub(inc.NotifiedAt) >= RemindAfter
	if notify {
		inc.NotifiedAt = now
	}
	_, err = s.db.ExecContext(ctx, `UPDATE incidents SET count=?, last_at=?, last_run=?, notified_at=? WHERE signature=?`,
		inc.Count, now.Unix(), runID, inc.NotifiedAt.Unix(), sig)
	return inc, notify && err == nil, err
}

// ResolveIncidents closes every open incident for a repo and job, which is
// what a success means, and prunes resolved incidents past their keep.
func (s *Store) ResolveIncidents(ctx context.Context, repo, job string, now time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE incidents SET resolved_at=? WHERE repo=? AND job=? AND resolved_at=0`, now.Unix(), repo, job)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	_, err = s.db.ExecContext(ctx, `DELETE FROM incidents WHERE resolved_at>0 AND resolved_at<?`, now.Add(-keepResolved).Unix())
	return int(n), err
}

// AckIncident silences an open incident's reminders. prefix may be any
// unambiguous start of the signature.
func (s *Store) AckIncident(ctx context.Context, prefix string, now time.Time) (Incident, error) {
	if len(prefix) < 4 || strings.ContainsAny(prefix, "%_") {
		return Incident{}, errors.New("give at least four characters of the signature")
	}
	open, err := s.ListIncidents(ctx, false)
	if err != nil {
		return Incident{}, err
	}
	var hit []Incident
	for _, i := range open {
		if strings.HasPrefix(i.Signature, prefix) {
			hit = append(hit, i)
		}
	}
	switch len(hit) {
	case 0:
		return Incident{}, fmt.Errorf("no open incident %q", prefix)
	case 1:
	default:
		return Incident{}, fmt.Errorf("%q matches %d incidents", prefix, len(hit))
	}
	inc := hit[0]
	inc.AckedAt = now
	_, err = s.db.ExecContext(ctx, `UPDATE incidents SET acked_at=? WHERE signature=?`, now.Unix(), inc.Signature)
	return inc, err
}

// ListIncidents returns open incidents, newest first; all includes the
// resolved ones still kept.
func (s *Store) ListIncidents(ctx context.Context, all bool) ([]Incident, error) {
	q := `SELECT ` + incidentCols + ` FROM incidents`
	if !all {
		q += ` WHERE resolved_at=0`
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY last_at DESC LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Incident
	for rows.Next() {
		i, err := scanIncident(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

const incidentCols = `signature, repo, job, sample, count, first_at, last_at, last_run, notified_at, acked_at, resolved_at`

func (s *Store) incident(ctx context.Context, sig string) (Incident, bool, error) {
	i, err := scanIncident(s.db.QueryRowContext(ctx, `SELECT `+incidentCols+` FROM incidents WHERE signature=?`, sig))
	if errors.Is(err, sql.ErrNoRows) {
		return Incident{}, false, nil
	}
	return i, err == nil, err
}

func scanIncident(r interface{ Scan(...any) error }) (Incident, error) {
	var i Incident
	var first, last, notified, acked, resolved int64
	err := r.Scan(&i.Signature, &i.Repo, &i.Job, &i.Sample, &i.Count, &first, &last, &i.LastRun, &notified, &acked, &resolved)
	i.FirstAt, i.LastAt, i.NotifiedAt = time.Unix(first, 0), time.Unix(last, 0), time.Unix(notified, 0)
	if acked > 0 {
		i.AckedAt = time.Unix(acked, 0)
	}
	if resolved > 0 {
		i.ResolvedAt = time.Unix(resolved, 0)
	}
	return i, err
}

// Keys for the quota hold, which is account-wide and so fleet-wide.
const (
	quotaUntilKey    = "quota_until"
	quotaWhyKey      = "quota_why"
	quotaNotifiedKey = "quota_notified"
)

// QuotaHold returns the current hold and its reason; zero when none was
// ever set. A hold in the past is simply over.
func (s *Store) QuotaHold(ctx context.Context) (time.Time, string) {
	v, _ := s.Setting(ctx, quotaUntilKey, "")
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, ""
	}
	why, _ := s.Setting(ctx, quotaWhyKey, "")
	return t, why
}

// SetQuotaHold records a hold. It only ever extends one in force: two runs
// stopping at once must not shorten each other's window. A zero until
// clears it, which is the operator's override.
func (s *Store) SetQuotaHold(ctx context.Context, until time.Time, why string) error {
	if until.IsZero() {
		if err := s.SetSetting(ctx, quotaUntilKey, ""); err != nil {
			return err
		}
		return s.SetSetting(ctx, quotaWhyKey, "")
	}
	if cur, _ := s.QuotaHold(ctx); cur.After(until) {
		return nil
	}
	if err := s.SetSetting(ctx, quotaUntilKey, until.UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	return s.SetSetting(ctx, quotaWhyKey, why)
}

// ClaimQuotaNotice reports whether this hold has not been announced yet,
// and marks it announced.
func (s *Store) ClaimQuotaNotice(ctx context.Context, until time.Time) bool {
	key := until.UTC().Format(time.RFC3339)
	if v, _ := s.Setting(ctx, quotaNotifiedKey, ""); v == key {
		return false
	}
	return s.SetSetting(ctx, quotaNotifiedKey, key) == nil
}
