package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestRunRoundTrip(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	r := &Run{Job: "posture-audit", Repo: "spotify-tui", Class: "hardening", Adapter: "claude-code", Model: "m"}
	if err := s.CreateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	if len(r.ID) != 8 {
		t.Fatalf("id %q", r.ID)
	}
	for i, k := range []string{"note", "tool", "text"} {
		if err := s.AppendEvent(ctx, r.ID, Event{Kind: k, Text: string(rune('a' + i))}); err != nil {
			t.Fatal(err)
		}
	}
	r.Status = Proposed
	r.FinishedAt = time.Now()
	r.Turns = 7
	r.CostUSD = 1.25
	r.PRURL = "https://github.com/x/y/pull/1"
	r.Summary = "did things"
	if err := s.UpdateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetRun(ctx, r.ID[:4])
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != Proposed || got.Turns != 7 || got.CostUSD != 1.25 || got.PRURL != r.PRURL || got.FinishedAt.IsZero() {
		t.Errorf("%+v", got)
	}
	ev, err := s.Events(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ev) != 3 || ev[0].Text != "a" || ev[2].Kind != "text" {
		t.Errorf("%+v", ev)
	}
	list, err := s.ListRuns(ctx, "spotify-tui", 0)
	if err != nil || len(list) != 1 {
		t.Fatalf("list %v %v", list, err)
	}
	if n, _ := s.CountSince(ctx, "spotify-tui", time.Now().Add(-time.Hour), Proposed, Merged); n != 1 {
		t.Errorf("count = %d", n)
	}
	if usd, _ := s.SpendSince(ctx, time.Now().Add(-time.Hour)); usd != 1.25 {
		t.Errorf("spend = %v", usd)
	}
	if _, err := s.GetRun(ctx, "zzzz"); err == nil {
		t.Error("want not found")
	}
	if err := s.UpdateRun(ctx, &Run{ID: "nope"}); err == nil {
		t.Error("update of unknown run should fail")
	}
}

func TestTrust(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if _, ok, err := s.GetTrust(ctx, "r", "c"); ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if err := s.PutTrust(ctx, Trust{Repo: "r", Class: "c", Level: "review", Streak: 2}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutTrust(ctx, Trust{Repo: "r", Class: "c", Level: "soak", Streak: 3, Approvals: 3}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.GetTrust(ctx, "r", "c")
	if err != nil || !ok || got.Level != "soak" || got.Streak != 3 {
		t.Errorf("%+v %v %v", got, ok, err)
	}
	all, _ := s.ListTrust(ctx)
	if len(all) != 1 {
		t.Errorf("%d rows", len(all))
	}
}
