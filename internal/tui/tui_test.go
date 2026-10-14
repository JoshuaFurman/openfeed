package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/JoshuaFurman/openfeed/internal/config"
	"github.com/JoshuaFurman/openfeed/internal/feed"
	"github.com/JoshuaFurman/openfeed/internal/reader"
	"github.com/JoshuaFurman/openfeed/internal/store"
)

// TestMain pins the zone that ages format dates in, so expectations hold everywhere.
func TestMain(m *testing.M) {
	time.Local = time.UTC
	os.Exit(m.Run())
}

var now = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

type post struct {
	feed, title, url string
	age              time.Duration
}

func feedURL(name string) string {
	return "https://" + strings.ReplaceAll(name, " ", "-") + ".example/feed"
}

// fetcher serves refreshes. It stamps every fetch at now, before the real clock, so feeds are always due.
type fetcher struct {
	mu    sync.Mutex
	posts map[string][]feed.Item
	fails map[string]error
	calls int
}

func (f *fetcher) Fetch(_ context.Context, url string, _ feed.Validators) (feed.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if err := f.fails[url]; err != nil {
		return feed.Result{}, err
	}
	return feed.Result{Items: f.posts[url], FetchedAt: now}, nil
}

// serve sets what the next refreshes of a feed return.
func (f *fetcher) serve(name string, items ...feed.Item) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posts[feedURL(name)] = items
}

func (f *fetcher) fail(name string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fails[feedURL(name)] = err
}

func fresh(key, title string, age time.Duration) feed.Item {
	return feed.Item{Key: key, Title: title, URL: "https://fresh.example/" + key, PublishedAt: now.Add(-age)}
}

