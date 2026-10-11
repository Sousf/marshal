package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestNormaliseCollapsesVariableParts(t *testing.T) {
	a := Signature("r", "ci-fix", "run 3fa9c2e1: clone /tmp/marshal-123/x failed after 12.5s at 2026-10-11T14:00:00Z")
	b := Signature("r", "ci-fix", "run 77b0d4aa: clone /tmp/marshal-999/y failed after 3s at 2026-10-12T09:13:44Z\nmore log")
	if a != b {
		t.Errorf("same fault, different signatures:\n%s\n%s", Normalise("run 3fa9c2e1: clone /tmp/marshal-123/x failed after 12.5s"), Normalise("run 77b0d4aa: clone /tmp/marshal-999/y failed after 3s"))
	}
	if a == Signature("r", "ci-fix", "pre-flight: test: FAIL") || a == Signature("other", "ci-fix", "run 3fa9c2e1: clone /tmp/x failed after 1s at 2026-10-11T14:00:00Z") {
		t.Error("different faults or repos share a signature")
	}
}

func TestIncidentLifecycle(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	t0 := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	rec := func(at time.Time, run string) (Incident, bool) {
		i, n, err := st.RecordIncident(ctx, "r", "j", "boom in run "+run, run, at)
		if err != nil {
			t.Fatal(err)
		}
		return i, n
	}
	if _, n := rec(t0, "aaaaaaa1"); !n {
		t.Error("a new incident must notify")
	}
	if i, n := rec(t0.Add(time.Hour), "aaaaaaa2"); n || i.Count != 2 {
		t.Errorf("repeat within a day: notify=%v count=%d", n, i.Count)
	}
	i, n := rec(t0.Add(25*time.Hour), "aaaaaaa3")
	if !n {
		t.Error("an unacked incident reminds once a day")
	}
	if _, err := st.AckIncident(ctx, i.Signature[:6], t0.Add(26*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, n := rec(t0.Add(60*time.Hour), "aaaaaaa4"); n {
		t.Error("an acked incident stays quiet")
	}
	if n, _ := st.ResolveIncidents(ctx, "r", "j", t0.Add(61*time.Hour)); n != 1 {
		t.Errorf("resolved %d", n)
	}
	if open, _ := st.ListIncidents(ctx, false); len(open) != 0 {
		t.Errorf("open after resolve: %+v", open)
	}
	if i, n := rec(t0.Add(62*time.Hour), "aaaaaaa5"); !n || i.Count != 1 || !i.AckedAt.IsZero() {
		t.Errorf("a resolved incident that comes back is new: notify=%v %+v", n, i)
	}
	if _, err := st.AckIncident(ctx, "zz", t0); err == nil {
		t.Error("short prefix accepted")
	}
}

func TestQuotaHoldOnlyExtends(t *testing.T) {
	ctx := context.Background()
	st, _ := Open(filepath.Join(t.TempDir(), "m.db"))
	defer st.Close()
	t0 := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	st.SetQuotaHold(ctx, t0.Add(2*time.Hour), "five hour")
	st.SetQuotaHold(ctx, t0.Add(time.Hour), "429")
	if u, why := st.QuotaHold(ctx); !u.Equal(t0.Add(2*time.Hour)) || why != "five hour" {
		t.Errorf("hold shortened: %v %q", u, why)
	}
	if !st.ClaimQuotaNotice(ctx, t0.Add(2*time.Hour)) || st.ClaimQuotaNotice(ctx, t0.Add(2*time.Hour)) {
		t.Error("a hold is announced exactly once")
	}
	st.SetQuotaHold(ctx, time.Time{}, "")
	if u, _ := st.QuotaHold(ctx); !u.IsZero() {
		t.Error("clear did not clear")
	}
}
