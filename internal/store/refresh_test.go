package store

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

type cachedItem struct {
	RefreshItem
	ID             int64
	FirstFetchedAt time.Time
}

type cachedFeed struct {
	State FeedState
	Items []cachedItem
}

func sampleRefresh() Refresh {
	return Refresh{
		ETag:         `W/"initial"`,
		LastModified: "Tue, 02 Sep 2025 13:00:00 GMT",
		FetchedAt:    time.Date(2025, 9, 2, 13, 0, 0, 123456789, time.UTC),
		Items: []RefreshItem{{
			Key:         "id:post",
			Title:       "Title",
			URL:         "https://example.com/post",
			Content:     "<p>Feed content</p>",
			PublishedAt: time.Date(2025, 8, 26, 12, 0, 0, 987654321, time.UTC),
		}},
	}
}

func feedState(t *testing.T, s *Store, url string) FeedState {
	t.Helper()
	state, err := s.FeedState(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func saveRefresh(t *testing.T, s *Store, url string, refresh Refresh) {
	t.Helper()
	if err := s.SaveRefresh(t.Context(), feedState(t, s, url), refresh); err != nil {
		t.Fatal(err)
	}
}

func readCache(t *testing.T, s *Store, url string) cachedFeed {
	t.Helper()
	cache := cachedFeed{State: feedState(t, s, url)}
	rows, err := s.db.QueryContext(t.Context(), `
		SELECT id, key, title, url, content, published_at, first_fetched_at
		FROM items WHERE feed_url = ? ORDER BY key
	`, url)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var item cachedItem
		var publishedAt sql.NullString
		var firstFetchedAt string
		if err := rows.Scan(&item.ID, &item.Key, &item.Title, &item.URL, &item.Content, &publishedAt, &firstFetchedAt); err != nil {
			t.Fatal(err)
		}
		if publishedAt.Valid {
			item.PublishedAt, err = time.Parse(time.RFC3339Nano, publishedAt.String)
			if err != nil {
				t.Fatal(err)
			}
		}
		item.FirstFetchedAt, err = time.Parse(time.RFC3339Nano, firstFetchedAt)
		if err != nil {
			t.Fatal(err)
		}
		cache.Items = append(cache.Items, item)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return cache
}

func TestRefreshSchemaUpgradeAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "openfeed.db")
	old := openMigrationStore(t, path)
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	s := openStore(t, path)
	const url = "https://example.com/feed"
	state := feedState(t, s, url)
	if state.ETag != "" || state.LastModified != "" || !state.FetchedAt.IsZero() || state.revision != 0 {
		t.Fatalf("uncached feed has metadata: %+v", state)
	}
	if got := queryInt(t, s.db, "SELECT COUNT(*) FROM feeds"); got != 0 {
		t.Fatalf("reading state created %d feeds", got)
	}
	refresh := sampleRefresh()
	if err := s.SaveRefresh(t.Context(), state, refresh); err != nil {
		t.Fatal(err)
	}
	before := readCache(t, s, url)
	if before.State.ETag != refresh.ETag || before.State.LastModified != refresh.LastModified ||
		before.State.FetchedAt != refresh.FetchedAt || before.State.revision != 1 {
		t.Errorf("cached state = %+v, want metadata from %+v", before.State, refresh)
	}
	if len(before.Items) != 1 || before.Items[0].RefreshItem != refresh.Items[0] ||
		before.Items[0].ID <= 0 || before.Items[0].FirstFetchedAt != refresh.FetchedAt {
		t.Fatalf("incorrect cached items: %+v", before.Items)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openStore(t, path)
	if after := readCache(t, s, url); !reflect.DeepEqual(after, before) {
		t.Fatalf("reopen changed cache: %+v, want %+v", after, before)
	}
	refresh.FetchedAt = refresh.FetchedAt.Add(time.Hour)
	// Snapshots remain usable across handles and reopen of the same database.
	if err := s.SaveRefresh(t.Context(), before.State, refresh); err != nil {
		t.Fatal(err)
	}
	after := readCache(t, s, url)
	if !reflect.DeepEqual(after.Items, before.Items) || after.State.revision != 2 {
		t.Errorf("repeat refresh changed identity or first-fetch state: %+v", after)
	}
}

func TestRefreshDuplicateWinners(t *testing.T) {
	first := RefreshItem{Key: "id:same", Title: "First"}
	second := RefreshItem{Key: "id:same", Content: "More content", PublishedAt: sampleRefresh().FetchedAt}
	for _, tc := range []struct {
		name    string
		entries []RefreshItem
	}{
		{"source order", []RefreshItem{first, second}},
		{"reversed", []RefreshItem{second, first}},
		{"identical", []RefreshItem{first, first}},
	} {
		for _, existing := range []bool{false, true} {
			name := tc.name + "/insert"
			if existing {
				name = tc.name + "/update"
			}
			t.Run(name, func(t *testing.T) {
				s := openStore(t, filepath.Join(t.TempDir(), "openfeed.db"))
				const url = "https://example.com/feed"
				refresh := sampleRefresh()
				var before cachedFeed
				if existing {
					refresh.Items[0].Key = first.Key
					saveRefresh(t, s, url, refresh)
					before = readCache(t, s, url)
					refresh.FetchedAt = refresh.FetchedAt.Add(time.Hour)
				}
				refresh.Items = tc.entries
				saveRefresh(t, s, url, refresh)
				after := readCache(t, s, url)
				if len(after.Items) != 1 || after.Items[0].RefreshItem != tc.entries[0] {
					t.Fatalf("duplicate winner = %+v, want exactly %+v", after.Items, tc.entries[0])
				}
				if existing && (after.Items[0].ID != before.Items[0].ID || after.Items[0].FirstFetchedAt != before.Items[0].FirstFetchedAt) {
					t.Error("duplicate update replaced stored identity or first-fetch time")
				}
			})
		}
	}
}

func TestRefreshReplacesPublisherFields(t *testing.T) {
	for _, tc := range []struct {
		name string
		item RefreshItem
	}{
		{"corrections", RefreshItem{Key: "id:post", Title: "Corrected", URL: "https://example.com/correction", Content: "Revised", PublishedAt: sampleRefresh().FetchedAt}},
		{"title only", RefreshItem{Key: "id:post", Title: "Only title remains"}},
		{"URL only", RefreshItem{Key: "id:post", URL: "https://example.com/only-link"}},
		{"content only", RefreshItem{Key: "id:post", Content: "Only content remains"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openStore(t, filepath.Join(t.TempDir(), "openfeed.db"))
			const url = "https://example.com/feed"
			refresh := sampleRefresh()
			saveRefresh(t, s, url, refresh)
			before := readCache(t, s, url)
			refresh.Items = []RefreshItem{tc.item}
			refresh.FetchedAt = refresh.FetchedAt.Add(-time.Hour)
			saveRefresh(t, s, url, refresh)
			after := readCache(t, s, url)
			if len(after.Items) != 1 || after.Items[0].RefreshItem != tc.item ||
				after.Items[0].ID != before.Items[0].ID || after.Items[0].FirstFetchedAt != before.Items[0].FirstFetchedAt {
				t.Errorf("replacement = %+v; before = %+v, new publisher fields = %+v", after.Items, before.Items, tc.item)
			}
			if after.State.FetchedAt != refresh.FetchedAt {
				t.Error("accepted an earlier fetch time without persisting it exactly")
			}
		})
	}
}

func TestRefreshPublicationTransitions(t *testing.T) {
	s := openStore(t, filepath.Join(t.TempDir(), "openfeed.db"))
	const url = "https://example.com/feed"
	refresh := sampleRefresh()
	firstFetchedAt := refresh.FetchedAt
	publishedAt := refresh.Items[0].PublishedAt
	for _, tc := range []struct {
		name      string
		published time.Time
	}{
		{"initially undated", time.Time{}},
		{"still undated", time.Time{}},
		{"newly supplied", publishedAt},
		{"corrected earlier", publishedAt.Add(-24 * time.Hour)},
		{"corrected later", publishedAt.Add(24 * time.Hour)},
		{"removed", time.Time{}},
		{"still removed", time.Time{}},
		{"restored", publishedAt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refresh.Items[0].PublishedAt = tc.published
			saveRefresh(t, s, url, refresh)
			cache := readCache(t, s, url)
			if len(cache.Items) != 1 || cache.Items[0].PublishedAt != tc.published || cache.Items[0].FirstFetchedAt != firstFetchedAt {
				t.Fatalf("incorrect date state: %+v", cache.Items)
			}
			want := tc.published
			if want.IsZero() {
				want = firstFetchedAt
			}
			var effective string
			if err := s.db.QueryRowContext(t.Context(), "SELECT COALESCE(published_at, first_fetched_at) FROM items").Scan(&effective); err != nil {
				t.Fatal(err)
			}
			got, err := time.Parse(time.RFC3339Nano, effective)
			if err != nil || !got.Equal(want) {
				t.Errorf("effective date = %q, want %v; error = %v", effective, want, err)
			}
		})
		refresh.FetchedAt = refresh.FetchedAt.Add(time.Hour)
	}
}

