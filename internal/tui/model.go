package tui

import (
	"context"
	"strings"
	"sync"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/JoshuaFurman/openfeed/internal/reader"
)

const (
	prefetch     = 10 // How close the cursor gets to a window edge before the next page loads.
	maxNameWidth = 20
	minSpin      = 600 * time.Millisecond // A refresh that finds every feed fresh still registers.
	reloadLimit  = 200
	doubleTap    = 400 * time.Millisecond // Longest gap between the two taps of a pull.
	heldRepeat   = 80 * time.Millisecond  // Presses closer than this are a held key's autorepeat, not taps.
	saveDelay    = 2 * time.Second        // Idle time before the reading position is saved.
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

// Page messages carry the request or window generation they belong to, so superseded results are dropped.
type (
	windowLoaded struct {
		items        []reader.Item
		older, newer *reader.Position
		at           *reader.Position // The saved position a resume read placed the window at; nil otherwise.
		jump         bool             // Loaded by g, which reaches the newest post.
		err          error
		request      int
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
	spanReloaded struct {
		items  []reader.Item
		bottom reader.Position
		err    error
		window int
	}
	refreshed struct {
		report reader.Report
		err    error
	}
	seenIdle struct{ reach int }
	opened   struct {
		err error
		row row
	}
	ageTick struct{}
)

// model holds one contiguous span of the river, newest first, with the cursor on one post.
type model struct {
	ctx       context.Context // Canceled when the program ends; see finish.
	cancel    context.CancelFunc
	gate      sync.RWMutex // Held for reading while a command uses the river; see finish.
	closed    bool
	river     River
	open      func(url string) error
	now       func() time.Time
	tick      tea.Cmd       // Redraws ages; nil in tests.
	animate   bool          // Runs the spinner; off in tests, whose commands run synchronously.
	minSpin   time.Duration // Shortest visible refresh.
	saveDelay time.Duration // Idle time before saving the reading position; zero never schedules a save.

	window        int                 // Generation of the loaded span.
	headRequest   int                 // Latest head read; earlier ones are dropped.
	headPending   bool                // The latest window read has not arrived.
	pendingRead   func() windowLoaded // That read, which a supersede or retry reruns; nil once it lands.
	placePending  bool                // The resume placement awaits the terminal size.
	reloading     bool                // A span reload is in flight; at most one runs at a time.
	reloadAgain   bool                // A refresh finished during the reload in flight.
	reloadFailed  bool                // The last span reload failed; the next move retries it.
	rows          []row
	ids           map[reader.ItemID]bool
	older, newer  *reader.Position // Continuations past each edge; nil newer means the window reaches the head.
	loadingOlder  bool
	loadingNewer  bool
	loaded        bool
	headFailed    bool
	divider       *reader.Position // The saved position at launch, fixed for the session.
	reach         int              // Counts navigations, so only the latest schedules a save.
	refreshing    bool
	lastUp        time.Time // Latest k or up press anywhere, to recognize a held key.
	pullArmed     time.Time // First tap of a pull awaiting its second; zero when disarmed.
	spinner       spinner.Model
	cursor        int
	offset        int // First visible row.
	width, height int
	nameWidth     int
	failing       []reader.FeedResult // Feeds that failed the latest refresh.
	notice        string              // Replaces the status line until the next key press.
}

func newModel(ctx context.Context, river River, open func(string) error, now func() time.Time) *model {
	ctx, cancel := context.WithCancel(ctx)
	m := &model{
		ctx: ctx, cancel: cancel, river: river, open: open, now: now, ids: make(map[reader.ItemID]bool),
		tick:    tea.Every(time.Minute, func(time.Time) tea.Msg { return ageTick{} }),
		animate: true, minSpin: minSpin, saveDelay: saveDelay,
		spinner: spinner.New(spinner.WithSpinner(spinner.MiniDot)),
	}
	// Sized from the configuration, so the column never shifts as pages load.
	for _, f := range river.Feeds() {
		m.nameWidth = min(max(m.nameWidth, ansi.StringWidth(Sanitize(f.Name))), maxNameWidth)
	}
	return m
}

// Init resumes the cached river where the reader left off while the launch refresh runs.
func (m *model) Init() tea.Cmd { return tea.Batch(m.readWindow(m.resumeRead), m.refresh(), m.tick) }

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		if m.placePending {
			m.placePending = false
			m.offset = m.cursor - m.visible()*2/3
		}
		m.follow()
	case tea.KeyPressMsg:
		m.notice = ""
		m.placePending = false // The reader has taken over the view.
		return m, m.key(msg)
	case ageTick:
		return m, m.tick
	case seenIdle:
		if msg.reach == m.reach {
			return m, m.saveSeen()
		}
	case spinner.TickMsg:
		if !m.refreshing {
			break // Dropping the tick stops the animation.
		}
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case refreshed:
		m.refreshing = false
		if msg.err != nil {
			break // Only cancellation ends a refresh early, and then the program is exiting.
		}
		m.failing = nil
		for _, result := range msg.report.Feeds {
			if result.Status == reader.Failing {
				m.failing = append(m.failing, result)
			}
		}
		return m, tea.Batch(m.reload(), m.saveSeen())
	case windowLoaded:
		if msg.request != m.headRequest {
			break
		}
		m.headPending = false
		m.loaded = true
		m.headFailed = msg.err != nil
		if msg.err != nil {
			m.notice = msg.err.Error()
			break
		}
		m.pendingRead = nil
		if msg.at != nil {
			m.divider = msg.at // Only a resume read carries the saved position; it is fixed for the session.
		}
		m.replace(msg.items, msg.older)
		m.newer = msg.newer
		m.place(msg.at)
		if msg.jump && len(m.rows) > 0 {
			return m, tea.Batch(m.reached(m.rows[0]), m.prefetch())
		}
		return m, m.prefetch()
	case spanReloaded:
		m.reloading = false
		again := m.reloadAgain
		m.reloadAgain = false
		switch {
		case msg.window != m.window: // The window was replaced; a pending refresh still needs its reload.
		case msg.err != nil:
			m.notice = msg.err.Error()
			m.reloadFailed = true
		case m.reconcile(msg.items, msg.bottom):
			again = true // Rows loaded below the span meanwhile; reread them too.
		}
		var cmds []tea.Cmd
		if again {
			cmds = append(cmds, m.reload())
		}
		return m, tea.Batch(append(cmds, m.prefetch())...)
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
			break
		}
		return m, m.reached(msg.row) // Only an article that opened counts as reached.
	}
	return m, nil
}

