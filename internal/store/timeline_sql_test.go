package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"modernc.org/sqlite"
)

func TestTimelineUpgradeAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "openfeed.db")
	old := openMigrationStore(t, path)
	refreshSchema, err := fs.ReadFile(migrations, "migrations/002_refresh.sql")
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate(t.Context(), old.db, migrationFiles(t, string(refreshSchema))); err != nil {
		t.Fatal(err)
	}
	const url = "https://example.com/feed"
	at := sampleRefresh().FetchedAt
	saveRefresh(t, old, url, Refresh{FetchedAt: at, Items: []RefreshItem{
		timelineEntry("newer", at), timelineEntry("older", at.Add(-time.Hour)),
	}})
	cache := readCache(t, old, url)
	query := TimelineQuery{FeedURLs: []string{url}, Limit: 1}
	before := readTimeline(t, old, query)
	if got := queryInt(t, old.db, "SELECT COUNT(*) FROM sqlite_schema WHERE name = 'items_timeline'"); got != 0 {
		t.Fatal("upgrade fixture already has timeline index")
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		s := openStore(t, path)
		if got := queryInt(t, s.db, "SELECT COUNT(*) FROM sqlite_schema WHERE type = 'index' AND name = 'items_timeline'"); got != 1 {
			t.Fatal("upgrade did not create timeline index")
		}
		if got := queryInt(t, s.db, "SELECT MAX(version) FROM schema_migrations"); got != schemaVersion(t) {
			t.Errorf("incorrect upgraded version: %d", got)
		}
		if got := readCache(t, s, url); !reflect.DeepEqual(got, cache) {
			t.Error("index migration changed cached items or refresh metadata")
		}
		if got := readTimeline(t, s, query); !reflect.DeepEqual(got, before) {
			t.Error("upgrade/reopen changed timeline data or cursor")
		}
		continued := readTimeline(t, s, TimelineQuery{FeedURLs: []string{url}, Before: before.Next, Limit: 1})
		if !slices.Equal(timelineTitles(continued.Items), []string{"older"}) || continued.Next != nil {
			t.Errorf("pre-upgrade cursor did not survive: %+v", continued)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func timelineIndexSQL(t *testing.T, s *Store) string {
	t.Helper()
	var statement string
	if err := s.db.QueryRowContext(t.Context(), "SELECT sql FROM sqlite_schema WHERE type = 'index' AND name = 'items_timeline'").Scan(&statement); err != nil {
		t.Fatal(err)
	}
	return statement
}

func TestTimelineFirstFetchCapUpgrade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "openfeed.db")
	old := openMigrationStore(t, path)
	var scripts []string
	for _, name := range []string{"002_refresh.sql", "003_timeline.sql", "004_seen.sql"} {
		script, err := fs.ReadFile(migrations, "migrations/"+name)
		if err != nil {
			t.Fatal(err)
		}
		scripts = append(scripts, string(script))
	}
	if err := migrate(t.Context(), old.db, migrationFiles(t, scripts...)); err != nil {
		t.Fatal(err)
	}
	const early, late = "https://early.example/feed", "https://late.example/feed"
	at := sampleRefresh().FetchedAt
	saveRefresh(t, old, early, Refresh{FetchedAt: at, Items: []RefreshItem{
		timelineEntry("future", at.Add(100*time.Hour)), timelineEntry("recent", at.Add(-time.Hour)),
	}})
	saveRefresh(t, old, late, Refresh{FetchedAt: at.Add(10 * time.Hour), Items: []RefreshItem{timelineEntry("later", at.Add(5*time.Hour))}})
	if strings.Contains(timelineIndexSQL(t, old), "MIN(") {
		t.Fatal("upgrade fixture already caps timeline dates")
	}
	caches := map[string]cachedFeed{early: readCache(t, old, early), late: readCache(t, old, late)}
	saved := TimelineCursor{date: at.Add(-time.Hour).Format(timestampLayout), id: ItemID(caches[early].Items[1].ID)}
	execSQL(t, old.db, fmt.Sprintf("INSERT INTO seen_position VALUES (1, '%s', %d)", saved.date, saved.id))
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	s := openStore(t, path)
	if got := timelineIndexSQL(t, s); !strings.Contains(got, effectiveDate) {
		t.Fatalf("timeline index does not match the query expression: %s", got)
	}
	for url, cache := range caches {
		if got := readCache(t, s, url); !reflect.DeepEqual(got, cache) {
			t.Errorf("index rebuild changed cached data for %s", url)
		}
	}
	if got := seenPosition(t, s); got == nil || *got != saved {
		t.Errorf("index rebuild changed seen position to %+v", got)
	}
	items := collectTimeline(t, s, TimelineQuery{FeedURLs: []string{early, late}})
	if got := timelineTitles(items); !slices.Equal(got, []string{"later", "future", "recent"}) {
		t.Fatalf("future-dated post is not ordered at first fetch: %v", got)
	}
	if items[1].PublishedAt != at.Add(100*time.Hour) || items[1].Position.date != at.Format(timestampLayout) {
		t.Errorf("cap changed the publisher date or missed the position: %+v", items[1])
	}
}

