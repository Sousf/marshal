package sched

import (
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	for in, want := range map[string]string{
		"": "manual", "manual": "manual", "Hourly": "hourly", "daily": "daily",
		"weekly": "weekly", "on-failure": "on-failure", "@every 6h": "@every 6h0m0s",
	} {
		s, err := Parse(in)
		if err != nil || s.String() != want {
			t.Errorf("Parse(%q) = %v, %v; want %s", in, s, err, want)
		}
	}
	for _, bad := range []string{"monthly", "@every 10s", "@every", "0 * * * *"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) should fail", bad)
		}
	}
}

func TestDue(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	daily, _ := Parse("daily")
	if !daily.Due(time.Time{}, now) {
		t.Error("never run should be due")
	}
	if daily.Due(now.Add(-23*time.Hour), now) {
		t.Error("23h ago should not be due")
	}
	if !daily.Due(now.Add(-25*time.Hour), now) {
		t.Error("25h ago should be due")
	}
	manual, _ := Parse("")
	if manual.Due(time.Time{}, now) || !manual.Manual() {
		t.Error("manual never fires")
	}
	onf, _ := Parse("on-failure")
	if onf.Due(time.Time{}, now) {
		t.Error("on-failure is not interval driven")
	}
	if !onf.Cooldown(time.Time{}, now) || onf.Cooldown(now.Add(-time.Hour), now) {
		t.Error("cooldown wrong")
	}
}
