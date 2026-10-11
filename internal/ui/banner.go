package ui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// bannerLines is MARSHAL in a block font. Six rows, every row the same
// width, so the gradient and the right-aligned status line up.
var bannerLines = []string{
	"███╗   ███╗ █████╗ ██████╗ ███████╗██╗  ██╗ █████╗ ██╗     ",
	"████╗ ████║██╔══██╗██╔══██╗██╔════╝██║  ██║██╔══██╗██║     ",
	"██╔████╔██║███████║██████╔╝███████╗███████║███████║██║     ",
	"██║╚██╔╝██║██╔══██║██╔══██╗╚════██║██╔══██║██╔══██║██║     ",
	"██║ ╚═╝ ██║██║  ██║██║  ██║███████║██║  ██║██║  ██║███████╗",
	"╚═╝     ╚═╝╚═╝  ╚═╝╚═╝  ╚═╝╚══════╝╚═╝  ╚═╝╚═╝  ╚═╝╚══════╝",
}

const tagline = "Maintains · Audits · Repairs · Secures · Hardens · And · Learns"

// bannerWidth is the rendered width of one banner row.
var bannerWidth = lipgloss.Width(bannerLines[0])

// bannerHeight is rows plus the tagline.
var bannerHeight = len(bannerLines) + 1

// showBanner decides whether there is room for the art. Below this the
// header falls back to a one-line title so the body keeps its space.
func showBanner(width, height int) bool {
	return width >= bannerWidth+2 && height >= 28
}

// renderBanner draws the art with a vertical gradient and the tagline.
func renderBanner(width int) string {
	var b strings.Builder
	for i, line := range bannerLines {
		c := gradient[i%len(gradient)]
		b.WriteString(" " + lipgloss.NewStyle().Foreground(c).Render(line) + "\n")
	}
	b.WriteString(" " + sTagline.Render(tagline))
	return b.String()
}
