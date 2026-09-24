package store

import (
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestTimelineInsertionsBetweenPages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "openfeed.db")
	s, writer := openStore(t, path), openStore(t, path)
	const url = "https://example.com/feed"
	at := sampleRefresh().FetchedAt
	saveRefresh(t, s, url, Refresh{FetchedAt: fixtureFetch(), Items: []RefreshItem{
		timelineEntry("a", at.Add(4*time.Hour)), timelineEntry("b", at.Add(3*time.Hour)),
		timelineEntry("c", at.Add(2*time.Hour)), timelineEntry("d", at.Add(time.Hour)),
	}})
	query := TimelineQuery{FeedURLs: []string{url}, Limit: 2}
	first := readTimeline(t, s, query)
	if first.Next == nil || !slices.Equal(timelineTitles(first.Items), []string{"a", "b"}) {
		t.Fatalf("incorrect first page: %+v", first)
	}
	saveRefresh(t, writer, url, Refresh{FetchedAt: at.Add(24 * time.Hour), Items: []RefreshItem{
		timelineEntry("newer", at.Add(5*time.Hour)), timelineEntry("late", at.Add(150*time.Minute)),
		timelineEntry("same-date", at.Add(3*time.Hour)), timelineEntry("oldest", at),
	}})
	query.Before = first.Next
	if got := timelineTitles(collectTimeline(t, s, query)); !slices.Equal(got, []string{"late", "c", "d", "oldest"}) {
		t.Errorf("insertion shifted continuation: %v", got)
	}
	query.Before = nil
	if got := timelineTitles(collectTimeline(t, s, query)); !slices.Equal(got, []string{"newer", "a", "same-date", "b", "late", "c", "d", "oldest"}) {
		t.Errorf("fresh traversal missed insertions: %v", got)
	}
}

func TestTimelineObservedBoundary(t *testing.T) {
	for _, name := range []string{"moved newer", "moved older", "date removed", "deleted"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "openfeed.db")
			s := openStore(t, path)
			const url = "https://example.com/feed"
			at := sampleRefresh().FetchedAt
			saveRefresh(t, s, url, Refresh{FetchedAt: fixtureFetch(), Items: []RefreshItem{
				timelineEntry("a", at.Add(4*time.Hour)), timelineEntry("b", at.Add(3*time.Hour)),
				timelineEntry("c", at.Add(2*time.Hour)), timelineEntry("d", at.Add(time.Hour)),
			}})
			query := TimelineQuery{FeedURLs: []string{url}, Limit: 2}
			first := readTimeline(t, s, query)
			boundary := first.Items[1]
			before, through := first.Next, boundary.Position
			first.Items[1].PublishedAt = at.Add(100 * time.Hour)
			first.Items[1].Position = TimelineCursor{}
			if before == nil || *before != through {
				t.Fatal("mutating returned items changed the continuation")
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s = openStore(t, path)
			wantOlder, wantNewer := []string{"c", "d"}, []string{"b", "a"}
			entry := timelineEntry("b", at.Add(10*time.Hour))
			switch name {
			case "moved older":
				entry.PublishedAt = at
				wantOlder, wantNewer = []string{"c", "d", "b"}, []string{"a"}
			case "date removed":
				entry.PublishedAt = time.Time{}
			case "deleted":
				if _, err := s.db.ExecContext(t.Context(), "DELETE FROM items WHERE id = ?", boundary.ID); err != nil {
					t.Fatal(err)
				}
				wantNewer = []string{"a"}
			}
			if name != "deleted" {
				saveRefresh(t, s, url, Refresh{FetchedAt: at.Add(20 * time.Hour), Items: []RefreshItem{entry}})
			}
			query.Before = before
			if got := timelineTitles(collectTimeline(t, s, query)); !slices.Equal(got, wantOlder) {
				t.Errorf("older boundary moved: %v, want %v", got, wantOlder)
			}
			query.Before, query.Through = nil, &through
			if got := timelineTitles(collectTimeline(t, s, query)); !slices.Equal(got, wantNewer) {
				t.Errorf("inclusive boundary moved: %v, want %v", got, wantNewer)
			}
		})
	}
}

