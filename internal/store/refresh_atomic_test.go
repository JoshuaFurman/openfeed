package store

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"modernc.org/sqlite"
)

func TestRefreshRollback(t *testing.T) {
	for _, tc := range []struct {
		name, setup, want string
	}{
		{"metadata", `CREATE TRIGGER fail_refresh BEFORE INSERT ON feeds
			BEGIN SELECT RAISE(ABORT, 'forced metadata failure'); END`, "save feed metadata"},
		{"item", `CREATE TRIGGER fail_refresh BEFORE INSERT ON items
			WHEN NEW.key = 'id:fail' BEGIN SELECT RAISE(ABORT, 'forced item failure'); END`, "save item"},
		{"commit", `CREATE TABLE commit_guard (item_id INTEGER REFERENCES items(id) DEFERRABLE INITIALLY DEFERRED) STRICT;
			CREATE TRIGGER fail_refresh AFTER INSERT ON items
			WHEN NEW.key = 'id:fail' BEGIN INSERT INTO commit_guard VALUES (-1); END`, "commit refresh"},
	} {
		for _, existing := range []bool{false, true} {
			name := tc.name + "/new feed"
			if existing {
				name = tc.name + "/existing feed"
			}
			t.Run(name, func(t *testing.T) {
				s := openStore(t, filepath.Join(t.TempDir(), "openfeed.db"))
				const url = "https://example.com/feed"
				refresh := sampleRefresh()
				if existing {
					saveRefresh(t, s, url, refresh)
				}
				before := readCache(t, s, url)
				execSQL(t, s.db, tc.setup)
				refresh.FetchedAt = refresh.FetchedAt.Add(time.Hour)
				refresh.ETag, refresh.LastModified = `"must-not-advance"`, "must-not-advance"
				refresh.Items[0].Title = "must-not-replace"
				refresh.Items = append(refresh.Items, RefreshItem{Key: "id:fail", Title: "Failure after an earlier write"})
				err := s.SaveRefresh(t.Context(), before.State, refresh)
				if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), url) {
					t.Fatalf("error = %v, want %q and feed URL", err, tc.want)
				}
				var driverErr *sqlite.Error
				if errors.As(err, &driverErr) {
					t.Errorf("driver error escaped: %v", err)
				}
				if after := readCache(t, s, url); !reflect.DeepEqual(after, before) {
					t.Fatalf("partial refresh survived rollback: %+v, want %+v", after, before)
				}
				if tc.name == "commit" && queryInt(t, s.db, "SELECT COUNT(*) FROM commit_guard") != 0 {
					t.Fatal("commit failure left uncommitted trigger writes on the connection")
				}
				execSQL(t, s.db, "DROP TRIGGER fail_refresh")
				refresh.FetchedAt = refresh.FetchedAt.Add(time.Hour)
				if err := s.SaveRefresh(t.Context(), before.State, refresh); err != nil {
					t.Fatalf("failed transaction consumed the revision or left a lock: %v", err)
				}
				after := readCache(t, s, url)
				if len(after.Items) != 2 || after.State.revision != before.State.revision+1 {
					t.Fatalf("retry did not commit exactly once: %+v", after)
				}
				for _, item := range after.Items {
					want := refresh.FetchedAt
					if existing && item.Key == "id:post" {
						want = before.Items[0].FirstFetchedAt
					}
					if item.FirstFetchedAt != want {
						t.Errorf("item %q first fetch = %v, want %v", item.Key, item.FirstFetchedAt, want)
					}
				}
			})
		}
	}
}

