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
	st.UpdateRun(ctx, r)
	st.AppendEvent(ctx, r.ID, store.Event{Kind: "tool", Text: "Read go.mod"})
	st.AppendEvent(ctx, r.ID, store.Event{Kind: "text", Text: "Did it\n\nbody"})
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
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func TestRefreshAndNavigate(t *testing.T) {
	m, _ := fixture(t)
	msg := m.refresh()()
	m, _ = step(m, msg)
	if m.srcErr != "" || len(m.runs) != 1 || len(m.trust) != 2 {
		t.Fatalf("refresh: err=%q runs=%d trust=%d", m.srcErr, len(m.runs), len(m.trust))
	}
	v := m.View()
	for _, want := range []string{"MARSHAL", "alpha", "beta", "open PRs  1", "trust     c:review", "autonomy on"} {
		if !strings.Contains(v, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
	m, _ = step(m, key("2"))
	if m.screen != runs || !strings.Contains(m.View(), "#7") {
		t.Errorf("runs screen should list the PR number")
	}
	m, cmd := step(m, key("enter"))
	if m.screen != detail || cmd == nil {
		t.Fatal("enter on a run should open detail and load it")
	}
	m, _ = step(m, cmd())
	v = m.View()
	if !strings.Contains(v, "Read go.mod") || !strings.Contains(v, "Did it") || !strings.Contains(v, "proposed") {
		t.Errorf("detail missing content:\n%s", v)
	}
	m, _ = step(m, key("esc"))
	if m.screen != runs {
		t.Error("esc should return to runs")
	}
	m, _ = step(m, key("tab"))
	if m.screen != trust || !strings.Contains(m.View(), "review") {
		t.Error("tab should reach trust")
	}
	m, _ = step(m, key("tab"))
	if m.screen != dashboard {
		t.Error("tab should wrap to dashboard")
	}
}

func TestPickJobWithoutDaemon(t *testing.T) {
	m, _ := fixture(t)
	m, _ = step(m, m.refresh()())
	m, _ = step(m, key("j")) // beta
	m, _ = step(m, key("n"))
	if m.screen != pickJob || m.pickRepo.Name != "beta" || !strings.Contains(m.View(), "no daemon") {
		t.Fatalf("screen=%v repo=%s", m.screen, m.pickRepo.Name)
	}
	m, _ = step(m, key("j"))
	m, cmd := step(m, key("enter"))
	if m.pickIdx != 1 || cmd == nil {
		t.Fatal("enter should attempt the trigger")
	}
	m, _ = step(m, cmd())
	if !strings.Contains(m.footer(), "needs the daemon") {
		t.Errorf("footer = %q", m.footer())
	}
}

func TestAutonomyToggleNeedsConfirm(t *testing.T) {
	m, st := fixture(t)
	m, _ = step(m, m.refresh()())
	m, _ = step(m, key("a"))
	if !m.confirmAutonomy || !strings.Contains(m.footer(), "y/n") {
		t.Fatal("a should ask for confirmation")
	}
	m, _ = step(m, key("n"))
	if v, _ := st.Setting(context.Background(), "autonomy", "on"); v != "on" {
		t.Error("declining must change nothing")
	}
	m, _ = step(m, key("a"))
	m, cmd := step(m, key("y"))
	m, _ = step(m, cmd())
	if v, _ := st.Setting(context.Background(), "autonomy", "on"); v != "off" {
		t.Error("confirming should turn autonomy off")
	}
	m, _ = step(m, m.refresh()())
	if !strings.Contains(m.View(), "autonomy OFF") {
		t.Error("header should show autonomy OFF")
	}
}

func TestQuit(t *testing.T) {
	m, _ := fixture(t)
	_, cmd := step(m, key("q"))
	if cmd == nil {
		t.Fatal("q on a top-level screen should quit")
	}
}
