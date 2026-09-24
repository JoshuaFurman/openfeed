package store

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// collectNewer follows Newer continuations upward and returns everything above the start, newest first.
func collectNewer(t *testing.T, s *Store, query TimelineQuery) []TimelineItem {
	t.Helper()
	var items []TimelineItem
	for range 1000 {
		page := readTimeline(t, s, query)
		if page.Next != nil {
			t.Fatal("After read returned an older continuation")
		}
		if page.Newer != nil && *page.Newer != page.Items[0].Position {
			t.Fatal("Newer is not the newest returned position")
		}
		items = append(slices.Clone(page.Items), items...)
		if page.Newer == nil {
			return items
		}
		query.After = page.Newer
	}
	t.Fatal("upward reading did not reach the head")
	return nil
}

func TestTimelineAfterPaging(t *testing.T) {
	s := openStore(t, filepath.Join(t.TempDir(), "openfeed.db"))
	at := sampleRefresh().FetchedAt
	var urls []string
	for feed := range 3 {
		url := fmt.Sprintf("https://%d.example/feed", feed)
		var entries []RefreshItem
		for i := range 40 {
			published := at.Add(-time.Duration(i%3) * time.Nanosecond)
			if i%4 == 0 {
				published = time.Time{}
			}
			entries = append(entries, timelineEntry(fmt.Sprintf("%d-%d", feed, i), published))
		}
		saveRefresh(t, s, url, Refresh{Items: entries, FetchedAt: at})
		if feed != 1 {
			urls = append(urls, url) // Feed 1 stays cached but inactive.
		}
	}
	all := collectTimeline(t, s, TimelineQuery{FeedURLs: urls})
	for _, start := range []int{0, 1, 17, len(all) - 1} {
		for _, limit := range []int{0, 1, 2, 7, 200} {
			t.Run(fmt.Sprintf("above %d limit %d", start, limit), func(t *testing.T) {
				got := collectNewer(t, s, TimelineQuery{FeedURLs: urls, After: &all[start].Position, Limit: limit})
				if !slices.Equal(got, all[:start]) {
					t.Errorf("upward pages = %v, want %v", timelineTitles(got), timelineTitles(all[:start]))
				}
			})
		}
	}
}

func TestTimelineAfterEdges(t *testing.T) {
	s := openStore(t, filepath.Join(t.TempDir(), "openfeed.db"))
	const url = "https://example.com/feed"
	at := sampleRefresh().FetchedAt
	saveRefresh(t, s, url, Refresh{FetchedAt: fixtureFetch(), Items: []RefreshItem{
		timelineEntry("newest", at.Add(3*time.Hour)),
		timelineEntry("tie-low", at.Add(2*time.Hour)), timelineEntry("tie-high", at.Add(2*time.Hour)),
		timelineEntry("older", at.Add(time.Hour)), timelineEntry("oldest", at),
	}})
	all := readTimeline(t, s, TimelineQuery{FeedURLs: []string{url}}).Items
	if got := timelineTitles(all); !slices.Equal(got, []string{"newest", "tie-high", "tie-low", "older", "oldest"}) {
		t.Fatalf("fixture order = %v", got)
	}
	for _, tc := range []struct {
		name  string
		after int
		limit int
		want  []string
		newer bool
	}{
		{"above the newest", 0, 0, nil, false},
		{"exactly filled", 4, 4, []string{"newest", "tie-high", "tie-low", "older"}, false},
		{"nearest first", 4, 3, []string{"tie-high", "tie-low", "older"}, true},
		{"above a tie", 2, 0, []string{"newest", "tie-high"}, false},
		{"nearest above a tie", 2, 1, []string{"tie-high"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page := readTimeline(t, s, TimelineQuery{FeedURLs: []string{url}, After: &all[tc.after].Position, Limit: tc.limit})
			if got := timelineTitles(page.Items); !slices.Equal(got, tc.want) || (page.Newer != nil) != tc.newer || page.Next != nil {
				t.Errorf("page = %v, newer %v, next %v; want %v, newer %v", got, page.Newer, page.Next, tc.want, tc.newer)
			}
		})
	}
	if page := readTimeline(t, s, TimelineQuery{After: &all[4].Position}); len(page.Items) != 0 || page.Newer != nil {
		t.Errorf("empty selection returned %+v", page)
	}
}

