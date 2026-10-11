package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/Sousf/marshal/internal/store"
)

func (m Model) View() string {
	if m.width == 0 {
		return "starting…"
	}
	var b strings.Builder
	b.WriteString(m.header())
	b.WriteString("\n")
	b.WriteString(m.tabs())
	b.WriteString("\n\n")
	if m.modal != none {
		b.WriteString(m.renderModal())
	} else {
		if m.screen == runs && (m.filtering || m.filter.Value() != "") {
			b.WriteString(" " + m.filter.View() + "\n")
		}
		var body string
		switch m.screen {
		case dashboard:
			body = m.viewDashboard()
		case runs:
			body = m.runList.render("No runs yet. Press n to start one, or wait for a schedule.")
			if p := m.previewRows(); p > 0 {
				body = padLines(body, m.runList.height) + "\n" + m.viewPreview(p)
			}
		case trust:
			body = m.viewTrust()
		case detail:
			body = m.view.View()
		}
		b.WriteString(clipLines(body, m.bodyHeight()))
	}
	out := b.String()
	// Pin the footer to the last row whatever the body drew.
	lines := strings.Count(out, "\n") + 1
	if pad := m.height - 1 - lines; pad > 0 {
		out += strings.Repeat("\n", pad)
	}
	return out + "\n" + m.footer()
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func versionLabel(v string) string {
	if v == "" || v == "dev" {
		return "dev build"
	}
	// Semver gets a v; a commit hash or anything else is shown as is.
	if v[0] >= '0' && v[0] <= '9' {
		return "v" + v
	}
	return v
}

// padLines pads s with blank lines to exactly n lines.
func padLines(s string, n int) string {
	c := strings.Count(s, "\n") + 1
	if c >= n {
		return clipLines(s, n)
	}
	return s + strings.Repeat("\n", n-c)
}

// viewPreview is the log pane: the selected run's latest events, live.
func (m Model) viewPreview(rows int) string {
	var b strings.Builder
	title := "log"
	if m.runList.selected() != nil {
		title = "log · run " + m.visible[m.runList.cursor].ID
	}
	rule := strings.Repeat("─", max(m.width-6-lipgloss.Width(title), 4))
	b.WriteString(" " + sRule.Render("── ") + sHeader.Render(title) + " " + sRule.Render(rule) + "\n")
	rows-- // the rule
	sel := ""
	if m.runList.selected() != nil {
		sel = m.visible[m.runList.cursor].ID
	}
	if sel == "" || m.preview.id != sel {
		b.WriteString(" " + sDim.Render("loading…"))
		return b.String()
	}
	lines := m.eventLines(m.preview.events, true)
	if r := m.preview.run; r.Status != store.Running && r.Summary != "" {
		first, _, _ := strings.Cut(strings.TrimSpace(r.Summary), "\n")
		lines = append(lines, " "+sDim.Render("report  ")+first)
	}
	if len(lines) == 0 {
		lines = []string{" " + sDim.Render("no events yet")}
	}
	if len(lines) > rows {
		lines = lines[len(lines)-rows:]
	}
	for i, l := range lines {
		b.WriteString(ansi.Truncate(l, m.width-1, "…"))
		if i < len(lines)-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// eventLines renders events as the transcript does. With compact set, the
// agent's text events show their first line only.
func (m Model) eventLines(events []store.Event, compact bool) []string {
	var out []string
	for _, e := range events {
		var kind string
		switch e.Kind {
		case "tool":
			kind = sDim.Render("tool   ")
		case "denied", "blocked":
			kind = sBad.Render(fmt.Sprintf("%-7s", e.Kind))
		case "text":
			kind = lipgloss.NewStyle().Foreground(accent).Render("text   ")
		default:
			kind = sFaint.Render(fmt.Sprintf("%-7s", e.Kind))
		}
		text := e.Text
		if e.Kind == "text" || compact {
			text, _, _ = strings.Cut(text, "\n")
			if text == "" {
				text = "…"
			}
		}
		out = append(out, fmt.Sprintf(" %s %s %s", sFaint.Render(e.At.Format("15:04:05")), kind, text))
	}
	return out
}

// clipLines keeps the first n lines so a body can never push the footer off
// the screen.
func clipLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[:n], "\n")
}

// header is the banner (or a one-line title) with the status line.
func (m Model) header() string {
	status := m.statusLine()
	if showBanner(m.width, m.height) {
		return renderBanner(m.width) + "\n" + m.alignRight(sDim.Render(versionLabel(m.version)), status)
	}
	return m.alignRight(sTitle.Render(" MARSHAL ")+sTagline.Render(tagline), status)
}

func (m Model) alignRight(left, right string) string {
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right) - 2
	if gap < 1 {
		return " " + left + "\n " + right
	}
	return " " + left + strings.Repeat(" ", gap) + right
}