// seed caches posts for each named feed and returns a reader over them, with the fetcher its refreshes use.
func seed(t *testing.T, posts []post, extraFeeds ...string) (*reader.Reader, *fetcher) {
	t.Helper()
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "openfeed.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	byFeed := map[string][]store.RefreshItem{}
	var feeds []config.Feed
	for _, name := range extraFeeds {
		feeds = append(feeds, config.Feed{URL: feedURL(name), Name: name})
	}
	for i, p := range posts {
		url := feedURL(p.feed)
		if byFeed[url] == nil {
			feeds = append(feeds, config.Feed{URL: url, Name: p.feed})
		}
		byFeed[url] = append(byFeed[url], store.RefreshItem{
			Key: fmt.Sprintf("id:%d", i), Title: p.title, URL: p.url, PublishedAt: now.Add(-p.age),
		})
	}
	for url, items := range byFeed {
		state, err := st.FeedState(t.Context(), url)
		if err == nil {
			err = st.SaveRefresh(t.Context(), state, store.Refresh{FetchedAt: now, Items: items})
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	f := &fetcher{posts: map[string][]feed.Item{}, fails: map[string]error{}}
	return reader.New(feeds, &subscriptions{}, st, f, nil), f
}

// numbered makes n posts in one feed, a minute apart, newest first.
func numbered(n int) []post {
	posts := make([]post, n)
	for i := range posts {
		posts[i] = post{feed: "alpha", title: fmt.Sprintf("post %d", i), url: fmt.Sprintf("https://alpha.example/%d", i), age: time.Duration(i+1) * time.Minute}
	}
	return posts
}

// flaky fails the first reads of the river it wraps.
type flaky struct {
	*reader.Reader
	failures int
}

func (f *flaky) Timeline(ctx context.Context, query reader.Query) (reader.Page, error) {
	if f.failures > 0 {
		f.failures--
		return reader.Page{}, errors.New("read timeline: database is locked")
	}
	return f.Reader.Timeline(ctx, query)
}

// spanFlaky fails the first span reloads (reads bounded by Through) of the river it wraps.
type spanFlaky struct {
	*reader.Reader
	failures int
}

func (f *spanFlaky) Timeline(ctx context.Context, query reader.Query) (reader.Page, error) {
	if query.Through != nil && f.failures > 0 {
		f.failures--
		return reader.Page{}, errors.New("read timeline: database is locked")
	}
	return f.Reader.Timeline(ctx, query)
}

// slowRefresh refreshes until canceled, then takes a moment to wind down, as a round of fetches does.
type slowRefresh struct {
	*reader.Reader
	finished atomic.Bool
}

func (s *slowRefresh) Refresh(ctx context.Context) (reader.Report, error) {
	<-ctx.Done()
	time.Sleep(20 * time.Millisecond)
	s.finished.Store(true)
	return reader.Report{}, ctx.Err()
}

func key(s string) tea.KeyPressMsg {
	switch s {
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "shift+tab":
		return tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "ctrl+c":
		return tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	case "ctrl+z":
		return tea.KeyPressMsg{Code: 'z', Mod: tea.ModCtrl}
	}
	return tea.KeyPressMsg{Code: rune(s[0]), Text: s}
}

// send delivers msg, then runs each resulting command and delivers its message, as the event loop does.
// It reports whether the model asked to quit.
func send(t *testing.T, m *model, msg tea.Msg) bool {
	t.Helper()
	queue := []tea.Msg{msg}
	for len(queue) > 0 {
		msg, queue = queue[0], queue[1:]
		switch msg := msg.(type) {
		case nil:
		case tea.QuitMsg:
			return true
		case tea.BatchMsg:
			for _, cmd := range msg {
				if cmd != nil {
					queue = append(queue, cmd())
				}
			}
		default:
			if _, cmd := m.Update(msg); cmd != nil {
				queue = append(queue, cmd())
			}
		}
	}
	return false
}

// advance moves the model's clock forward; ages move by minutes, so small steps leave the screen alone.
func advance(m *model, d time.Duration) {
	current := m.now()
	m.now = func() time.Time { return current.Add(d) }
}

// pull double-taps k at the newest post, the refresh gesture.
func pull(t *testing.T, m *model) {
	t.Helper()
	advance(m, time.Second)
	press(t, m, "k")
	advance(m, 150*time.Millisecond)
	press(t, m, "k")
}

func press(t *testing.T, m *model, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if send(t, m, key(k)) {
			t.Fatalf("%q quit", k)
		}
	}
}

// build makes a model without timers, which would block these synchronous tests.
func build(t *testing.T, river River, open func(string) error) *model {
	m := newModel(t.Context(), river, open, func() time.Time { return now })
	m.tick, m.animate, m.minSpin, m.saveDelay = nil, false, 0, 0
	return m
}

// start opens the river as a launch does, running the launch refresh and its reconciliation.
func start(t *testing.T, river River, width, height int, open func(string) error) *model {
	t.Helper()
	m := build(t, river, open)
	send(t, m, tea.WindowSizeMsg{Width: width, Height: height})
	send(t, m, m.Init()())
	return m
}

func noOpen(t *testing.T) func(string) error {
	return func(url string) error {
		t.Errorf("opened %s", url)
		return nil
	}
}

func screen(m *model) []string {
	return strings.Split(ansi.Strip(m.View().Content), "\n")
}

func selected(t *testing.T, m *model) string {
	t.Helper()
	for _, line := range screen(m) {
		if strings.HasPrefix(line, "▸ ") {
			return line
		}
	}
	t.Fatalf("no selected row:\n%s", strings.Join(screen(m), "\n"))
	return ""
}

func rowTitles(rows []row) []string {
	var titles []string
	for _, r := range rows {
		titles = append(titles, r.title)
	}
	return titles
}

func itemTitles(items []reader.Item) []string {
	var titles []string
	for _, item := range items {
		titles = append(titles, item.Title)
	}
	return titles
}

func TestRiverRendersNewestFirst(t *testing.T) {
	river, _ := seed(t, []post{
		{"alpha", "Older \x1b[2Jpost", "https://alpha.example/1", 3 * time.Hour},
		{"日本", "Wide name \u202eexe.txt", "https://wide.example/1", 30 * time.Minute},
		{"alpha", "", "https://alpha.example/4", 5 * time.Hour},
		{"alpha", "Last week", "https://alpha.example/2", 10 * 24 * time.Hour},
		{"alpha", "Last year", "https://alpha.example/3", 400 * 24 * time.Hour},
	})
	m := start(t, river, 60, 8, noOpen(t))
	lines := screen(m)
	want := []string{
		title + strings.Repeat(" ", 60-ansi.StringWidth(title)-ansi.StringWidth(hints)) + strings.TrimSpace(hints),
		"▸ 30m       日本   Wide name exe.txt",
		"  3h        alpha  Older  [2Jpost",
		"  5h        alpha  (untitled)",
		"  Sep 14    alpha  Last week",
		"  Aug 2025  alpha  Last year",
		"",
		"",
	}
	for i := range want {
		if i >= len(lines) || strings.TrimRight(lines[i], " ") != want[i] {
			t.Fatalf("screen:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
		}
	}
	if len(lines) != 8 {
		t.Errorf("screen has %d lines, want the terminal height", len(lines))
	}
	// Styling adds its own escape sequences, so check for the feed's clear-screen and bidi override.
	if content := m.View().Content; strings.Contains(content, "\x1b[2J") || strings.Contains(content, "\u202e") {
		t.Error("feed text reached the terminal unsanitized")
	}
	if !m.View().AltScreen {
		t.Error("the river does not use the alternate screen")
	}
}

func TestLinesFitTheTerminal(t *testing.T) {
	long := strings.Repeat("x", 100)
	river, fetch := seed(t, []post{{"a-feed-with-a-very-long-configured-name", strings.Repeat("long title ", 20), "https://a.example/1", time.Hour}}, long)
	fetch.fail(long, errors.New("down"))
	for _, width := range []int{12, 30, 50, 80} {
		m := start(t, river, width, 5, noOpen(t))
		for _, line := range screen(m) {
			if w := ansi.StringWidth(line); w > width {
				t.Errorf("width %d: line %q is %d cells", width, line, w)
			}
		}
	}
}

func TestNameColumnIsSizedFromConfig(t *testing.T) {
	// The newest page holds only "hn"; the long name arrives with an older page.
	posts := numbered(60)
	for i := range posts {
		posts[i].feed = "hn"
	}
	posts = append(posts, post{"go-blog", "Go 1.27 is released", "https://go.example/1", 100 * time.Hour})
	river, _ := seed(t, posts, "The Go Programming Blog")
	m := start(t, river, 80, 12, noOpen(t))
	column := strings.Index(selected(t, m), "post 0")
	for range 55 {
		press(t, m, "j")
	}
	if len(m.rows) != 61 || strings.Index(selected(t, m), "post 55") != column {
		t.Errorf("titles moved from column %d as pages loaded: %q", column, selected(t, m))
	}
	if m.nameWidth != maxNameWidth {
		t.Errorf("name column = %d, want the longest configured name capped at %d", m.nameWidth, maxNameWidth)
	}
}

func TestNavigationLoadsOlderPages(t *testing.T) {
	river, _ := seed(t, numbered(130))
	m := start(t, river, 40, 12, noOpen(t))
	if len(m.rows) != 50 || m.older == nil {
		t.Fatalf("first page holds %d posts", len(m.rows))
	}
	pull(t, m) // Finds nothing new.
	if m.cursor != 0 || len(m.rows) != 50 || m.refreshing {
		t.Fatal("pulling without new posts moved the cursor or changed the window")
	}
	for range 45 {
		press(t, m, "j")
	}
	if len(m.rows) != 100 || !strings.Contains(selected(t, m), "post 45") {
		t.Fatalf("after 45 moves: %d posts loaded, selected %q", len(m.rows), selected(t, m))
	}
	for range 200 {
		press(t, m, "down")
	}
	if len(m.rows) != 130 || m.older != nil || m.cursor != 129 || !strings.Contains(selected(t, m), "post 129") {
		t.Fatalf("at the end: %d posts, cursor %d, selected %q", len(m.rows), m.cursor, selected(t, m))
	}
	want := make([]string, 130)
	for i := range want {
		want[i] = fmt.Sprintf("post %d", i)
	}
	if got := rowTitles(m.rows); !slices.Equal(got, want) {
		t.Errorf("window = %v", got)
	}
	press(t, m, "k", "up")
	if !strings.Contains(selected(t, m), "post 127") {
		t.Errorf("moving up selected %q", selected(t, m))
	}
}

// resumeAt loads the window the way a resumed launch will: the nearest newer page, then everything below it.
func resumeAt(t *testing.T, river *reader.Reader, width, height int, saved reader.Position) *model {
	t.Helper()
	m := start(t, river, width, height, noOpen(t))
	above, err := river.Timeline(t.Context(), reader.Query{After: &saved})
	if err != nil {
		t.Fatal(err)
	}
	below, err := river.Timeline(t.Context(), reader.Query{Before: &above.Items[len(above.Items)-1].Position})
	if err != nil {
		t.Fatal(err)
	}
	m.window++
	m.rows, m.ids = nil, make(map[reader.ItemID]bool)
	m.append(above.Items)
	m.append(below.Items)
	m.newer, m.older = above.Newer, below.Next
	m.cursor = len(above.Items)
	m.follow()
	return m
}

func TestNewerPagesKeepTheCursorOnItsPost(t *testing.T) {
	river, _ := seed(t, numbered(130))
	all, err := river.Timeline(t.Context(), reader.Query{Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	m := resumeAt(t, river, 40, 12, all.Items[100].Position)
	if m.newer == nil || !strings.Contains(selected(t, m), "post 100") {
		t.Fatalf("resumed at %q with newer %v", selected(t, m), m.newer)
	}
	for m.cursor > prefetch {
		press(t, m, "k")
	}
	id, loaded := m.rows[m.cursor].id, len(m.rows)
	press(t, m, "k") // Crosses into the prefetch zone and loads the next newer page.
	if len(m.rows) <= loaded || m.rows[m.cursor+1].id != id {
		t.Fatalf("newer page did not load (%d posts), or moved the cursor off the post above %d", len(m.rows), id)
	}
	for m.cursor > 0 {
		press(t, m, "k")
	}
	if m.newer != nil || !strings.Contains(selected(t, m), "post 0") || !slices.Equal(rowTitles(m.rows), itemTitles(all.Items)) {
		t.Errorf("reading upward did not reach the head exactly once per post: %v", rowTitles(m.rows))
	}
}

func TestJumpToNewest(t *testing.T) {
	river, _ := seed(t, numbered(130))
	all, err := river.Timeline(t.Context(), reader.Query{Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	m := resumeAt(t, river, 40, 12, all.Items[100].Position)
	press(t, m, "g")
	if m.newer != nil || m.cursor != 0 || !strings.Contains(selected(t, m), "post 0") || len(m.rows) != 50 {
		t.Fatalf("g selected %q with %d posts loaded", selected(t, m), len(m.rows))
	}

	// A page requested by the old window is dropped once g has replaced it. Resuming far below the
	// head makes that page hold posts the new window lacks, so merging it would show.
	deep, _ := seed(t, numbered(250))
	deepAll, err := deep.Timeline(t.Context(), reader.Query{Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	m = resumeAt(t, deep, 40, 12, deepAll.Items[180].Position)
	var stale tea.Msg
	for m.cursor >= prefetch && stale == nil {
		_, cmd := m.Update(key("k"))
		if cmd != nil {
			stale = cmd()
		}
	}
	if _, ok := stale.(newerLoaded); !ok {
		t.Fatalf("expected a pending newer page, got %T", stale)
	}
	press(t, m, "g")
	send(t, m, stale)
	if !slices.Equal(rowTitles(m.rows), itemTitles(deepAll.Items[:50])) || m.newer != nil {
		t.Errorf("stale page merged into the replaced window: %v", rowTitles(m.rows))
	}
}

func TestPagesNeverDuplicatePosts(t *testing.T) {
	river, _ := seed(t, numbered(130))
	all, err := river.Timeline(t.Context(), reader.Query{Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	m := resumeAt(t, river, 40, 12, all.Items[100].Position)
	before := rowTitles(m.rows)
	id := m.rows[m.cursor].id
	// A refresh can move posts between reads, so pages may overlap what is already loaded.
	send(t, m, olderLoaded{page: reader.Page{Items: all.Items[90:95], Next: m.older}, window: m.window})
	send(t, m, newerLoaded{page: reader.Page{Items: all.Items[50:55], Newer: m.newer}, window: m.window})
	if !slices.Equal(rowTitles(m.rows), before) || m.rows[m.cursor].id != id {
		t.Errorf("overlapping pages duplicated posts or moved the cursor: %v", rowTitles(m.rows))
	}
}

func TestLaunchReadFailureRetries(t *testing.T) {
	inner, _ := seed(t, numbered(3))
	// The launch read and the reread after the launch refresh both fail, then one retry fails too.
	river := &flaky{Reader: inner, failures: 3}
	m := start(t, river, 60, 6, noOpen(t))
	lines := screen(m)
	if !strings.Contains(lines[1], "Could not read the river") || !strings.Contains(lines[5], "database is locked") {
		t.Fatalf("launch failure screen:\n%s", strings.Join(lines, "\n"))
	}
	press(t, m, "j") // Still failing: the retry fails again.
	if !m.headFailed || !strings.Contains(screen(m)[5], "database is locked") {
		t.Fatalf("second failure not shown:\n%s", strings.Join(screen(m), "\n"))
	}
	press(t, m, "k")
	if m.headFailed || len(m.rows) != 3 || !strings.Contains(selected(t, m), "post 0") {
		t.Errorf("retry did not load the river:\n%s", strings.Join(screen(m), "\n"))
	}
}

func TestOpenArticle(t *testing.T) {
	river, _ := seed(t, []post{
		{"alpha", "linked", "https://alpha.example/1", time.Minute},
		{"alpha", "unlinked", "", 2 * time.Minute},
	})
	var opened []string
	failure := errors.New("no handler\x1b[2J")
	m := start(t, river, 60, 6, func(url string) error {
		opened = append(opened, url)
		if len(opened) > 1 {
			return failure
		}
		return nil
	})
	press(t, m, "o")
	if !slices.Equal(opened, []string{"https://alpha.example/1"}) || m.notice != "" {
		t.Fatalf("opened %v with notice %q", opened, m.notice)
	}
	press(t, m, "enter")
	status := screen(m)[5]
	if !strings.Contains(status, "Could not open the article: no handler") || strings.Contains(m.View().Content, "\x1b[2J") {
		t.Errorf("opener failure status = %q", status)
	}
	press(t, m, "j", "o")
	if len(opened) != 2 || !strings.Contains(screen(m)[5], "no article link") {
		t.Errorf("post without a link: opened %v, status %q", opened, screen(m)[5])
	}
	press(t, m, "k")
	if strings.TrimSpace(screen(m)[5]) != "" {
		t.Error("a key press did not clear the notice")
	}
}

func TestResizeKeepsCursorVisible(t *testing.T) {
	river, _ := seed(t, numbered(30))
	m := start(t, river, 40, 20, noOpen(t))
	for range 25 {
		press(t, m, "j")
	}
	for _, size := range []tea.WindowSizeMsg{{Width: 40, Height: 6}, {Width: 15, Height: 4}, {Width: 40, Height: 2}, {Width: 40, Height: 1}, {Width: 80, Height: 40}} {
		send(t, m, size)
		selected(t, m) // The cursor's row is on screen, though narrow widths truncate its title.
		if m.rows[m.cursor].title != "post 25" {
			t.Errorf("%dx%d moved the cursor to %q", size.Width, size.Height, m.rows[m.cursor].title)
		}
		if len(screen(m)) != size.Height {
			t.Errorf("%dx%d rendered %d lines", size.Width, size.Height, len(screen(m)))
		}
	}
}

func TestStatusAndEmptyStates(t *testing.T) {
	unsubscribed, _ := seed(t, nil)
	if got := strings.Join(screen(start(t, unsubscribed, 80, 6, noOpen(t))), "\n"); !strings.Contains(got, "No subscriptions yet. Press n to add a feed.") {
		t.Errorf("no subscriptions screen:\n%s", got)
	}

	empty, _ := seed(t, nil, "alpha")
	m := build(t, empty, noOpen(t))
	m.refreshing = true // As if the launch refresh were still running.
	if !strings.Contains(strings.Join(screen(m), "\n"), "Loading…") {
		t.Error("no loading state before the first page")
	}
	if !strings.Contains(screen(m)[0], "refreshing") {
		t.Error("the header does not show a running refresh")
	}
	send(t, m, m.loadHead()())
	if !strings.Contains(strings.Join(screen(m), "\n"), "No posts yet. Refreshing…") {
		t.Error("an empty river during a refresh does not say so")
	}
	m.refreshing = false
	if !strings.Contains(strings.Join(screen(m), "\n"), "No posts yet. Press k twice to refresh.") {
		t.Error("no empty state")
	}

	for _, tc := range []struct {
		failing []string
		want    string
	}{
		{[]string{"lobsters"}, "lobsters failed: HTTP 503"},
		{[]string{"lobsters", "go blog"}, "2 feeds failing: lobsters, go blog (details in the log)"},
	} {
		river, fetch := seed(t, nil, "lobsters", "go blog", "fine")
		for _, name := range tc.failing {
			fetch.fail(name, errors.New("HTTP 503"))
		}
		m := start(t, river, 80, 4, noOpen(t))
		if status := screen(m)[3]; strings.TrimSpace(status) != tc.want {
			t.Errorf("status = %q, want %q", status, tc.want)
		}
		if header := screen(m)[0]; !strings.Contains(header, strings.TrimSpace(hints)) {
			t.Errorf("failures hid the key hints: %q", header)
		}
	}
}

func TestPageErrorsShowInStatus(t *testing.T) {
	river, _ := seed(t, numbered(130))
	m := start(t, river, 40, 12, noOpen(t))
	loaded := len(m.rows)
	send(t, m, olderLoaded{err: errors.New("read timeline: disk I/O error"), window: m.window})
	if m.loadingOlder || len(m.rows) != loaded || !strings.Contains(screen(m)[11], "disk I/O error") {
		t.Errorf("older page failure: loading %v, %d posts, status %q", m.loadingOlder, len(m.rows), screen(m)[11])
	}
	send(t, m, newerLoaded{err: errors.New("read timeline: locked"), window: m.window})
	if m.loadingNewer || !strings.Contains(screen(m)[11], "locked") {
		t.Errorf("newer page failure: status %q", screen(m)[11])
	}
	for range 45 {
		press(t, m, "j") // Scrolling retries the failed page.
	}
	if len(m.rows) != 100 {
		t.Errorf("scrolling did not retry after a failed page: %d posts", len(m.rows))
	}
}

func TestSuspendQuitAndTick(t *testing.T) {
	river, _ := seed(t, numbered(3))
	for _, k := range []string{"q", "ctrl+c"} {
		m := start(t, river, 40, 6, noOpen(t))
		if !send(t, m, key(k)) {
			t.Errorf("%s did not quit", k)
		}
	}
	m := start(t, river, 40, 6, noOpen(t))
	if _, cmd := m.Update(key("ctrl+z")); cmd == nil {
		t.Error("ctrl+z did nothing")
	} else if _, ok := cmd().(tea.SuspendMsg); !ok {
		t.Error("ctrl+z did not suspend")
	}

	type ticked struct{}
	m.tick = func() tea.Msg { return ticked{} }
	if _, cmd := m.Update(ageTick{}); cmd == nil {
		t.Fatal("the age tick did not reschedule itself")
	} else if _, ok := cmd().(ticked); !ok {
		t.Error("the age tick rescheduled something else")
	}
	production := newModel(t.Context(), river, noOpen(t), time.Now)
	if production.tick == nil || !production.animate || production.minSpin != minSpin {
		t.Error("production models do not redraw ages, animate the spinner, or hold it long enough")
	}
}

func TestRunReturnsContextError(t *testing.T) {
	river, _ := seed(t, numbered(3))
	ctx, cancel := context.WithCancel(t.Context())
	m := build(t, river, noOpen(t))
	m.ctx = ctx
	time.AfterFunc(50*time.Millisecond, cancel)
	err := run(ctx, m, tea.WithInput(nil), tea.WithOutput(io.Discard))
	if !errors.Is(err, context.Canceled) || errors.Is(err, tea.ErrProgramKilled) {
		t.Errorf("canceled run returned %v; want the context's error alone", err)
	}
}

func TestAge(t *testing.T) {
	for _, tc := range []struct {
		ago  time.Duration
		want string
	}{
		{-time.Minute, "now"}, {30 * time.Second, "now"}, {59 * time.Minute, "59m"}, {23 * time.Hour, "23h"},
		{6 * 24 * time.Hour, "6d"}, {10 * 24 * time.Hour, "Sep 14"}, {400 * 24 * time.Hour, "Aug 2025"},
	} {
		if got := age(now.Add(-tc.ago), now); got != tc.want {
			t.Errorf("age(%s ago) = %q, want %q", tc.ago, got, tc.want)
		}
	}
}

func TestLaunchRefreshLandsNewPostsAboveTheCursor(t *testing.T) {
	river, fetch := seed(t, numbered(20))
	fetch.serve("alpha", fresh("new-1", "fresh one", 0), fresh("new-2", "fresh two", 0))
	m := start(t, river, 60, 10, noOpen(t))
	if got := rowTitles(m.rows[:3]); !slices.Equal(got, []string{"fresh two", "fresh one", "post 0"}) {
		t.Fatalf("window starts %v", got)
	}
	// The cursor stays on the post it was on when the cached river showed; the new posts sit above it.
	if !strings.Contains(selected(t, m), "post 0") || m.cursor != 2 || m.offset != 0 {
		t.Errorf("cursor %d, offset %d, selected %q", m.cursor, m.offset, selected(t, m))
	}
	if fetch.calls != 1 || m.refreshing {
		t.Errorf("launch refreshed %d times, still refreshing %v", fetch.calls, m.refreshing)
	}
}

func TestPullToRefresh(t *testing.T) {
	river, fetch := seed(t, numbered(20))
	m := start(t, river, 60, 10, noOpen(t))
	fetch.serve("alpha", fresh("new-1", "pulled", 0))
	press(t, m, "j", "k")
	if fetch.calls != 1 {
		t.Fatal("moving up to the newest post refreshed")
	}

	advance(m, time.Second)
	press(t, m, "k")
	advance(m, 150*time.Millisecond)
	_, cmd := m.Update(key("k")) // The second tap pulls.
	if !m.refreshing || cmd == nil || !strings.Contains(screen(m)[0], "refreshing") {
		t.Fatal("pulling did not start a visible refresh")
	}
	advance(m, 150*time.Millisecond)
	m.Update(key("up"))
	advance(m, 150*time.Millisecond)
	if _, again := m.Update(key("up")); again != nil {
		t.Error("a second pull started another refresh while one was running")
	}
	send(t, m, cmd())
	if m.refreshing || fetch.calls != 2 || strings.Contains(screen(m)[0], "refreshing") {
		t.Errorf("refresh did not finish: refreshing %v, %d fetches", m.refreshing, fetch.calls)
	}
	if m.rows[0].title != "pulled" || !strings.Contains(selected(t, m), "post 0") {
		t.Errorf("pulled post is not above the cursor: %v, selected %q", rowTitles(m.rows[:2]), selected(t, m))
	}

	// Below the head, the top of the window loads newer posts instead of refreshing.
	all, err := river.Timeline(t.Context(), reader.Query{Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	below := resumeAt(t, river, 60, 10, all.Items[5].Position)
	calls := fetch.calls
	below.newer = &all.Items[0].Position // Pretend newer posts remain above the window.
	below.cursor, below.loadingNewer = 0, true
	if _, cmd := below.Update(key("k")); cmd != nil || below.refreshing || fetch.calls != calls {
		t.Error("pulling below the head refreshed")
	}
	// A refresh that finishes below the head leaves new posts waiting above the window.
	window, generation := rowTitles(below.rows), below.window
	send(t, below, refreshed{})
	if !slices.Equal(rowTitles(below.rows), window) || below.window != generation || below.newer == nil {
		t.Error("a refresh below the head reloaded the window")
	}
}

func TestRefreshReconcilesTheSpan(t *testing.T) {
	river, fetch := seed(t, numbered(20))
	m := start(t, river, 60, 12, noOpen(t))
	for range 5 {
		press(t, m, "j")
	}
	if !strings.Contains(selected(t, m), "post 5") {
		t.Fatalf("selected %q", selected(t, m))
	}
	// Post 5 (key id:5) is corrected to be newer, a late discovery lands inside the span, and post 2 moves older.
	fetch.serve("alpha",
		fresh("id:5", "post 5", 30*time.Second),
		fresh("late", "late discovery", 150*time.Second),
		fresh("id:2", "post 2", 17*time.Minute),
	)
	for m.cursor > 0 {
		press(t, m, "k")
	}
	pull(t, m)
	want := []string{"post 5", "post 0", "post 1", "late discovery", "post 3", "post 4", "post 6"}
	if got := rowTitles(m.rows[:7]); !slices.Equal(got, want) {
		t.Fatalf("reconciled window = %v, want %v", got, want)
	}
	if !strings.Contains(selected(t, m), "post 0") {
		t.Errorf("cursor left its post: %q", selected(t, m))
	}
	if i := slices.Index(rowTitles(m.rows), "post 2"); i != 17 {
		t.Errorf("corrected post 2 is at %d, want between post 15 and post 16", i)
	}
	if len(m.rows) != 21 {
		t.Errorf("window holds %d posts, want every post once", len(m.rows))
	}
}

func TestRefreshMovesTheCursorWhenItsPostLeaves(t *testing.T) {
	river, fetch := seed(t, numbered(60))
	m := start(t, river, 60, 12, noOpen(t))
	for range 10 {
		press(t, m, "j")
	}
	if !strings.Contains(selected(t, m), "post 10") {
		t.Fatalf("selected %q", selected(t, m))
	}
	// The selected post is corrected to far older than the loaded span, so the reload no longer holds it.
	fetch.serve("alpha", fresh("id:10", "post 10", 1000*time.Hour))
	send(t, m, m.refresh()())
	if !strings.Contains(selected(t, m), "post 11") || slices.Contains(rowTitles(m.rows), "post 10") {
		t.Errorf("cursor should move to the nearest older post, selected %q", selected(t, m))
	}
}

func TestReloadKeepsRowsLoadedMeanwhile(t *testing.T) {
	river, _ := seed(t, numbered(130))
	m := start(t, river, 60, 12, noOpen(t))
	reload := m.reload()() // Requested for the first 50 posts.
	for range 45 {
		press(t, m, "j") // Meanwhile the user scrolls past them and an older page loads.
	}
	if len(m.rows) != 100 {
		t.Fatalf("%d posts loaded", len(m.rows))
	}
	send(t, m, reload)
	if len(m.rows) != 100 || !strings.Contains(selected(t, m), "post 45") {
		t.Errorf("reload dropped rows loaded meanwhile: %d posts, selected %q", len(m.rows), selected(t, m))
	}
}

func TestRefreshUpdatesFailureStatus(t *testing.T) {
	river, fetch := seed(t, numbered(3))
	fetch.fail("alpha", errors.New("HTTP 503"))
	m := start(t, river, 60, 6, noOpen(t))
	if status := strings.TrimSpace(screen(m)[5]); status != "alpha failed: HTTP 503" {
		t.Fatalf("status = %q", status)
	}
	fetch.fail("alpha", nil)
	send(t, m, refreshed{report: reader.Report{Feeds: []reader.FeedResult{{Feed: config.Feed{Name: "alpha"}, Status: reader.Refreshed}}}})
	if status := strings.TrimSpace(screen(m)[5]); status != "" {
		t.Errorf("a recovered feed still shows as failing: %q", status)
	}
	send(t, m, refreshed{err: context.Canceled})
	if m.refreshing {
		t.Error("a canceled refresh left the spinner running")
	}
}

func TestEmptyRiverFillsAfterRefresh(t *testing.T) {
	river, fetch := seed(t, nil, "alpha")
	fetch.serve("alpha", fresh("first", "first post", time.Minute))
	m := start(t, river, 60, 6, noOpen(t))
	if len(m.rows) != 1 || !strings.Contains(selected(t, m), "first post") {
		t.Errorf("launch refresh did not fill an empty river: %v", rowTitles(m.rows))
	}
}

func TestStaleHeadReadsAreDropped(t *testing.T) {
	river, _ := seed(t, numbered(3))
	m := build(t, river, noOpen(t))
	send(t, m, tea.WindowSizeMsg{Width: 60, Height: 6})
	first := m.loadHead()()
	second := m.loadHead()()
	send(t, m, second)
	m.cursor = 2
	send(t, m, first)
	if m.cursor != 2 {
		t.Error("a superseded head read replaced the window")
	}
}

func TestSpinnerRunsOnlyWhileRefreshing(t *testing.T) {
	river, _ := seed(t, numbered(3))
	m := start(t, river, 60, 6, noOpen(t))
	tick := m.spinner.Tick()
	if _, cmd := m.Update(tick); cmd != nil {
		t.Error("the spinner kept ticking after the refresh")
	}
	m.refreshing = true
	if _, cmd := m.Update(tick); cmd == nil {
		t.Error("the spinner stopped during a refresh")
	}

	m.refreshing, m.minSpin = false, 80*time.Millisecond
	advance(m, time.Second)
	press(t, m, "k")
	advance(m, 150*time.Millisecond)
	started := time.Now()
	send(t, m, key("k"))
	if elapsed := time.Since(started); elapsed < m.minSpin {
		t.Errorf("an instant refresh showed for %s, want at least %s", elapsed, m.minSpin)
	}
}

func TestFailedReloadRetriesOnTheNextMove(t *testing.T) {
	inner, fetch := seed(t, numbered(20))
	fetch.serve("alpha", fresh("new", "fresh post", 0))
	river := &spanFlaky{Reader: inner, failures: 1}
	m := start(t, river, 60, 10, noOpen(t))
	if m.rows[0].title == "fresh post" || !m.reloadFailed || !strings.Contains(screen(m)[9], "database is locked") {
		t.Fatalf("the launch reload did not fail visibly: %v", rowTitles(m.rows[:2]))
	}
	press(t, m, "j")
	if m.reloadFailed || m.rows[0].title != "fresh post" {
		t.Errorf("moving did not retry the reload: %v", rowTitles(m.rows[:2]))
	}
}

func TestRefreshDuringReloadReloadsAgain(t *testing.T) {
	river, fetch := seed(t, numbered(20))
	m := start(t, river, 60, 10, noOpen(t))
	first := m.reload()() // In flight when the next refresh finishes.
	fetch.serve("alpha", fresh("new", "saved by the second refresh", 0))
	if _, err := river.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	m.Update(refreshed{}) // Its save may run; its reload must wait for the one in flight.
	if !m.reloading || !m.reloadAgain {
		t.Fatal("a second reload ran alongside the first instead of waiting for it")
	}
	send(t, m, first)
	if m.rows[0].title != "saved by the second refresh" || m.reloading {
		t.Errorf("the second refresh's posts never loaded: %v", rowTitles(m.rows[:2]))
	}
}

func TestRowsLoadedDuringAReloadAreReread(t *testing.T) {
	river, fetch := seed(t, numbered(130))
	m := start(t, river, 60, 12, noOpen(t))
	reload := m.reload()() // Covers the first 50 posts.
	for range 45 {
		press(t, m, "j") // Meanwhile an older page loads posts 50–99.
	}
	// A refresh backfills a post among those older rows before the reload lands.
	fetch.serve("alpha", fresh("backfill", "backfilled", 70*time.Minute+30*time.Second))
	if _, err := river.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	send(t, m, reload)
	if !slices.Contains(rowTitles(m.rows), "backfilled") || len(m.rows) != 101 || !strings.Contains(selected(t, m), "post 45") {
		t.Errorf("rows loaded during the reload were not reread: %d posts, selected %q", len(m.rows), selected(t, m))
	}
}

func TestRefreshDuringAHeadReadReissuesIt(t *testing.T) {
	river, fetch := seed(t, numbered(130))
	all, err := river.Timeline(t.Context(), reader.Query{Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	m := resumeAt(t, river, 60, 12, all.Items[100].Position)
	_, cmd := m.Update(key("g"))
	stale := cmd() // Read before the refresh below saves.
	fetch.serve("alpha", fresh("new", "saved during g", 0))
	if _, err := river.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	send(t, m, refreshed{})
	send(t, m, stale)
	if m.rows[0].title != "saved during g" || m.headPending {
		t.Errorf("the head read from before the refresh won: %v", rowTitles(m.rows[:2]))
	}
}

func TestQuitWaitsForWorkBeforeReturning(t *testing.T) {
	inner, _ := seed(t, numbered(3))
	river := &slowRefresh{Reader: inner}
	ctx, cancel := context.WithCancel(t.Context())
	m := newModel(ctx, river, noOpen(t), func() time.Time { return now })
	m.tick, m.animate = nil, false
	time.AfterFunc(50*time.Millisecond, cancel)
	if err := run(ctx, m, tea.WithInput(nil), tea.WithOutput(io.Discard)); !errors.Is(err, context.Canceled) {
		t.Fatalf("run returned %v", err)
	}
	if !river.finished.Load() {
		t.Error("run returned while the refresh was still using the river")
	}
	called := false
	if msg := m.work(func() tea.Msg { called = true; return nil })(); msg != nil || called {
		t.Error("a command started after the program finished still used the river")
	}
}

func TestDoubleTapToRefresh(t *testing.T) {
	river, fetch := seed(t, numbered(20))
	tap := func(m *model, after time.Duration, msg tea.KeyPressMsg) {
		advance(m, after)
		send(t, m, msg)
	}
	repeat := tea.KeyPressMsg{Code: 'k', Text: "k", IsRepeat: true}
	for _, tc := range []struct {
		name    string
		taps    func(m *model)
		refresh bool
	}{
		{"single tap", func(m *model) { tap(m, time.Second, key("k")) }, false},
		{"double tap", func(m *model) { tap(m, time.Second, key("k")); tap(m, 150*time.Millisecond, key("k")) }, true},
		{"arrow double tap", func(m *model) { tap(m, time.Second, key("up")); tap(m, 300*time.Millisecond, key("up")) }, true},
		{"taps too far apart", func(m *model) { tap(m, time.Second, key("k")); tap(m, 500*time.Millisecond, key("k")) }, false},
		{"another key between taps", func(m *model) {
			tap(m, time.Second, key("k"))
			tap(m, 100*time.Millisecond, key("o"))
			tap(m, 100*time.Millisecond, key("k"))
		}, false},
		{"flagged autorepeat", func(m *model) {
			for range 20 {
				tap(m, 150*time.Millisecond, repeat)
			}
		}, false},
		{"unflagged autorepeat", func(m *model) {
			for range 20 {
				tap(m, 30*time.Millisecond, key("k"))
			}
		}, false},
		{"single tap after holding", func(m *model) {
			for range 20 {
				tap(m, 30*time.Millisecond, key("k"))
			}
			tap(m, 200*time.Millisecond, key("k"))
		}, false},
		{"double tap after holding", func(m *model) {
			for range 20 {
				tap(m, 30*time.Millisecond, key("k"))
			}
			tap(m, 200*time.Millisecond, key("k"))
			tap(m, 150*time.Millisecond, key("k"))
		}, true},
		{"arriving at the top is not a tap", func(m *model) {
			tap(m, time.Second, key("j"))
			tap(m, time.Second, key("k"))
			tap(m, 150*time.Millisecond, key("k"))
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := start(t, river, 60, 8, func(string) error { return nil })
			calls := fetch.calls
			tc.taps(m)
			if refreshed := fetch.calls > calls; refreshed != tc.refresh {
				t.Errorf("refreshed = %v, want %v", refreshed, tc.refresh)
			}
		})
	}

	m := start(t, river, 60, 8, noOpen(t))
	tap(m, time.Second, key("k"))
	if status := strings.TrimSpace(screen(m)[7]); status != "Press again to refresh." {
		t.Errorf("the first tap did not explain the gesture: %q", status)
	}
}

// counting records how often the TUI reports reaching a post.
type counting struct {
	*reader.Reader
	reaches atomic.Int32
}

func (c *counting) Reached(position reader.Position) {
	c.reaches.Add(1)
	c.Reader.Reached(position)
}

// saveAt makes the post titled title the saved reading position, as an earlier session would have.
func saveAt(t *testing.T, river *reader.Reader, title string) reader.Position {
	t.Helper()
	for query := (reader.Query{Limit: 200}); ; {
		page, err := river.Timeline(t.Context(), query)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range page.Items {
			if item.Title == title {
				river.Reached(item.Position)
				if err := river.SaveSeen(t.Context()); err != nil {
					t.Fatal(err)
				}
				return item.Position
			}
		}
		if page.Next == nil {
			t.Fatalf("no post titled %q", title)
		}
		query.Before = page.Next
	}
}

func savedTitle(t *testing.T, m *model) string {
	t.Helper()
	position, err := m.river.SeenPosition(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if position == nil {
		return ""
	}
	all, err := m.river.Timeline(t.Context(), reader.Query{Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range all.Items {
		if item.Position == *position {
			return item.Title
		}
	}
	return "(not in the newest 200)"
}

// dividerAt returns the screen line index of the divider, or -1.
func dividerAt(m *model) int {
	for i, line := range screen(m) {
		if strings.HasPrefix(line, "── You left off here ") {
			return i
		}
	}
	return -1
}

func TestResumesWhereTheReaderLeftOff(t *testing.T) {
	inner, _ := seed(t, numbered(130))
	saved := saveAt(t, inner, "post 100")
	river := &counting{Reader: inner}
	m := start(t, river, 60, 16, noOpen(t))
	if !strings.Contains(selected(t, m), "post 100") || m.divider == nil || *m.divider != saved {
		t.Fatalf("resumed at %q with divider %v", selected(t, m), m.divider)
	}
	// The nearest newer page (posts 50–99) with more above it, then everything below: posts 100–129.
	if m.newer == nil || m.rows[0].title != "post 50" || len(m.rows) != 80 {
		t.Errorf("window starts at %q with %d posts, newer %v", m.rows[0].title, len(m.rows), m.newer)
	}
	lines := screen(m)
	divider := dividerAt(m)
	if divider < 0 || !strings.HasPrefix(lines[divider+1], "▸ ") || !strings.Contains(lines[divider-1], "post 99") {
		t.Fatalf("divider is not between post 99 and the resumed post:\n%s", strings.Join(lines, "\n"))
	}
	if screenRow := m.cursor - m.offset; screenRow < m.visible()/2 {
		t.Errorf("resumed post is on screen row %d, want the lower part so newer posts show above it", screenRow)
	}
	if got := strings.TrimRight(lines[divider], " "); got != "── You left off here "+strings.Repeat("─", 60-21) {
		t.Errorf("divider = %q", got)
	}
	if river.reaches.Load() != 0 || savedTitle(t, m) != "post 100" {
		t.Errorf("launch placement or the launch refresh reached %d posts; saved %q", river.reaches.Load(), savedTitle(t, m))
	}
}

func TestResumeEdges(t *testing.T) {
	t.Run("no saved position", func(t *testing.T) {
		river, _ := seed(t, numbered(10))
		m := start(t, river, 60, 12, noOpen(t))
		if m.divider != nil || dividerAt(m) >= 0 || !strings.Contains(selected(t, m), "post 0") {
			t.Errorf("first launch drew a divider or did not open at the newest post")
		}
	})
	t.Run("saved at the newest post", func(t *testing.T) {
		river, _ := seed(t, numbered(10))
		saveAt(t, river, "post 0")
		m := start(t, river, 60, 12, noOpen(t))
		if !strings.Contains(selected(t, m), "post 0") || m.newer != nil || dividerAt(m) != 1 {
			t.Errorf("caught up: selected %q, divider on line %d", selected(t, m), dividerAt(m))
		}
	})
	t.Run("nothing at or below the saved position", func(t *testing.T) {
		river, fetch := seed(t, numbered(10))
		saveAt(t, river, "post 9")
		fetch.serve("alpha", fresh("id:9", "post 9", 30*time.Second)) // The oldest post moves to the top.
		if _, err := river.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
		m := start(t, river, 60, 16, noOpen(t))
		if !strings.Contains(selected(t, m), "post 8") || m.cursor != len(m.rows)-1 {
			t.Errorf("cursor should start on the oldest post above the saved position, got %q", selected(t, m))
		}
		if divider := dividerAt(m); divider < 0 || !strings.HasPrefix(screen(m)[divider-1], "▸ ") {
			t.Errorf("divider should sit below every loaded post:\n%s", strings.Join(screen(m), "\n"))
		}
	})
}

func TestReadingUpwardSavesTheFurthestPost(t *testing.T) {
	inner, _ := seed(t, numbered(130))
	saveAt(t, inner, "post 100")
	river := &counting{Reader: inner}
	m := start(t, river, 60, 16, noOpen(t))
	press(t, m, "k", "k")
	if river.reaches.Load() != 4 {
		t.Errorf("two moves reported %d reaches, want the post left and landed on for each", river.reaches.Load())
	}
	stale := m.reach - 1
	send(t, m, seenIdle{stale})
	if savedTitle(t, m) != "post 100" {
		t.Error("a superseded idle timer saved")
	}
	send(t, m, seenIdle{m.reach})
	if savedTitle(t, m) != "post 98" {
		t.Errorf("after pausing, saved %q, want the furthest post reached", savedTitle(t, m))
	}
	for range 5 {
		press(t, m, "j") // Reading older never moves the saved position back.
	}
	send(t, m, seenIdle{m.reach})
	if savedTitle(t, m) != "post 98" {
		t.Errorf("scrolling older moved the saved position to %q", savedTitle(t, m))
	}
}

func TestFirstLaunchSavesTheHeadOnTheFirstMove(t *testing.T) {
	river, _ := seed(t, numbered(10))
	m := start(t, river, 60, 12, noOpen(t))
	press(t, m, "j")
	send(t, m, seenIdle{m.reach})
	if savedTitle(t, m) != "post 0" {
		t.Errorf("saved %q, want the newest post the reader moved away from", savedTitle(t, m))
	}
}

func TestJumpAndOpenReachTheirPost(t *testing.T) {
	inner, _ := seed(t, numbered(130))
	saveAt(t, inner, "post 100")
	m := start(t, inner, 60, 16, noOpen(t))
	press(t, m, "g")
	send(t, m, seenIdle{m.reach})
	if !strings.Contains(selected(t, m), "post 0") || savedTitle(t, m) != "post 0" {
		t.Errorf("g selected %q and saved %q; want the newest post for both", selected(t, m), savedTitle(t, m))
	}

	fresh, _ := seed(t, numbered(10))
	m = start(t, fresh, 60, 12, func(string) error { return nil })
	press(t, m, "o")
	send(t, m, seenIdle{m.reach})
	if savedTitle(t, m) != "post 0" {
		t.Errorf("opening a post saved %q", savedTitle(t, m))
	}
}

func TestRefreshSavesAndNeverReaches(t *testing.T) {
	inner, fetch := seed(t, numbered(20))
	river := &counting{Reader: inner}
	m := start(t, river, 60, 12, noOpen(t))
	press(t, m, "j", "k") // Reached without pausing.
	reaches := river.reaches.Load()
	fetch.serve("alpha", fresh("new", "fresh post", 0))
	pull(t, m)
	if savedTitle(t, m) != "post 0" {
		t.Errorf("a refresh did not save the reached position: %q", savedTitle(t, m))
	}
	if river.reaches.Load() != reaches {
		t.Error("the refresh or its reconciliation reported reaching a post")
	}
}

func TestExitSavesTheReadingPosition(t *testing.T) {
	river, _ := seed(t, numbered(3))
	ctx, cancel := context.WithCancel(t.Context())
	m := newModel(ctx, river, noOpen(t), func() time.Time { return now })
	m.tick, m.animate, m.saveDelay = nil, false, 0
	head, err := river.Timeline(t.Context(), reader.Query{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	river.Reached(head.Items[0].Position) // Reached, but not yet saved, when a signal ends the program.
	time.AfterFunc(50*time.Millisecond, cancel)
	if err := run(ctx, m, tea.WithInput(nil), tea.WithOutput(io.Discard)); !errors.Is(err, context.Canceled) {
		t.Fatalf("run returned %v", err)
	}
	if savedTitle(t, m) != "post 0" {
		t.Errorf("exit did not save the reached position: %q", savedTitle(t, m))
	}
}

func TestDividerAppearsOnlyWhereItFalls(t *testing.T) {
	river, _ := seed(t, numbered(130))
	saveAt(t, river, "post 100")
	m := start(t, river, 60, 16, noOpen(t))
	press(t, m, "g")                                     // The newest page does not reach post 100.
	send(t, m, tea.WindowSizeMsg{Width: 60, Height: 60}) // Show the whole window, down to its last post.
	if len(m.rows) != 50 || m.older == nil || dividerAt(m) >= 0 {
		t.Fatalf("a divider was drawn next to posts that are not loaded (%d loaded):\n%s", len(m.rows), strings.Join(screen(m), "\n"))
	}
	send(t, m, tea.WindowSizeMsg{Width: 60, Height: 16})
	for !strings.Contains(selected(t, m), "post 100") {
		press(t, m, "j")
	}
	divider := dividerAt(m)
	if divider < 0 || !strings.Contains(screen(m)[divider-1], "post 99") || !strings.Contains(screen(m)[divider+1], "post 100") {
		t.Errorf("divider not between post 99 and post 100 after scrolling down:\n%s", strings.Join(screen(m), "\n"))
	}
	for _, height := range []int{3, 4, 5, 8} {
		send(t, m, tea.WindowSizeMsg{Width: 60, Height: height})
		if len(screen(m)) != height {
			t.Errorf("height %d rendered %d lines", height, len(screen(m)))
		}
		selected(t, m) // The divider never pushes the cursor off screen.
	}
}

func TestLaunchRefreshBeforeTheWindowStillResumes(t *testing.T) {
	river, _ := seed(t, numbered(130))
	saveAt(t, river, "post 100")
	m := build(t, river, noOpen(t))
	send(t, m, tea.WindowSizeMsg{Width: 60, Height: 16})
	first := m.readWindow(m.resumeRead)() // The launch read, still in flight.
	send(t, m, refreshed{})               // The launch refresh finishes first and rereads the window.
	send(t, m, first)
	if !strings.Contains(selected(t, m), "post 100") || m.divider == nil {
		t.Errorf("rereading during launch did not resume: selected %q", selected(t, m))
	}
}

func TestJumpSurvivesASupersedeAndARetry(t *testing.T) {
	inner, _ := seed(t, numbered(130))
	saveAt(t, inner, "post 100")
	m := start(t, inner, 60, 16, noOpen(t))
	_, cmd := m.Update(key("g"))
	stale := cmd() // The jump read, overtaken by a refresh's reread below.
	send(t, m, refreshed{})
	send(t, m, stale)
	send(t, m, seenIdle{m.reach})
	if !strings.Contains(selected(t, m), "post 0") || savedTitle(t, m) != "post 0" {
		t.Errorf("a superseded jump lost its skip: selected %q, saved %q", selected(t, m), savedTitle(t, m))
	}

	failing, _ := seed(t, numbered(130))
	saveAt(t, failing, "post 100")
	river := &flaky{Reader: failing}
	m = start(t, river, 60, 16, noOpen(t))
	river.failures = 1
	press(t, m, "g") // Fails; the next move retries the jump, not a plain newest-posts read.
	press(t, m, "j")
	send(t, m, seenIdle{m.reach})
	if !strings.Contains(selected(t, m), "post 0") || savedTitle(t, m) != "post 0" {
		t.Errorf("a retried jump lost its skip: selected %q, saved %q", selected(t, m), savedTitle(t, m))
	}
}

func TestResumePlacementWaitsForTheTerminalSize(t *testing.T) {
	river, _ := seed(t, numbered(130))
	saveAt(t, river, "post 100")
	m := build(t, river, noOpen(t))
	send(t, m, m.readWindow(m.resumeRead)()) // Arrives before the first size.
	send(t, m, tea.WindowSizeMsg{Width: 60, Height: 16})
	if screenRow := m.cursor - m.offset; !strings.Contains(selected(t, m), "post 100") || screenRow < m.visible()/2 {
		t.Errorf("resumed post is on screen row %d, want the lower part", screenRow)
	}
}

func TestDividerReservesALineOnlyWhenLoaded(t *testing.T) {
	river, _ := seed(t, numbered(130))
	saveAt(t, river, "post 100")
	m := start(t, river, 60, 16, noOpen(t))
	press(t, m, "g") // The newest page does not reach the divider.
	for range m.visible() - 3 {
		press(t, m, "j") // Down to the last row that fits with a two-row margin below it.
	}
	if m.offset != 0 {
		t.Errorf("scrolled early (offset %d): a line was set aside for a divider that is not loaded", m.offset)
	}
}

func TestOnlyOpenedArticlesAreReached(t *testing.T) {
	inner, _ := seed(t, numbered(3))
	river := &counting{Reader: inner}
	m := start(t, river, 60, 8, func(string) error { return errors.New("no browser") })
	press(t, m, "o")
	if river.reaches.Load() != 0 || !strings.Contains(screen(m)[7], "no browser") {
		t.Errorf("a failed open reached %d posts", river.reaches.Load())
	}
}

func TestEachMoveSchedulesOneSave(t *testing.T) {
	river, _ := seed(t, numbered(10))
	m := start(t, river, 60, 8, noOpen(t))
	before := m.reach
	press(t, m, "j")
	if m.reach != before+1 {
		t.Errorf("one move scheduled %d saves", m.reach-before)
	}
}
