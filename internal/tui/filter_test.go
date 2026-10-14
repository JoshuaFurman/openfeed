package tui

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// topics tags alpha "tech" and beta "news".
var topics = map[string][]string{"alpha": {"tech"}, "beta": {"news"}}

// mixed makes n posts a minute apart, newest first: even ones in alpha, odd ones in beta.
func mixed(n int) []post {
	posts := make([]post, n)
	for i := range posts {
		name := []string{"alpha", "beta"}[i%2]
		posts[i] = post{feed: name, title: fmt.Sprintf("post %d", i), url: fmt.Sprintf("https://%s.example/%d", name, i), age: time.Duration(i+1) * time.Minute}
	}
	return posts
}

// choices returns the tag picker's choices as drawn, the selected one marked with ▸.
func choices(m *model) []string {
	var got []string
	for _, line := range screen(m) {
		first, last := strings.Index(line, "│"), strings.LastIndex(line, "│")
		if first < 0 || first == last {
			continue
		}
		if text := strings.TrimSpace(line[first+len("│") : last]); text != "" && text != pickerTitle && text != pickerHints {
			got = append(got, text)
		}
	}
	return got
}

func TestTagPicker(t *testing.T) {
	tags := map[string][]string{"alpha": {"tech"}, "beta": {"news", "TECH"}, "gamma": {"friends\x1b[2J"}}
	river, _ := seedTagged(t, tags, mixed(6), "gamma")
	m := start(t, river, 80, 12, noOpen(t))

	press(t, m, "/")
	if got := choices(m); !slices.Equal(got, []string{"All posts", "▸ friends [2J", "tech", "news"}) {
		t.Fatalf("picker = %q, want every tag once, in config order, starting on the first", got)
	}
	if strings.Contains(m.View().Content, "\x1b[2J") {
		t.Error("a tag reached the terminal unsanitized")
	}
	if !strings.Contains(m.View().Content, lipgloss.NewStyle().Foreground(lipgloss.Magenta).Render("tech")) {
		t.Error("the picker does not draw tech in its color, the second in config order")
	}
	press(t, m, "j")
	if got := choices(m); !slices.Equal(got, []string{"All posts", "friends [2J", "▸ tech", "news"}) || m.cursor != 0 {
		t.Errorf("after j: picker %q, river cursor %d; want the picker to move and the river to stay", got, m.cursor)
	}
	press(t, m, "j", "j", "down")
	if got := choices(m); got[3] != "▸ news" {
		t.Errorf("picker = %q, want it to stop on the last tag", got)
	}
	press(t, m, "k", "k", "k", "up", "k")
	if got := choices(m); got[0] != "▸ All posts" {
		t.Errorf("picker = %q, want it to stop on All posts", got)
	}

	for _, closing := range []string{"esc", "q"} {
		press(t, m, closing)
		if m.picker != nil || m.tag != "" || len(choices(m)) > 0 {
			t.Errorf("%s left the picker open or changed the filter to %q", closing, m.tag)
		}
		press(t, m, "/")
	}
	press(t, m, "j", "enter")
	if m.picker != nil || m.tag != "tech" || !strings.Contains(screen(m)[0], " openfeed #tech ") {
		t.Errorf("enter: tag %q, header %q; want the tech filter shown", m.tag, screen(m)[0])
	}
	press(t, m, "/")
	if got := choices(m); got[2] != "▸ tech" {
		t.Errorf("reopened picker = %q, want it to start on the active filter", got)
	}
	if !send(t, m, key("ctrl+c")) {
		t.Error("ctrl+c in the picker did not quit")
	}

	untagged, _ := seed(t, numbered(3))
	m = start(t, untagged, 80, 6, noOpen(t))
	press(t, m, "/")
	if m.picker != nil || !strings.Contains(screen(m)[5], "No tags yet.") {
		t.Errorf("without tags, / opened a picker or said nothing: %q", screen(m)[5])
	}
}

