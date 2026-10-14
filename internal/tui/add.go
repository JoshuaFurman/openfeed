package tui

import (
	"context"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/JoshuaFurman/openfeed/internal/config"
)

const (
	maxFormWidth = 64 // Borders included.
	labelWidth   = 6
	formHints    = "enter add · tab next field · esc cancel"
)

// The popup's fields, in tab order.
const (
	urlField = iota
	nameField
	tagsField
	fieldCount
)

var (
	fieldLabels       = [fieldCount]string{"URL", "Name", "Tags"}
	fieldPlaceholders = [fieldCount]string{"https://example.com/feed.xml", "optional; defaults to the host", "optional, comma-separated"}
	formStyle         = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)
)

// subscribed answers a popup's submission; form identifies the popup that asked.
type subscribed struct {
	feed config.Feed
	err  error
	form int
}

// addForm is the popup that subscribes to a feed. While it shows, keys type into it.
type addForm struct {
	id       int
	width    int // Borders included.
	fields   [fieldCount]textinput.Model
	focus    int
	checking bool               // Subscribe is fetching the feed; only Esc applies meanwhile.
	cancel   context.CancelFunc // Abandons that check.
	err      string
}

func newAddForm(id, width int) *addForm {
	f := &addForm{id: id}
	for i := range f.fields {
		f.fields[i] = textinput.New()
		f.fields[i].Prompt = ""
		f.fields[i].Placeholder = fieldPlaceholders[i]
		f.fields[i].SetVirtualCursor(false) // The terminal's own cursor needs no blink timer.
	}
	f.fields[urlField].Focus()
	f.resize(width)
	return f
}

func (f *addForm) resize(width int) {
	f.width = width
	for i := range f.fields {
		// An input draws one cell past its width for the cursor.
		f.fields[i].SetWidth(max(width-formStyle.GetHorizontalFrameSize()-labelWidth-1, 1))
	}
}

func (f *addForm) focusOn(field int) {
	f.fields[f.focus].Blur()
	f.focus = (field + fieldCount) % fieldCount
	f.fields[f.focus].Focus()
}

// edit applies typing or pasting to the focused field, which holds only sanitized text.
func (f *addForm) edit(msg tea.Msg) tea.Cmd {
	field := &f.fields[f.focus]
	before := field.Value()
	var cmd tea.Cmd
	*field, cmd = field.Update(msg)
	if clean := Sanitize(field.Value()); clean != field.Value() {
		field.SetValue(clean)
	}
	if field.Value() != before {
		f.err = ""
	}
	return cmd
}

// feed is the subscription the popup describes, with tags split at commas.
func (f *addForm) feed() config.Feed {
	var tags []string
	for _, tag := range strings.Split(f.fields[tagsField].Value(), ",") {
		if tag = strings.TrimSpace(tag); tag != "" {
			tags = append(tags, tag)
		}
	}
	return config.Feed{
		URL:  strings.TrimSpace(f.fields[urlField].Value()),
		Name: strings.TrimSpace(f.fields[nameField].Value()),
		Tags: tags,
	}
}

// view renders the popup and where the terminal cursor sits within it, if it shows.
func (f *addForm) view(spinner string) (string, *tea.Cursor) {
	inner := f.width - formStyle.GetHorizontalFrameSize()
	lines := []string{headerStyle.Render("Add a feed"), ""}
	for i := range f.fields {
		label := dimStyle
		if i == f.focus {
			label = headerStyle
		}
		lines = append(lines, label.Render(pad(fieldLabels[i], labelWidth))+f.fields[i].View())
	}
	lines = append(lines, "")
	switch {
	case f.checking:
		lines = append(lines, fit(spinner+" Checking the feed…", inner))
	case f.err != "":
		lines = append(lines, lipgloss.NewStyle().Width(inner).Render(Sanitize(f.err)))
	}
	lines = append(lines, dimStyle.Render(fit(formHints, inner)))
	box := formStyle.Width(f.width).Render(strings.Join(lines, "\n"))

	cursor := f.fields[f.focus].Cursor()
	if f.checking || cursor == nil {
		return box, nil
	}
	cursor.X += formStyle.GetBorderLeftSize() + formStyle.GetPaddingLeft() + labelWidth
	cursor.Y += formStyle.GetBorderTopSize() + 2 + f.focus // Below the title and a blank line.
	return box, cursor
}

func (m *model) formWidth() int {
	if m.width == 0 {
		return maxFormWidth
	}
	return min(maxFormWidth, m.width)
}

func (m *model) openForm() {
	m.forms++
	m.form = newAddForm(m.forms, m.formWidth())
}

// closeForm hides the popup, abandoning a check in flight.
func (m *model) closeForm() {
	if m.form.cancel != nil {
		m.form.cancel()
	}
	m.form = nil
}

// formKey edits the popup: Enter subscribes, Tab and the arrows change fields, and Esc closes it.
func (m *model) formKey(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "ctrl+c":
		return tea.Quit
	case "ctrl+z":
		return tea.Suspend
	case "esc":
		m.closeForm()
		return nil
	}
	if m.form.checking {
		return nil
	}
	switch msg.String() {
	case "enter":
		return m.submit()
	case "tab", "down":
		m.form.focusOn(m.form.focus + 1)
	case "shift+tab", "up":
		m.form.focusOn(m.form.focus - 1)
	default:
		return m.form.edit(msg)
	}
	return nil
}

// submit subscribes to the popup's feed, which checks that it fetches; the answer arrives as subscribed.
func (m *model) submit() tea.Cmd {
	f := m.form
	input := f.feed()
	if input.URL == "" {
		f.err = "Enter the feed's URL."
		return nil
	}
	ctx, cancel := context.WithCancel(m.ctx)
	f.checking, f.cancel, f.err = true, cancel, ""
	id := f.id
	cmds := []tea.Cmd{m.work(func() tea.Msg {
		defer cancel()
		added, err := m.river.Subscribe(ctx, input)
		return subscribed{added, err, id}
	})}
	if m.animate {
		cmds = append(cmds, m.spinner.Tick)
	}
	return tea.Batch(cmds...)
}

// subscribed shows a popup's answer: an error stays in the popup, and a new feed joins the river.
// An abandoned popup's error is dropped, but its subscription, once made, still shows.
func (m *model) subscribed(msg subscribed) tea.Cmd {
	current := m.form != nil && m.form.id == msg.form
	if msg.err != nil {
		if current {
			m.form.checking, m.form.cancel, m.form.err = false, nil, msg.err.Error()
		}
		return nil
	}
	if current {
		m.form = nil
	}
	m.notice = "Subscribed to " + msg.feed.Name + "."
	m.sizeNames()
	return m.reload() // As after a refresh: the new feed's posts are reconciled into the span.
}

// overlay draws a popup's box centered over the river, moving its cursor, if any, into place on the screen.
func (m *model) overlay(river, box string, cursor *tea.Cursor) (string, *tea.Cursor) {
	if m.width == 0 {
		return box, cursor
	}
	x := max((m.width-lipgloss.Width(box))/2, 0)
	y := max((m.height-lipgloss.Height(box))/2, 0)
	// A compositor places layers at their offsets; a layer drawn alone fills the whole canvas.
	layers := lipgloss.NewCompositor(lipgloss.NewLayer(river), lipgloss.NewLayer(box).X(x).Y(y).Z(1))
	canvas := lipgloss.NewCanvas(m.width, max(m.height, lipgloss.Height(river)))
	if cursor != nil {
		cursor.X += x
		cursor.Y += y
	}
	return canvas.Compose(layers).Render(), cursor
}
