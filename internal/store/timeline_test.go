package store

import (
	"cmp"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"
)

func timelineEntry(name string, published time.Time) RefreshItem {
	return RefreshItem{
		Key: "id:" + name, Title: name, URL: "https://example.com/" + name,
		Content: "<p>" + name + "</p>", PublishedAt: published,
	}
}

func readTimeline(t *testing.T, s *Store, query TimelineQuery) TimelinePage {
	t.Helper()
	page, err := s.Timeline(t.Context(), query)
	if err != nil {
		t.Fatal(err)
	}
	if page.Next != nil && (len(page.Items) == 0 || *page.Next != page.Items[len(page.Items)-1].Position) {
		t.Fatal("continuation is not the last returned position")
	}
	return page
}

func collectTimeline(t *testing.T, s *Store, query TimelineQuery) []TimelineItem {
	t.Helper()
	var items []TimelineItem
	for range 1000 {
		page := readTimeline(t, s, query)
		items = append(items, page.Items...)
		if page.Next == nil {
			return items
		}
		query.Before = page.Next
	}
	t.Fatal("timeline did not reach the end")
	return nil
}

func timelineTitles(items []TimelineItem) []string {
	titles := make([]string, len(items))
	for i, item := range items {
		titles[i] = item.Title
	}
	return titles
}

func TestTimelineVisibility(t *testing.T) {
	s := openStore(t, filepath.Join(t.TempDir(), "openfeed.db"))
	const (
		a        = "https://a.example/feed"
		b        = "https://b.example/feed"
		inactive = "https://inactive.example/feed"
		quoted   = "https://example.com/feed?q=\"雪\"&x=' OR 1=1 --"
		empty    = "https://empty.example/feed"
		unknown  = "https://unknown.example/feed"
	)
	at := sampleRefresh().FetchedAt
	for _, feed := range []struct {
		url   string
		items []RefreshItem
	}{
		{a, []RefreshItem{timelineEntry("shared", at), timelineEntry("new-a", at.Add(2*time.Hour))}},
		{b, []RefreshItem{timelineEntry("shared", at)}},
		{inactive, []RefreshItem{timelineEntry("inactive", at.Add(100*time.Hour))}},
		{quoted, []RefreshItem{timelineEntry("quoted", at.Add(time.Hour))}},
		{empty, nil},
	} {
		saveRefresh(t, s, feed.url, Refresh{Items: feed.items, FetchedAt: at})
	}
	before := make(map[string]cachedFeed)
	for _, url := range []string{a, b, inactive, quoted, empty, unknown} {
		before[url] = readCache(t, s, url)
	}
	for _, tc := range []struct {
		name  string
		urls  []string
		want  []string
		feeds []string
	}{
		{"nil selection", nil, nil, nil},
		{"empty selection", []string{}, nil, nil},
		{"uncached", []string{unknown}, nil, nil},
		{"empty cached", []string{empty}, nil, nil},
		{"one active", []string{a}, []string{"new-a", "shared"}, []string{a, a}},
		{"merged", []string{a, b, unknown, empty}, []string{"new-a", "shared", "shared"}, []string{a, b, a}},
		{"reordered and repeated", []string{b, a, b, a}, []string{"new-a", "shared", "shared"}, []string{a, b, a}},
		{"literal URL", []string{quoted}, []string{"quoted"}, []string{quoted}},
		{"reactivated", []string{inactive, a}, []string{"inactive", "new-a", "shared"}, []string{inactive, a, a}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			items := collectTimeline(t, s, TimelineQuery{FeedURLs: tc.urls, Limit: 1})
			if got := timelineTitles(items); !slices.Equal(got, tc.want) {
				t.Fatalf("titles = %v, want %v", got, tc.want)
			}
			ids := make(map[ItemID]bool)
			for i, item := range items {
				if item.FeedURL != tc.feeds[i] || item.ID <= 0 || ids[item.ID] {
					t.Errorf("incorrect source/identity: %+v", item)
				}
				ids[item.ID] = true
				var expected cachedItem
				for _, cached := range before[item.FeedURL].Items {
					if ItemID(cached.ID) == item.ID {
						expected = cached
					}
				}
				if item.Title != expected.Title || item.URL != expected.URL || item.Content != expected.Content ||
					item.PublishedAt != expected.PublishedAt || item.FirstFetchedAt != expected.FirstFetchedAt {
					t.Errorf("publisher data changed: %+v, want %+v", item, expected)
				}
			}
		})
	}
	for url, want := range before {
		if got := readCache(t, s, url); !reflect.DeepEqual(got, want) {
			t.Errorf("query changed cache for %s", url)
		}
	}
	if got := queryInt(t, s.db, "SELECT COUNT(*) FROM feeds"); got != 5 {
		t.Errorf("queries created subscriptions: %d feeds", got)
	}
}