func (m Model) statusLine() string {
	var parts []string
	if m.srcErr != "" {
		parts = append(parts, sBad.Render("⚠ "+m.srcErr))
	} else if m.status.Version == "(no daemon)" {
		parts = append(parts, sWarn.Render("○ no daemon"))
	} else {
		parts = append(parts, sGood.Render("● daemon"))
	}
	if m.status.Autonomy {
		parts = append(parts, sGood.Render("autonomy on"))
	} else {
		parts = append(parts, sWarn.Render("autonomy OFF"))
	}
	if n := len(m.status.Running); n > 0 {
		parts = append(parts, sGood.Render(fmt.Sprintf("%s %d running", m.spin.View(), n)))
	} else {
		parts = append(parts, sDim.Render("idle"))
	}
	parts = append(parts, sDim.Render(fmt.Sprintf("$%.2f / 24h", m.status.Spend24h)))
	return strings.Join(parts, sFaint.Render("  │  "))
}

func (m Model) tabs() string {
	names := []string{"dashboard", "runs", "trust"}
	cur := m.screen
	if cur == detail {
		cur = runs
	}
	var out []string
	for i, n := range names {
		// The key is bracketed so "2 runs" cannot read as a count.
		label := fmt.Sprintf("[%d] %s", i+1, n)
		if i == 1 && m.anyRunning() {
			label += " " + m.spin.View()
		}
		if screen(i) == cur {
			out = append(out, sTabOn.Render(label))
		} else {
			out = append(out, sTabOff.Render(label))
		}
	}
	line := " " + strings.Join(out, " ")
	if m.screen == detail {
		line += sDim.Render(" › run " + m.run.ID)
	}
	return line
}

func (m Model) footer() string {
	if m.flash != "" && time.Since(m.flashT) < 6*time.Second {
		style := sWarn
		if strings.HasPrefix(m.flash, "✓") {
			style = sGood
		}
		if strings.HasPrefix(m.flash, "✗") {
			style = sBad
		}
		return " " + style.Render(m.flash)
	}
	var keys [][2]string
	switch {
	case m.modal == modalPick:
		keys = [][2]string{{"h/l", "repo"}, {"j/k", "job"}, {"enter", "start"}, {"esc", "cancel"}}
	case m.modal == modalAutonomy:
		keys = [][2]string{{"y", "yes"}, {"n", "no"}}
	case m.modal == modalHelp:
		keys = [][2]string{{"any key", "close"}}
	case m.filtering:
		keys = [][2]string{{"type", "filter"}, {"enter", "keep"}, {"esc", "clear"}}
	case m.screen == dashboard:
		keys = [][2]string{{"j/k", "repo"}, {"enter", "start job"}, {"o", "its runs"}, {"a", "autonomy"}, {"?", "help"}, {"q", "quit"}}
	case m.screen == runs:
		keys = [][2]string{{"j/k", "select"}, {"enter", "full log"}, {"/", "filter"}, {"o", "open PR"}, {"x", "fix PR now"}, {"n", "new run"}, {"?", "help"}}
	case m.screen == trust:
		keys = [][2]string{{"j/k", "move"}, {"a", "autonomy"}, {"?", "help"}, {"q", "quit"}}
	case m.screen == detail:
		keys = [][2]string{{"j/k", "scroll"}, {"f", "follow"}, {"g/G", "top/end"}, {"o", "open PR"}, {"esc", "back"}}
	}
	var parts []string
	for _, k := range keys {
		parts = append(parts, sKey.Render(k[0])+" "+sDim.Render(k[1]))
	}
	return " " + strings.Join(parts, sFaint.Render("  ·  "))
}

// dashboard

