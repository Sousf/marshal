package ui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Sousf/marshal/internal/store"
)

func TestRunsCursorMoves(t *testing.T) {
	m, st := fixture(t)
	for i := 0; i < 3; i++ {
		st.CreateRun(context.Background(), &store.Run{Job: "audit", Repo: "alpha", Class: "c", Status: store.Running})
	}
	m, _ = step(m, m.refresh()())
	m, _ = step(m, key("2"))
	if m.runList.cursor != 0 {
		t.Fatalf("start cursor %d", m.runList.cursor)
	}
	m, _ = step(m, key("j"))
	m, _ = step(m, tea.KeyMsg{Type: tea.KeyDown})
	if m.runList.cursor != 2 {
		t.Fatalf("after j and down: cursor %d", m.runList.cursor)
	}
	// A spinner tick and a data refresh must not reset the cursor.
	m, _ = step(m, m.spin.Tick())
	m, _ = step(m, m.refresh()())
	if m.runList.cursor != 2 {
		t.Fatalf("after tick/refresh: cursor %d", m.runList.cursor)
	}
	m, _ = step(m, key("k"))
	if m.runList.cursor != 1 {
		t.Fatalf("after k: cursor %d", m.runList.cursor)
	}
	// Trust list moves too.
	m, _ = step(m, key("3"))
	m, _ = step(m, key("j"))
	if m.trustList.cursor != 1 {
		t.Fatalf("trust cursor %d", m.trustList.cursor)
	}
}

func TestPreviewFollowsSelection(t *testing.T) {
	m, st := fixture(t)
	ctx := context.Background()
	r := &store.Run{Job: "audit", Repo: "beta", Class: "c", Status: store.Running}
	st.CreateRun(ctx, r)
	st.AppendEvent(ctx, r.ID, store.Event{Kind: "tool", Text: "Bash go test ./..."})
	m, _ = step(m, m.refresh()())
	m, cmd := step(m, key("2"))
	if cmd == nil {
		t.Fatal("entering runs should load the preview")
	}
	m, _ = step(m, cmd())
	if m.previewRows() == 0 {
		t.Fatal("40 rows should leave room for a preview pane")
	}
	v := m.View()
	if !strings.Contains(v, "log · run "+r.ID) || !strings.Contains(v, "Bash go test ./...") {
		t.Errorf("preview pane missing:\n%s", v)
	}
	if n := strings.Count(v, "\n") + 1; n != 40 {
		t.Errorf("view height %d", n)
	}
	// Moving the cursor requests the next run's preview.
	m, cmd = step(m, key("j"))
	if cmd == nil {
		t.Fatal("cursor move should load a preview")
	}
	m, _ = step(m, cmd())
	if m.preview.id == r.ID {
		t.Error("preview should follow the selection")
	}
	// Small terminals drop the pane and still fill the screen exactly.
	small, _ := step(m, tea.WindowSizeMsg{Width: 80, Height: 16})
	if small.previewRows() != 0 {
		t.Error("no pane on a short screen")
	}
	if n := strings.Count(small.View(), "\n") + 1; n != 16 {
		t.Errorf("small view height %d", n)
	}
}
