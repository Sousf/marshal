package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/Sousf/marshal/internal/store"
)

var (
	accent = lipgloss.AdaptiveColor{Light: "#5a32d6", Dark: "#b39dff"}
	dim    = lipgloss.AdaptiveColor{Light: "#6b6b6b", Dark: "#8a8a8a"}
	good   = lipgloss.AdaptiveColor{Light: "#1a7f37", Dark: "#56d364"}
	warn   = lipgloss.AdaptiveColor{Light: "#9a6700", Dark: "#e3b341"}
	bad    = lipgloss.AdaptiveColor{Light: "#cf222e", Dark: "#ff7b72"}

	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(accent)
	dimStyle   = lipgloss.NewStyle().Foreground(dim)
	tabOn      = lipgloss.NewStyle().Bold(true).Underline(true)
	tabOff     = lipgloss.NewStyle().Foreground(dim)
	cardStyle  = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(dim).Padding(0, 1)
	cardOn     = cardStyle.BorderForeground(accent)
	flashStyle = lipgloss.NewStyle().Foreground(warn)
)

func (m Model) View() string {
	if m.width == 0 {
		return "starting..."
	}
	var b strings.Builder
	b.WriteString(m.header())
	b.WriteString("\n")
	switch m.screen {
	case dashboard:
		b.WriteString(m.viewDashboard())
	case runs:
		b.WriteString(m.runTable.View())
	case trust:
		b.WriteString(m.trustTable.View())
	case detail:
		b.WriteString(m.view.View())
	case pickJob:
		b.WriteString(m.viewPick())
	}
	b.WriteString("\n")
	b.WriteString(m.footer())
	return b.String()
}

func (m Model) header() string {
	tabs := []string{"1 dashboard", "2 runs", "3 trust"}
	cur := m.screen
	if cur == detail || cur == pickJob {
		cur = m.back
	}
	var parts []string
	for i, t := range tabs {
		if screen(i) == cur {
			parts = append(parts, tabOn.Render(t))
		} else {
			parts = append(parts, tabOff.Render(t))
		}
	}
	left := titleStyle.Render("MARSHAL") + "  " + strings.Join(parts, "  ")

	auto := lipgloss.NewStyle().Foreground(good).Render("autonomy on")
	if !m.status.Autonomy {
		auto = lipgloss.NewStyle().Foreground(warn).Render("autonomy OFF")
	}
	daemon := dimStyle.Render(m.status.Version)
	if m.srcErr != "" {
		daemon = lipgloss.NewStyle().Foreground(bad).Render("source: " + m.srcErr)
	}
	right := fmt.Sprintf("%s · %d running · $%.2f/24h · %s", auto, len(m.status.Running), m.status.Spend24h, daemon)
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right) - 2
	if gap < 1 {
		gap = 1
	}
	return " " + left + strings.Repeat(" ", gap) + right
}

func (m Model) footer() string {
	if m.confirmAutonomy {
		word := "OFF"
		if !m.status.Autonomy {
			word = "ON"
		}
		return " " + flashStyle.Render(fmt.Sprintf("turn autonomy %s? every auto-merge stops until it is back on. y/n", word))
	}
	if m.flash != "" && time.Since(m.flashT) < 6*time.Second {
		return " " + flashStyle.Render(m.flash)
	}
	var keys string
	switch m.screen {
	case dashboard:
		keys = "j/k select · enter/n new run · a autonomy · r refresh · q quit · ? help"
	case runs:
		keys = "j/k select · enter open · n new run · a autonomy · q quit"
	case trust:
		keys = "j/k scroll · a autonomy · q quit"
	case detail:
		keys = "j/k scroll · f follow · g/G top/bottom · esc back"
	case pickJob:
		keys = "h/l repo · j/k job · enter start · esc cancel"
	}
	return " " + dimStyle.Render(keys)
}

