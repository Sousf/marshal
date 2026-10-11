package ui

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"

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
)

type modal int

const (
	none modal = iota
	modalAutonomy
	modalPick
	modalHelp
	modalTask
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
	modal  modal
	width  int
	height int

	status api.Status
	srcErr string
	runs   []store.Run
	trust  []store.Trust
	flash  string
	flashT time.Time
	spin   spinner.Model

	repoCursor int
	runList    list
	trustList  list
	filter     textinput.Model
	filtering  bool
	visible    []store.Run // runs after filtering, aligned with runList.rows

	// detail
	run     store.Run
	verdict *store.Verdict
	events  []store.Event
	lastSq  int64
	view    viewport.Model
	follow  bool

	// pick modal
	pickRepo config.Repo
	pickIdx  int

	// task modal
	taskInput textinput.Model
	taskRepo  int // index into fleet repos, -1 for none

	// preview is the selected run's log under the list.
	preview struct {
		id     string
		run    store.Run
		events []store.Event
	}
}

// New builds the model.
func New(src Source, fleet *config.Fleet, version string) Model {
	m := Model{src: src, fleet: fleet, version: version, follow: true}
	m.runList = list{cols: []column{
		{title: "RUN", width: 8}, {title: "WHEN", width: 8}, {title: "REPO", width: 16},
		{title: "JOB", width: 16}, {title: "STATUS", width: 10}, {title: "TURNS", width: 5, right: true},
		{title: "USD", width: 6, right: true}, {title: "PR", width: 0},
	}}
	m.trustList = list{cols: []column{
		{title: "REPO", width: 18}, {title: "CLASS", width: 14}, {title: "LEVEL", width: 8},
		{title: "STREAK", width: 6, right: true}, {title: "APPROVED", width: 8, right: true},
		{title: "REJECTED", width: 8, right: true}, {title: "REVERTED", width: 8, right: true}, {title: "", width: 0},
	}}
	m.filter = textinput.New()
	m.filter.Prompt = "/ "
	m.filter.Placeholder = "filter by repo, job, status, run id"
	m.filter.PromptStyle = sKey
	m.taskInput = textinput.New()
	m.taskInput.Prompt = "> "
	m.taskInput.Placeholder = "what should MARSHAL do?"
	m.taskInput.PromptStyle = sKey
	m.taskInput.CharLimit = 2000
	m.taskRepo = -1
	m.spin = spinner.New(spinner.WithSpinner(spinner.MiniDot), spinner.WithStyle(sGood))
	m.view = viewport.New(80, 20)
	return m
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
	run     store.Run
	events  []store.Event
	verdict *store.Verdict
	err     error
}

type previewLoaded struct {
	id     string
	run    store.Run
	events []store.Event
}

type actionDone struct {
	msg string
	err error
}

type tick struct{}

func (m Model) Init() tea.Cmd {
	return tea.Batch(m.refresh(), tickCmd(), m.spin.Tick)
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
		if r.runs, err = src.Runs(ctx, "", 500); err != nil {
			r.err = err
		}
		if r.trust, err = src.Trust(ctx); err != nil {
			r.err = err
		}
		return r
	}
}

// previewRows is how many lines the log pane gets; zero means no pane.
func (m Model) previewRows() int {
	body := m.bodyHeight()
	if body < 14 {
		return 0
	}
	return body * 2 / 5
}