func TestRefreshRejectsInvalidOutcomes(t *testing.T) {
	for _, name := range []string{
		"zero snapshot", "unchanged uncached feed", "unchanged with items", "zero fetch time",
		"fetch before supported range", "fetch after supported range", "UTC conversion outside range",
		"empty key", "empty item", "unsupported publication time",
	} {
		t.Run(name, func(t *testing.T) {
			s := openStore(t, filepath.Join(t.TempDir(), "openfeed.db"))
			const url = "https://example.com/feed"
			refresh := sampleRefresh()
			if name != "unchanged uncached feed" {
				saveRefresh(t, s, url, refresh)
			}
			before := readCache(t, s, url)
			previous := before.State
			refresh.ETag = `"must-not-advance"`
			want := ""
			switch name {
			case "zero snapshot":
				previous = FeedState{}
				want = "snapshot"
			case "unchanged uncached feed":
				refresh.Unchanged, refresh.Items = true, nil
				want = "no cached feed"
			case "unchanged with items":
				refresh.Unchanged = true
				want = "contains items"
			case "zero fetch time":
				refresh.FetchedAt = time.Time{}
				want = "fetch time"
			case "fetch before supported range":
				refresh.FetchedAt = time.Date(-1, 12, 31, 23, 59, 59, 0, time.UTC)
				want = "fetch time"
			case "fetch after supported range":
				refresh.FetchedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
				want = "fetch time"
			case "UTC conversion outside range":
				refresh.FetchedAt = time.Date(0, 1, 1, 0, 0, 0, 0, time.FixedZone("east", 3600))
				want = "fetch time"
			case "empty key":
				refresh.Items = append(refresh.Items, RefreshItem{Title: "No identity"})
				want = "key is empty"
			case "empty item":
				refresh.Items = append(refresh.Items, RefreshItem{Key: "id:empty"})
				want = "no title, URL, or content"
			case "unsupported publication time":
				refresh.Items = append(refresh.Items, RefreshItem{Key: "id:invalid-date", Title: "Bad date", PublishedAt: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)})
				want = "publication time"
			}
			if err := s.SaveRefresh(t.Context(), previous, refresh); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("error = %v, want %q", err, want)
			}
			if after := readCache(t, s, url); !reflect.DeepEqual(after, before) {
				t.Errorf("invalid outcome changed cache: %+v, want %+v", after, before)
			}
		})
	}
}

func TestRefreshRejectsStaleSnapshots(t *testing.T) {
	for _, name := range []string{"initial", "full after full", "unchanged after full", "full after unchanged", "unchanged after unchanged"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "openfeed.db")
			s, other := openStore(t, path), openStore(t, path)
			const url = "https://example.com/feed"
			initial := sampleRefresh()
			if name != "initial" {
				saveRefresh(t, s, url, initial)
			}
			stale := feedState(t, s, url)
			winner := sampleRefresh()
			winner.ETag = `"winner"`
			winner.Items[0].Title = "Winner"
			winner.FetchedAt = winner.FetchedAt.Add(time.Hour)
			if strings.HasSuffix(name, "after unchanged") {
				winner.Unchanged, winner.Items = true, nil
			}
			saveRefresh(t, other, url, winner)
			before := readCache(t, s, url)
			loser := sampleRefresh()
			if strings.HasPrefix(name, "unchanged") {
				loser.Unchanged, loser.Items = true, nil
			}
			if err := s.SaveRefresh(t.Context(), stale, loser); !errors.Is(err, ErrRefreshConflict) || !strings.Contains(err.Error(), url) {
				t.Fatalf("stale outcome error = %v, want contextual ErrRefreshConflict", err)
			}
			if after := readCache(t, other, url); !reflect.DeepEqual(after, before) {
				t.Errorf("stale refresh changed cache: %+v, want %+v", after, before)
			}
		})
	}
}

func TestRefreshConcurrentWriters(t *testing.T) {
	for _, separateHandles := range []bool{false, true} {
		name := "same store"
		if separateHandles {
			name = "separate handles"
		}
		t.Run(name, func(t *testing.T) {
			for _, existing := range []bool{false, true} {
				name := "new feed"
				if existing {
					name = "existing feed"
				}
				t.Run(name, func(t *testing.T) {
					path := filepath.Join(t.TempDir(), "openfeed.db")
					s := openStore(t, path)
					other := s
					if separateHandles {
						other = openStore(t, path)
					}
					const url = "https://example.com/feed"
					if existing {
						saveRefresh(t, s, url, sampleRefresh())
					}
					states := []FeedState{feedState(t, s, url), feedState(t, other, url)}
					refreshes := []Refresh{sampleRefresh(), sampleRefresh()}
					refreshes[0].ETag, refreshes[0].Items[0].Title = "first", "First"
					refreshes[1].ETag, refreshes[1].Items[0].Title = "second", "Second"
					type result struct {
						index int
						err   error
					}
					start := make(chan struct{})
					results := make(chan result, 2)
					for i, writer := range []*Store{s, other} {
						go func() {
							<-start
							results <- result{i, writer.SaveRefresh(t.Context(), states[i], refreshes[i])}
						}()
					}
					close(start)
					winner, conflicts := -1, 0
					for range 2 {
						r := <-results
						if r.err == nil {
							if winner != -1 {
								t.Error("both outcomes committed against the same revision")
							}
							winner = r.index
						} else if errors.Is(r.err, ErrRefreshConflict) {
							conflicts++
						} else {
							t.Error(r.err)
						}
					}
					if winner == -1 || conflicts != 1 {
						t.Fatalf("winner = %d, conflicts = %d; want one of each", winner, conflicts)
					}
					cache := readCache(t, s, url)
					if cache.State.ETag != refreshes[winner].ETag || cache.State.revision != states[winner].revision+1 ||
						len(cache.Items) != 1 || cache.Items[0].RefreshItem != refreshes[winner].Items[0] {
						t.Errorf("winning metadata/items do not match: %+v", cache)
					}
				})
			}
		})
	}
}

