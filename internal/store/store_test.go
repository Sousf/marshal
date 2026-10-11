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

func TestStatusQueriesAndSettings(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	a := &Run{Job: "j", Repo: "r", Status: Proposed}
	b := &Run{Job: "j", Repo: "r", Status: Failed}
	s.CreateRun(ctx, a)
	s.CreateRun(ctx, b)
	list, err := s.ListByStatus(ctx, Proposed, Merged)
	if err != nil || len(list) != 1 || list[0].ID != a.ID {
		t.Errorf("%v %v", list, err)
	}
	last, _ := s.LastStarted(ctx, "j", "r")
	if last.IsZero() {
		t.Error("last started should be set")
	}
	if never, _ := s.LastStarted(ctx, "j", "other"); !never.IsZero() {
		t.Error("unknown pair should be zero")
	}
	a.Status = Merged
	a.MergedBy = "auto"
	a.FinishedAt = time.Now()
	s.UpdateRun(ctx, a)
	if n, _ := s.CountMergedBy(ctx, "r", "auto", time.Now().Add(-time.Hour)); n != 1 {
		t.Errorf("auto merges = %d", n)
	}
	got, _ := s.GetRun(ctx, a.ID)
	if got.MergedBy != "auto" {
		t.Errorf("merged_by not persisted: %+v", got)
	}
	s.AppendEvent(ctx, a.ID, Event{Kind: "x", Text: "1"})
	s.AppendEvent(ctx, a.ID, Event{Kind: "x", Text: "2"})
	ev, _ := s.Events(ctx, a.ID)
	tail, _ := s.EventsAfter(ctx, a.ID, ev[0].Seq)
	if len(tail) != 1 || tail[0].Text != "2" {
		t.Errorf("tail = %+v", tail)
	}
	if v, _ := s.Setting(ctx, "autonomy", "on"); v != "on" {
		t.Errorf("default = %q", v)
	}
	s.SetSetting(ctx, "autonomy", "off")
	s.SetSetting(ctx, "autonomy", "off")
	if v, _ := s.Setting(ctx, "autonomy", "on"); v != "off" {
		t.Errorf("set = %q", v)
	}
}

func TestFollowups(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	p := &Run{Job: "j", Repo: "r", Status: Proposed}
	s.CreateRun(ctx, p)
	f := &Run{Job: "fix-checks", Repo: "r", Status: Pushed, Parent: p.ID, BaseHead: "abc"}
	s.CreateRun(ctx, f)
	got, err := s.Followups(ctx, p.ID)
	if err != nil || len(got) != 1 || got[0].BaseHead != "abc" || got[0].Parent != p.ID {
		t.Errorf("%+v %v", got, err)
	}
	if none, _ := s.Followups(ctx, "zzz"); len(none) != 0 {
		t.Error("unknown parent has no followups")
	}
}

func TestCancelRunningUnsettledSummary(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	r1 := &Run{Job: "j", Repo: "r", Status: Running}
	r2 := &Run{Job: "j", Repo: "r", Status: Running}
	r3 := &Run{Job: "j", Repo: "r", Status: Proposed}
	for _, r := range []*Run{r1, r2, r3} {
		s.CreateRun(ctx, r)
	}
	ids, err := s.CancelRunning(ctx, "daemon restarted")
	if err != nil || len(ids) != 2 {
		t.Fatalf("%v %v", ids, err)
	}
	got, _ := s.GetRun(ctx, r1.ID)
	if got.Status != Cancelled || got.Error != "daemon restarted" || got.FinishedAt.IsZero() {
		t.Errorf("%+v", got)
	}
	if again, _ := s.CancelRunning(ctx, "x"); len(again) != 0 {
		t.Error("nothing left to cancel")
	}

	m := &Run{Job: "j", Repo: "r", Status: Merged}
	s.CreateRun(ctx, m)
	m.MergedBy = "auto"
	m.MergeSHA = "abc"
	m.FinishedAt = time.Now()
	s.UpdateRun(ctx, m)
	h := &Run{Job: "j", Repo: "r", Status: Merged}
	s.CreateRun(ctx, h)
	h.MergedBy = "human"
	s.UpdateRun(ctx, h)
	un, _ := s.Unsettled(ctx)
	if len(un) != 1 || un[0].ID != m.ID || un[0].MergeSHA != "abc" {
		t.Errorf("unsettled = %+v", un)
	}
	m.Settled = true
	s.UpdateRun(ctx, m)
	if un, _ := s.Unsettled(ctx); len(un) != 0 {
		t.Error("settled runs drop out")
	}

	sum, err := s.Summarise(ctx, time.Now().Add(-time.Hour))
	if err != nil || sum.Runs != 5 || sum.ByStatus[Cancelled] != 2 || len(sum.Proposals) != 1 {
		t.Errorf("%+v %v", sum, err)
	}
}

func TestVerdicts(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	r := &Run{Job: "j", Repo: "r", Status: Proposed}
	s.CreateRun(ctx, r)
	if _, ok, err := s.LatestVerdict(ctx, r.ID); ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	s.PutVerdict(ctx, Verdict{RunID: r.ID, Head: "aaa", Verdict: "FAIL", Summary: "bad", CostUSD: 0.5})
	s.PutVerdict(ctx, Verdict{RunID: r.ID, Head: "bbb", Verdict: "PASS", Summary: "good", Findings: `[]`, CostUSD: 0.25})
	v, ok, err := s.LatestVerdict(ctx, r.ID)
	if err != nil || !ok || v.Head != "bbb" || v.Verdict != "PASS" || v.At.IsZero() {
		t.Errorf("%+v %v %v", v, ok, err)
	}
	if usd, _ := s.VerdictSpendSince(ctx, time.Now().Add(-time.Hour)); usd != 0.75 {
		t.Errorf("spend = %v", usd)
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

func TestPruneEvents(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Now()
	old := now.Add(-60 * 24 * time.Hour)
	mk := func(st Status, fin time.Time, mergedBy string) *Run {
		r := &Run{Job: "j", Repo: "r"}
		if err := s.CreateRun(ctx, r); err != nil {
			t.Fatal(err)
		}
		r.Status, r.FinishedAt, r.MergedBy = st, fin, mergedBy
		if err := s.UpdateRun(ctx, r); err != nil {
			t.Fatal(err)
		}
		s.AppendEvent(ctx, r.ID, Event{Kind: "text", Text: "x"})
		s.AppendEvent(ctx, r.ID, Event{Kind: "text", Text: "y"})
		return r
	}
	gone := mk(Proposed, old, "")
	human := mk(Merged, old, "human")
	recent := mk(Proposed, now, "")
	watched := mk(Merged, old, "auto")
	running := mk(Running, time.Time{}, "")

	n, err := s.PruneEvents(ctx, now.Add(-30*24*time.Hour))
	if err != nil || n != 4 {
		t.Fatalf("pruned %d, %v", n, err)
	}
	for r, want := range map[*Run]int{gone: 0, human: 0, recent: 2, watched: 2, running: 2} {
		ev, _ := s.Events(ctx, r.ID)
		if len(ev) != want {
			t.Errorf("run %s (%s): %d events, want %d", r.ID, r.Status, len(ev), want)
		}
	}
	if got, _ := s.GetRun(ctx, gone.ID); got.Status != Proposed {
		t.Errorf("the run row must survive: %+v", got)
	}
}