// loadPreview fetches the selected run's events for the pane.
func (m Model) loadPreview() tea.Cmd {
	if m.screen != runs || m.previewRows() == 0 || m.runList.selected() == nil {
		return nil
	}
	id := m.visible[m.runList.cursor].ID
	src := m.src
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		run, err := src.Run(ctx, id)
		if err != nil {
			return nil
		}
		ev, _ := src.EventsAfter(ctx, id, 0)
		return previewLoaded{id: id, run: run, events: ev}
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
		d := detailLoaded{run: run, events: ev, err: err}
		if v, ok, verr := src.Verdict(ctx, run.ID); verr == nil && ok {
			d.verdict = &v
		}
		return d
	}
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		if m.screen == detail {
			m.view.SetContent(m.renderDetail())
		}
		return m, nil
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		// Row strings carry the spinner frame baked in, so rebuild them
		// while anything is running; otherwise the list spinners freeze
		// between data refreshes.
		if m.anyRunning() {
			m.fillLists()
		}
		return m, cmd
	case tick:
		cmds := []tea.Cmd{m.refresh(), tickCmd()}
		if m.screen == detail && (m.run.Status == store.Running || m.run.Status == "") {
			cmds = append(cmds, m.loadDetail(m.run.ID, m.lastSq))
		}
		if m.screen == runs {
			cmds = append(cmds, m.loadPreview())
		}
		return m, tea.Batch(cmds...)
	case previewLoaded:
		m.preview.id, m.preview.run, m.preview.events = msg.id, msg.run, msg.events
		return m, nil
	case refreshed:
		if msg.err != nil {
			m.srcErr = msg.err.Error()
		} else {
			m.srcErr = ""
		}
		m.status, m.runs, m.trust = msg.status, msg.runs, msg.trust
		m.fillLists()
		if m.screen == runs && m.preview.id == "" {
			return m, m.loadPreview()
		}
		return m, nil
	case detailLoaded:
		if msg.err != nil {
			m.setFlash(msg.err.Error())
			return m, nil
		}
		first := m.lastSq == 0 && len(m.events) == 0
		m.run = msg.run
		m.verdict = msg.verdict
		if len(msg.events) > 0 {
			m.events = append(m.events, msg.events...)
			m.lastSq = msg.events[len(msg.events)-1].Seq
		}
		if first {
			// A live run is read from the bottom as it streams; a finished
			// one from the top, where the status and any error are.
			m.follow = m.run.Status == store.Running
		}
		m.view.SetContent(m.renderDetail())
		if m.follow {
			m.view.GotoBottom()
		} else if first {
			m.view.GotoTop()
		}
		return m, nil
	case actionDone:
		if msg.err != nil {
			m.setFlash("✗ " + msg.err.Error())
		} else {
			m.setFlash("✓ " + msg.msg)
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

// bodyHeight is what is left for the main area after the chrome.
func (m Model) bodyHeight() int {
	h := m.height - 4 // status, tabs, blank, footer
	if showBanner(m.width, m.height) {
		h -= bannerHeight + 1
	} else {
		h -= 1 // compact title
	}
	if m.filtering || m.filter.Value() != "" {
		h--
	}
	return max(h, 3)
}

func (m *Model) layout() {
	if m.width == 0 {
		return
	}
	body := m.bodyHeight()
	m.runList.width = m.width - 2
	m.runList.height = body
	if p := m.previewRows(); p > 0 {
		m.runList.height = body - p - 1 // one row for the rule
	}
	m.trustList.width = m.width - 2
	m.trustList.height = body
	m.view.Width = m.width - 2
	m.view.Height = body
	m.filter.Width = m.width - 6
}

func (m *Model) fillLists() {
	q := strings.ToLower(strings.TrimSpace(m.filter.Value()))
	m.visible = m.visible[:0]
	rows := make([][]string, 0, len(m.runs))
	for _, r := range m.runs {
		if q != "" && !strings.Contains(strings.ToLower(r.Repo+" "+r.Job+" "+string(r.Status)+" "+r.ID), q) {
			continue
		}
		m.visible = append(m.visible, r)
		st := statusCell(r.Status, m.spin.View())
		job := r.Job
		if r.Parent != "" {
			job = sDim.Render("↳ ") + job
		}
		repoName := r.Repo
		if repoName == "" {
			repoName = sDim.Render("(no repo)")
		}
		rows = append(rows, []string{r.ID, sDim.Render(ago(r.StartedAt)), repoName, job, st,
			fmt.Sprint(r.Turns), fmt.Sprintf("%.2f", r.CostUSD), sDim.Render(shortPR(r.PRURL))})
	}
	m.runList.setRows(rows)
	trows := make([][]string, 0, len(m.trust))
	for _, t := range m.trust {
		trows = append(trows, []string{t.Repo, t.Class, levelStyle(t.Level).Render(t.Level), fmt.Sprint(t.Streak),
			fmt.Sprint(t.Approvals), fmt.Sprint(t.Rejections), fmt.Sprint(t.Reverts), ""})
	}
	m.trustList.setRows(trows)
}

func (m Model) anyRunning() bool {
	if len(m.status.Running) > 0 {
		return true
	}
	for _, r := range m.runs {
		if r.Status == store.Running {
			return true
		}
	}
	return false
}

func statusCell(s store.Status, spin string) string {
	switch s {
	case store.Running:
		return sGood.Render(spin + " running")
	case store.Merged:
		return sGood.Render("● merged")
	case store.Proposed:
		return sWarn.Render("◆ proposed")
	case store.Committed:
		return sGood.Render("● committed")
	case store.Pushed:
		return sGood.Render("● pushed")
	case store.Reported:
		return sGood.Render("● reported")
	case store.NoChange:
		return sDim.Render("○ no change")
	case store.Failed:
		return sBad.Render("✗ failed")
	case store.Blocked:
		return sBad.Render("⊘ blocked")
	case store.Rejected:
		return sBad.Render("✗ rejected")
	case store.Cancelled:
		return sDim.Render("– cancelled")
	}
	return string(s)
}

// key routing

func (m Model) key(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	s := k.String()
	if m.modal != none {
		return m.keyModal(k)
	}
	if m.filtering {
		return m.keyFilter(k)
	}
	switch s {
	case "ctrl+c":
		return m, tea.Quit
	case "q", "esc":
		if m.screen == detail {
			m.screen = runs
			return m, nil
		}
		if s == "q" {
			return m, tea.Quit
		}
		return m, nil
	case "?":
		m.modal = modalHelp
		return m, nil
	case "1":
		return m.goTo(dashboard)
	case "2":
		return m.goTo(runs)
	case "3":
		return m.goTo(trust)
	case "tab", "shift+tab":
		cur := m.screen
		if cur == detail {
			cur = runs
		}
		if s == "tab" {
			cur = (cur + 1) % 3
		} else {
			cur = (cur + 2) % 3
		}
		return m.goTo(cur)
	case "R":
		m.setFlash("refreshing")
		return m, m.refresh()
	case "a":
		m.modal = modalAutonomy
		return m, nil
	case "n":
		return m.openPick(m.repoCursor)
	case "t":
		m.modal = modalTask
		m.taskInput.Reset()
		m.taskRepo = -1
		if m.screen == dashboard && len(m.fleet.Repos) > 0 {
			m.taskRepo = m.repoCursor
		}
		m.taskInput.Width = max(m.width-12, 20)
		return m, m.taskInput.Focus()
	}
	switch m.screen {
	case dashboard:
		return m.keyDashboard(s)
	case runs:
		return m.keyRuns(s)
	case trust:
		return m.keyList(trust, s)
	case detail:
		return m.keyDetail(k)
	}
	return m, nil
}

func (m Model) goTo(s screen) (tea.Model, tea.Cmd) {
	m.screen = s
	m.layout()
	if s == runs {
		return m, m.loadPreview()
	}
	return m, nil
}

// keyList moves the cursor of the screen's list. It picks the list off its
// own receiver: a *list into the caller's copy would move the caller's
// copy while this copy, the one returned, stayed put. That was a bug once.
func (m Model) keyList(which screen, s string) (tea.Model, tea.Cmd) {
	l := &m.runList
	if which == trust {
		l = &m.trustList
	}
	before := l.cursor
	switch s {
	case "j", "down":
		l.move(1)
	case "k", "up":
		l.move(-1)
	case "g", "home":
		l.top()
	case "G", "end":
		l.bottom()
	case "pgdown", "ctrl+d":
		l.move(max(l.height-2, 1))
	case "pgup", "ctrl+u":
		l.move(-max(l.height-2, 1))
	}
	if which == runs && l.cursor != before {
		return m, m.loadPreview()
	}
	return m, nil
}

func (m Model) keyDashboard(s string) (tea.Model, tea.Cmd) {
	switch s {
	case "j", "down", "l", "right":
		if m.repoCursor < len(m.fleet.Repos)-1 {
			m.repoCursor++
		}
	case "k", "up", "h", "left":
		if m.repoCursor > 0 {
			m.repoCursor--
		}
	case "enter":
		return m.openPick(m.repoCursor)
	case "o":
		// Jump to this repo's runs.
		if len(m.fleet.Repos) > 0 {
			m.filter.SetValue(m.fleet.Repos[m.repoCursor].Name)
			m.fillLists()
			return m.goTo(runs)
		}
	}
	return m, nil
}

func (m Model) keyRuns(s string) (tea.Model, tea.Cmd) {
	switch s {
	case "enter":
		if m.runList.selected() == nil {
			return m, nil
		}
		return m.openDetail(m.visible[m.runList.cursor].ID)
	case "/":
		m.filtering = true
		m.layout()
		return m, m.filter.Focus()
	case "o":
		if m.runList.selected() != nil {
			return m, openPR(m.visible[m.runList.cursor])
		}
		return m, nil
	case "x":
		if m.runList.selected() == nil {
			return m, nil
		}
		r := m.visible[m.runList.cursor]
		src := m.src
		return m, func() tea.Msg {
			id, err := src.Fix(context.Background(), r.ID)
			if err != nil {
				return actionDone{err: err}
			}
			return actionDone{msg: fmt.Sprintf("fix run %s started for %s", id, r.ID)}
		}
	case "backspace":
		if m.filter.Value() != "" {
			m.filter.Reset()
			m.fillLists()
			m.layout()
		}
		return m, nil
	}
	return m.keyList(runs, s)
}

func (m Model) keyFilter(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "enter", "esc":
		m.filtering = false
		m.filter.Blur()
		if k.String() == "esc" {
			m.filter.Reset()
		}
		m.fillLists()
		m.layout()
		return m, nil
	}
	var cmd tea.Cmd
	m.filter, cmd = m.filter.Update(k)
	m.fillLists()
	return m, cmd
}