func TestRefreshIdentityAndOmission(t *testing.T) {
	s := openStore(t, filepath.Join(t.TempDir(), "openfeed.db"))
	const url = "https://example.com/feed"
	const otherURL = "https://example.com/other-feed"
	refresh := sampleRefresh()
	refresh.Items[0].Key = "sha256:original"
	saveRefresh(t, s, url, refresh)
	before := readCache(t, s, url)
	refresh.FetchedAt = refresh.FetchedAt.Add(time.Hour)
	saveRefresh(t, s, otherURL, refresh)
	other := readCache(t, s, otherURL)
	if len(other.Items) != 1 || other.Items[0].ID == before.Items[0].ID || other.Items[0].FirstFetchedAt != refresh.FetchedAt {
		t.Fatalf("subscriptions shared an identity: %+v, %+v", before.Items, other.Items)
	}
	refresh.Items[0].Key = "sha256:edited"
	saveRefresh(t, s, url, refresh)
	after := readCache(t, s, url)
	if len(after.Items) != 2 || after.Items[0].Key != "sha256:edited" || after.Items[1] != before.Items[0] || after.Items[0].FirstFetchedAt != refresh.FetchedAt {
		t.Fatalf("changed key merged identities or omitted item was deleted: %+v", after.Items)
	}
	refresh.Items = nil
	refresh.ETag, refresh.LastModified = "", ""
	saveRefresh(t, s, url, refresh)
	empty := readCache(t, s, url)
	if !reflect.DeepEqual(empty.Items, after.Items) || empty.State.ETag != "" || empty.State.LastModified != "" {
		t.Errorf("full empty response did not retain items and clear tokens: %+v", empty)
	}
	if got := readCache(t, s, otherURL); !reflect.DeepEqual(got, other) {
		t.Error("refresh changed another subscription")
	}
}

