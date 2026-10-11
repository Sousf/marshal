package ui

import "github.com/charmbracelet/lipgloss"

// One palette, adaptive to light and dark terminals. Purple is the brand,
// green/amber/red carry meaning and nothing else does, so a glance at any
// screen says what is healthy and what wants attention.
var (
	accent  = lipgloss.AdaptiveColor{Light: "#5a32d6", Dark: "#b39dff"}
	accent2 = lipgloss.AdaptiveColor{Light: "#7c5cff", Dark: "#9d86ff"}
	accent3 = lipgloss.AdaptiveColor{Light: "#9b86ff", Dark: "#8670ea"}
	accent4 = lipgloss.AdaptiveColor{Light: "#b7a7ff", Dark: "#6f5bd0"}
	fg      = lipgloss.AdaptiveColor{Light: "#1f1f1f", Dark: "#e6e6e6"}
	dim     = lipgloss.AdaptiveColor{Light: "#6b6b6b", Dark: "#8a8a8a"}
	faint   = lipgloss.AdaptiveColor{Light: "#b0b0b0", Dark: "#4a4a4a"}
	good    = lipgloss.AdaptiveColor{Light: "#1a7f37", Dark: "#56d364"}
	warn    = lipgloss.AdaptiveColor{Light: "#9a6700", Dark: "#e3b341"}
	bad     = lipgloss.AdaptiveColor{Light: "#cf222e", Dark: "#ff7b72"}
	selBG   = lipgloss.AdaptiveColor{Light: "#ece7ff", Dark: "#2d2545"}

	sTitle   = lipgloss.NewStyle().Bold(true).Foreground(accent)
	sDim     = lipgloss.NewStyle().Foreground(dim)
	sFaint   = lipgloss.NewStyle().Foreground(faint)
	sGood    = lipgloss.NewStyle().Foreground(good)
	sWarn    = lipgloss.NewStyle().Foreground(warn)
	sBad     = lipgloss.NewStyle().Foreground(bad)
	sKey     = lipgloss.NewStyle().Foreground(accent).Bold(true)
	sTabOn   = lipgloss.NewStyle().Bold(true).Foreground(fg).Background(selBG).Padding(0, 1)
	sTabOff  = lipgloss.NewStyle().Foreground(dim).Padding(0, 1)
	sHeader  = lipgloss.NewStyle().Bold(true).Foreground(dim)
	sRow     = lipgloss.NewStyle()
	sRowSel  = lipgloss.NewStyle().Background(selBG).Bold(true)
	sCard    = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(faint).Padding(0, 1)
	sCardOn  = sCard.BorderForeground(accent)
	sModal   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(accent).Padding(1, 2)
	sPill    = lipgloss.NewStyle().Padding(0, 1).Foreground(lipgloss.Color("0"))
	sTagline = lipgloss.NewStyle().Foreground(dim).Italic(true)
	sRule    = lipgloss.NewStyle().Foreground(faint)
)

// gradient colors the banner line by line.
var gradient = []lipgloss.AdaptiveColor{accent, accent, accent2, accent3, accent4, accent4}

func levelStyle(l string) lipgloss.Style {
	switch l {
	case "auto":
		return sGood
	case "soak":
		return sWarn
	case "locked":
		return sBad
	}
	return lipgloss.NewStyle().Foreground(fg)
}

func levelPill(l string) string {
	var bg lipgloss.AdaptiveColor
	switch l {
	case "auto":
		bg = good
	case "soak":
		bg = warn
	case "locked":
		bg = bad
	default:
		bg = dim
	}
	return sPill.Background(bg).Render(l)
}
