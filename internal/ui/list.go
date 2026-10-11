package ui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// column describes one list column. Width 0 means take what is left.
type column struct {
	title string
	width int
	right bool
}

// list is a cursor over rows with a scrolling window. The stock table
// cannot carry per-cell colour under a selected row, and status colour is
// the whole point of the runs screen, so this draws its own.
type list struct {
	cols   []column
	rows   [][]string
	cursor int
	offset int
	height int
	width  int
}

func (l *list) setRows(rows [][]string) {
	l.rows = rows
	if l.cursor >= len(rows) {
		l.cursor = max(len(rows)-1, 0)
	}
	l.clamp()
}

func (l *list) move(by int) {
	l.cursor += by
	if l.cursor < 0 {
		l.cursor = 0
	}
	if l.cursor >= len(l.rows) {
		l.cursor = max(len(l.rows)-1, 0)
	}
	l.clamp()
}

func (l *list) top()    { l.cursor = 0; l.clamp() }
func (l *list) bottom() { l.cursor = max(len(l.rows)-1, 0); l.clamp() }

func (l *list) clamp() {
	body := l.height - 1 // header row
	if body < 1 {
		body = 1
	}
	if l.cursor < l.offset {
		l.offset = l.cursor
	}
	if l.cursor >= l.offset+body {
		l.offset = l.cursor - body + 1
	}
	if l.offset < 0 {
		l.offset = 0
	}
}

// selected returns the row under the cursor, or nil.
func (l *list) selected() []string {
	if l.cursor < 0 || l.cursor >= len(l.rows) {
		return nil
	}
	return l.rows[l.cursor]
}

// widths resolves the flexible column against the list width.
func (l *list) widths() []int {
	ws := make([]int, len(l.cols))
	used := 0
	flex := -1
	for i, c := range l.cols {
		ws[i] = c.width
		if c.width == 0 {
			flex = i
		} else {
			used += c.width + 1
		}
	}
	if flex >= 0 {
		ws[flex] = max(l.width-used-1, 8)
	}
	return ws
}

// render draws header plus the visible window, every row padded to width so
// the selection bar spans the whole line.
func (l *list) render(empty string) string {
	ws := l.widths()
	var b strings.Builder
	var hdr []string
	for i, c := range l.cols {
		hdr = append(hdr, fit(c.title, ws[i], c.right))
	}
	b.WriteString(" " + sHeader.Render(strings.Join(hdr, " ")) + "\n")
	body := l.height - 1
	if len(l.rows) == 0 {
		b.WriteString("\n " + sDim.Render(empty))
		return b.String()
	}
	end := min(l.offset+body, len(l.rows))
	for r := l.offset; r < end; r++ {
		var cells []string
		for i := range l.cols {
			v := ""
			if i < len(l.rows[r]) {
				v = l.rows[r][i]
			}
			cells = append(cells, fit(v, ws[i], l.cols[i].right))
		}
		line := strings.Join(cells, " ")
		if r == l.cursor {
			line = sRowSel.Render(sKey.Render("▌") + line)
		} else {
			line = sRow.Render(" " + line)
		}
		b.WriteString(line)
		if r < end-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// fit pads or truncates a possibly-styled string to width, ANSI aware.
func fit(s string, width int, right bool) string {
	if width <= 0 {
		return ""
	}
	w := ansi.StringWidth(s)
	if w > width {
		return ansi.Truncate(s, width, "…")
	}
	pad := strings.Repeat(" ", width-w)
	if right {
		return pad + s
	}
	return s + pad
}