func (m *model) key(msg tea.KeyPressMsg) tea.Cmd {
	key := msg.String()
	previousUp := m.lastUp
	if key == "k" || key == "up" {
		m.lastUp = m.now()
	}
	atTop := (key == "k" || key == "up") && m.cursor == 0 && m.newer == nil && m.loaded
	if !atTop {
		m.pullArmed = time.Time{} // Any other key breaks a double-tap.
	}
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
	from := m.cursor
	switch key {
	case "j", "down":
		if m.cursor < len(m.rows)-1 {
			m.cursor++
		}
	case "k", "up":
		if atTop {
			return m.pull(msg.IsRepeat, previousUp)
		}
		if m.cursor > 0 {
			m.cursor--
		}
	case "g", "home":
		if m.newer != nil {
			return m.readWindow(m.jumpRead) // The head is not loaded; replace the window with it.
		}
		m.cursor = 0
	}
	m.follow()
	cmds := []tea.Cmd{m.prefetch()}
	if m.cursor != from {
		// Navigation reaches both the post left and the post landed on.
		cmds = append(cmds, m.reached(m.rows[from], m.rows[m.cursor]))
	}
	if m.reloadFailed {
		cmds = append(cmds, m.reload()) // Any move retries a reload that failed.
	}
	return tea.Batch(cmds...)
}

// reached records that the user navigated to posts and schedules saving once they pause.
func (m *model) reached(rows ...row) tea.Cmd {
	for _, r := range rows {
		m.river.Reached(r.position)
	}
	m.reach++
	if m.saveDelay == 0 {
		return nil
	}
	reach := m.reach
	return tea.Tick(m.saveDelay, func(time.Time) tea.Msg { return seenIdle{reach} })
}

// saveSeen persists the furthest reached position; the reader logs a failure, which never blocks reading.
func (m *model) saveSeen() tea.Cmd {
	return m.work(func() tea.Msg {
		m.river.SaveSeen(m.ctx)
		return nil
	})
}

func (m *model) openSelected() tea.Cmd {
	if len(m.rows) == 0 {
		return nil
	}
	selected := m.rows[m.cursor]
	if !strings.HasPrefix(selected.url, "https://") && !strings.HasPrefix(selected.url, "http://") {
		m.notice = "This post has no article link."
		return nil
	}
	return func() tea.Msg { return opened{m.open(selected.url), selected} }
}