func TestTimelineDateCorrectionsAcrossPages(t *testing.T) {
	for _, tc := range []struct {
		name         string
		aDate, cDate time.Duration
		older        []string
		fresh        []string
	}{
		{"different dates", 0, 5 * time.Hour, []string{"d", "a"}, []string{"c", "b", "d", "a"}},
		{"tie with boundary", 3 * time.Hour, 3 * time.Hour, []string{"a", "d"}, []string{"c", "b", "a", "d"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openStore(t, filepath.Join(t.TempDir(), "openfeed.db"))
			const url = "https://example.com/feed"
			at := sampleRefresh().FetchedAt
			saveRefresh(t, s, url, Refresh{FetchedAt: fixtureFetch(), Items: []RefreshItem{
				timelineEntry("a", at.Add(4*time.Hour)), timelineEntry("b", at.Add(3*time.Hour)),
				timelineEntry("c", at.Add(2*time.Hour)), timelineEntry("d", at.Add(time.Hour)),
			}})
			first := readTimeline(t, s, TimelineQuery{FeedURLs: []string{url}, Limit: 2})
			saveRefresh(t, s, url, Refresh{FetchedAt: at, Items: []RefreshItem{
				timelineEntry("a", at.Add(tc.aDate)), timelineEntry("c", at.Add(tc.cDate)),
			}})
			older := collectTimeline(t, s, TimelineQuery{FeedURLs: []string{url}, Before: first.Next, Limit: 1})
			if got := timelineTitles(older); !slices.Equal(got, tc.older) {
				t.Fatalf("raw pages should repeat moved a and omit moved c, got %v", got)
			}
			for _, item := range older {
				if item.Title == "a" && item.ID != first.Items[0].ID {
					t.Error("date correction changed identity")
				}
			}
			if got := timelineTitles(collectTimeline(t, s, TimelineQuery{FeedURLs: []string{url}, Limit: 1})); !slices.Equal(got, tc.fresh) {
				t.Errorf("fresh chronology = %v", got)
			}
		})
	}
}

func TestTimelineRefreshLoadedRange(t *testing.T) {
	s := openStore(t, filepath.Join(t.TempDir(), "openfeed.db"))
	const url = "https://example.com/feed"
	at := sampleRefresh().FetchedAt
	var old []RefreshItem
	for i := range 10 {
		old = append(old, timelineEntry(fmt.Sprintf("old-%d", i), at.Add(time.Duration(10-i)*time.Minute)))
	}
	fetched := fixtureFetch()
	saveRefresh(t, s, url, Refresh{FetchedAt: fetched, Items: old})
	query := TimelineQuery{FeedURLs: []string{url}, Limit: 3}
	first := readTimeline(t, s, query)
	query.Before = first.Next
	second := readTimeline(t, s, query)
	floor := second.Items[len(second.Items)-1].Position
	originalID := first.Items[2].ID

	var refresh []RefreshItem
	var want []string
	for i := range 125 {
		refresh = append(refresh, timelineEntry(fmt.Sprintf("new-%d", i), at.Add(time.Duration(11+i)*time.Minute)))
		want = append(want, fmt.Sprintf("new-%d", 124-i))
	}
	corrected := old[2]
	corrected.Title, corrected.Content = "corrected-old-2", "replacement content"
	corrected.PublishedAt = at.Add(630 * time.Second)
	refresh = append(refresh, corrected, timelineEntry("late", at.Add(390*time.Second)))
	saveRefresh(t, s, url, Refresh{FetchedAt: fetched.Add(24 * time.Hour), Items: refresh})

	query.Before, query.Through, query.Limit = nil, &floor, 50
	reloaded := collectTimeline(t, s, query)
	want = append(want, "corrected-old-2", "old-0", "old-1", "old-3", "late", "old-4", "old-5")
	if got := timelineTitles(reloaded); !slices.Equal(got, want) {
		t.Fatalf("refreshed span has a gap or stale ordering: %v, want %v", got, want)
	}
	if reloaded[125].ID != originalID || reloaded[125].Content != corrected.Content || reloaded[125].FirstFetchedAt != fetched {
		t.Error("refresh changed identity or retained stale content")
	}
	seen := make(map[ItemID]bool)
	for _, item := range reloaded {
		if seen[item.ID] {
			t.Errorf("duplicate identity %d in reloaded range", item.ID)
		}
		seen[item.ID] = true
	}
	query.Before, query.Through = &reloaded[len(reloaded)-1].Position, nil
	if got := timelineTitles(collectTimeline(t, s, query)); !slices.Equal(got, []string{"old-6", "old-7", "old-8", "old-9"}) {
		t.Errorf("could not resume older scrolling after range exhaustion: %v", got)
	}
}

func TestTimelineExhaustionIsCurrent(t *testing.T) {
	s := openStore(t, filepath.Join(t.TempDir(), "openfeed.db"))
	const url = "https://example.com/feed"
	at := sampleRefresh().FetchedAt
	saveRefresh(t, s, url, Refresh{FetchedAt: at, Items: []RefreshItem{timelineEntry("original", at)}})
	page := readTimeline(t, s, TimelineQuery{FeedURLs: []string{url}, Limit: 1})
	if page.Next != nil || len(page.Items) != 1 {
		t.Fatalf("incorrect exhaustion: %+v", page)
	}
	query := TimelineQuery{FeedURLs: []string{url}, Before: &page.Items[0].Position, Limit: 1}
	if got := readTimeline(t, s, query); len(got.Items) != 0 || got.Next != nil {
		t.Fatal("items appeared below oldest position before insertion")
	}
	saveRefresh(t, s, url, Refresh{FetchedAt: at, Items: []RefreshItem{timelineEntry("older", at.Add(-time.Hour))}})
	if got := timelineTitles(readTimeline(t, s, query).Items); !slices.Equal(got, []string{"older"}) {
		t.Errorf("old exhaustion hid a later discovery: %v", got)
	}
}