func (m Model) viewDashboard() string {
	if len(m.fleet.Repos) == 0 {
		return sDim.Render("  No repositories in the fleet file. Add some and run `marshal check`.")
	}
	trustByRepo := map[string][]store.Trust{}
	for _, t := range m.trust {
		trustByRepo[t.Repo] = append(trustByRepo[t.Repo], t)
	}
	last := map[string]store.Run{}
	open := map[string]int{}
	running := map[string]bool{}
	spend := map[string]float64{}
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
		if time.Since(r.StartedAt) < 7*24*time.Hour {
			spend[r.Repo] += r.CostUSD
		}
	}
	cols := 1
	cardW := m.width - 4
	if m.width >= 100 && len(m.fleet.Repos) > 1 {
		cols = 2
		cardW = (m.width-5)/2 - 2
	}
	var cards []string
	for i, repo := range m.fleet.Repos {
		var b strings.Builder
		name := sTitle.Render(repo.Name)
		if running[repo.Name] {
			name += "  " + sGood.Render(m.spin.View()+" running")
		}
		b.WriteString(name + "\n")
		b.WriteString(sDim.Render(repo.URL) + "\n\n")
		row := func(k, v string) { b.WriteString(sDim.Render(fmt.Sprintf("%-9s", k)) + v + "\n") }
		if r, ok := last[repo.Name]; ok {
			row("last run", fmt.Sprintf("%s  %s  %s", statusCell(r.Status, m.spin.View()), r.Job, sDim.Render(ago(r.StartedAt))))
		} else {
			row("last run", sDim.Render("never"))
		}
		openStr := sDim.Render("none")
		if open[repo.Name] > 0 {
			openStr = sWarn.Render(fmt.Sprintf("%d waiting", open[repo.Name]))
		}
		row("open PRs", openStr)
		row("jobs", strings.Join(repo.Jobs, ", "))
		row("7d spend", fmt.Sprintf("$%.2f", spend[repo.Name]))
		var pills []string
		for _, t := range trustByRepo[repo.Name] {
			pills = append(pills, sDim.Render(t.Class+" ")+levelPill(t.Level))
		}
		if len(pills) > 0 {
			row("trust", strings.Join(pills, "  "))
		}
		style := sCard
		if i == m.repoCursor {
			style = sCardOn
		}
		cards = append(cards, style.Width(cardW).Render(strings.TrimRight(b.String(), "\n")))
	}
	// Lay cards out in rows, then scroll the rows so the selected card is
	// always on screen. A fleet of twenty repos on a laptop screen must still
	// be navigable with j/k alone.
	var rows []string
	selRow := m.repoCursor / cols
	for i := 0; i < len(cards); i += cols {
		parts := []string{" ", cards[i]}
		if cols == 2 && i+1 < len(cards) {
			parts = append(parts, " ", cards[i+1])
		}
		rows = append(rows, lipgloss.JoinHorizontal(lipgloss.Top, parts...))
	}
	limit := m.bodyHeight()
	start := 0
	for {
		h := 0
		for r := start; r <= selRow; r++ {
			h += lipgloss.Height(rows[r])
		}
		if h <= limit || start >= selRow {
			break
		}
		start++
	}
	return lipgloss.JoinVertical(lipgloss.Left, rows[start:]...)
}

// trust

func (m Model) viewTrust() string {
	legend := " " + sDim.Render("levels  ") +
		levelPill("locked") + sDim.Render(" never proposed   ") +
		levelPill("review") + sDim.Render(" waits for you   ") +
		levelPill("soak") + sDim.Render(" merges after a quiet window   ") +
		levelPill("auto") + sDim.Render(" merges when green")
	l := m.trustList
	l.height = max(m.trustList.height-2, 3)
	return l.render("No repos with jobs yet.") + "\n\n" + legend
}

// detail

func (m Model) renderDetail() string {
	r := m.run
	var b strings.Builder
	b.WriteString(fmt.Sprintf(" %s  %s %s %s  %s %s\n", sTitle.Render("run "+r.ID), sDim.Render("job"), r.Job, sDim.Render("on "+r.Repo), sDim.Render("class"), r.Class))
	meta := []string{statusCell(r.Status, m.spin.View()), fmt.Sprintf("%d turns", r.Turns), fmt.Sprintf("$%.2f", r.CostUSD), r.Model}
	if !r.StartedAt.IsZero() {
		meta = append(meta, "started "+r.StartedAt.Format("Jan 2 15:04"))
	}
	if !r.FinishedAt.IsZero() {
		meta = append(meta, "took "+r.FinishedAt.Sub(r.StartedAt).Round(time.Second).String())
	}
	b.WriteString(" " + strings.Join(meta, sFaint.Render("  ·  ")) + "\n")
	if r.PRURL != "" {
		b.WriteString(" " + sDim.Render("pr ") + r.PRURL + "\n")
	}
	if r.Error != "" {
		b.WriteString(" " + sBad.Render(strings.ReplaceAll(r.Error, "\n", "\n ")) + "\n")
	}
	if v := m.verdict; v != nil {
		style, icon := sWarn, "⚠"
		switch v.Verdict {
		case "PASS":
			style, icon = sGood, "✓"
		case "FAIL":
			style, icon = sBad, "✗"
		}
		b.WriteString(fmt.Sprintf(" %s %s  %s\n", sDim.Render("verifier"), style.Render(icon+" "+v.Verdict), sDim.Render(fmt.Sprintf("head %s · %d turns · $%.2f", short(v.Head), v.Turns, v.CostUSD))))
		if v.Summary != "" {
			b.WriteString("          " + strings.ReplaceAll(v.Summary, "\n", "\n          ") + "\n")
		}
	}
	if r.Feedback != "" {
		b.WriteString(" " + sDim.Render("reviewer") + "\n          " + strings.ReplaceAll(r.Feedback, "\n", "\n          ") + "\n")
	}
	if r.Parent != "" {
		b.WriteString(" " + sDim.Render("follow-up of run "+r.Parent) + "\n")
	}
	b.WriteString(" " + sRule.Render(strings.Repeat("─", max(m.width-4, 10))) + "\n")
	// The final text is the report, rendered in full below; in the stream
	// text events show their first line only so the tool calls stay dense.
	for _, l := range m.eventLines(m.events, false) {
		b.WriteString(l + "\n")
	}
	if r.Summary != "" && r.Status != store.Running {
		b.WriteString(" " + sRule.Render(strings.Repeat("─", max(m.width-4, 10))) + "\n")
		b.WriteString(" " + sHeader.Render("REPORT") + "\n")
		b.WriteString(renderMarkdown(r.Summary, m.width))
	}
	return b.String()
}

