package ui

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Sousf/marshal/internal/config"
	"github.com/Sousf/marshal/internal/store"
)

func fixture(t *testing.T) (Model, *store.Store) {
	t.Helper()
	f, err := config.Parse([]byte(`
version: 1
repos:
  - {name: alpha, url: u, jobs: [audit]}
  - {name: beta, url: v, jobs: [audit, fix]}
jobs:
  - {name: audit, class: c, prompt: p, description: harden things}
  - {name: fix, class: c, prompt: p}
policy:
  classes: {c: {start: review}}
`))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	r := &store.Run{Job: "audit", Repo: "alpha", Class: "c", Status: store.Proposed}
	st.CreateRun(ctx, r)
	r.PRURL = "https://github.com/x/y/pull/7"
	r.Summary = "# Did it\n\nbody"
	st.UpdateRun(ctx, r)
	st.AppendEvent(ctx, r.ID, store.Event{Kind: "tool", Text: "Read go.mod"})
	st.AppendEvent(ctx, r.ID, store.Event{Kind: "text", Text: "Did it\n\nbody"})
	r2 := &store.Run{Job: "fix", Repo: "beta", Class: "c", Status: store.Failed}
	st.CreateRun(ctx, r2)
	m := New(Local{Store: st, Fleet: f}, f, "test")
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = mm.(Model)
	return m, st
}

func step(m Model, msg tea.Msg) (Model, tea.Cmd) {
	mm, cmd := m.Update(msg)
	return mm.(Model), cmd
}