// pull refreshes on the second of two quick, separate taps at the newest post, so a stray press never does.
// A held key's autorepeat never counts, whether or not the terminal flags it as a repeat.
func (m *model) pull(repeat bool, previousUp time.Time) tea.Cmd {
	now := m.now()
	switch {
	case repeat || (!previousUp.IsZero() && now.Sub(previousUp) < heldRepeat):
		m.pullArmed = time.Time{}
	case !m.pullArmed.IsZero() && now.Sub(m.pullArmed) <= doubleTap:
		m.pullArmed = time.Time{}
		return m.refresh()
	default:
		m.pullArmed = now
		m.notice = "Press again to refresh."
	}
	return nil
}

// refresh starts a round unless one is already showing; the reader merges overlapping calls anyway.
func (m *model) refresh() tea.Cmd {
	if m.refreshing {
		return nil
	}
	m.refreshing = true
	cmds := []tea.Cmd{m.work(func() tea.Msg {
		started := time.Now()
		report, err := m.river.Refresh(m.ctx)
		if wait := m.minSpin - time.Since(started); err == nil && wait > 0 {
			select {
			case <-time.After(wait):
			case <-m.ctx.Done():
			}
		}
		return refreshed{report, err}
	})}
	if m.animate {
		cmds = append(cmds, m.spinner.Tick)
	}
	return tea.Batch(cmds...)
}

// reload brings in what a refresh saved. A window at the head is reread through its oldest post, so new
// posts land above the cursor; a window below the head leaves new posts waiting above it.
func (m *model) reload() tea.Cmd {
	switch {
	case m.headPending || !m.loaded || m.headFailed || len(m.rows) == 0:
		return m.loadHead() // Also supersedes a head read that began before the refresh saved.
	case m.newer != nil:
		return nil
	case m.reloading:
		m.reloadAgain = true
		return nil
	}
	m.reloading, m.reloadFailed = true, false
	bottom, window := m.rows[len(m.rows)-1].position, m.window
	return m.work(func() tea.Msg {
		var items []reader.Item
		query := reader.Query{Through: &bottom, Limit: reloadLimit}
		for {
			page, err := m.river.Timeline(m.ctx, query)
			if err != nil {
				return spanReloaded{err: err, window: window}
			}
			items = append(items, page.Items...)
			if page.Next == nil {
				return spanReloaded{items: items, bottom: bottom, window: window}
			}
			query.Before = page.Next
		}
	})
}

// reconcile replaces the window with a reloaded span, keeping older rows loaded meanwhile and reporting
// whether there were any. The cursor stays on its post, or the nearest older one, at the same screen row.
func (m *model) reconcile(items []reader.Item, bottom reader.Position) bool {
	screenRow := m.cursor - m.offset
	var anchor *row
	if len(m.rows) > 0 {
		selected := m.rows[m.cursor]
		anchor = &selected
	}
	var tail []row
	for _, r := range m.rows {
		if r.position.Compare(bottom) < 0 {
			tail = append(tail, r)
		}
	}
	older := m.older
	if len(tail) == 0 {
		older = &bottom // Probe below the span again; the refresh may have added older posts.
	}
	m.replace(items, older)
	for _, r := range tail {
		if !m.ids[r.id] {
			m.ids[r.id] = true
			m.rows = append(m.rows, r)
		}
	}
	m.cursor = 0
	if anchor != nil {
		m.cursor = m.nearest(*anchor)
	}
	m.offset = m.cursor - screenRow
	m.follow()
	return len(tail) > 0
}

// nearest finds anchor's post in the window, or else the newest post older than where it was.
func (m *model) nearest(anchor row) int {
	for i, r := range m.rows {
		if r.id == anchor.id {
			return i
		}
	}
	return m.atOrBelow(anchor.position)
}

// atOrBelow finds the newest post at or below position, or the oldest post when none is.
func (m *model) atOrBelow(position reader.Position) int {
	for i, r := range m.rows {
		if r.position.Compare(position) <= 0 {
			return i
		}
	}
	return max(len(m.rows)-1, 0)
}

// place puts the cursor on the newest post at or below at, in the lower third of the screen so the
// newer posts above it show; a nil at means the newest post. Without a terminal size yet, the resize does it.
func (m *model) place(at *reader.Position) {
	if at == nil {
		m.cursor, m.offset = 0, 0
		return
	}
	m.cursor = m.atOrBelow(*at)
	m.placePending = m.height == 0
	m.offset = m.cursor - m.visible()*2/3
	m.follow()
}

