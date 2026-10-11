package runner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDetectLimit(t *testing.T) {
	now := time.Date(2026, 10, 11, 14, 0, 0, 0, time.UTC)
	cases := []struct {
		text  string
		limit bool
		until time.Time
	}{
		{"Claude AI usage limit reached|1760200000", true, time.Unix(1760200000, 0)},
		{"You've hit your limit · resets 3pm", true, time.Date(2026, 10, 11, 15, 0, 0, 0, time.UTC)},
		{"5-hour limit reached ∙ resets 9:30am", true, time.Date(2026, 10, 12, 9, 30, 0, 0, time.UTC)},
		{"API Error: 429 rate_limit_error", true, time.Time{}},
		{"API Error: 529 overloaded_error", false, time.Time{}},
		{"exit status 1: tests failed", false, time.Time{}},
	}
	for _, c := range cases {
		l := DetectLimit(c.text, now)
		if (l != nil) != c.limit {
			t.Errorf("%q: limit=%v", c.text, l != nil)
			continue
		}
		if l != nil && !l.Until.Equal(c.until) {
			t.Errorf("%q: until=%v want %v", c.text, l.Until, c.until)
		}
	}
}

func TestParseRateLimitEvent(t *testing.T) {
	stream := `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","resetsAt":1760200000}}
{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1760200000,"rateLimitType":"five_hour"}}
{"type":"result","subtype":"success","is_error":true,"result":"limit"}
`
	res, err := ParseStream(strings.NewReader(stream), func(Event) {})
	if err != nil || res.Limit == nil || !res.Limit.Until.Equal(time.Unix(1760200000, 0)) {
		t.Fatalf("res=%+v err=%v", res.Limit, err)
	}
}

type limited struct {
	calls int
	err   error
}

func (*limited) Name() string    { return "limited" }
func (*limited) Available() bool { return true }
func (l *limited) Run(context.Context, Request, func(Event)) (Result, error) {
	l.calls++
	return Result{IsError: true}, l.err
}

func TestGuardHoldsAndParks(t *testing.T) {
	now := time.Date(2026, 10, 11, 14, 0, 0, 0, time.UTC)
	var held time.Time
	inner := &limited{err: &Limit{Text: "usage limit"}}
	g := Guard{
		Adapter:   inner,
		HeldUntil: func(context.Context) time.Time { return held },
		Hold:      func(_ context.Context, u time.Time, _ string) { held = u },
		Now:       func() time.Time { return now },
	}
	ctx := context.Background()
	_, err := g.Run(ctx, Request{}, func(Event) {})
	var l *Limit
	if !errors.As(err, &l) || !held.Equal(now.Add(DefaultHold)) {
		t.Fatalf("err=%v held=%v", err, held)
	}
	// While held, nothing starts.
	_, err = g.Run(ctx, Request{}, func(Event) {})
	if !errors.As(err, &l) || !l.Held || inner.calls != 1 {
		t.Fatalf("held run: err=%v calls=%d", err, inner.calls)
	}
	// A claimed reset a month out is clamped.
	held = time.Time{}
	inner.err = &Limit{Text: "usage limit", Until: now.AddDate(0, 1, 0)}
	g.Run(ctx, Request{}, func(Event) {})
	if held.Sub(now) > maxHold {
		t.Errorf("hold not clamped: %v", held)
	}
	// Ordinary failures never hold.
	held = time.Time{}
	inner.err = errors.New("exit status 1")
	g.Run(ctx, Request{}, func(Event) {})
	if !held.IsZero() {
		t.Error("an ordinary failure started a hold")
	}
}