func (m Model) openDetail(id string) (tea.Model, tea.Cmd) {
	m.screen = detail
	m.run = store.Run{ID: id}
	m.verdict = nil
	m.events = nil
	m.lastSq = 0
	m.follow = true
	m.layout()
	m.view.SetContent(sDim.Render(" loading…"))
	return m, m.loadDetail(id, 0)
}

func (m Model) keyDetail(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "f":
		m.follow = !m.follow
		if m.follow {
			m.view.GotoBottom()
		}
		return m, nil
	case "g", "home":
		m.follow = false
		m.view.GotoTop()
		return m, nil
	case "G", "end":
		m.follow = true
		m.view.GotoBottom()
		return m, nil
	case "o":
		return m, openPR(m.run)
	case "k", "up", "pgup", "ctrl+u":
		m.follow = false
	}
	var cmd tea.Cmd
	m.view, cmd = m.view.Update(k)
	return m, cmd
}

func (m Model) openPick(repoIdx int) (tea.Model, tea.Cmd) {
	if len(m.fleet.Repos) == 0 {
		m.setFlash("no repositories in the fleet file")
		return m, nil
	}
	m.repoCursor = repoIdx
	m.pickRepo, _ = m.fleet.Repo(m.fleet.Repos[repoIdx].Name)
	m.pickIdx = 0
	m.modal = modalPick
	return m, nil
}

