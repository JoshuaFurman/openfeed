package tui

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/JoshuaFurman/openfeed/internal/config"
	"github.com/JoshuaFurman/openfeed/internal/reader"
)

// subscriptions records the feeds a river adds, resolved as config.toml would.
type subscriptions struct {
	mu    sync.Mutex
	added []config.Feed
}

func (s *subscriptions) Add(f config.Feed) (config.Feed, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := f.Normalize(); err != nil {
		return config.Feed{}, err
	}
	s.added = append(s.added, f)
	return f, nil
}

// typeText types s into the model a key at a time.
func typeText(t *testing.T, m *model, s string) {
	t.Helper()
	for _, r := range s {
		if send(t, m, tea.KeyPressMsg{Code: r, Text: string(r)}) {
			t.Fatalf("typing %q quit", r)
		}
	}
}

func popupShows(m *model) bool {
	return strings.Contains(strings.Join(screen(m), "\n"), "Add a feed")
}

// popupError is the popup's message line: an error, or the check in progress.
func popupError(m *model) string {
	lines := screen(m)
	for i, line := range lines {
		if strings.Contains(line, formHints) && i > 0 {
			return strings.Trim(lines[i-1], "│ ")
		}
	}
	return ""
}

func TestAddFeedFromThePopup(t *testing.T) {
	river, fetch := seed(t, []post{{feed: "alpha", title: "alpha post", url: "https://alpha.example/1", age: time.Hour}})
	fetch.serve("the beta blog", fresh("b-1", "beta post", time.Minute), fresh("b-2", "old beta post", 48*time.Hour))
	m := start(t, river, 80, 20, noOpen(t))
	if !strings.Contains(screen(m)[0], "n add") {
		t.Errorf("header hints do not mention n: %q", screen(m)[0])
	}

	press(t, m, "n")
	if !popupShows(m) {
		t.Fatalf("n did not open the popup:\n%s", strings.Join(screen(m), "\n"))
	}
	// Keys that act on the river type into the popup instead.
	typeText(t, m, "  https://The-Beta-Blog.example/feed ")
	press(t, m, "tab")
	typeText(t, m, "beta blog, the longest name")
	press(t, m, "shift+tab", "tab", "down")
	typeText(t, m, " tech,, daily ")
	press(t, m, "up", "up", "up", "enter")

	if popupShows(m) {
		t.Fatalf("popup still open after subscribing:\n%s", strings.Join(screen(m), "\n"))
	}
	want := config.Feed{URL: "https://the-beta-blog.example/feed", Name: "beta blog, the longest name", Tags: []string{"tech", "daily"}}
	if got := river.Feeds(); len(got) != 2 || !slices.Equal(got[1].Tags, want.Tags) || got[1].URL != want.URL || got[1].Name != want.Name {
		t.Errorf("Feeds = %+v, want %+v added", got, want)
	}
	if got := rowTitles(m.rows); !slices.Equal(got, []string{"beta post", "alpha post", "old beta post"}) {
		t.Errorf("river = %v, want the new feed's posts in place", got)
	}
	if status := screen(m)[19]; strings.TrimSpace(status) != "Subscribed to beta blog, the longest name." {
		t.Errorf("status = %q", status)
	}
	if m.nameWidth != maxNameWidth {
		t.Errorf("name column = %d, want it widened to %d", m.nameWidth, maxNameWidth)
	}
	if line := selected(t, m); !strings.Contains(line, "alpha post") {
		t.Errorf("cursor left its post for %q", line)
	}
}

func TestPopupKeysNeverReachTheRiver(t *testing.T) {
	river, _ := seed(t, numbered(5))
	m := start(t, river, 80, 20, noOpen(t))
	press(t, m, "j")
	before := m.cursor
	press(t, m, "n")
	typeText(t, m, "qjkgon")
	press(t, m, "enter", "esc") // An invalid URL fails without leaving the popup; Esc then closes it.
	if m.cursor != before || len(river.Feeds()) != 1 {
		t.Errorf("popup keys moved the river or subscribed: cursor %d, feeds %+v", m.cursor, river.Feeds())
	}
	if popupShows(m) {
		t.Error("esc did not close the popup")
	}
	press(t, m, "n")
	if got := m.form.fields[urlField].Value(); got != "" {
		t.Errorf("a reopened popup kept %q", got)
	}
	if !send(t, m, key("ctrl+c")) {
		t.Error("ctrl+c did not quit from the popup")
	}
}

func TestPopupErrors(t *testing.T) {
	river, fetch := seed(t, numbered(3))
	m := start(t, river, 80, 20, noOpen(t))
	press(t, m, "n", "enter")
	if got := popupError(m); got != "Enter the feed's URL." {
		t.Errorf("empty URL message = %q", got)
	}

	typeText(t, m, "ftp://beta.example/feed")
	if got := popupError(m); got != "" {
		t.Errorf("typing kept the message %q", got)
	}
	press(t, m, "enter")
	if got := popupError(m); !strings.Contains(got, "scheme must be http or https") {
		t.Errorf("invalid URL message = %q", got)
	}

	fetch.fail("beta", errors.New("HTTP 404\x1b[2J\u202e"))
	m.form.fields[urlField].SetValue(feedURL("beta"))
	press(t, m, "enter")
	if got := popupError(m); got != "HTTP 404 [2J" {
		t.Errorf("fetch failure message = %q, want the escape neutralized", got)
	}
	if content := m.View().Content; strings.Contains(content, "\x1b[2J") || strings.Contains(content, "\u202e") {
		t.Error("an unsanitized error reached the terminal")
	}
	if !popupShows(m) || len(river.Feeds()) != 1 {
		t.Errorf("a failed subscription closed the popup or subscribed: %+v", river.Feeds())
	}
	if !strings.Contains(m.form.fields[urlField].Value(), "beta.example") {
		t.Error("a failure discarded the URL instead of letting it be corrected")
	}
}