func TestTimelineInvalidQuery(t *testing.T) {
	s := openStore(t, filepath.Join(t.TempDir(), "openfeed.db"))
	for _, tc := range []struct {
		name  string
		query TimelineQuery
	}{
		{"negative limit", TimelineQuery{Limit: -1}},
		{"oversized limit", TimelineQuery{Limit: 201}},
		{"maximum int", TimelineQuery{Limit: int(^uint(0) >> 1)}},
		{"zero before", TimelineQuery{Before: &TimelineCursor{}}},
		{"zero through", TimelineQuery{Through: &TimelineCursor{}}},
	} {
		for _, urls := range [][]string{nil, {"https://unknown.example/feed"}} {
			t.Run(fmt.Sprintf("%s/%d feeds", tc.name, len(urls)), func(t *testing.T) {
				query := tc.query
				query.FeedURLs = urls
				page, err := s.Timeline(t.Context(), query)
				if err == nil || !strings.Contains(err.Error(), "read timeline") || !reflect.DeepEqual(page, TimelinePage{}) {
					t.Errorf("invalid query returned %+v, %v", page, err)
				}
			})
		}
	}
}

func TestTimelinePublicErrors(t *testing.T) {
	for _, name := range []string{"canceled", "deadline", "closed", "broken schema", "publication time", "first-fetch time", "scan", "query execution"} {
		t.Run(name, func(t *testing.T) {
			s := openStore(t, filepath.Join(t.TempDir(), "openfeed.db"))
			const url = "https://example.com/feed"
			at := sampleRefresh().FetchedAt
			saveRefresh(t, s, url, Refresh{FetchedAt: at, Items: []RefreshItem{
				timelineEntry("new", at), timelineEntry("old", at.Add(-time.Hour)),
			}})
			ctx := t.Context()
			var wantIs error
			switch name {
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				wantIs = context.Canceled
			case "deadline":
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer cancel()
				wantIs = context.DeadlineExceeded
			case "closed":
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
			case "broken schema":
				execSQL(t, s.db, "DROP TABLE items")
			case "publication time":
				execSQL(t, s.db, "UPDATE items SET published_at = '0000-99-01T00:00:00.000000000Z' WHERE key = 'id:old'")
			case "first-fetch time":
				execSQL(t, s.db, "UPDATE items SET first_fetched_at = '0000-99-01T00:00:00.000000000Z' WHERE key = 'id:old'")
			case "scan":
				execSQL(t, s.db, `ALTER TABLE items RENAME TO saved_items;
					CREATE TABLE items AS SELECT * FROM saved_items;
					UPDATE items SET title = NULL WHERE key = 'id:old'`)
			case "query execution":
				execSQL(t, s.db, `ALTER TABLE items RENAME TO saved_items;
					CREATE VIEW items AS SELECT id, feed_url, key, title, url,
					CASE WHEN key = 'id:old' THEN json_extract('invalid', '$') ELSE content END AS content,
					published_at, first_fetched_at FROM saved_items`)
			}
			page, err := s.Timeline(ctx, TimelineQuery{FeedURLs: []string{url}})
			if err == nil || !strings.Contains(err.Error(), "read timeline") || !reflect.DeepEqual(page, TimelinePage{}) {
				t.Fatalf("failed query returned %+v, %v", page, err)
			}
			var driverErr *sqlite.Error
			if errors.As(err, &driverErr) || (wantIs != nil && !errors.Is(err, wantIs)) {
				t.Errorf("wrong public error cause: %v", err)
			}
			if got := s.db.Stats().InUse; got != 0 {
				t.Errorf("failed query retained %d connections", got)
			}
		})
	}
}

