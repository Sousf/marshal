package ui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/Sousf/marshal/internal/api"
	"github.com/Sousf/marshal/internal/config"
	"github.com/Sousf/marshal/internal/store"
)

type screen int

const (
	dashboard screen = iota
	runs
	trust
	detail
	pickJob
)

// refreshEvery is how often the interface re-reads its source. The daemon
// ticks once a minute, so anything faster than a couple of seconds here is
// polling for its own sake.
const refreshEvery = 2 * time.Second

// Model is the Bubble Tea model.
type Model struct {
	src     Source
	fleet   *config.Fleet
	version string

	screen screen
	back   screen // where esc from detail returns
	width  int
	height int

	status api.Status
	srcErr string
	runs   []store.Run
	trust  []store.Trust
	flash  string
	flashT time.Time

	repoCursor int
	runTable   table.Model
	trustTable table.Model

	// detail
	run    store.Run
	events []store.Event
	lastSq int64
	view   viewport.Model
	follow bool

	// pickJob
	pickRepo config.Repo
	pickIdx  int

	confirmAutonomy bool
}

// New builds the model.
func New(src Source, fleet *config.Fleet, version string) Model {
	m := Model{src: src, fleet: fleet, version: version, follow: true}
	m.runTable = table.New(
		table.WithColumns(runColumns(80)),
		table.WithFocused(true),
		table.WithHeight(10),
	)
	m.trustTable = table.New(
		table.WithColumns([]table.Column{
			{Title: "REPO", Width: 18}, {Title: "CLASS", Width: 12}, {Title: "LEVEL", Width: 8},
			{Title: "STREAK", Width: 7}, {Title: "APPROVED", Width: 9}, {Title: "REJECTED", Width: 9}, {Title: "REVERTED", Width: 9},
		}),
		table.WithFocused(true),
		table.WithHeight(10),
	)
	st := table.DefaultStyles()
	st.Header = st.Header.Bold(true).Foreground(dim)
	st.Selected = st.Selected.Foreground(lipgloss.Color("0")).Background(accent).Bold(false)
	m.runTable.SetStyles(st)
	m.trustTable.SetStyles(st)
	m.view = viewport.New(80, 20)
	return m
}

func runColumns(width int) []table.Column {
	pr := width - (10 + 9 + 14 + 15 + 11 + 6 + 7) - 14
	if pr < 10 {
		pr = 10
	}
	return []table.Column{
		{Title: "RUN", Width: 10}, {Title: "WHEN", Width: 9}, {Title: "REPO", Width: 14},
		{Title: "JOB", Width: 15}, {Title: "STATUS", Width: 11}, {Title: "TURNS", Width: 6},
		{Title: "USD", Width: 7}, {Title: "PR", Width: pr},
	}
}

// Run starts the program.
func Run(ctx context.Context, src Source, fleet *config.Fleet, version string) error {
	p := tea.NewProgram(New(src, fleet, version), tea.WithAltScreen(), tea.WithContext(ctx))
	_, err := p.Run()
	return err
}

// messages

type refreshed struct {
	status api.Status
	runs   []store.Run
	trust  []store.Trust
	err    error
}

type detailLoaded struct {
	run    store.Run
	events []store.Event
	err    error
}

type actionDone struct {
	msg string
	err error
}

type tick struct{}

func (m Model) Init() tea.Cmd {
	return tea.Batch(m.refresh(), tickCmd())
}

func tickCmd() tea.Cmd {
	return tea.Tick(refreshEvery, func(time.Time) tea.Msg { return tick{} })
}

func (m Model) refresh() tea.Cmd {
	src := m.src
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var r refreshed
		var err error
		if r.status, err = src.Status(ctx); err != nil {
			r.err = err
		}
		if r.runs, err = src.Runs(ctx, "", 200); err != nil {
			r.err = err
		}
		if r.trust, err = src.Trust(ctx); err != nil {
			r.err = err
		}
		return r
	}
}

