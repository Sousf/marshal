package ui

import (
	"fmt"
	"os"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Sousf/marshal/internal/config"
	"github.com/Sousf/marshal/internal/store"
)

// TestRenderReal prints every screen against a real fleet and database as
// plain text, for eyeballing layout changes:
//
//	RENDER_FLEET=~/.config/marshal/fleet.yaml RENDER_DB=~/.local/share/marshal/marshal.db \
//	  go test ./internal/ui -run TestRenderReal -v | sed 's/\x1b\[[0-9;]*m//g'
//
// It is skipped unless both variables are set.
func TestRenderReal(t *testing.T) {
	cfg := os.Getenv("RENDER_FLEET")
	if cfg == "" {
		t.Skip("set RENDER_FLEET and RENDER_DB to render real data")
	}
	f, err := config.Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(os.Getenv("RENDER_DB"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := New(Local{Store: st, Fleet: f}, f, "dev")
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 110, Height: 36})
	m = mm.(Model)
	m, _ = step(m, m.refresh()())
	fmt.Println("=== DASHBOARD")
	fmt.Println(m.View())
	m, _ = step(m, key("2"))
	fmt.Println("=== RUNS")
	fmt.Println(m.View())
	m, _ = step(m, key("G"))
	m, cmd := step(m, key("enter"))
	m, _ = step(m, cmd())
	fmt.Println("=== DETAIL")
	fmt.Println(m.View())
	m, _ = step(m, key("esc"))
	m, _ = step(m, key("n"))
	fmt.Println("=== PICK")
	fmt.Println(m.View())
	m, _ = step(m, key("esc"))
	m, _ = step(m, key("?"))
	fmt.Println("=== HELP")
	fmt.Println(m.View())
}