func TestTimelineCancellation(t *testing.T) {
	for _, name := range []string{"connection wait", "during execution"} {
		t.Run(name, func(t *testing.T) {
			s := openStore(t, filepath.Join(t.TempDir(), "openfeed.db"))
			const url = "https://example.com/feed"
			saveRefresh(t, s, url, sampleRefresh())
			if name == "connection wait" {
				conn, err := s.db.Conn(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
			} else {
				execSQL(t, s.db, `ALTER TABLE items RENAME TO saved_items;
					CREATE VIEW items AS
					WITH RECURSIVE expand(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM expand WHERE n < 100000000)
					SELECT id+n AS id, feed_url, title, url, content, published_at, first_fetched_at
					FROM saved_items CROSS JOIN expand`)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
			defer cancel()
			start := time.Now()
			page, err := s.Timeline(ctx, TimelineQuery{FeedURLs: []string{url}})
			if !errors.Is(err, context.DeadlineExceeded) || !reflect.DeepEqual(page, TimelinePage{}) || time.Since(start) >= lockWait/2 {
				t.Fatalf("canceled query returned %+v, %v after %s", page, err, time.Since(start))
			}
			if name == "during execution" {
				execSQL(t, s.db, "DROP VIEW items; ALTER TABLE saved_items RENAME TO items")
				if got := readTimeline(t, s, TimelineQuery{FeedURLs: []string{url}}); len(got.Items) != 1 {
					t.Error("query did not recover after cancellation")
				}
			}
		})
	}
}

func TestTimelineWALReadDuringWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "openfeed.db")
	reader, writer := openStore(t, path), openStore(t, path)
	const url = "https://example.com/feed"
	saveRefresh(t, writer, url, sampleRefresh())
	query := TimelineQuery{FeedURLs: []string{url}, Limit: 1}
	before := readTimeline(t, reader, query)
	tx, err := writer.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(t.Context(), "UPDATE items SET title = 'replacement', published_at = NULL"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	page, err := reader.Timeline(ctx, query)
	if err != nil || !reflect.DeepEqual(page, before) {
		t.Fatalf("query saw uncommitted data or waited for writer: %+v, %v", page, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	page = readTimeline(t, reader, query)
	if len(page.Items) != 1 || page.Items[0].Title != "replacement" || !page.Items[0].PublishedAt.IsZero() || page.Items[0].ID != before.Items[0].ID {
		t.Fatalf("query did not see committed replacement: %+v", page)
	}
	if got := reader.db.Stats().InUse; got != 0 {
		t.Errorf("query kept %d connections in use", got)
	}
}

func TestTimelineConcurrentQueries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "openfeed.db")
	reader, writer := openStore(t, path), openStore(t, path)
	const url = "https://example.com/feed"
	at := sampleRefresh().FetchedAt
	entries := make([]RefreshItem, 20)
	for i := range entries {
		entries[i] = timelineEntry(fmt.Sprintf("post-%d", i), at)
		entries[i].Title = "generation-0"
	}
	saveRefresh(t, writer, url, Refresh{FetchedAt: at, Items: entries})
	const readers = 8
	start := make(chan struct{})
	results := make(chan error, readers+1)
	for range readers {
		go func() {
			<-start
			for range 20 {
				page, err := reader.Timeline(t.Context(), TimelineQuery{FeedURLs: []string{url}})
				if err != nil {
					results <- err
					return
				}
				if len(page.Items) != 20 || page.Next != nil {
					results <- fmt.Errorf("incomplete concurrent page: %d items", len(page.Items))
					return
				}
				for _, item := range page.Items {
					if item.Title != page.Items[0].Title {
						results <- fmt.Errorf("page mixed committed refreshes: %q and %q", item.Title, page.Items[0].Title)
						return
					}
				}
			}
			results <- nil
		}()
	}
	go func() {
		<-start
		for generation := range 20 {
			for i := range entries {
				entries[i].Title = fmt.Sprintf("generation-%d", generation+1)
			}
			state, err := writer.FeedState(t.Context(), url)
			if err == nil {
				err = writer.SaveRefresh(t.Context(), state, Refresh{FetchedAt: at, Items: entries})
			}
			if err != nil {
				results <- err
				return
			}
		}
		results <- nil
	}()
	close(start)
	for range readers + 1 {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
}

func TestTimelineLargeURLSet(t *testing.T) {
	s := openStore(t, filepath.Join(t.TempDir(), "openfeed.db"))
	const url = "https://example.com/feed"
	saveRefresh(t, s, url, sampleRefresh())
	urls := make([]string, 40000)
	for i := range urls {
		urls[i] = fmt.Sprintf("https://example.com/uncached/%d", i)
	}
	urls = append(urls, url, url)
	query := TimelineQuery{FeedURLs: urls, Limit: 1}
	page := readTimeline(t, s, query)
	if len(page.Items) != 1 || page.Items[0].FeedURL != url || page.Next != nil {
		t.Errorf("large selection returned %+v", page)
	}
	_, args := timelineSQL(query)
	if len(args) != 2 {
		t.Errorf("selection size changed binding count to %d", len(args))
	}
	if got := queryInt(t, s.db, "SELECT COUNT(*) FROM feeds"); got != 1 {
		t.Errorf("query persisted selection: %d feeds", got)
	}
}

func TestTimelineQueryPlans(t *testing.T) {
	for _, fixture := range []string{"ties", "sparse active"} {
		t.Run(fixture, func(t *testing.T) {
			s := openStore(t, filepath.Join(t.TempDir(), "openfeed.db"))
			const active = "https://active.example/feed"
			at := sampleRefresh().FetchedAt
			for _, url := range []string{active, "https://inactive.example/feed"} {
				count := 64
				if url != active {
					count = 4096
				}
				entries := make([]RefreshItem, count)
				for i := range entries {
					date := at
					if fixture == "sparse active" {
						date = at.Add(time.Duration(i) * time.Minute)
					}
					entries[i] = timelineEntry(fmt.Sprintf("post-%d", i), date)
				}
				saveRefresh(t, s, url, Refresh{FetchedAt: fixtureFetch(), Items: entries})
			}
			execSQL(t, s.db, "ANALYZE")
			items := readTimeline(t, s, TimelineQuery{FeedURLs: []string{active}, Limit: 200}).Items
			if len(items) != 64 {
				t.Fatalf("sparse selection returned %d items", len(items))
			}
			for _, bounds := range []string{"head", "before", "through", "both", "after"} {
				t.Run(bounds, func(t *testing.T) {
					query := TimelineQuery{FeedURLs: []string{active}, Limit: 7}
					start, end := 0, len(items)
					if bounds == "before" || bounds == "both" {
						query.Before, start = &items[20].Position, 21
					}
					if bounds == "through" || bounds == "both" {
						query.Through, end = &items[40].Position, 41
					}
					if bounds == "after" {
						query.After = &items[40].Position
						if got := collectNewer(t, s, query); !slices.Equal(got, items[:40]) {
							t.Error("indexed upward query returned incorrect range")
						}
					} else if got := collectTimeline(t, s, query); !slices.Equal(got, items[start:end]) {
						t.Error("indexed query returned incorrect range")
					}
					statement, args := timelineSQL(query)
					rows, err := s.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+statement, args...)
					if err != nil {
						t.Fatal(err)
					}
					defer rows.Close()
					var details []string
					for rows.Next() {
						var id, parent, unused int
						var detail string
						if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
							t.Fatal(err)
						}
						details = append(details, detail)
					}
					if err := rows.Err(); err != nil {
						t.Fatal(err)
					}
					plan := strings.Join(details, "\n")
					want := "SCAN items USING INDEX items_timeline"
					if bounds != "head" {
						want = "SEARCH items USING INDEX items_timeline"
					}
					if !strings.Contains(plan, want) || strings.Contains(plan, "TEMP B-TREE FOR ORDER BY") {
						t.Errorf("expected ordered timeline index access (%s), got:\n%s", want, plan)
					}
				})
			}
		})
	}
}
