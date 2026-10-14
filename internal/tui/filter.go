package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

const (
	pickerTitle = "Filter by tag"
	pickerHints = "enter apply · esc cancel"
	allPosts    = "All posts"
)

// tagPicker is the popup that filters the river by tag. Its first choice, "", is the whole river.
type tagPicker struct {
	tags   []string
	cursor int
}

// openPicker lists the configured tags, starting on the filter shown, or else on the first tag.
func (m *model) openPicker() {
	tags := m.river.Tags()
	if len(tags) == 0 {
		m.notice = "No tags yet. Tag a feed in config.toml, or when adding one with n."
		return
	}
	p := &tagPicker{tags: append([]string{""}, tags...), cursor: 1}
	for i, tag := range p.tags {
		if tag == m.tag && tag != "" {
			p.cursor = i
		}
	}
	m.picker = p
}

// pickerKey moves through the picker: Enter applies the choice, and Esc or q closes it.
func (m *model) pickerKey(msg tea.KeyPressMsg) tea.Cmd {
	p := m.picker
	switch msg.String() {
	case "ctrl+c":
		return tea.Quit
	case "ctrl+z":
		return tea.Suspend
	case "esc", "q":
		m.picker = nil
	case "j", "down":
		p.cursor = min(p.cursor+1, len(p.tags)-1)
	case "k", "up":
		p.cursor = max(p.cursor-1, 0)
	case "enter":
		m.picker = nil
		return m.filter(p.tags[p.cursor])
	}
	return nil
}

// view renders the picker at most width cells wide and, given a height, scrolled to fit it.
func (p *tagPicker) view(width, height int) string {
	frame := formStyle.GetHorizontalFrameSize()
	inner := max(ansi.StringWidth(pickerTitle), ansi.StringWidth(pickerHints))
	for _, tag := range p.tags {
		inner = max(inner, ansi.StringWidth(Sanitize(tag))+2)
	}
	inner = min(inner, max(width-frame, 1))

	shown := len(p.tags)
	if height > 0 {
		// The border, the title and a blank line above the choices, and a blank line and hints below.
		shown = max(min(shown, height-formStyle.GetVerticalFrameSize()-4), 1)
	}
	first := max(p.cursor-shown+1, 0)
	lines := []string{headerStyle.Render(fit(pickerTitle, inner)), ""}
	for i := first; i < first+shown; i++ {
		label := Sanitize(p.tags[i])
		if label == "" {
			label = allPosts
		}
		if i == p.cursor {
			lines = append(lines, selectedStyle.Render(pad(fit("▸ "+label, inner), inner)))
		} else {
			lines = append(lines, fit("  "+label, inner))
		}
	}
	lines = append(lines, "", dimStyle.Render(fit(pickerHints, inner)))
	return formStyle.Width(inner + frame).Render(strings.Join(lines, "\n"))
}