func TestTagPickerFitsTheTerminal(t *testing.T) {
	long := strings.Repeat("long", 30)
	tags := map[string][]string{"alpha": {"a", "b", "c", "d", "e", "f", long}}
	river, _ := seedTagged(t, tags, mixed(4))
	for _, size := range [][2]int{{80, 24}, {40, 9}, {20, 8}, {12, 3}} {
		m := start(t, river, size[0], size[1], noOpen(t))
		press(t, m, "/")
		for range 6 {
			press(t, m, "j")
		}
		lines := screen(m)
		for _, line := range lines {
			if w := ansi.StringWidth(line); w > size[0] {
				t.Errorf("%dx%d: line %q is %d cells", size[0], size[1], line, w)
			}
		}
		if len(lines) > size[1] {
			t.Errorf("%dx%d: %d lines", size[0], size[1], len(lines))
		}
		if got := choices(m); size[1] > 6 && !slices.ContainsFunc(got, func(c string) bool { return strings.HasPrefix(c, "▸ long") }) {
			t.Errorf("%dx%d: picker %q does not show the selected tag", size[0], size[1], got)
		}
	}
}

func TestFilterNarrowsTheRiver(t *testing.T) {
	river, _ := seedTagged(t, topics, mixed(130))
	m := start(t, river, 60, 16, noOpen(t))
	press(t, m, "/", "enter")
	for range 80 {
		press(t, m, "j") // Through every older page.
	}
	var want []string
	for i := 0; i < 130; i += 2 {
		want = append(want, fmt.Sprintf("post %d", i))
	}
	if got := rowTitles(m.rows); !slices.Equal(got, want) {
		t.Errorf("tech river = %q, want alpha's posts once each", got)
	}

	press(t, m, "esc")
	if m.tag != "" || !strings.Contains(selected(t, m), "post 0") || len(m.rows) != 50 {
		t.Errorf("esc: tag %q, selected %q, %d rows; want the whole river where it was left", m.tag, selected(t, m), len(m.rows))
	}
	press(t, m, "esc") // Already the whole river.
	if !strings.Contains(selected(t, m), "post 0") {
		t.Errorf("a second esc moved the cursor to %q", selected(t, m))
	}
}

func TestFilterKeepsThePlaceAndClearingReturns(t *testing.T) {
	inner, _ := seedTagged(t, topics, mixed(130))
	saveAt(t, inner, "post 101")
	river := &counting{Reader: inner}
	opened := 0
	m := start(t, river, 60, 16, func(string) error { opened++; return nil })

	press(t, m, "/", "enter")
	if !strings.Contains(selected(t, m), "post 102") {
		t.Fatalf("tech opened at %q, want the newest tech post at or below post 101", selected(t, m))
	}
	if d := dividerAt(m); d < 0 || !strings.Contains(screen(m)[d+1], "post 102") || !strings.Contains(screen(m)[d-1], "post 100") {
		t.Errorf("the divider is not at the saved position in the filtered view:\n%s", strings.Join(screen(m), "\n"))
	}
	for range 20 {
		press(t, m, "k")
	}
	press(t, m, "o", "g")
	send(t, m, seenIdle{m.reach})
	if river.reaches.Load() != 0 || savedTitle(t, m) != "post 101" || opened != 1 {
		t.Errorf("a filtered view reached %d posts and saved %q; want neither", river.reaches.Load(), savedTitle(t, m))
	}

	press(t, m, "j", "j", "/", "j", "enter") // From post 4 to the news filter.
	if m.tag != "news" || !strings.Contains(selected(t, m), "post 5") {
		t.Errorf("switching tags: %q at %q, want news at post 5", m.tag, selected(t, m))
	}
	press(t, m, "esc")
	if m.tag != "" || !strings.Contains(selected(t, m), "post 101") {
		t.Errorf("esc returned to %q, want post 101, where the whole river was left", selected(t, m))
	}
	press(t, m, "k") // One post up, not to where the filtered view had read.
	send(t, m, seenIdle{m.reach})
	if river.reaches.Load() == 0 || savedTitle(t, m) != "post 100" {
		t.Errorf("back in the whole river, a move reached %d posts and saved %q", river.reaches.Load(), savedTitle(t, m))
	}
}