func TestTimelinePaginationWithTies(t *testing.T) {
	s := openStore(t, filepath.Join(t.TempDir(), "openfeed.db"))
	at := sampleRefresh().FetchedAt
	var urls []string
	var expected []TimelineItem
	for feed := range 3 {
		url := fmt.Sprintf("https://%d.example/feed", feed)
		var entries []RefreshItem
		for i := range 193 {
			published := at.Add(time.Duration(i%3) * time.Nanosecond)
			if i%4 == 0 {
				published = time.Time{}
			}
			entries = append(entries, timelineEntry(fmt.Sprintf("%d-%d", feed, i), published))
		}
		saveRefresh(t, s, url, Refresh{Items: entries, FetchedAt: at})
		if feed == 1 {
			continue
		}
		urls = append(urls, url)
		for _, item := range readCache(t, s, url).Items {
			expected = append(expected, TimelineItem{
				ID: ItemID(item.ID), FeedURL: url, Title: item.Title, URL: item.URL, Content: item.Content,
				PublishedAt: item.PublishedAt, FirstFetchedAt: item.FirstFetchedAt,
			})
		}
	}
	effective := func(item TimelineItem) time.Time {
		if item.PublishedAt.IsZero() {
			return item.FirstFetchedAt
		}
		return item.PublishedAt
	}
	slices.SortFunc(expected, func(a, b TimelineItem) int {
		if order := effective(b).Compare(effective(a)); order != 0 {
			return order
		}
		return cmp.Compare(b.ID, a.ID)
	})
	for _, limit := range []int{0, 1, 2, 7, 50, 200} {
		t.Run(fmt.Sprintf("limit %d", limit), func(t *testing.T) {
			got := collectTimeline(t, s, TimelineQuery{FeedURLs: urls, Limit: limit})
			if len(got) != len(expected) {
				t.Fatalf("got %d items, want %d", len(got), len(expected))
			}
			for i := range got {
				if got[i].Position.date != effective(expected[i]).Format(timestampLayout) || got[i].Position.id != expected[i].ID {
					t.Errorf("wrong cursor at %d: %+v", i, got[i].Position)
				}
				got[i].Position = TimelineCursor{}
				if got[i] != expected[i] {
					t.Fatalf("item %d = %+v, want %+v", i, got[i], expected[i])
				}
			}
		})
	}
}

