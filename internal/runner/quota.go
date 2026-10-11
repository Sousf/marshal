package runner

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Limit is a quota stop: the account behind the agent hit a usage or rate
// limit. Running again before Until only burns a process start and fails
// the same way, and on a subscription it eats the operator's own window.
type Limit struct {
	// Until is when the limit lifts. Zero when the harness did not say.
	Until time.Time
	// Text is the harness's own words, clipped.
	Text string
	// Held is set when the guard refused to start a run at all.
	Held bool
}

func (l *Limit) Error() string {
	when := "unknown"
	if !l.Until.IsZero() {
		when = l.Until.Local().Format("Mon 15:04")
	}
	if l.Held {
		return "quota: held until " + when + "; no agent started"
	}
	return "quota: " + l.Text + " (until " + when + ")"
}

// limitText matches what the harness says when the account is out of
// quota. Checked only against the harness's own error text, never the
// agent's prose, so a report that discusses rate limiting does not park the
// fleet. Overload (529) is transient and deliberately absent.
var limitText = regexp.MustCompile(`(?i)usage limit|hit your (usage )?limit|limit reached|rate[ _-]?limit|\b429\b|too many requests`)

// resetsEpoch is the older "Claude AI usage limit reached|1760000000" form.
var resetsEpoch = regexp.MustCompile(`limit reached\|(\d{9,11})`)

// resetsClock is the newer "resets 3pm" or "resets 7:30pm (Europe/London)".
var resetsClock = regexp.MustCompile(`(?i)resets\s+(?:at\s+)?(\d{1,2})(?::(\d{2}))?\s*(am|pm)(?:\s*\(([^)]+)\))?`)

// DetectLimit reads a harness error for a quota stop. It returns nil when
// the text is some other failure.
func DetectLimit(text string, now time.Time) *Limit {
	if !limitText.MatchString(text) {
		return nil
	}
	l := &Limit{Text: clipLine(text, 200)}
	if m := resetsEpoch.FindStringSubmatch(text); m != nil {
		if sec, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			l.Until = time.Unix(sec, 0)
		}
	} else if m := resetsClock.FindStringSubmatch(text); m != nil {
		loc := now.Location()
		if m[4] != "" {
			if z, err := time.LoadLocation(m[4]); err == nil {
				loc = z
			}
		}
		h, _ := strconv.Atoi(m[1])
		min, _ := strconv.Atoi(m[2])
		h %= 12
		if strings.EqualFold(m[3], "pm") {
			h += 12
		}
		n := now.In(loc)
		t := time.Date(n.Year(), n.Month(), n.Day(), h, min, 0, 0, loc)
		if !t.After(n) {
			t = t.AddDate(0, 0, 1)
		}
		l.Until = t
	}
	return l
}

func clipLine(s string, n int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > n {
		s = s[:n-3] + "..."
	}
	return s
}

// Guard parks every agent run while a quota hold is in force, and starts
// one when a run reports a quota stop. It wraps the adapter once, so jobs,
// follow-ups, tasks and the verifier all stop together: the quota belongs
// to the account, not to a job.
type Guard struct {
	Adapter
	// HeldUntil returns the current hold; zero or past means none.
	HeldUntil func(context.Context) time.Time
	// Hold records a hold. It is only called with a time in the future.
	Hold func(ctx context.Context, until time.Time, why string)
	// Now is for tests.
	Now func() time.Time
}

// DefaultHold is how long a quota stop parks the fleet when the harness
// does not say when the limit lifts.
const DefaultHold = 30 * time.Minute

// maxHold bounds a hold whatever the harness claims, so a garbled reset
// time cannot park the fleet for a month. A weekly limit fits inside it.
const maxHold = 8 * 24 * time.Hour

func (g Guard) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

// Run implements Adapter.
func (g Guard) Run(ctx context.Context, req Request, emit func(Event)) (Result, error) {
	now := g.now()
	if g.HeldUntil != nil {
		if u := g.HeldUntil(ctx); u.After(now) {
			return Result{IsError: true}, &Limit{Until: u, Held: true}
		}
	}
	res, err := g.Adapter.Run(ctx, req, emit)
	var l *Limit
	if errors.As(err, &l) && !l.Held && g.Hold != nil {
		until := l.Until
		if !until.After(now) {
			until = now.Add(DefaultHold)
		}
		if until.Sub(now) > maxHold {
			until = now.Add(maxHold)
		}
		emit(Event{At: now, Kind: Note, Text: fmt.Sprintf("quota stop; holding every agent run until %s", until.Local().Format("Mon 15:04"))})
		g.Hold(ctx, until, l.Text)
	}
	return res, err
}
