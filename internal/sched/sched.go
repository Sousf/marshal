// Package sched parses the schedule field of a job and decides when it is
// due. Deliberately small: a personal fleet needs "daily" and "weekly", not
// cron, and a scheduler you can explain in one sentence is one you trust to
// run unattended.
package sched

import (
	"fmt"
	"strings"
	"time"
)

// Schedule is when a job runs.
type Schedule struct {
	// Every is the interval. Zero means the job is only run by hand.
	Every time.Duration
	// OnFailure means the job runs when the repo's default branch CI is red,
	// at most once per Every (default 24h) so a stubborn failure does not
	// burn the budget.
	OnFailure bool
}

// Parse accepts "", "manual", "hourly", "daily", "weekly", "on-failure",
// and "@every <duration>".
func Parse(s string) (Schedule, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	switch s {
	case "", "manual":
		return Schedule{}, nil
	case "hourly":
		return Schedule{Every: time.Hour}, nil
	case "daily":
		return Schedule{Every: 24 * time.Hour}, nil
	case "weekly":
		return Schedule{Every: 7 * 24 * time.Hour}, nil
	case "on-failure":
		return Schedule{OnFailure: true, Every: 24 * time.Hour}, nil
	}
	if rest, ok := strings.CutPrefix(s, "@every "); ok {
		d, err := time.ParseDuration(strings.TrimSpace(rest))
		if err != nil || d < time.Minute {
			return Schedule{}, fmt.Errorf("schedule %q: want a duration of at least 1m", s)
		}
		return Schedule{Every: d}, nil
	}
	return Schedule{}, fmt.Errorf("schedule %q: want manual, hourly, daily, weekly, on-failure, or @every <duration>", s)
}

// Manual reports whether the schedule never fires on its own.
func (s Schedule) Manual() bool { return s.Every == 0 && !s.OnFailure }

// Due reports whether an interval schedule should fire now, given when it
// last started. A zero last means it has never run. OnFailure schedules are
// not interval-driven and always return false here; the caller checks CI
// and uses Cooldown.
func (s Schedule) Due(last, now time.Time) bool {
	if s.Every == 0 || s.OnFailure {
		return false
	}
	return last.IsZero() || now.Sub(last) >= s.Every
}

// Cooldown reports whether enough time has passed since the last run for an
// on-failure job to fire again.
func (s Schedule) Cooldown(last, now time.Time) bool {
	return last.IsZero() || now.Sub(last) >= s.Every
}

func (s Schedule) String() string {
	switch {
	case s.OnFailure:
		return "on-failure"
	case s.Every == 0:
		return "manual"
	case s.Every == time.Hour:
		return "hourly"
	case s.Every == 24*time.Hour:
		return "daily"
	case s.Every == 7*24*time.Hour:
		return "weekly"
	}
	return "@every " + s.Every.String()
}