// prefetch loads the next page past whichever window edge the cursor is near.
func (m *model) prefetch() tea.Cmd {
	var cmds []tea.Cmd
	if m.older != nil && !m.loadingOlder && m.cursor >= len(m.rows)-prefetch {
		m.loadingOlder = true
		before, window := m.older, m.window
		cmds = append(cmds, m.work(func() tea.Msg {
			page, err := m.river.Timeline(m.ctx, reader.Query{Before: before})
			return olderLoaded{page, err, window}
		}))
	}
	if m.newer != nil && !m.loadingNewer && m.cursor < prefetch {
		m.loadingNewer = true
		after, window := m.newer, m.window
		cmds = append(cmds, m.work(func() tea.Msg {
			page, err := m.river.Timeline(m.ctx, reader.Query{After: after})
			return newerLoaded{page, err, window}
		}))
	}
	return tea.Batch(cmds...)
}

// loadHead rereads the window: the read still pending if any (the launch resume, or a jump), so a
// supersede or retry keeps its purpose, and otherwise the newest posts.
func (m *model) loadHead() tea.Cmd {
	if m.pendingRead != nil {
		return m.readWindow(m.pendingRead)
	}
	return m.readWindow(m.headRead)
}

// jumpRead reads the newest posts for g, which reaches the newest post once it loads.
func (m *model) jumpRead() windowLoaded {
	loaded := m.headRead()
	loaded.jump = true
	return loaded
}

func (m *model) readWindow(read func() windowLoaded) tea.Cmd {
	m.headRequest++
	m.headPending = true
	m.pendingRead = read
	request := m.headRequest
	return m.work(func() tea.Msg {
		loaded := read()
		loaded.request = request
		return loaded
	})
}

func (m *model) headRead() windowLoaded {
	page, err := m.river.Timeline(m.ctx, reader.Query{})
	return windowLoaded{items: page.Items, older: page.Next, err: err}
}

// resumeRead reads the saved position and the posts around it: the nearest newer page, then everything
// at or below the oldest of those. With nothing newer, it is the head page.
func (m *model) resumeRead() windowLoaded {
	saved, err := m.river.SeenPosition(m.ctx)
	if err != nil {
		return windowLoaded{err: err}
	}
	if saved == nil {
		return m.headRead()
	}
	above, err := m.river.Timeline(m.ctx, reader.Query{After: saved})
	if err != nil {
		return windowLoaded{err: err}
	}
	below := reader.Query{}
	if len(above.Items) > 0 {
		below.Before = &above.Items[len(above.Items)-1].Position
	}
	page, err := m.river.Timeline(m.ctx, below)
	if err != nil {
		return windowLoaded{err: err}
	}
	return windowLoaded{items: append(above.Items, page.Items...), older: page.Next, newer: above.Newer, at: saved}
}

// work wraps a command that uses the river so that it does nothing once the program has finished.
func (m *model) work(fn func() tea.Msg) tea.Cmd {
	return func() tea.Msg {
		m.gate.RLock()
		defer m.gate.RUnlock()
		if m.closed {
			return nil
		}
		return fn()
	}
}

// finish cancels commands still using the river and waits for them, so the caller can close it safely.
// Bubble Tea does not wait for running commands when a program quits.
func (m *model) finish() {
	m.cancel()
	m.gate.Lock()
	m.closed = true
	m.gate.Unlock()
}

// replace starts a new window generation holding items, reaching the head.
func (m *model) replace(items []reader.Item, older *reader.Position) {
	m.window++
	m.rows, m.ids = nil, make(map[reader.ItemID]bool)
	m.older, m.newer = older, nil
	m.loadingOlder, m.loadingNewer = false, false
	m.append(items)
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

// dividerLoaded reports whether the divider falls among the loaded posts, where it is drawn.
func (m *model) dividerLoaded() bool {
	for i := range len(m.rows) + 1 {
		if m.dividerBefore(i) {
			return true
		}
	}
	return false
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
// A divider takes a line, so one is set aside while it falls among the loaded posts.
func (m *model) follow() {
	rows := m.visible()
	if m.dividerLoaded() && rows > 1 {
		rows--
	}
	margin := min(2, (rows-1)/2)
	if m.cursor < m.offset+margin {
		m.offset = m.cursor - margin
	}
	if m.cursor > m.offset+rows-1-margin {
		m.offset = m.cursor - rows + 1 + margin
	}
	m.offset = max(0, min(m.offset, len(m.rows)-rows))
}
