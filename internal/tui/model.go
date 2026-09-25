package tui

import (
	"context"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/JoshuaFurman/openfeed/internal/reader"
)

const (
	prefetch     = 10 // How close the cursor gets to a window edge before the next page loads.
	maxNameWidth = 20
)

// row is the part of a post the river displays, sanitized once when its page loads.
// Feed content stays in the store, so a long session holds only what is on the list.
type row struct {
	id       reader.ItemID
	position reader.Position
	date     time.Time
	feed     string
	title    string
	url      string
}

func newRow(item reader.Item) row {
	return row{
		id: item.ID, position: item.Position, date: item.EffectiveDate(),
		feed: Sanitize(item.FeedName), title: Sanitize(item.Title), url: item.URL,
	}
}

// Page messages carry the window generation that requested them; replacing the window drops stale pages.
type (
	headLoaded struct {
		page reader.Page
		err  error
	}
	olderLoaded struct {
		page   reader.Page
		err    error
		window int
	}
	newerLoaded struct {
		page   reader.Page
		err    error
		window int
	}
	opened  struct{ err error }
	ageTick struct{}
)

// model holds one contiguous span of the river, newest first, with the cursor on one post.
type model struct {
	ctx   context.Context
	river River
	open  func(url string) error
	now   func() time.Time
	tick  tea.Cmd // Redraws ages; nil in tests.

	window        int // Generation of the loaded span.
	rows          []row
	ids           map[reader.ItemID]bool
	older, newer  *reader.Position // Continuations past each edge; nil newer means the window reaches the head.
	loadingOlder  bool
	loadingNewer  bool
	loaded        bool
	headFailed    bool
	cursor        int
	offset        int // First visible row.
	width, height int
	nameWidth     int
	failing       []reader.FeedResult // Feeds that failed the launch refresh.
	notice        string              // Replaces the status line until the next key press.
}

func newModel(ctx context.Context, river River, launch reader.Report, open func(string) error, now func() time.Time) *model {
	m := &model{
		ctx: ctx, river: river, open: open, now: now, ids: make(map[reader.ItemID]bool),
		tick: tea.Every(time.Minute, func(time.Time) tea.Msg { return ageTick{} }),
	}
	// Sized from the configuration, so the column never shifts as pages load.
	for _, f := range river.Feeds() {
		m.nameWidth = min(max(m.nameWidth, ansi.StringWidth(Sanitize(f.Name))), maxNameWidth)
	}
	for _, result := range launch.Feeds {
		if result.Status == reader.Failing {
			m.failing = append(m.failing, result)
		}
	}
	return m
}

func (m *model) Init() tea.Cmd { return tea.Batch(m.loadHead(), m.tick) }

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.follow()
	case tea.KeyPressMsg:
		m.notice = ""
		return m, m.key(msg.String())
	case ageTick:
		return m, m.tick
	case headLoaded:
		m.loaded = true
		m.headFailed = msg.err != nil
		if msg.err != nil {
			m.notice = msg.err.Error()
			break
		}
		m.window++
		m.rows, m.ids = nil, make(map[reader.ItemID]bool)
		m.cursor, m.offset = 0, 0
		m.older, m.newer = msg.page.Next, nil
		m.loadingOlder, m.loadingNewer = false, false
		m.append(msg.page.Items)
		return m, m.prefetch()
	case olderLoaded:
		if msg.window != m.window {
			break
		}
		m.loadingOlder = false
		if msg.err != nil {
			m.notice = msg.err.Error()
			break
		}
		m.older = msg.page.Next
		m.append(msg.page.Items)
		return m, m.prefetch()
	case newerLoaded:
		if msg.window != m.window {
			break
		}
		m.loadingNewer = false
		if msg.err != nil {
			m.notice = msg.err.Error()
			break
		}
		m.newer = msg.page.Newer
		m.prepend(msg.page.Items)
		return m, m.prefetch()
	case opened:
		if msg.err != nil {
			m.notice = "Could not open the article: " + msg.err.Error()
		}
	}
	return m, nil
}

func (m *model) key(key string) tea.Cmd {
	switch key {
	case "q", "ctrl+c":
		return tea.Quit
	case "ctrl+z":
		return tea.Suspend // Raw mode delivers Ctrl-Z as a key rather than a stop signal.
	case "o", "enter":
		return m.openSelected()
	}
	if m.headFailed {
		return m.loadHead() // Any move retries a launch read that failed.
	}
	switch key {
	case "j", "down":
		if m.cursor < len(m.rows)-1 {
			m.cursor++
		}
	case "k", "up":
		if m.cursor > 0 {
			m.cursor--
		}
	case "g", "home":
		if m.newer != nil {
			return m.loadHead() // The head is not loaded; replace the window with it.
		}
		m.cursor = 0
	}
	m.follow()
	return m.prefetch()
}

func (m *model) openSelected() tea.Cmd {
	if len(m.rows) == 0 {
		return nil
	}
	url := m.rows[m.cursor].url
	if !strings.HasPrefix(url, "https://") && !strings.HasPrefix(url, "http://") {
		m.notice = "This post has no article link."
		return nil
	}
	return func() tea.Msg { return opened{m.open(url)} }
}

// prefetch loads the next page past whichever window edge the cursor is near.
func (m *model) prefetch() tea.Cmd {
	var cmds []tea.Cmd
	if m.older != nil && !m.loadingOlder && m.cursor >= len(m.rows)-prefetch {
		m.loadingOlder = true
		before, window := m.older, m.window
		cmds = append(cmds, func() tea.Msg {
			page, err := m.river.Timeline(m.ctx, reader.Query{Before: before})
			return olderLoaded{page, err, window}
		})
	}
	if m.newer != nil && !m.loadingNewer && m.cursor < prefetch {
		m.loadingNewer = true
		after, window := m.newer, m.window
		cmds = append(cmds, func() tea.Msg {
			page, err := m.river.Timeline(m.ctx, reader.Query{After: after})
			return newerLoaded{page, err, window}
		})
	}
	return tea.Batch(cmds...)
}

func (m *model) loadHead() tea.Cmd {
	return func() tea.Msg {
		page, err := m.river.Timeline(m.ctx, reader.Query{})
		return headLoaded{page, err}
	}
}

// append adds older posts below the window, skipping any already loaded.
func (m *model) append(items []reader.Item) {
	for _, item := range items {
		if !m.ids[item.ID] {
			m.ids[item.ID] = true
			m.rows = append(m.rows, newRow(item))
		}
	}
}

// prepend adds newer posts above the window, keeping the cursor and view on the same posts.
func (m *model) prepend(items []reader.Item) {
	var fresh []row
	for _, item := range items {
		if !m.ids[item.ID] {
			m.ids[item.ID] = true
			fresh = append(fresh, newRow(item))
		}
	}
	m.rows = append(fresh, m.rows...)
	m.cursor += len(fresh)
	m.offset += len(fresh)
}

// chrome reports whether the terminal has room for the header and status lines.
func (m *model) chrome() bool { return m.height == 0 || m.height >= 3 }

// visible is how many posts fit on screen.
func (m *model) visible() int {
	if m.chrome() {
		return max(m.height-2, 1)
	}
	return m.height
}

// follow scrolls just enough to keep the cursor visible, with a little context around it.
func (m *model) follow() {
	rows := m.visible()
	margin := min(2, (rows-1)/2)
	if m.cursor < m.offset+margin {
		m.offset = m.cursor - margin
	}
	if m.cursor > m.offset+rows-1-margin {
		m.offset = m.cursor - rows + 1 + margin
	}
	m.offset = max(0, min(m.offset, len(m.rows)-rows))
}