func TestTimelineCursorCompare(t *testing.T) {
	s := openStore(t, filepath.Join(t.TempDir(), "openfeed.db"))
	const a, b = "https://a.example/feed", "https://b.example/feed"
	at := sampleRefresh().FetchedAt
	for _, url := range []string{a, b} {
		var entries []RefreshItem
		for i := range 30 {
			published := at.Add(time.Duration(i%3) * time.Nanosecond)
			if i%4 == 0 {
				published = time.Time{}
			}
			entries = append(entries, timelineEntry(fmt.Sprintf("%s-%d", url, i), published))
		}
		saveRefresh(t, s, url, Refresh{Items: entries, FetchedAt: at})
	}
	all := collectTimeline(t, s, TimelineQuery{FeedURLs: []string{a, b}, Limit: 7})
	for i, item := range all {
		if item.Position.Compare(item.Position) != 0 {
			t.Fatalf("position %d does not equal itself", i)
		}
		if i > 0 && (all[i-1].Position.Compare(item.Position) <= 0 || item.Position.Compare(all[i-1].Position) >= 0) {
			t.Fatalf("Compare disagrees with timeline order at %d", i)
		}
	}
	// Positions from different URL sets order as the combined river does.
	merged := slices.Concat(collectTimeline(t, s, TimelineQuery{FeedURLs: []string{a}}), collectTimeline(t, s, TimelineQuery{FeedURLs: []string{b}}))
	slices.SortFunc(merged, func(x, y TimelineItem) int { return y.Position.Compare(x.Position) })
	if got, want := timelineTitles(merged), timelineTitles(all); !slices.Equal(got, want) {
		t.Errorf("cross-selection order = %v, want %v", got, want)
	}
}

func TestTimelineLimitsAndExhaustion(t *testing.T) {
	for _, count := range []int{0, 1, 49, 50, 51, 200, 201} {
		t.Run(fmt.Sprintf("%d items", count), func(t *testing.T) {
			s := openStore(t, filepath.Join(t.TempDir(), "openfeed.db"))
			const url = "https://example.com/feed"
			at := sampleRefresh().FetchedAt
			var entries []RefreshItem
			for i := range count {
				entries = append(entries, timelineEntry(fmt.Sprintf("post-%d", i), at))
			}
			saveRefresh(t, s, url, Refresh{Items: entries, FetchedAt: at})
			for _, limit := range []int{0, 1, 50, 200} {
				page := readTimeline(t, s, TimelineQuery{FeedURLs: []string{url}, Limit: limit})
				size := limit
				if size == 0 {
					size = 50
				}
				if len(page.Items) != min(count, size) || (page.Next != nil) != (count > size) {
					t.Errorf("limit %d: got %d items, Next=%v", limit, len(page.Items), page.Next)
				}
			}
		})
	}
}

func TestTimelineRangeEdges(t *testing.T) {
	s := openStore(t, filepath.Join(t.TempDir(), "openfeed.db"))
	const url = "https://example.com/feed"
	at := sampleRefresh().FetchedAt
	saveRefresh(t, s, url, Refresh{FetchedAt: at, Items: []RefreshItem{
		timelineEntry("above", at.Add(time.Nanosecond)),
		timelineEntry("tie-a", at), timelineEntry("tie-b", at), timelineEntry("tie-c", at),
		timelineEntry("below", at.Add(-time.Nanosecond)),
	}})
	all := readTimeline(t, s, TimelineQuery{FeedURLs: []string{url}}).Items
	if got := timelineTitles(all); !slices.Equal(got, []string{"above", "tie-c", "tie-b", "tie-a", "below"}) {
		t.Fatalf("incorrect initial order: %v", got)
	}
	for _, tc := range []struct {
		name            string
		before, through int
		want            []TimelineItem
	}{
		{"unbounded", -1, -1, all},
		{"exclusive tie", 1, -1, all[2:]},
		{"inclusive tie", -1, 3, all[:4]},
		{"bounded ties", 1, 3, all[2:4]},
		{"equal boundaries", 1, 1, nil},
		{"reversed boundaries", 3, 1, nil},
		{"reversed dates", 4, 0, nil},
		{"one inclusive", -1, 0, all[:1]},
		{"one exclusive", 3, -1, all[4:]},
		{"below oldest", 4, -1, nil},
		{"ends", 0, 4, all[1:]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			query := TimelineQuery{FeedURLs: []string{url}, Limit: 1}
			if tc.before >= 0 {
				query.Before = &all[tc.before].Position
			}
			if tc.through >= 0 {
				query.Through = &all[tc.through].Position
			}
			if got := collectTimeline(t, s, query); !slices.Equal(got, tc.want) {
				t.Errorf("titles = %v, want %v", timelineTitles(got), timelineTitles(tc.want))
			}
		})
	}
}

