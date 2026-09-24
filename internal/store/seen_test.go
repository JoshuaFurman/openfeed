package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"modernc.org/sqlite"
)

func seenPosition(t *testing.T, s *Store) *TimelineCursor {
	t.Helper()
	position, err := s.SeenPosition(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return position
}

func advanceSeen(t *testing.T, s *Store, reached TimelineCursor) {
	t.Helper()
	if err := s.AdvanceSeen(t.Context(), reached); err != nil {
		t.Fatal(err)
	}
}

func itemsByTitle(items []TimelineItem) map[string]TimelineItem {
	byTitle := make(map[string]TimelineItem, len(items))
	for _, item := range items {
		byTitle[item.Title] = item
	}
	return byTitle
}

// dividerSplit partitions titles as a divider at seen would, preserving timeline order.
func dividerSplit(items []TimelineItem, seen TimelineCursor) (above, below []string) {
	for _, item := range items {
		if item.Position.Compare(seen) > 0 {
			above = append(above, item.Title)
		} else {
			below = append(below, item.Title)
		}
	}
	return above, below
}

func checkDivider(t *testing.T, items []TimelineItem, seen TimelineCursor, wantAbove, wantBelow []string) {
	t.Helper()
	above, below := dividerSplit(items, seen)
	if !slices.Equal(above, wantAbove) || !slices.Equal(below, wantBelow) {
		t.Errorf("divider split %v | %v, want %v | %v", above, below, wantAbove, wantBelow)
	}
}

func TestSeenUpgradeAndReopen(t *testing.T) {
	if got := seenPosition(t, openStore(t, filepath.Join(t.TempDir(), "openfeed.db"))); got != nil {
		t.Fatalf("fresh database has seen position %+v", got)
	}

	path := filepath.Join(t.TempDir(), "openfeed.db")
	old := openMigrationStore(t, path)
	var scripts []string
	for _, name := range []string{"002_refresh.sql", "003_timeline.sql"} {
		script, err := fs.ReadFile(migrations, "migrations/"+name)
		if err != nil {
			t.Fatal(err)
		}
		scripts = append(scripts, string(script))
	}
	if err := migrate(t.Context(), old.db, migrationFiles(t, scripts...)); err != nil {
		t.Fatal(err)
	}
	const url = "https://example.com/feed"
	at := sampleRefresh().FetchedAt
	saveRefresh(t, old, url, Refresh{FetchedAt: at, Items: []RefreshItem{
		timelineEntry("newest", at.Add(-time.Hour)), timelineEntry("middle", at.Add(-2*time.Hour)),
		timelineEntry("oldest", at.Add(-3*time.Hour)),
	}})
	cache := readCache(t, old, url)
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	s := openStore(t, path)
	if got := seenPosition(t, s); got != nil {
		t.Fatalf("upgraded database has seen position %+v", got)
	}
	if got := queryInt(t, s.db, "SELECT COUNT(*) FROM seen_position"); got != 0 {
		t.Fatalf("reading created %d seen rows", got)
	}
	if got := readCache(t, s, url); !reflect.DeepEqual(got, cache) {
		t.Error("seen migration changed cached feeds")
	}
	query := TimelineQuery{FeedURLs: []string{url}}
	items := collectTimeline(t, s, query)
	reached := itemsByTitle(items)["middle"].Position
	advanceSeen(t, s, reached)
	if got := seenPosition(t, openStore(t, path)); got == nil || *got != reached {
		t.Fatalf("separate handle read %+v, want %+v", got, reached)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s = openStore(t, path)
	seen := seenPosition(t, s)
	if seen == nil || *seen != reached {
		t.Fatalf("reopened seen position = %+v, want %+v", seen, reached)
	}
	checkDivider(t, collectTimeline(t, s, query), *seen, []string{"newest"}, []string{"middle", "oldest"})
	query.Before = seen
	if got := timelineTitles(collectTimeline(t, s, query)); !slices.Equal(got, []string{"oldest"}) {
		t.Errorf("seen position as Before bound = %v", got)
	}
	query.Before, query.Through = nil, seen
	if got := timelineTitles(collectTimeline(t, s, query)); !slices.Equal(got, []string{"newest", "middle"}) {
		t.Errorf("seen position as Through bound = %v", got)
	}
}

func TestSeenAdvanceIsMonotonic(t *testing.T) {
	s := openStore(t, filepath.Join(t.TempDir(), "openfeed.db"))
	const url = "https://example.com/feed"
	at := sampleRefresh().FetchedAt
	saveRefresh(t, s, url, Refresh{FetchedAt: at, Items: []RefreshItem{
		timelineEntry("newer", at.Add(-time.Hour)), timelineEntry("tie-low", at.Add(-2*time.Hour)),
		timelineEntry("tie-high", at.Add(-2*time.Hour)), timelineEntry("older", at.Add(-3*time.Hour)),
	}})
	items := itemsByTitle(collectTimeline(t, s, TimelineQuery{FeedURLs: []string{url}}))
	if items["tie-high"].ID <= items["tie-low"].ID {
		t.Fatal("fixture IDs do not follow source order")
	}
	for _, step := range []struct{ reach, want string }{
		{"tie-low", "tie-low"},
		{"older", "tie-low"},
		{"tie-low", "tie-low"},
		{"tie-high", "tie-high"},
		{"tie-low", "tie-high"},
		{"newer", "newer"},
		{"older", "newer"},
	} {
		advanceSeen(t, s, items[step.reach].Position)
		if got := seenPosition(t, s); got == nil || *got != items[step.want].Position {
			t.Fatalf("after reaching %s, seen = %+v, want %s", step.reach, got, step.want)
		}
	}
}

func TestSeenLaterTiesSortAbove(t *testing.T) {
	s := openStore(t, filepath.Join(t.TempDir(), "openfeed.db"))
	const dated, other = "https://dated.example/feed", "https://other.example/feed"
	urls := []string{dated, other}
	at := sampleRefresh().FetchedAt
	day := time.Date(2025, 9, 1, 0, 0, 0, 0, time.UTC) // Date-only publishing stamps every post at midnight.
	saveRefresh(t, s, dated, Refresh{FetchedAt: at, Items: []RefreshItem{
		timelineEntry("first", day), timelineEntry("second", day), timelineEntry("yesterday", day.Add(-24*time.Hour)),
	}})
	advanceSeen(t, s, itemsByTitle(collectTimeline(t, s, TimelineQuery{FeedURLs: urls}))["first"].Position)

	saveRefresh(t, s, dated, Refresh{FetchedAt: at.Add(time.Hour), Items: []RefreshItem{
		timelineEntry("first", day), timelineEntry("second", day), timelineEntry("third", day),
	}})
	saveRefresh(t, s, other, Refresh{FetchedAt: at.Add(2 * time.Hour), Items: []RefreshItem{timelineEntry("elsewhere", day)}})
	items := collectTimeline(t, s, TimelineQuery{FeedURLs: urls})
	checkDivider(t, items, *seenPosition(t, s),
		[]string{"elsewhere", "third", "second"}, []string{"first", "yesterday"})
}

func TestSeenLateDiscoveriesAndCorrections(t *testing.T) {
	const url, added = "https://example.com/feed", "https://added.example/feed"
	at := sampleRefresh().FetchedAt
	base := at.Add(-24 * time.Hour)
	for _, tc := range []struct {
		name             string
		reachedDate      time.Time
		wantAbove, below []string
	}{
		{"date removed", time.Time{}, []string{"reached", "catalog-new", "new", "a", "d"}, []string{"late", "catalog-old", "e", "b"}},
		{"corrected newer", base.Add(330 * time.Minute), []string{"catalog-new", "new", "reached", "a", "d"}, []string{"late", "catalog-old", "e", "b"}},
		{"corrected older", base.Add(15 * time.Minute), []string{"catalog-new", "new", "a", "d"}, []string{"late", "catalog-old", "e", "b", "reached"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openStore(t, filepath.Join(t.TempDir(), "openfeed.db"))
			saveRefresh(t, s, url, Refresh{FetchedAt: at, Items: []RefreshItem{
				timelineEntry("a", base.Add(5*time.Hour)), timelineEntry("b", base.Add(4*time.Hour)),
				timelineEntry("reached", base.Add(3*time.Hour)), timelineEntry("d", base.Add(2*time.Hour)),
				timelineEntry("e", base.Add(time.Hour)),
			}})
			reached := itemsByTitle(collectTimeline(t, s, TimelineQuery{FeedURLs: []string{url}}))["reached"].Position
			advanceSeen(t, s, reached)

			saveRefresh(t, s, url, Refresh{FetchedAt: at.Add(time.Hour), Items: []RefreshItem{
				timelineEntry("new", base.Add(6*time.Hour)),
				timelineEntry("late", base.Add(150*time.Minute)),
				timelineEntry("b", base.Add(30*time.Minute)),
				timelineEntry("d", base.Add(270*time.Minute)),
				timelineEntry("reached", tc.reachedDate),
			}})
			saveRefresh(t, s, added, Refresh{FetchedAt: at.Add(2 * time.Hour), Items: []RefreshItem{
				timelineEntry("catalog-new", base.Add(7*time.Hour)), timelineEntry("catalog-old", base.Add(90*time.Minute)),
			}})
			seen := seenPosition(t, s)
			if seen == nil || *seen != reached {
				t.Fatalf("refresh moved seen position to %+v, want %+v", seen, reached)
			}
			checkDivider(t, collectTimeline(t, s, TimelineQuery{FeedURLs: []string{url, added}}), *seen, tc.wantAbove, tc.below)
		})
	}
}

func TestSeenFutureDatedPosts(t *testing.T) {
	s := openStore(t, filepath.Join(t.TempDir(), "openfeed.db"))
	const url = "https://example.com/feed"
	query := TimelineQuery{FeedURLs: []string{url}}
	at := sampleRefresh().FetchedAt
	saveRefresh(t, s, url, Refresh{FetchedAt: at, Items: []RefreshItem{
		timelineEntry("undated", time.Time{}), timelineEntry("future", at.Add(100*time.Hour)),
		timelineEntry("past", at.Add(-time.Hour)),
	}})
	future := itemsByTitle(collectTimeline(t, s, query))["future"]
	if want := (TimelineCursor{date: at.Format(timestampLayout), id: future.ID}); future.Position != want {
		t.Fatalf("future-dated post observed at %+v, want first-fetch position %+v", future.Position, want)
	}
	advanceSeen(t, s, future.Position)
	seen := seenPosition(t, s)
	if seen == nil || *seen != future.Position {
		t.Fatalf("seen position = %+v, want %+v", seen, future.Position)
	}

	saveRefresh(t, s, url, Refresh{FetchedAt: at.Add(2 * time.Hour), Items: []RefreshItem{timelineEntry("real", at.Add(time.Hour))}})
	checkDivider(t, collectTimeline(t, s, query), *seen, []string{"real"}, []string{"future", "undated", "past"})
	query.Before = seen
	if got := timelineTitles(collectTimeline(t, s, query)); !slices.Equal(got, []string{"undated", "past"}) {
		t.Errorf("seen position as Before bound = %v", got)
	}
}

func TestSeenOutlivesItemAndSubscription(t *testing.T) {
	path := filepath.Join(t.TempDir(), "openfeed.db")
	s := openStore(t, path)
	const a, b = "https://a.example/feed", "https://b.example/feed"
	at := sampleRefresh().FetchedAt
	saveRefresh(t, s, a, Refresh{FetchedAt: at, Items: []RefreshItem{
		timelineEntry("a-new", at.Add(-time.Hour)), timelineEntry("a-reached", at.Add(-3*time.Hour)),
		timelineEntry("a-old", at.Add(-5*time.Hour)),
	}})
	saveRefresh(t, s, b, Refresh{FetchedAt: at, Items: []RefreshItem{
		timelineEntry("b-new", at.Add(-2*time.Hour)), timelineEntry("b-old", at.Add(-4*time.Hour)),
	}})
	reached := itemsByTitle(collectTimeline(t, s, TimelineQuery{FeedURLs: []string{a, b}}))["a-reached"]
	advanceSeen(t, s, reached.Position)
	checkDivider(t, collectTimeline(t, s, TimelineQuery{FeedURLs: []string{b}}), reached.Position,
		[]string{"b-new"}, []string{"b-old"})

	// No API deletes items yet; a foreign key would reject this removal.
	if _, err := s.db.ExecContext(t.Context(), "DELETE FROM items WHERE id = ?", reached.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openStore(t, path)
	seen := seenPosition(t, s)
	if seen == nil || *seen != reached.Position {
		t.Fatalf("item removal changed seen position to %+v", seen)
	}
	query := TimelineQuery{FeedURLs: []string{a, b}, Before: seen}
	if got := timelineTitles(collectTimeline(t, s, query)); !slices.Equal(got, []string{"b-old", "a-old"}) {
		t.Errorf("orphaned position as Before bound = %v", got)
	}
	query.Before, query.Through = nil, seen
	if got := timelineTitles(collectTimeline(t, s, query)); !slices.Equal(got, []string{"a-new", "b-new"}) {
		t.Errorf("orphaned position as Through bound = %v", got)
	}
	advanceSeen(t, s, *seen)
	if got := seenPosition(t, s); got == nil || *got != reached.Position {
		t.Errorf("re-saving an orphaned point changed it to %+v", got)
	}
}

func TestSeenInvalidPositions(t *testing.T) {
	s := openStore(t, filepath.Join(t.TempDir(), "openfeed.db"))
	const url = "https://example.com/feed"
	saveRefresh(t, s, url, sampleRefresh())
	item := readTimeline(t, s, TimelineQuery{FeedURLs: []string{url}}).Items[0]
	invalid := []TimelineCursor{{}, {date: item.Position.date}, {id: item.ID}}
	for _, saved := range []bool{false, true} {
		if saved {
			advanceSeen(t, s, item.Position)
		}
		for _, position := range invalid {
			if err := s.AdvanceSeen(t.Context(), position); err == nil || !strings.Contains(err.Error(), "advance seen position") {
				t.Errorf("invalid position %+v returned %v", position, err)
			}
		}
		if got := seenPosition(t, s); (got != nil) != saved || (saved && *got != item.Position) {
			t.Errorf("invalid positions changed seen state to %+v", got)
		}
	}
}

func TestSeenPublicErrors(t *testing.T) {
	for _, name := range []string{"canceled", "deadline", "closed", "missing seen table"} {
		t.Run(name, func(t *testing.T) {
			s := openStore(t, filepath.Join(t.TempDir(), "openfeed.db"))
			const url = "https://example.com/feed"
			saveRefresh(t, s, url, sampleRefresh())
			reached := readTimeline(t, s, TimelineQuery{FeedURLs: []string{url}}).Items[0].Position
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
			case "missing seen table":
				execSQL(t, s.db, "DROP TABLE seen_position")
			}
			var driverErr *sqlite.Error
			position, err := s.SeenPosition(ctx)
			if err == nil || position != nil || !strings.Contains(err.Error(), "read seen position") ||
				errors.As(err, &driverErr) || (wantIs != nil && !errors.Is(err, wantIs)) {
				t.Errorf("failed read returned %+v, %v", position, err)
			}
			err = s.AdvanceSeen(ctx, reached)
			if err == nil || !strings.Contains(err.Error(), "advance seen position") ||
				errors.As(err, &driverErr) || (wantIs != nil && !errors.Is(err, wantIs)) {
				t.Errorf("failed advance returned %v", err)
			}
			if name != "closed" {
				if got := s.db.Stats().InUse; got != 0 {
					t.Errorf("failed operation retained %d connections", got)
				}
			}
		})
	}
}

func TestSeenLockWait(t *testing.T) {
	for _, name := range []string{"release", "cancel"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "openfeed.db")
			s, blocker := openStore(t, path), openStore(t, path)
			const url = "https://example.com/feed"
			at := sampleRefresh().FetchedAt
			saveRefresh(t, s, url, Refresh{FetchedAt: at, Items: []RefreshItem{
				timelineEntry("newer", at.Add(-time.Hour)), timelineEntry("older", at.Add(-2*time.Hour)),
			}})
			items := readTimeline(t, s, TimelineQuery{FeedURLs: []string{url}}).Items
			advanceSeen(t, s, items[1].Position)

			tx, err := blocker.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err := tx.ExecContext(t.Context(), "UPDATE seen_position SET item_id = item_id + 1000"); err != nil {
				t.Fatal(err)
			}
			readCtx, stop := context.WithTimeout(t.Context(), time.Second)
			defer stop()
			if got, err := s.SeenPosition(readCtx); err != nil || got == nil || *got != items[1].Position {
				t.Fatalf("read saw uncommitted data or waited for writer: %+v, %v", got, err)
			}

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			results := make(chan error, 1)
			start := time.Now()
			go func() { results <- s.AdvanceSeen(ctx, items[0].Position) }()
			select {
			case err := <-results:
				t.Fatalf("advance returned while lock held: %v", err)
			case <-time.After(50 * time.Millisecond):
			}
			want := items[1].Position
			if name == "release" {
				if err := tx.Rollback(); err != nil {
					t.Fatal(err)
				}
				if err := <-results; err != nil {
					t.Fatalf("released lock did not unblock advance: %v", err)
				}
				want = items[0].Position
			} else {
				cancel()
				if err := <-results; !errors.Is(err, context.Canceled) || time.Since(start) >= lockWait/2 {
					t.Fatalf("cancel returned %v after %s", err, time.Since(start))
				}
				if err := tx.Rollback(); err != nil {
					t.Fatal(err)
				}
			}
			if got := seenPosition(t, s); got == nil || *got != want {
				t.Errorf("seen position = %+v, want %+v", got, want)
			}
		})
	}
}

