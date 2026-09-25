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
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/JoshuaFurman/openfeed/internal/config"
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

// seed caches posts for each named feed and returns a reader over them, plus the store for tests to break.
func seed(t *testing.T, posts []post, extraFeeds ...string) (*reader.Reader, *store.Store) {
	t.Helper()
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "openfeed.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	byFeed := map[string][]store.RefreshItem{}
	var feeds []config.Feed
	for _, name := range extraFeeds {
		feeds = append(feeds, config.Feed{URL: "https://" + strings.ReplaceAll(name, " ", "-") + ".example/feed", Name: name})
	}
	for i, p := range posts {
		url := "https://" + p.feed + ".example/feed"
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
	return reader.New(feeds, st, nil, nil), st
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

func key(s string) tea.KeyPressMsg {
	switch s {
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
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

func press(t *testing.T, m *model, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if send(t, m, key(k)) {
			t.Fatalf("%q quit", k)
		}
	}
}

// build makes a model without its minute tick, which would block these synchronous tests.
func build(t *testing.T, river River, launch reader.Report, open func(string) error) *model {
	m := newModel(t.Context(), river, launch, open, func() time.Time { return now })
	m.tick = nil
	return m
}

func start(t *testing.T, river River, launch reader.Report, width, height int, open func(string) error) *model {
	t.Helper()
	m := build(t, river, launch, open)
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
	m := start(t, river, reader.Report{}, 60, 8, noOpen(t))
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
	river, _ := seed(t, []post{{"a-feed-with-a-very-long-configured-name", strings.Repeat("long title ", 20), "https://a.example/1", time.Hour}})
	launch := reader.Report{Feeds: []reader.FeedResult{{Feed: config.Feed{Name: strings.Repeat("x", 100)}, Status: reader.Failing, Err: errors.New("down")}}}
	for _, width := range []int{12, 30, 50, 80} {
		m := start(t, river, launch, width, 5, noOpen(t))
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
	m := start(t, river, reader.Report{}, 80, 12, noOpen(t))
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
	m := start(t, river, reader.Report{}, 40, 12, noOpen(t))
	if len(m.rows) != 50 || m.older == nil {
		t.Fatalf("first page holds %d posts", len(m.rows))
	}
	press(t, m, "k") // Nothing above the newest post yet: pull-to-refresh arrives in increment 3.
	if m.cursor != 0 {
		t.Fatal("moved above the newest post")
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
	m := start(t, river, reader.Report{}, width, height, noOpen(t))
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
	river := &flaky{Reader: inner, failures: 2}
	m := start(t, river, reader.Report{}, 60, 6, noOpen(t))
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
	m := start(t, river, reader.Report{}, 60, 6, func(url string) error {
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
	m := start(t, river, reader.Report{}, 40, 20, noOpen(t))
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
	empty, _ := seed(t, nil)
	m := build(t, empty, reader.Report{}, noOpen(t))
	if !strings.Contains(strings.Join(screen(m), "\n"), "Loading…") {
		t.Error("no loading state before the first page")
	}
	send(t, m, m.Init()())
	if !strings.Contains(strings.Join(screen(m), "\n"), "No posts yet") {
		t.Error("no empty state")
	}

	failing := func(names ...string) reader.Report {
		var report reader.Report
		for _, name := range names {
			report.Feeds = append(report.Feeds, reader.FeedResult{Feed: config.Feed{Name: name}, Status: reader.Failing, Err: errors.New("HTTP 503")})
		}
		report.Feeds = append(report.Feeds, reader.FeedResult{Feed: config.Feed{Name: "fine"}, Status: reader.Refreshed})
		return report
	}
	for _, tc := range []struct {
		report reader.Report
		want   string
	}{
		{failing("lobsters"), "lobsters failed: HTTP 503"},
		{failing("lobsters", "go blog"), "2 feeds failing: lobsters, go blog (details in the log)"},
	} {
		m := start(t, empty, tc.report, 80, 4, noOpen(t))
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
	m := start(t, river, reader.Report{}, 40, 12, noOpen(t))
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
		m := start(t, river, reader.Report{}, 40, 6, noOpen(t))
		if !send(t, m, key(k)) {
			t.Errorf("%s did not quit", k)
		}
	}
	m := start(t, river, reader.Report{}, 40, 6, noOpen(t))
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
	production := newModel(t.Context(), river, reader.Report{}, noOpen(t), time.Now)
	if production.tick == nil {
		t.Error("ages are never redrawn while idle")
	}
}

func TestRunReturnsContextError(t *testing.T) {
	river, _ := seed(t, numbered(3))
	ctx, cancel := context.WithCancel(t.Context())
	m := build(t, river, reader.Report{}, noOpen(t))
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