func (m Model) viewDashboard() string {
	if len(m.fleet.Repos) == 0 {
		return dimStyle.Render("  no repositories in the fleet file")
	}
	trustByRepo := map[string][]store.Trust{}
	for _, t := range m.trust {
		trustByRepo[t.Repo] = append(trustByRepo[t.Repo], t)
	}
	last := map[string]store.Run{}
	open := map[string]int{}
	running := map[string]bool{}
	for _, r := range m.runs {
		if _, ok := last[r.Repo]; !ok {
			last[r.Repo] = r // runs are newest first
		}
		if r.Status == store.Proposed {
			open[r.Repo]++
		}
		if r.Status == store.Running {
			running[r.Repo] = true
		}
	}
	cardW := (m.width - 4) / 2
	if cardW < 36 || len(m.fleet.Repos) == 1 {
		cardW = m.width - 4
	}
	var cards []string
	for i, repo := range m.fleet.Repos {
		var b strings.Builder
		name := titleStyle.Render(repo.Name)
		if running[repo.Name] {
			name += lipgloss.NewStyle().Foreground(good).Render("  ● running")
		}
		b.WriteString(name + "\n")
		b.WriteString(dimStyle.Render(repo.URL) + "\n")
		if r, ok := last[repo.Name]; ok {
			b.WriteString(fmt.Sprintf("last run  %s %s %s", r.Job, statusStyle(r.Status).Render(string(r.Status)), dimStyle.Render(ago(r.StartedAt))))
		} else {
			b.WriteString(dimStyle.Render("last run  never"))
		}
		b.WriteString("\n")
		b.WriteString(fmt.Sprintf("open PRs  %d    jobs  %s\n", open[repo.Name], strings.Join(repo.Jobs, ", ")))
		var levels []string
		for _, t := range trustByRepo[repo.Name] {
			levels = append(levels, t.Class+":"+levelStyle(t.Level).Render(t.Level))
		}
		if len(levels) > 0 {
			b.WriteString("trust     " + strings.Join(levels, "  "))
		}
		style := cardStyle
		if i == m.repoCursor {
			style = cardOn
		}
		cards = append(cards, style.Width(cardW).Render(b.String()))
	}
	if cardW == m.width-4 {
		return lipgloss.JoinVertical(lipgloss.Left, cards...)
	}
	var rows []string
	for i := 0; i < len(cards); i += 2 {
		if i+1 < len(cards) {
			rows = append(rows, lipgloss.JoinHorizontal(lipgloss.Top, cards[i], " ", cards[i+1]))
		} else {
			rows = append(rows, cards[i])
		}
	}
	return lipgloss.JoinVertical(lipgloss.Left, rows...)
}

func (m Model) viewPick() string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("  start a job on %s\n\n", titleStyle.Render(m.pickRepo.Name)))
	for i, jn := range m.pickRepo.Jobs {
		j, _ := m.fleet.Job(jn)
		cur := "  "
		if i == m.pickIdx {
			cur = lipgloss.NewStyle().Foreground(accent).Render("> ")
		}
		b.WriteString(fmt.Sprintf("  %s%-16s %s\n", cur, jn, dimStyle.Render(j.Description)))
	}
	if m.status.Version == "(no daemon)" {
		b.WriteString("\n  " + flashStyle.Render("no daemon is running; starting a job needs `marshal serve`"))
	}
	return b.String()
}

func (m Model) renderDetail() string {
	r := m.run
	var b strings.Builder
	b.WriteString(fmt.Sprintf("%s  %s on %s  ·  class %s  ·  %s\n", titleStyle.Render("run "+r.ID), r.Job, r.Repo, r.Class, r.Model))
	line := fmt.Sprintf("%s  %d turns  $%.2f  started %s", statusStyle(r.Status).Render(string(r.Status)), r.Turns, r.CostUSD, r.StartedAt.Format("Jan 2 15:04"))
	if !r.FinishedAt.IsZero() {
		line += "  took " + r.FinishedAt.Sub(r.StartedAt).Round(time.Second).String()
	}
	if r.PRURL != "" {
		line += "  " + r.PRURL
	}
	b.WriteString(line + "\n")
	if r.Error != "" {
		b.WriteString(lipgloss.NewStyle().Foreground(bad).Render(r.Error) + "\n")
	}
	b.WriteString("\n")
	for _, e := range m.events {
		kind := e.Kind
		switch kind {
		case "tool":
			kind = dimStyle.Render("tool   ")
		case "denied", "blocked":
			kind = lipgloss.NewStyle().Foreground(bad).Render(fmt.Sprintf("%-7s", kind))
		case "text":
			kind = lipgloss.NewStyle().Foreground(accent).Render("text   ")
		default:
			kind = dimStyle.Render(fmt.Sprintf("%-7s", kind))
		}
		text := e.Text
		if e.Kind == "text" && strings.Contains(text, "\n") {
			text = strings.ReplaceAll(text, "\n", "\n                 ")
		}
		b.WriteString(fmt.Sprintf("%s %s %s\n", dimStyle.Render(e.At.Format("15:04:05")), kind, text))
	}
	if r.Summary != "" && r.Status != store.Running {
		b.WriteString("\n" + dimStyle.Render(strings.Repeat("─", 40)) + "\n")
		b.WriteString(r.Summary + "\n")
	}
	return b.String()
}

func statusStyle(s store.Status) lipgloss.Style {
	switch s {
	case store.Merged, store.Proposed, store.Committed:
		return lipgloss.NewStyle().Foreground(good)
	case store.Running:
		return lipgloss.NewStyle().Foreground(accent)
	case store.Failed, store.Blocked, store.Rejected:
		return lipgloss.NewStyle().Foreground(bad)
	}
	return dimStyle
}

func levelStyle(l string) lipgloss.Style {
	switch l {
	case "auto":
		return lipgloss.NewStyle().Foreground(good)
	case "soak":
		return lipgloss.NewStyle().Foreground(warn)
	case "locked":
		return lipgloss.NewStyle().Foreground(bad)
	}
	return lipgloss.NewStyle()
}