func TestSeenConcurrentAdvances(t *testing.T) {
	path := filepath.Join(t.TempDir(), "openfeed.db")
	const handles = 4
	stores := make([]*Store, handles)
	for i := range stores {
		stores[i] = openStore(t, path)
	}
	const url, other = "https://example.com/feed", "https://other.example/feed"
	at := sampleRefresh().FetchedAt
	var entries []RefreshItem
	for i := range 40 {
		entries = append(entries, timelineEntry(fmt.Sprintf("post-%d", i), at.Add(-time.Duration(i/2)*time.Minute)))
	}
	saveRefresh(t, stores[0], url, Refresh{FetchedAt: at, Items: entries})
	items := collectTimeline(t, stores[0], TimelineQuery{FeedURLs: []string{url}})
	positions := make([]TimelineCursor, len(items))
	for i, item := range items {
		positions[i] = item.Position
	}
	rand.New(rand.NewPCG(1, 2)).Shuffle(len(positions), func(i, j int) {
		positions[i], positions[j] = positions[j], positions[i]
	})

	start := make(chan struct{})
	results := make(chan error, handles+1)
	for i, s := range stores {
		go func() {
			<-start
			var last TimelineCursor
			for j := i; j < len(positions); j += handles {
				if err := s.AdvanceSeen(t.Context(), positions[j]); err != nil {
					results <- err
					return
				}
				got, err := s.SeenPosition(t.Context())
				if err != nil {
					results <- err
					return
				}
				if got == nil || got.Compare(positions[j]) < 0 || got.Compare(last) < 0 {
					results <- fmt.Errorf("seen position regressed to %+v", got)
					return
				}
				last = *got
			}
			results <- nil
		}()
	}
	go func() {
		<-start
		for generation := range 20 {
			state, err := stores[0].FeedState(t.Context(), other)
			if err == nil {
				entry := timelineEntry(fmt.Sprintf("other-%d", generation), at.Add(-time.Hour))
				err = stores[0].SaveRefresh(t.Context(), state, Refresh{FetchedAt: at, Items: []RefreshItem{entry}})
			}
			if err != nil {
				results <- err
				return
			}
		}
		results <- nil
	}()
	close(start)
	for range handles + 1 {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
	if got := seenPosition(t, stores[1]); got == nil || *got != items[0].Position {
		t.Errorf("concurrent advances ended at %+v, want newest %+v", got, items[0].Position)
	}
}