func (m Model) loadDetail(id string, after int64) tea.Cmd {
	src := m.src
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		run, err := src.Run(ctx, id)
		if err != nil {
			return detailLoaded{err: err}
		}
		ev, err := src.EventsAfter(ctx, id, after)
		return detailLoaded{run: run, events: ev, err: err}
	}
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		return m, nil
	case tick:
		cmds := []tea.Cmd{m.refresh(), tickCmd()}
		if m.screen == detail && m.run.Status == store.Running {
			cmds = append(cmds, m.loadDetail(m.run.ID, m.lastSq))
		}
		return m, tea.Batch(cmds...)
	case refreshed:
		if msg.err != nil {
			m.srcErr = msg.err.Error()
		} else {
			m.srcErr = ""
		}
		m.status, m.runs, m.trust = msg.status, msg.runs, msg.trust
		m.fillTables()
		return m, nil
	case detailLoaded:
		if msg.err != nil {
			m.setFlash(msg.err.Error())
			return m, nil
		}
		m.run = msg.run
		if len(msg.events) > 0 {
			m.events = append(m.events, msg.events...)
			m.lastSq = msg.events[len(msg.events)-1].Seq
		}
		m.view.SetContent(m.renderDetail())
		if m.follow {
			m.view.GotoBottom()
		}
		return m, nil
	case actionDone:
		if msg.err != nil {
			m.setFlash("error: " + msg.err.Error())
		} else {
			m.setFlash(msg.msg)
		}
		return m, m.refresh()
	case tea.KeyMsg:
		return m.key(msg)
	}
	return m, nil
}

func (m *Model) setFlash(s string) {
	m.flash = s
	m.flashT = time.Now()
}

func (m *Model) layout() {
	if m.width == 0 {
		return
	}
	body := m.height - 6
	if body < 5 {
		body = 5
	}
	m.runTable.SetColumns(runColumns(m.width - 2))
	m.runTable.SetWidth(m.width - 2)
	m.runTable.SetHeight(body)
	m.trustTable.SetWidth(m.width - 2)
	m.trustTable.SetHeight(body)
	m.view.Width = m.width - 2
	m.view.Height = body
}

func (m *Model) fillTables() {
	rows := make([]table.Row, 0, len(m.runs))
	for _, r := range m.runs {
		rows = append(rows, table.Row{r.ID, ago(r.StartedAt), r.Repo, r.Job, string(r.Status),
			fmt.Sprint(r.Turns), fmt.Sprintf("%.2f", r.CostUSD), shortPR(r.PRURL)})
	}
	m.runTable.SetRows(rows)
	trows := make([]table.Row, 0, len(m.trust))
	for _, t := range m.trust {
		trows = append(trows, table.Row{t.Repo, t.Class, t.Level, fmt.Sprint(t.Streak),
			fmt.Sprint(t.Approvals), fmt.Sprint(t.Rejections), fmt.Sprint(t.Reverts)})
	}
	m.trustTable.SetRows(trows)
}

func (m Model) key(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	s := k.String()
	if m.confirmAutonomy {
		m.confirmAutonomy = false
		if s == "y" || s == "Y" {
			on := !m.status.Autonomy
			src := m.src
			return m, func() tea.Msg {
				err := src.SetAutonomy(context.Background(), on)
				word := "off"
				if on {
					word = "on"
				}
				return actionDone{msg: "autonomy " + word, err: err}
			}
		}
		m.setFlash("kept as is")
		return m, nil
	}
	switch s {
	case "ctrl+c", "q":
		if m.screen == detail || m.screen == pickJob {
			m.screen = m.back
			return m, nil
		}
		return m, tea.Quit
	case "1":
		m.screen = dashboard
		return m, nil
	case "2":
		m.screen = runs
		return m, nil
	case "3":
		m.screen = trust
		return m, nil
	case "tab":
		switch m.screen {
		case dashboard:
			m.screen = runs
		case runs:
			m.screen = trust
		case trust, detail, pickJob:
			m.screen = dashboard
		}
		return m, nil
	case "r":
		return m, m.refresh()
	case "a":
		m.confirmAutonomy = true
		return m, nil
	case "?":
		m.setFlash("1 dashboard · 2 runs · 3 trust · enter open · n new run · a autonomy · q back/quit")
		return m, nil
	}
	switch m.screen {
	case dashboard:
		return m.keyDashboard(s)
	case runs:
		return m.keyRuns(k)
	case trust:
		var cmd tea.Cmd
		m.trustTable, cmd = m.trustTable.Update(k)
		return m, cmd
	case detail:
		return m.keyDetail(k)
	case pickJob:
		return m.keyPick(s)
	}
	return m, nil
}