func key(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "shift+tab":
		return tea.KeyMsg{Type: tea.KeyShiftTab}
	case "backspace":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func loaded(t *testing.T) Model {
	t.Helper()
	m, _ := fixture(t)
	m, _ = step(m, m.refresh()())
	if m.srcErr != "" {
		t.Fatal(m.srcErr)
	}
	return m
}

func TestBannerAndDashboard(t *testing.T) {
	m := loaded(t)
	v := m.View()
	for _, want := range []string{"███╗", "Learns", "alpha", "beta", "1 waiting", "review", "autonomy on", "no daemon", "[1] dashboard"} {
		if !strings.Contains(v, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
	small, _ := step(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	if v := small.View(); strings.Contains(v, "███╗") || !strings.Contains(v, "MARSHAL") {
		t.Error("small terminal should show the one-line title, not the art")
	}
	if n := strings.Count(small.View(), "\n") + 1; n != 24 {
		t.Errorf("view should fill exactly the terminal height, got %d lines", n)
	}
	// With the second repo selected on a short screen, its card must be the
	// one that is visible.
	small, _ = step(small, key("j"))
	if v := small.View(); !strings.Contains(v, "beta") {
		t.Error("dashboard should scroll to the selected card")
	}
	for _, size := range []tea.WindowSizeMsg{{Width: 60, Height: 16}, {Width: 200, Height: 60}} {
		mm, _ := step(m, size)
		for _, sc := range []screen{dashboard, runs, trust} {
			mm.screen = sc
			mm.layout()
			if n := strings.Count(mm.View(), "\n") + 1; n != size.Height {
				t.Errorf("%dx%d screen %d: %d lines", size.Width, size.Height, sc, n)
			}
		}
	}
}

func TestRunsFilterAndDetail(t *testing.T) {
	m := loaded(t)
	m, _ = step(m, key("2"))
	if m.screen != runs || len(m.visible) != 2 || !strings.Contains(m.View(), "#7") {
		t.Fatalf("runs screen: %d visible", len(m.visible))
	}
	m, _ = step(m, key("/"))
	for _, r := range "beta" {
		m, _ = step(m, key(string(r)))
	}
	if len(m.visible) != 1 || m.visible[0].Repo != "beta" {
		t.Fatalf("filter should narrow to beta: %+v", m.visible)
	}
	m, _ = step(m, key("enter")) // keep filter
	if m.filtering || len(m.visible) != 1 {
		t.Error("enter should keep the filter")
	}
	m, _ = step(m, key("backspace"))
	if len(m.visible) != 2 {
		t.Error("backspace should clear a kept filter")
	}
	// Both runs were created in the same second, so order is not a reliable
	// way to pick one; filter to alpha's proposal, which has events and a report.
	m, _ = step(m, key("/"))
	for _, r := range "alpha" {
		m, _ = step(m, key(string(r)))
	}
	m, _ = step(m, key("enter"))
	if len(m.visible) != 1 || m.visible[0].Repo != "alpha" {
		t.Fatalf("filter alpha: %+v", m.visible)
	}
	m, cmd := step(m, key("enter"))
	if m.screen != detail || cmd == nil {
		t.Fatal("enter should open detail")
	}
	m, _ = step(m, cmd())
	v := m.View()
	for _, want := range []string{"Read go.mod", "REPORT", "Did it", "proposed", "run " + m.run.ID} {
		if !strings.Contains(v, want) {
			t.Errorf("detail missing %q", want)
		}
	}
	m, _ = step(m, key("esc"))
	if m.screen != runs {
		t.Error("esc should return to runs")
	}
	m, _ = step(m, key("tab"))
	if m.screen != trust || !strings.Contains(m.View(), "merges when green") {
		t.Error("tab should reach trust with its legend")
	}
	m, _ = step(m, key("shift+tab"))
	if m.screen != runs {
		t.Error("shift+tab should go back")
	}
}

func TestDashboardOpensRepoRuns(t *testing.T) {
	m := loaded(t)
	m, _ = step(m, key("j"))
	m, _ = step(m, key("o"))
	if m.screen != runs || len(m.visible) != 1 || m.visible[0].Repo != "beta" {
		t.Errorf("o should show beta's runs: %+v", m.visible)
	}
}

func TestPickModalWithoutDaemon(t *testing.T) {
	m := loaded(t)
	m, _ = step(m, key("j"))
	m, _ = step(m, key("enter"))
	if m.modal != modalPick || m.pickRepo.Name != "beta" {
		t.Fatalf("modal=%v repo=%s", m.modal, m.pickRepo.Name)
	}
	v := m.View()
	if !strings.Contains(v, "Start a job") || !strings.Contains(v, "No daemon") || !strings.Contains(v, "harden things") {
		t.Errorf("pick modal content:\n%s", v)
	}
	m, _ = step(m, key("h")) // alpha
	if m.pickRepo.Name != "alpha" {
		t.Error("h should move to the previous repo")
	}
	m, _ = step(m, key("l"))
	m, _ = step(m, key("j"))
	m, cmd := step(m, key("enter"))
	if m.pickIdx != 1 || cmd == nil || m.modal != none {
		t.Fatal("enter should attempt the trigger and close the modal")
	}
	m, _ = step(m, cmd())
	if !strings.Contains(m.footer(), "needs the daemon") {
		t.Errorf("footer = %q", m.footer())
	}
}

func TestAutonomyModal(t *testing.T) {
	m, st := fixture(t)
	m, _ = step(m, m.refresh()())
	m, _ = step(m, key("a"))
	if m.modal != modalAutonomy || !strings.Contains(m.View(), "Turn autonomy") {
		t.Fatal("a should open the confirm modal")
	}
	m, _ = step(m, key("n"))
	if v, _ := st.Setting(context.Background(), "autonomy", "on"); v != "on" || m.modal != none {
		t.Error("declining must change nothing and close")
	}
	m, _ = step(m, key("a"))
	m, cmd := step(m, key("y"))
	m, _ = step(m, cmd())
	if v, _ := st.Setting(context.Background(), "autonomy", "on"); v != "off" {
		t.Error("confirming should turn autonomy off")
	}
	m, _ = step(m, m.refresh()())
	if !strings.Contains(m.View(), "autonomy OFF") {
		t.Error("status line should show autonomy OFF")
	}
}

func TestTaskModal(t *testing.T) {
	m := loaded(t)
	m, _ = step(m, key("t"))
	if m.modal != modalTask || m.taskRepo != 0 {
		t.Fatalf("modal=%v repo=%d", m.modal, m.taskRepo)
	}
	for _, r := range "do x" {
		m, _ = step(m, key(string(r)))
	}
	m, _ = step(m, tea.KeyMsg{Type: tea.KeyCtrlR})
	m, _ = step(m, tea.KeyMsg{Type: tea.KeyCtrlR})
	if m.taskRepo != -1 || !strings.Contains(m.View(), "no repository") {
		t.Errorf("ctrl+r should cycle to no repo: %d", m.taskRepo)
	}
	m, cmd := step(m, key("enter"))
	if cmd == nil || m.modal != none {
		t.Fatal("enter should submit and close")
	}
	m, _ = step(m, cmd())
	if !strings.Contains(m.footer(), "needs the daemon") {
		t.Errorf("footer = %q", m.footer())
	}
}

func TestHelpAndQuit(t *testing.T) {
	m := loaded(t)
	m, _ = step(m, key("?"))
	if m.modal != modalHelp || !strings.Contains(m.View(), "follow the live stream") {
		t.Error("? should show help")
	}
	m, _ = step(m, key("x"))
	if m.modal != none {
		t.Error("any key closes help")
	}
	_, cmd := step(m, key("q"))
	if cmd == nil {
		t.Fatal("q on a top-level screen should quit")
	}
}

func TestSpinnerAdvancesRows(t *testing.T) {
	m, st := fixture(t)
	st.CreateRun(context.Background(), &store.Run{Job: "audit", Repo: "alpha", Class: "c", Status: store.Running})
	m, _ = step(m, m.refresh()())
	m, _ = step(m, key("2"))
	before := m.View()
	// Drive the spinner through its own tick messages, as the runtime would.
	cmd := m.spin.Tick
	changed := false
	for i := 0; i < 6; i++ {
		msg := cmd()
		var next tea.Cmd
		m, next = step(m, msg)
		if next == nil {
			t.Fatal("spinner tick must schedule the next tick")
		}
		if m.View() != before {
			changed = true
		}
		cmd = next
	}
	if !changed {
		t.Error("list rows should re-render with the spinner while a run is in flight")
	}
	if !strings.Contains(m.View(), "[2] runs") {
		t.Error("tab label should bracket the key")
	}
}

func TestVersionLabel(t *testing.T) {
	for in, want := range map[string]string{"dev": "dev build", "": "dev build", "0.3.1": "v0.3.1", "e63dc89-dirty": "e63dc89-dirty"} {
		if got := versionLabel(in); got != want {
			t.Errorf("versionLabel(%q) = %q want %q", in, got, want)
		}
	}
}

func TestListRender(t *testing.T) {
	l := list{cols: []column{{title: "A", width: 4}, {title: "B", width: 0}}, width: 20, height: 3}
	l.setRows([][]string{{"1", "one"}, {"2", "two"}, {"3", "three"}, {"4", "four"}})
	l.move(3)
	out := l.render("")
	if !strings.Contains(out, "four") || strings.Contains(out, "one") {
		t.Errorf("window should scroll to show the cursor:\n%s", out)
	}
	if fit("abcdefgh", 5, false) != "abcd…" || fit("ab", 4, true) != "  ab" {
		t.Error("fit pads and truncates")
	}
}

func TestOpenPRURLs(t *testing.T) {
	runs := []store.Run{ // newest first, as the source returns them
		{ID: "5", Status: store.Proposed, PRURL: "https://x/pull/3"},
		{ID: "4", Status: store.Merged, PRURL: "https://x/pull/2"},
		{ID: "3", Status: store.Proposed, PRURL: "https://x/pull/1"}, // updated release: same URL twice
		{ID: "2", Status: store.Proposed, PRURL: "https://x/pull/1"},
		{ID: "1", Status: store.Proposed},
	}
	if got := strings.Join(openPRURLs(runs), " "); got != "https://x/pull/1 https://x/pull/3" {
		t.Errorf("urls = %q", got)
	}
}