func (m Model) keyModal(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	s := k.String()
	switch m.modal {
	case modalHelp:
		m.modal = none
		return m, nil
	case modalTask:
		switch s {
		case "esc":
			m.modal = none
			m.taskInput.Blur()
			return m, nil
		case "ctrl+r":
			// Cycle the target: none, then each repo.
			m.taskRepo++
			if m.taskRepo >= len(m.fleet.Repos) {
				m.taskRepo = -1
			}
			return m, nil
		case "enter":
			prompt := strings.TrimSpace(m.taskInput.Value())
			if prompt == "" {
				return m, nil
			}
			repo := ""
			if m.taskRepo >= 0 {
				repo = m.fleet.Repos[m.taskRepo].Name
			}
			src := m.src
			m.modal = none
			m.taskInput.Blur()
			m.screen = runs
			m.layout()
			return m, func() tea.Msg {
				id, err := src.Task(context.Background(), prompt, repo)
				if err != nil {
					return actionDone{err: err}
				}
				return actionDone{msg: fmt.Sprintf("task started as run %s", id)}
			}
		}
		var cmd tea.Cmd
		m.taskInput, cmd = m.taskInput.Update(k)
		return m, cmd
	case modalAutonomy:
		m.modal = none
		if s == "y" || s == "Y" || s == "enter" {
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
		return m, nil
	case modalPick:
		switch s {
		case "esc", "q":
			m.modal = none
		case "j", "down":
			if m.pickIdx < len(m.pickRepo.Jobs)-1 {
				m.pickIdx++
			}
		case "k", "up":
			if m.pickIdx > 0 {
				m.pickIdx--
			}
		case "h", "left", "l", "right":
			d := 1
			if s == "h" || s == "left" {
				d = -1
			}
			n := m.repoCursor + d
			if n >= 0 && n < len(m.fleet.Repos) {
				m.repoCursor = n
				m.pickRepo, _ = m.fleet.Repo(m.fleet.Repos[n].Name)
				m.pickIdx = 0
			}
		case "enter":
			if len(m.pickRepo.Jobs) == 0 {
				return m, nil
			}
			job, repo := m.pickRepo.Jobs[m.pickIdx], m.pickRepo.Name
			src := m.src
			m.modal = none
			m.screen = runs
			m.layout()
			return m, func() tea.Msg {
				id, err := src.Trigger(context.Background(), job, repo)
				if err != nil {
					return actionDone{err: err}
				}
				return actionDone{msg: fmt.Sprintf("started %s on %s as run %s", job, repo, id)}
			}
		}
	}
	return m, nil
}

// openPR opens the run's pull request in the browser via gh, which knows
// the user's preferred browser and works over an ssh session with
// BROWSER set.
func openPR(r store.Run) tea.Cmd {
	if r.PRURL == "" {
		return func() tea.Msg { return actionDone{err: fmt.Errorf("run %s has no pull request", r.ID)} }
	}
	return func() tea.Msg {
		cmd := exec.Command("gh", "pr", "view", r.PRURL, "--web")
		if err := cmd.Run(); err != nil {
			return actionDone{err: fmt.Errorf("open %s: %v", r.PRURL, err)}
		}
		return actionDone{msg: "opened " + r.PRURL}
	}
}

// renderMarkdown renders the agent's report. Falls back to plain text if
// the renderer is unhappy, which beats showing nothing.
func renderMarkdown(md string, width int) string {
	r, err := glamour.NewTermRenderer(glamour.WithAutoStyle(), glamour.WithWordWrap(max(width-4, 20)))
	if err != nil {
		return md
	}
	out, err := r.Render(md)
	if err != nil {
		return md
	}
	return strings.TrimRight(out, "\n")
}

// helpers

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