func TestRefreshLockWait(t *testing.T) {
	for _, name := range []string{"release", "cancel", "deadline", "busy limit", "connection wait"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "openfeed.db")
			s, blocker := openStore(t, path), openStore(t, path)
			const url = "https://example.com/feed"
			before := readCache(t, s, url)
			if name == "connection wait" {
				blocker = s
			}
			tx, err := blocker.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if name == "deadline" || name == "connection wait" {
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(ctx, 100*time.Millisecond)
				defer stop()
			}
			results := make(chan error, 1)
			start := time.Now()
			go func() { results <- s.SaveRefresh(ctx, before.State, sampleRefresh()) }()
			if name == "release" || name == "cancel" {
				select {
				case err := <-results:
					t.Fatalf("save returned while lock held: %v", err)
				case <-time.After(50 * time.Millisecond):
				}
				if name == "release" {
					if err := tx.Rollback(); err != nil {
						t.Fatal(err)
					}
				} else {
					cancel()
				}
			}
			err = <-results
			elapsed := time.Since(start)
			switch name {
			case "release":
				if err != nil {
					t.Fatalf("released lock did not unblock refresh: %v", err)
				}
				return
			case "cancel":
				if !errors.Is(err, context.Canceled) || elapsed >= lockWait/2 {
					t.Fatalf("cancel returned %v after %s", err, elapsed)
				}
			case "deadline", "connection wait":
				if !errors.Is(err, context.DeadlineExceeded) || elapsed >= lockWait/2 {
					t.Fatalf("deadline returned %v after %s", err, elapsed)
				}
			case "busy limit":
				if err == nil || !strings.Contains(err.Error(), "remained busy") || elapsed < lockWait || elapsed > 2*lockWait {
					t.Fatalf("busy limit returned %v after %s", err, elapsed)
				}
			}
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			if after := readCache(t, s, url); !reflect.DeepEqual(after, before) {
				t.Errorf("failed lock acquisition changed cache: %+v", after)
			}
			saveRefresh(t, s, url, sampleRefresh())
		})
	}
}

func TestRefreshPublicErrors(t *testing.T) {
	for _, name := range []string{"canceled", "deadline", "closed", "broken schema", "invalid cached date", "empty URL"} {
		t.Run(name, func(t *testing.T) {
			s := openStore(t, filepath.Join(t.TempDir(), "openfeed.db"))
			url := "https://example.com/feed"
			saveRefresh(t, s, url, sampleRefresh())
			previous := feedState(t, s, url)
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
				execSQL(t, s.db, "DROP TABLE items; DROP TABLE feeds")
			case "invalid cached date":
				execSQL(t, s.db, "UPDATE feeds SET fetched_at = '2025-99-02T13:00:00.123456789Z'")
			case "empty URL":
				url = ""
			}
			state, err := s.FeedState(ctx, url)
			if err == nil || state != (FeedState{}) {
				t.Fatalf("failed state read = %+v, %v", state, err)
			}
			var driverErr *sqlite.Error
			if errors.As(err, &driverErr) || (wantIs != nil && !errors.Is(err, wantIs)) {
				t.Errorf("state read error has wrong public cause: %v", err)
			}
			if name == "invalid cached date" || name == "empty URL" {
				return
			}
			err = s.SaveRefresh(ctx, previous, sampleRefresh())
			if err == nil || errors.As(err, &driverErr) || (wantIs != nil && !errors.Is(err, wantIs)) {
				t.Errorf("save error has wrong public cause: %v", err)
			}
		})
	}
}