func (m Model) keyDashboard(s string) (tea.Model, tea.Cmd) {
	switch s {
	case "j", "down":
		if m.repoCursor < len(m.fleet.Repos)-1 {
			m.repoCursor++
		}
	case "k", "up":
		if m.repoCursor > 0 {
			m.repoCursor--
		}
	case "n", "enter":
		if len(m.fleet.Repos) == 0 {
			return m, nil
		}
		m.pickRepo, _ = m.fleet.Repo(m.fleet.Repos[m.repoCursor].Name)
		if len(m.pickRepo.Jobs) == 0 {
			m.setFlash(m.pickRepo.Name + " has no jobs")
			return m, nil
		}
		m.pickIdx = 0
		m.back = dashboard
		m.screen = pickJob
	}
	return m, nil
}

func (m Model) keyRuns(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "enter":
		row := m.runTable.SelectedRow()
		if row == nil {
			return m, nil
		}
		return m.openDetail(row[0], runs)
	case "n":
		if len(m.fleet.Repos) == 0 {
			return m, nil
		}
		m.pickRepo, _ = m.fleet.Repo(m.fleet.Repos[m.repoCursor].Name)
		m.pickIdx = 0
		m.back = runs
		m.screen = pickJob
		return m, nil
	}
	var cmd tea.Cmd
	m.runTable, cmd = m.runTable.Update(k)
	return m, cmd
}

func (m Model) openDetail(id string, back screen) (tea.Model, tea.Cmd) {
	m.screen = detail
	m.back = back
	m.run = store.Run{ID: id}
	m.events = nil
	m.lastSq = 0
	m.follow = true
	m.view.SetContent("loading...")
	return m, m.loadDetail(id, 0)
}

func (m Model) keyDetail(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "esc":
		m.screen = m.back
		return m, nil
	case "f":
		m.follow = !m.follow
		if m.follow {
			m.view.GotoBottom()
		}
		return m, nil
	case "g":
		m.follow = false
		m.view.GotoTop()
		return m, nil
	case "G":
		m.follow = true
		m.view.GotoBottom()
		return m, nil
	case "k", "up", "pgup":
		m.follow = false
	}
	var cmd tea.Cmd
	m.view, cmd = m.view.Update(k)
	return m, cmd
}

func (m Model) keyPick(s string) (tea.Model, tea.Cmd) {
	switch s {
	case "esc":
		m.screen = m.back
	case "j", "down":
		if m.pickIdx < len(m.pickRepo.Jobs)-1 {
			m.pickIdx++
		}
	case "k", "up":
		if m.pickIdx > 0 {
			m.pickIdx--
		}
	case "h", "left":
		if m.repoCursor > 0 {
			m.repoCursor--
			m.pickRepo, _ = m.fleet.Repo(m.fleet.Repos[m.repoCursor].Name)
			m.pickIdx = 0
		}
	case "l", "right":
		if m.repoCursor < len(m.fleet.Repos)-1 {
			m.repoCursor++
			m.pickRepo, _ = m.fleet.Repo(m.fleet.Repos[m.repoCursor].Name)
			m.pickIdx = 0
		}
	case "enter":
		if len(m.pickRepo.Jobs) == 0 {
			return m, nil
		}
		job, repo := m.pickRepo.Jobs[m.pickIdx], m.pickRepo.Name
		src := m.src
		m.screen = runs
		return m, func() tea.Msg {
			id, err := src.Trigger(context.Background(), job, repo)
			if err != nil {
				return actionDone{err: err}
			}
			return actionDone{msg: fmt.Sprintf("started %s on %s as run %s", job, repo, id)}
		}
	}
	return m, nil
}

// helpers shared with views

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t).Round(time.Minute)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return t.Format("Jan 2")
}

func shortPR(url string) string {
	if url == "" {
		return ""
	}
	if i := strings.LastIndex(url, "/pull/"); i >= 0 {
		return "#" + url[i+6:]
	}
	return url
}