func TestTimelinePublicationTransitions(t *testing.T) {
	s := openStore(t, filepath.Join(t.TempDir(), "openfeed.db"))
	const url = "https://example.com/feed"
	first := sampleRefresh().FetchedAt
	saveRefresh(t, s, url, Refresh{FetchedAt: first, Items: []RefreshItem{
		timelineEntry("anchor", first.Add(-12*time.Hour)), timelineEntry("target", time.Time{}),
	}})
	var id ItemID
	for i, tc := range []struct {
		name      string
		published time.Time
		newer     bool
	}{
		{"still undated", time.Time{}, true},
		{"newly dated", first.Add(-24 * time.Hour), false},
		{"corrected later", first.Add(time.Hour), true},
		{"corrected earlier", first.Add(-48 * time.Hour), false},
		{"removed", time.Time{}, true},
		{"still absent", time.Time{}, true},
		{"restored", first.Add(-36 * time.Hour), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entry := timelineEntry("target", tc.published)
			entry.URL, entry.Content = "", ""
			saveRefresh(t, s, url, Refresh{FetchedAt: first.Add(-time.Duration(i+1) * time.Hour), Items: []RefreshItem{entry}})
			items := collectTimeline(t, s, TimelineQuery{FeedURLs: []string{url}, Limit: 1})
			index := 1
			if tc.newer {
				index = 0
			}
			if len(items) != 2 || items[index].Title != "target" {
				t.Fatalf("incorrect chronology: %v", timelineTitles(items))
			}
			item := items[index]
			if id == 0 {
				id = item.ID
			}
			if item.ID != id || item.FirstFetchedAt != first || item.PublishedAt != tc.published || item.URL != "" || item.Content != "" {
				t.Errorf("wrong replacement: %+v", item)
			}
			saveRefresh(t, s, url, Refresh{FetchedAt: first.Add(100 * time.Hour), Unchanged: true})
			if got := collectTimeline(t, s, TimelineQuery{FeedURLs: []string{url}, Limit: 1}); !slices.Equal(got, items) {
				t.Error("unchanged refresh moved or changed timeline items")
			}
		})
	}
}

func TestTimelineTimestampRange(t *testing.T) {
	s := openStore(t, filepath.Join(t.TempDir(), "openfeed.db"))
	const url = "https://example.com/feed"
	dates := []time.Time{
		time.Date(0, 1, 1, 0, 0, 0, 1, time.UTC),
		time.Date(1600, 2, 29, 12, 0, 0, 123456789, time.UTC),
		time.Date(2025, 1, 1, 12, 0, 0, 2, time.FixedZone("offset", 2*60*60)),
		time.Date(2025, 1, 1, 10, 0, 0, 3, time.UTC),
		time.Date(2300, 1, 1, 0, 0, 0, 987654321, time.UTC),
		time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC),
	}
	for i := len(dates) - 1; i >= 0; i-- {
		saveRefresh(t, s, url, Refresh{FetchedAt: dates[i], Items: []RefreshItem{timelineEntry(fmt.Sprintf("post-%d", i), dates[i])}})
	}
	items := collectTimeline(t, s, TimelineQuery{FeedURLs: []string{url}, Limit: 1})
	if len(items) != len(dates) {
		t.Fatalf("got %d items, want %d", len(items), len(dates))
	}
	for i, item := range items {
		want := dates[len(dates)-1-i].UTC()
		if item.PublishedAt != want || item.FirstFetchedAt != want {
			t.Errorf("item %d: %+v, want %v", i, item, want)
		}
	}
	got := collectTimeline(t, s, TimelineQuery{FeedURLs: []string{url}, Before: &items[0].Position, Through: &items[len(items)-1].Position, Limit: 1})
	if !slices.Equal(got, items[1:]) {
		t.Error("range lost extreme dates or nanoseconds")
	}
}