func TestFilterDuringTheLaunchReadStillResumes(t *testing.T) {
	river, _ := seedTagged(t, topics, mixed(130))
	saved := saveAt(t, river, "post 101")
	m := build(t, river, noOpen(t))
	send(t, m, tea.WindowSizeMsg{Width: 60, Height: 16})
	launch := m.readWindow(windowRead{resume: true}) // Still in flight when the filter applies.
	press(t, m, "/", "enter")
	send(t, m, launch())
	if m.tag != "tech" || m.divider == nil || *m.divider != saved || !strings.Contains(selected(t, m), "post 102") {
		t.Errorf("filtering during launch: tag %q, selected %q, divider %v", m.tag, selected(t, m), m.divider)
	}
	press(t, m, "esc") // No place in the whole river was left, so it resumes at the divider.
	if !strings.Contains(selected(t, m), "post 101") {
		t.Errorf("esc returned to %q, want the saved post", selected(t, m))
	}
}

func TestFilteredRefreshAndSubscribe(t *testing.T) {
	river, fetch := seedTagged(t, topics, mixed(10))
	m := start(t, river, 60, 16, noOpen(t))
	press(t, m, "/", "enter")
	fetch.serve("alpha", fresh("a", "fresh tech", 30*time.Second))
	fetch.serve("beta", fresh("b", "fresh news", 20*time.Second))
	pull(t, m)
	if got := rowTitles(m.rows); len(got) != 6 || got[0] != "fresh tech" {
		t.Errorf("after a filtered refresh: %q, want the new tech post only", got)
	}

	fetch.serve("delta", fresh("d", "delta post", 10*time.Second))
	press(t, m, "n")
	typeText(t, m, feedURL("delta"))
	press(t, m, "tab", "tab")
	typeText(t, m, "Tech")
	press(t, m, "enter")
	if got := rowTitles(m.rows); len(got) != 7 || got[0] != "delta post" || m.tag != "tech" {
		t.Errorf("after subscribing a tech feed: %q under %q, want its post in the filtered river", got, m.tag)
	}
}

func TestEmptyFilteredView(t *testing.T) {
	river, _ := seedTagged(t, map[string][]string{"gamma": {"friends"}}, mixed(4), "gamma")
	m := start(t, river, 60, 6, noOpen(t))
	press(t, m, "/", "enter")
	if got := screen(m)[1]; !strings.Contains(got, "No posts tagged friends. Press Esc for all posts.") {
		t.Errorf("empty filtered view = %q", got)
	}
	press(t, m, "esc")
	if len(m.rows) != 4 {
		t.Errorf("esc from an empty view loaded %d posts, want the whole river", len(m.rows))
	}
}

func TestFailedFilterReadRetriesTheFilter(t *testing.T) {
	inner, _ := seedTagged(t, topics, mixed(10))
	river := &flaky{Reader: inner}
	m := start(t, river, 60, 16, noOpen(t))
	river.failures = 1
	press(t, m, "/", "enter")
	if m.tag != "" || !strings.Contains(screen(m)[15], "database is locked") {
		t.Errorf("a failed filter read: tag %q, status %q", m.tag, screen(m)[15])
	}
	press(t, m, "j")
	if m.tag != "tech" || len(m.rows) != 5 {
		t.Errorf("the retry read %q with %d posts, want the tech filter", m.tag, len(m.rows))
	}
}