// modals

func (m Model) renderModal() string {
	var content string
	switch m.modal {
	case modalAutonomy:
		word := sBad.Render("OFF")
		detail := "Every automatic merge stops until it is turned back on.\nProposals keep being opened; they just wait for you."
		if !m.status.Autonomy {
			word = sGood.Render("ON")
			detail = "Classes at soak or auto will merge their own PRs again,\nwhen CI is green and the gates pass."
		}
		content = sTitle.Render("Turn autonomy "+word+"?") + "\n\n" + detail + "\n\n" + sKey.Render("y") + sDim.Render(" yes    ") + sKey.Render("n") + sDim.Render(" no")
	case modalPick:
		var b strings.Builder
		b.WriteString(sTitle.Render("Start a job") + "\n\n")
		b.WriteString(sDim.Render("repo   "))
		for i, r := range m.fleet.Repos {
			if i == m.repoCursor {
				b.WriteString(sTabOn.Render(r.Name))
			} else {
				b.WriteString(sTabOff.Render(r.Name))
			}
		}
		b.WriteString("\n\n")
		if len(m.pickRepo.Jobs) == 0 {
			b.WriteString(sDim.Render("this repo has no jobs") + "\n")
		}
		for i, jn := range m.pickRepo.Jobs {
			j, _ := m.fleet.Job(jn)
			res := m.fleet.Resolve(j)
			cur := "  "
			name := jn
			if i == m.pickIdx {
				cur = sKey.Render("▶ ")
				name = sTitle.Render(jn)
			}
			b.WriteString(fmt.Sprintf("%s%-16s %s\n", cur, name, sDim.Render(j.Description)))
			b.WriteString(fmt.Sprintf("  %-16s %s\n", "", sFaint.Render(fmt.Sprintf("%s · %s · ≤%d turns · ≤$%.2f · class %s", res.Model, res.Effort, res.MaxTurns, res.MaxUSD, j.Class))))
		}
		if m.status.Version == "(no daemon)" {
			b.WriteString("\n" + sWarn.Render("No daemon is running. Start one with `marshal serve`."))
		}
		content = strings.TrimRight(b.String(), "\n")
	case modalHelp:
		content = m.helpText()
	}
	box := sModal.Render(content)
	return lipgloss.Place(m.width, m.bodyHeight(), lipgloss.Center, lipgloss.Center, box)
}

func (m Model) helpText() string {
	row := func(k, v string) string { return fmt.Sprintf("  %s  %s", sKey.Render(fmt.Sprintf("%-11s", k)), v) }
	sections := []struct {
		title string
		keys  [][2]string
	}{
		{"everywhere", [][2]string{{"1 2 3", "dashboard, runs, trust"}, {"tab / S-tab", "next, previous screen"}, {"n", "start a job"}, {"a", "autonomy on/off (asks first)"}, {"R", "refresh now"}, {"?", "this help"}, {"q / ctrl-c", "quit, or back from a run"}}},
		{"dashboard", [][2]string{{"j k h l", "select repo"}, {"enter", "start a job on it"}, {"o", "show its runs"}}},
		{"runs", [][2]string{{"j k g G", "move, top, bottom"}, {"pgup pgdn", "page"}, {"enter", "open the run"}, {"/", "filter · esc clears · backspace clears a kept filter"}, {"o", "open its PR in the browser"}, {"x", "push a fix to its PR now, outside the attempt cap"}}},
		{"run", [][2]string{{"j k", "scroll"}, {"f", "follow the live stream"}, {"g G", "top, bottom"}, {"o", "open its PR"}, {"esc", "back to runs"}}},
	}
	var b strings.Builder
	b.WriteString(sTitle.Render("Keys") + "\n")
	for _, s := range sections {
		b.WriteString("\n" + sHeader.Render(s.title) + "\n")
		for _, k := range s.keys {
			b.WriteString(row(k[0], k[1]) + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