func TestTimelineAfterRejectsCombinations(t *testing.T) {
	s := openStore(t, filepath.Join(t.TempDir(), "openfeed.db"))
	position := &TimelineCursor{date: sampleRefresh().FetchedAt.Format(timestampLayout), id: 1}
	for name, query := range map[string]TimelineQuery{
		"after with before":  {After: position, Before: position},
		"after with through": {After: position, Through: position},
	} {
		page, err := s.Timeline(t.Context(), query)
		if err == nil || !strings.Contains(err.Error(), "after cannot be combined") || len(page.Items) != 0 {
			t.Errorf("%s returned %+v, %v", name, page, err)
		}
	}
	if _, err := s.Timeline(t.Context(), TimelineQuery{After: &TimelineCursor{}}); err == nil || !strings.Contains(err.Error(), "after cursor") {
		t.Errorf("zero after cursor returned %v", err)
	}
}

func TestTimelineAfterResumesAtSavedPosition(t *testing.T) {
	for _, tc := range []struct {
		name         string
		saved        string
		change       func(t *testing.T, s *Store, url string, saved TimelineItem)
		above, below []string
	}{
		{name: "left-off post in place", saved: "p4", above: []string{"p2", "p3"}, below: []string{"p4", "p5", "p6"}},
		{name: "left-off post is newest", saved: "p1", below: []string{"p1", "p2", "p3", "p4", "p5", "p6"}},
		{
			name: "left-off post moved newer", saved: "p4",
			change: func(t *testing.T, s *Store, url string, _ TimelineItem) {
				saveRefresh(t, s, url, Refresh{FetchedAt: fixtureFetch(), Items: []RefreshItem{
					timelineEntry("p4", sampleRefresh().FetchedAt.Add(10*time.Hour)),
				}})
			},
			above: []string{"p2", "p3"}, below: []string{"p5", "p6"},
		},
		{
			// For example, the older posts' subscription was removed from config.
			name: "nothing at or below the saved position", saved: "p6",
			change: func(t *testing.T, s *Store, _ string, saved TimelineItem) {
				if _, err := s.db.ExecContext(t.Context(), "DELETE FROM items WHERE id = ?", saved.ID); err != nil {
					t.Fatal(err)
				}
			},
			above: []string{"p4", "p5"},
		},
		{
			name: "left-off post removed", saved: "p4",
			change: func(t *testing.T, s *Store, _ string, saved TimelineItem) {
				if _, err := s.db.ExecContext(t.Context(), "DELETE FROM items WHERE id = ?", saved.ID); err != nil {
					t.Fatal(err)
				}
			},
			above: []string{"p2", "p3"}, below: []string{"p5", "p6"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openStore(t, filepath.Join(t.TempDir(), "openfeed.db"))
			const url = "https://example.com/feed"
			at := sampleRefresh().FetchedAt
			var entries []RefreshItem
			for i := range 6 {
				entries = append(entries, timelineEntry(fmt.Sprintf("p%d", i+1), at.Add(-time.Duration(i)*time.Hour)))
			}
			saveRefresh(t, s, url, Refresh{FetchedAt: fixtureFetch(), Items: entries})
			var saved TimelineItem
			for _, item := range readTimeline(t, s, TimelineQuery{FeedURLs: []string{url}}).Items {
				if item.Title == tc.saved {
					saved = item
				}
			}
			if tc.change != nil {
				tc.change(t, s, url, saved)
			}

			above := readTimeline(t, s, TimelineQuery{FeedURLs: []string{url}, After: &saved.Position, Limit: 2})
			below := TimelineQuery{FeedURLs: []string{url}}
			if len(above.Items) > 0 {
				below.Before = &above.Items[len(above.Items)-1].Position
			}
			if got := timelineTitles(above.Items); !slices.Equal(got, tc.above) {
				t.Errorf("above the saved position = %v, want %v", got, tc.above)
			}
			if got := timelineTitles(collectTimeline(t, s, below)); !slices.Equal(got, tc.below) {
				t.Errorf("at and below the saved position = %v, want %v", got, tc.below)
			}
		})
	}
}
