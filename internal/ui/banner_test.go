package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

func TestBannerShape(t *testing.T) {
	for i, l := range bannerLines {
		if w := lipgloss.Width(l); w != bannerWidth {
			t.Errorf("line %d is %d wide, want %d", i, w, bannerWidth)
		}
	}
	if len(bannerLines) != 6 {
		t.Errorf("block font has 6 rows, got %d", len(bannerLines))
	}
	out := renderBanner(120)
	if strings.Count(out, "\n") != bannerHeight-1 {
		t.Errorf("rendered banner has %d newlines, want %d", strings.Count(out, "\n"), bannerHeight-1)
	}
	if !strings.Contains(out, "Learns") {
		t.Error("tagline missing")
	}
	if showBanner(60, 40) || !showBanner(120, 40) || showBanner(120, 20) {
		t.Error("showBanner thresholds wrong")
	}
}
