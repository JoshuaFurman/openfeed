package tui

import (
	"fmt"
	"image/color"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

const (
	ageWidth     = 8 // Fits "Jan 2006", the widest age.
	tagSeparator = ", "
	title        = " openfeed"
	hints        = "j/k move · g newest · o open · / tag · n add · q quit "
)

var (
	headerStyle   = lipgloss.NewStyle().Bold(true)
	selectedStyle = lipgloss.NewStyle().Reverse(true)
	dimStyle      = lipgloss.NewStyle().Faint(true)

	// tagPalette colors tags in config order from the terminal's own 16 colors, which follow its theme.
	// It leaves out black, white, and the grays, as one of them is always close to the background.
	tagPalette = []color.Color{
		lipgloss.Cyan, lipgloss.Magenta, lipgloss.Yellow, lipgloss.Green, lipgloss.Blue, lipgloss.Red,
		lipgloss.BrightCyan, lipgloss.BrightMagenta, lipgloss.BrightYellow, lipgloss.BrightGreen,
		lipgloss.BrightBlue, lipgloss.BrightRed,
	}
)

func (m *model) View() tea.View {
	var lines []string
	if m.chrome() {
		lines = append(lines, m.header())
	}
	switch {
	case m.feedCount == 0:
		lines = append(lines, fit(" No subscriptions yet. Press n to add a feed.", m.width))
	case !m.loaded:
		lines = append(lines, fit(" Loading…", m.width))
	case m.headFailed:
		lines = append(lines, fit(" Could not read the river. Press g to retry.", m.width))
	case len(m.rows) == 0 && m.tag != "":
		lines = append(lines, fit(" No posts tagged "+Sanitize(m.tag)+". Press Esc for all posts.", m.width))
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
	switch {
	case m.form != nil:
		box, cursor := m.form.view(m.spinner.View())
		view.Content, view.Cursor = m.overlay(view.Content, box, cursor)
	case m.picker != nil:
		view.Content, _ = m.overlay(view.Content, m.picker.view(m.formWidth(), m.height, m.tagStyle), nil)
	}
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

// header shows the title, the tag filter, the spinner while refreshing, and the key hints right-aligned
// when they fit.
func (m *model) header() string {
	left := headerStyle.Render(title)
	if m.tag != "" {
		left += " " + m.tagStyle(Sanitize(m.tag)).Render("#"+Sanitize(m.tag))
	}
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
	selected := i == m.cursor
	marker := "  "
	if selected {
		marker = "▸ "
	}
	text := r.title
	if text == "" {
		text = "(untitled)"
	}
	line := marker + pad(age(r.date, m.now()), ageWidth) + "  " +
		pad(ansi.Truncate(r.feed, m.nameWidth, "…"), m.nameWidth) + "  "
	if width := m.tagColumn(); width > 0 {
		line += pad(ansi.Truncate(m.tagList(r.tags, selected), width, "…"), width) + "  "
	}
	line = fit(line+text, m.width)
	if selected {
		return selectedStyle.Render(pad(line, m.width))
	}
	return line
}

// tagColumn is the tag column's width: zero when no feed has tags, or when the terminal is too narrow to
// keep minTitle cells of each title beside it.
func (m *model) tagColumn() int {
	before := 2 + ageWidth + 2 + m.nameWidth + 2 // The marker, the age, and the name, each with its gap.
	if m.tagWidth == 0 || (m.width > 0 && m.width-before-m.tagWidth-2 < minTitle) {
		return 0
	}
	return m.tagWidth
}

// tagList joins tags, each in its color unless plain. The selected row draws them plain, because a
// color's reset would end its highlight.
func (m *model) tagList(tags []string, plain bool) string {
	if plain {
		return strings.Join(tags, tagSeparator)
	}
	painted := make([]string, len(tags))
	for i, tag := range tags {
		painted[i] = m.tagStyle(tag).Render(tag)
	}
	return strings.Join(painted, tagSeparator)
}

// tagStyle colors text as a sanitized tag; an unknown tag stays plain.
func (m *model) tagStyle(tag string) lipgloss.Style {
	style := lipgloss.NewStyle()
	if c, ok := m.tagColors[tag]; ok {
		style = style.Foreground(c)
	}
	return style
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