func TestRefreshUnchanged(t *testing.T) {
	for _, empty := range []bool{false, true} {
		name := "cached items"
		if empty {
			name = "cached empty feed"
		}
		t.Run(name, func(t *testing.T) {
			s := openStore(t, filepath.Join(t.TempDir(), "openfeed.db"))
			const url = "https://example.com/feed"
			refresh := sampleRefresh()
			if empty {
				refresh.Items = []RefreshItem{}
			}
			saveRefresh(t, s, url, refresh)
			before := readCache(t, s, url)
			for _, tc := range []struct {
				name, etag, lastModified string
			}{
				{"preserve", refresh.ETag, refresh.LastModified},
				{"replace", `"new"`, "opaque modified token"},
				{"clear", "", ""},
			} {
				t.Run(tc.name, func(t *testing.T) {
					refresh = Refresh{ETag: tc.etag, LastModified: tc.lastModified, FetchedAt: refresh.FetchedAt.Add(time.Hour), Unchanged: true}
					saveRefresh(t, s, url, refresh)
					after := readCache(t, s, url)
					if !reflect.DeepEqual(after.Items, before.Items) || after.State.ETag != tc.etag || after.State.LastModified != tc.lastModified ||
						after.State.FetchedAt != refresh.FetchedAt || after.State.revision != before.State.revision+1 {
						t.Fatalf("unchanged outcome = %+v, before = %+v", after, before)
					}
					before = after
				})
			}
		})
	}
}

func TestRefreshTimestampEncoding(t *testing.T) {
	for _, tc := range []struct {
		name string
		time time.Time
		want string
	}{
		{"year zero", time.Date(0, 1, 1, 0, 0, 0, 1, time.UTC), "0000-01-01T00:00:00.000000001Z"},
		{"before UnixNano range", time.Date(1500, 1, 2, 3, 4, 5, 6, time.UTC), "1500-01-02T03:04:05.000000006Z"},
		{"epoch", time.Unix(0, 0), "1970-01-01T00:00:00.000000000Z"},
		{"nanoseconds", time.Date(2025, 9, 2, 13, 0, 0, 123456789, time.FixedZone("east", 7200)), "2025-09-02T11:00:00.123456789Z"},
		{"after UnixNano range", time.Date(2300, 1, 2, 3, 4, 5, 6, time.UTC), "2300-01-02T03:04:05.000000006Z"},
		{"last supported instant", time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC), "9999-12-31T23:59:59.999999999Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openStore(t, filepath.Join(t.TempDir(), "openfeed.db"))
			const url = "https://example.com/feed"
			refresh := sampleRefresh()
			refresh.FetchedAt = tc.time
			refresh.Items[0].PublishedAt = tc.time
			saveRefresh(t, s, url, refresh)
			var fetchedAt, publishedAt, firstFetchedAt string
			if err := s.db.QueryRowContext(t.Context(), `
				SELECT feeds.fetched_at, items.published_at, items.first_fetched_at
				FROM feeds JOIN items ON items.feed_url = feeds.url
			`).Scan(&fetchedAt, &publishedAt, &firstFetchedAt); err != nil {
				t.Fatal(err)
			}
			if fetchedAt != tc.want || publishedAt != tc.want || firstFetchedAt != tc.want {
				t.Errorf("stored timestamps = %q, %q, %q; want %q", fetchedAt, publishedAt, firstFetchedAt, tc.want)
			}
			got := feedState(t, s, url).FetchedAt
			if !got.Equal(tc.time) || got.Location() != time.UTC {
				t.Errorf("round-trip fetch time = %v, want %v in UTC", got, tc.time)
			}
		})
	}
}