func TestTagColumn(t *testing.T) {
	tags := map[string][]string{"alpha": {"tech"}, "beta": {"news", "Tech"}}
	river, _ := seedTagged(t, tags, []post{
		{"alpha", "a1", "https://a.example/1", time.Minute},
		{"beta", "b1", "https://b.example/1", 2 * time.Minute},
		{"gamma", "g1", "https://g.example/1", 3 * time.Minute},
	})
	m := start(t, river, 80, 6, noOpen(t))
	want := []string{
		"▸ 1m        alpha  tech        a1",
		"  2m        beta   news, tech  b1",
		"  3m        gamma              g1",
	}
	if got := screen(m)[1:4]; !slices.Equal(trimmed(got), want) {
		t.Errorf("river:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	tech := lipgloss.NewStyle().Foreground(lipgloss.Cyan).Render("tech")
	news := lipgloss.NewStyle().Foreground(lipgloss.Magenta).Render("news")
	lines := strings.Split(m.View().Content, "\n")
	if !strings.Contains(lines[2], news+", "+tech) {
		t.Errorf("beta's row %q does not color news and tech apart", lines[2])
	}
	if strings.Contains(lines[1], tech) {
		t.Errorf("the selected row %q colors its tags, which would end its highlight", lines[1])
	}
	press(t, m, "j")
	if lines = strings.Split(m.View().Content, "\n"); !strings.Contains(lines[1], tech) {
		t.Errorf("alpha's row %q, no longer selected, does not color tech", lines[1])
	}

	press(t, m, "/", "enter")
	if header := strings.Split(m.View().Content, "\n")[0]; !strings.Contains(header, lipgloss.NewStyle().Foreground(lipgloss.Cyan).Render("#tech")) {
		t.Errorf("header %q does not show the filter in its tag's color", header)
	}
	kept := []string{"  1m        alpha  tech        a1", "▸ 2m        beta   news, tech  b1"} // Still on b1.
	if got := trimmed(screen(m)[1:3]); !slices.Equal(got, kept) {
		t.Errorf("filtered river = %q, want the tag column kept", got)
	}
}

func TestTagColumnWidth(t *testing.T) {
	river, _ := seedTagged(t, map[string][]string{"alpha": {"technology", "programming"}, "beta": {"x\x1b[2J"}}, mixed(2))
	for _, test := range []struct {
		width int
		shown bool
	}{{80, true}, {57, true}, {56, false}, {20, false}} {
		m := start(t, river, test.width, 5, noOpen(t))
		press(t, m, "j") // Leave the first row, so the column is drawn in color.
		rows := strings.Join(screen(m)[1:3], "\n")
		if shown := strings.Contains(rows, "technology, pro…"); shown != test.shown {
			t.Errorf("width %d: column shown %v, want %v, capped at %d cells:\n%s", test.width, shown, test.shown, maxTagWidth, rows)
		}
		for _, line := range screen(m) {
			if w := ansi.StringWidth(line); w > test.width {
				t.Errorf("width %d: line %q is %d cells", test.width, line, w)
			}
		}
		if test.shown && (!strings.Contains(rows, "x [2J") || strings.Contains(m.View().Content, "\x1b[2J")) {
			t.Errorf("width %d: a tag reached the terminal unsanitized:\n%s", test.width, rows)
		}
	}

	untagged, fetch := seed(t, numbered(2))
	m := start(t, untagged, 80, 6, noOpen(t))
	if m.tagColumn() != 0 {
		t.Errorf("without tags, the tag column is %d cells", m.tagColumn())
	}
	fetch.serve("delta", fresh("d", "delta post", 10*time.Second))
	press(t, m, "n")
	typeText(t, m, feedURL("delta"))
	press(t, m, "tab", "tab")
	typeText(t, m, "news, video")
	press(t, m, "enter", "j")
	if got := strings.TrimRight(screen(m)[1], " "); got != "  now       delta.example  news, video  delta post" {
		t.Errorf("after subscribing a tagged feed, its row = %q, want a tag column", got)
	}
}

func trimmed(lines []string) []string {
	out := make([]string, len(lines))
	for i, line := range lines {
		out[i] = strings.TrimRight(line, " ")
	}
	return out
}