// uncancellable subscribes even after the popup that asked is abandoned, as one past its config write does.
type uncancellable struct{ *reader.Reader }

func (u uncancellable) Subscribe(ctx context.Context, f config.Feed) (config.Feed, error) {
	return u.Reader.Subscribe(context.WithoutCancel(ctx), f)
}

func TestAbandonedPopups(t *testing.T) {
	river, fetch := seed(t, numbered(3))
	fetch.serve("beta", fresh("b-1", "beta post", time.Minute))
	m := start(t, river, 80, 20, noOpen(t))
	press(t, m, "n")
	m.form.fields[urlField].SetValue(feedURL("beta"))
	_, check := m.Update(key("enter")) // Held, as if the fetch were slow.
	if got := popupError(m); !strings.Contains(got, "Checking the feed") {
		t.Errorf("checking message = %q", got)
	}
	if _, cmd := m.Update(m.spinner.Tick()); cmd == nil {
		t.Error("the spinner stopped while checking")
	}
	typeText(t, m, "x")
	if got := m.form.fields[urlField].Value(); got != feedURL("beta") {
		t.Errorf("typing during a check changed the URL to %q", got)
	}

	press(t, m, "esc", "n") // Abandon the check and open another popup.
	send(t, m, check())
	if got := popupError(m); got != "" || len(river.Feeds()) != 1 {
		t.Errorf("the abandoned check showed %q or subscribed %+v", got, river.Feeds())
	}
	if _, cmd := m.Update(spinner.TickMsg{}); cmd != nil {
		t.Error("the spinner kept running after the check ended")
	}

	// A check that subscribes despite being abandoned still brings its feed in.
	m.river = uncancellable{river}
	m.form.fields[urlField].SetValue(feedURL("beta"))
	_, check = m.Update(key("enter"))
	press(t, m, "esc")
	send(t, m, check())
	if len(river.Feeds()) != 2 || !slices.Contains(rowTitles(m.rows), "beta post") {
		t.Errorf("abandoned subscription missing: feeds %+v, rows %v", river.Feeds(), rowTitles(m.rows))
	}
}

func TestPastingIntoThePopup(t *testing.T) {
	river, _ := seed(t, numbered(3))
	m := start(t, river, 80, 20, noOpen(t))
	press(t, m, "n")
	send(t, m, tea.PasteMsg{Content: "https://beta.example/\u202efeed\n"})
	if got := m.form.feed().URL; got != "https://beta.example/feed" {
		t.Errorf("pasted URL = %q, want it sanitized and trimmed", got)
	}
}

func TestPopupLayoutAndCursor(t *testing.T) {
	river, _ := seed(t, numbered(30))
	for _, size := range []struct{ width, height int }{{80, 20}, {40, 12}, {20, 5}} {
		m := start(t, river, size.width, size.height, noOpen(t))
		press(t, m, "n")
		typeText(t, m, "https://b.example")
		view := m.View()
		lines := strings.Split(ansi.Strip(view.Content), "\n")
		for i, line := range lines {
			if w := ansi.StringWidth(line); w > size.width {
				t.Errorf("%dx%d: line %d is %d wide: %q", size.width, size.height, i, w, line)
			}
		}
		if size.height < 12 {
			continue // Too short to show the popup whole.
		}
		if view.Cursor == nil || view.Cursor.Y >= len(lines) {
			t.Fatalf("%dx%d: cursor %+v outside the screen", size.width, size.height, view.Cursor)
		}
		line := lines[view.Cursor.Y]
		if before := ansi.Truncate(line, view.Cursor.X, ""); !strings.Contains(line, "URL") || !strings.HasSuffix(before, "b.example") {
			t.Errorf("%dx%d: cursor at %d in %q, want it after the typed URL", size.width, size.height, view.Cursor.X, line)
		}
		if !strings.Contains(strings.Join(lines, "\n"), "alpha  post") {
			t.Errorf("%dx%d: the popup hid the whole river", size.width, size.height)
		}
	}
}

func TestFirstSubscriptionFillsAnEmptyRiver(t *testing.T) {
	river, fetch := seed(t, nil)
	fetch.serve("alpha", fresh("a-1", "first post", time.Minute))
	m := start(t, river, 80, 10, noOpen(t))
	press(t, m, "n")
	m.form.fields[urlField].SetValue(feedURL("alpha"))
	press(t, m, "enter")
	screenText := strings.Join(screen(m), "\n")
	if strings.Contains(screenText, "No subscriptions yet") || !strings.Contains(selected(t, m), "first post") {
		t.Errorf("first subscription did not fill the river:\n%s", screenText)
	}
}
