package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

const (
	ageWidth = 8 // Fits "Jan 2006", the widest age.
	title    = " openfeed"
	hints    = "j/k move · g newest · o open · q quit "
)

var (
	headerStyle   = lipgloss.NewStyle().Bold(true)
	selectedStyle = lipgloss.NewStyle().Reverse(true)
	dimStyle      = lipgloss.NewStyle().Faint(true)
)

func (m *model) View() tea.View {
	var lines []string
	if m.chrome() {
		lines = append(lines, m.header())
	}
	switch {
	case !m.loaded:
		lines = append(lines, fit(" Loading…", m.width))
	case m.headFailed:
		lines = append(lines, fit(" Could not read the river. Press g to retry.", m.width))
	case len(m.rows) == 0 && m.refreshing:
		lines = append(lines, fit(" No posts yet. Refreshing…", m.width))
	case len(m.rows) == 0:
		lines = append(lines, fit(" No posts yet. Press k twice to refresh.", m.width))
	}
	body := m.visible() // A state message above takes one of these lines.
	if m.chrome() {
		body++ // Below the header.
	}
	for i := m.offset; i <= len(m.rows) && len(lines) < body; i++ {
		// The divider gives way when drawing it would push the cursor's row off screen.
		if m.dividerBefore(i) && (i > m.cursor || body-len(lines)-1 >= m.cursor-i+1) {
			lines = append(lines, m.dividerLine())
		}
		if i < len(m.rows) && len(lines) < body {
			lines = append(lines, m.row(i))
		}
	}
	for len(lines) < body {
		lines = append(lines, "")
	}
	if m.chrome() {
		lines = append(lines, dimStyle.Render(fit(" "+m.status(), m.width)))
	}

	view := tea.NewView(strings.Join(lines, "\n"))
	view.AltScreen = true
	return view
}

// dividerBefore reports whether the saved position falls directly above row i, where i past the last row
// means below it. Where it falls among posts not loaded yet, it is not drawn.
func (m *model) dividerBefore(i int) bool {
	if m.divider == nil || len(m.rows) == 0 {
		return false
	}
	newerAbove := i > 0 && m.rows[i-1].position.Compare(*m.divider) > 0
	if i == 0 {
		newerAbove = m.newer == nil // Nothing newer exists above the first row.
	}
	if i == len(m.rows) {
		return newerAbove && m.older == nil
	}
	return newerAbove && m.rows[i].position.Compare(*m.divider) <= 0
}

func (m *model) dividerLine() string {
	label := "── You left off here "
	return dimStyle.Render(fit(label+strings.Repeat("─", max(m.width-ansi.StringWidth(label), 0)), m.width))
}

// header shows the title, the spinner while refreshing, and the key hints right-aligned when they fit.
func (m *model) header() string {
	left := headerStyle.Render(title)
	if m.refreshing {
		left += " " + m.spinner.View() + dimStyle.Render(" refreshing")
	}
	gap := m.width - ansi.StringWidth(left) - ansi.StringWidth(hints)
	if gap < 2 {
		return fit(left, m.width)
	}
	return left + strings.Repeat(" ", gap) + dimStyle.Render(hints)
}

func (m *model) row(i int) string {
	r := m.rows[i]
	marker := "  "
	if i == m.cursor {
		marker = "▸ "
	}
	text := r.title
	if text == "" {
		text = "(untitled)"
	}
	line := fit(marker+pad(age(r.date, m.now()), ageWidth)+"  "+
		pad(ansi.Truncate(r.feed, m.nameWidth, "…"), m.nameWidth)+"  "+text, m.width)
	if i == m.cursor {
		return selectedStyle.Render(pad(line, m.width))
	}
	return line
}

// status names the launch failures; one failure shows its reason, and more point to the log.
func (m *model) status() string {
	switch {
	case m.notice != "":
		return Sanitize(m.notice)
	case len(m.failing) == 1:
		return Sanitize(m.failing[0].Feed.Name + " failed: " + m.failing[0].Err.Error())
	case len(m.failing) > 1:
		names := make([]string, len(m.failing))
		for i, result := range m.failing {
			names[i] = result.Feed.Name
		}
		return Sanitize(fmt.Sprintf("%d feeds failing: %s (details in the log)", len(m.failing), strings.Join(names, ", ")))
	}
	return ""
}

// age is how long ago a post's river date was, in the most compact useful unit.
func age(date, now time.Time) string {
	elapsed := now.Sub(date)
	switch {
	case elapsed < time.Minute:
		return "now"
	case elapsed < time.Hour:
		return fmt.Sprintf("%dm", int(elapsed.Minutes()))
	case elapsed < 24*time.Hour:
		return fmt.Sprintf("%dh", int(elapsed.Hours()))
	case elapsed < 7*24*time.Hour:
		return fmt.Sprintf("%dd", int(elapsed.Hours()/24))
	case date.Local().Year() == now.Local().Year():
		return date.Local().Format("Jan 2")
	}
	return date.Local().Format("Jan 2006")
}

// fit truncates a line to the terminal width in display cells; a zero width means not yet known.
func fit(s string, width int) string {
	if width <= 0 {
		return s
	}
	return ansi.Truncate(s, width, "…")
}

// pad right-pads s to width display cells.
func pad(s string, width int) string {
	return s + strings.Repeat(" ", max(width-ansi.StringWidth(s), 0))
}
