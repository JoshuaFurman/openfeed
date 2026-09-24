package reader

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/JoshuaFurman/openfeed/internal/config"
	"github.com/JoshuaFurman/openfeed/internal/feed"
	"github.com/JoshuaFurman/openfeed/internal/store"
)

type outcome struct {
	report Report
	err    error
}

// checkReport verifies config order, statuses, and that only failing feeds carry errors.
func checkReport(t *testing.T, report Report, feeds []config.Feed, want ...Status) {
	t.Helper()
	if got := statuses(report); !slices.Equal(got, want) {
		t.Fatalf("statuses = %v, want %v", got, want)
	}
	for i, result := range report.Feeds {
		if result.Feed.URL != feeds[i].URL || (result.Err != nil) != (result.Status == Failing) {
			t.Errorf("result %d = %+v", i, result)
		}
	}
}

func served(feedURL string, c *clock) feed.Result {
	return feed.Result{Items: []feed.Item{post(strings.TrimPrefix(feedURL, "https://"), c.Now())}, FetchedAt: c.Now()}
}

func TestRefreshIsolatesFailures(t *testing.T) {
	feeds := []config.Feed{
		{URL: "https://ok.example/feed", Name: "ok"},
		{URL: "https://broken.example/feed", Name: "broken"},
		{URL: "https://cached.example/feed", Name: "cached"},
	}
	var logs bytes.Buffer
	r, _, c := newReader(t, feeds, nil)
	r.log = slog.New(slog.NewTextHandler(&logs, nil))
	var cachedFails atomic.Bool
	r.fetcher = fetchFunc(func(_ context.Context, feedURL string, _ feed.Validators) (feed.Result, error) {
		if feedURL == feeds[1].URL || (feedURL == feeds[2].URL && cachedFails.Load()) {
			return feed.Result{}, fmt.Errorf("fetch %s: HTTP 500", feedURL)
		}
		return served(feedURL, c), nil
	})
	checkReport(t, refresh(t, r), feeds, Refreshed, Failing, Refreshed)

	cachedFails.Store(true)
	c.Advance(refreshGap)
	checkReport(t, refresh(t, r), feeds, Refreshed, Failing, Failing)
	got := titles(river(t, r))
	slices.Sort(got)
	if !slices.Equal(got, []string{"cached.example/feed@cached", "ok.example/feed@ok"}) {
		t.Errorf("failures hid cached posts: %v", got)
	}
	for _, f := range feeds[1:] {
		if !strings.Contains(logs.String(), f.URL) {
			t.Errorf("failure of %s was not logged:\n%s", f.URL, logs.String())
		}
	}
}

func TestRefreshReportsStoreFailures(t *testing.T) {
	feeds := []config.Feed{{URL: "https://valid.example/feed", Name: "valid"}, {URL: "https://invalid.example/feed", Name: "invalid"}}
	r, st, c := newReader(t, feeds, nil)
	r.fetcher = fetchFunc(func(_ context.Context, feedURL string, _ feed.Validators) (feed.Result, error) {
		result := served(feedURL, c)
		if feedURL == feeds[1].URL {
			result.FetchedAt = time.Time{} // Rejected by SaveRefresh.
		}
		return result, nil
	})
	report := refresh(t, r)
	checkReport(t, report, feeds, Refreshed, Failing)
	if !strings.Contains(report.Feeds[1].Err.Error(), "save refresh") {
		t.Errorf("save failure = %v", report.Feeds[1].Err)
	}

	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	c.Advance(time.Hour)
	checkReport(t, refresh(t, r), feeds, Failing, Failing)
	if _, err := r.Timeline(t.Context(), Query{}); err == nil {
		t.Error("closed store returned a river")
	}
}

func TestRefreshConcurrentHostSlots(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var feeds []config.Feed
		for i := range 4 {
			feeds = append(feeds, config.Feed{URL: fmt.Sprintf("https://host-%d.example/feed", i), Name: "distinct"})
		}
		for i := range 5 {
			feeds = append(feeds, config.Feed{URL: fmt.Sprintf("https://shared.example/%d", i), Name: "shared"})
		}
		r, _, c := newReader(t, feeds, nil)
		release := make(chan struct{})
		var mu sync.Mutex
		active, peak, fetched := map[string]int{}, map[string]int{}, map[string]int{}
		r.fetcher = fetchFunc(func(_ context.Context, feedURL string, _ feed.Validators) (feed.Result, error) {
			u, _ := url.Parse(feedURL)
			mu.Lock()
			active[u.Host]++
			peak[u.Host] = max(peak[u.Host], active[u.Host])
			fetched[feedURL]++
			mu.Unlock()
			<-release
			mu.Lock()
			active[u.Host]--
			mu.Unlock()
			return served(feedURL, c), nil
		})
		done := make(chan outcome, 1)
		go func() {
			report, err := r.Refresh(t.Context())
			done <- outcome{report, err}
		}()

		synctest.Wait()
		mu.Lock()
		total := 0
		for _, n := range active {
			total += n
		}
		if total != 6 || active["shared.example"] != hostSlots {
			t.Errorf("in flight before any finished: %v; want every host at once, %d on the shared host", active, hostSlots)
		}
		mu.Unlock()
		close(release)
		result := <-done
		if result.err != nil {
			t.Fatal(result.err)
		}
		checkReport(t, result.report, feeds, slices.Repeat([]Status{Refreshed}, len(feeds))...)
		if peak["shared.example"] != hostSlots || len(fetched) != len(feeds) {
			t.Errorf("shared host peak = %d, fetched = %v", peak["shared.example"], fetched)
		}
		for feedURL, n := range fetched {
			if n != 1 {
				t.Errorf("%s fetched %d times", feedURL, n)
			}
		}
	})
}

func TestRefreshJoinsRoundInFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		feeds := []config.Feed{{URL: "https://a.example/feed", Name: "alpha"}}
		r, _, c := newReader(t, feeds, nil)
		release := make(chan struct{})
		var fetches atomic.Int32
		r.fetcher = fetchFunc(func(_ context.Context, feedURL string, _ feed.Validators) (feed.Result, error) {
			fetches.Add(1)
			<-release
			return served(feedURL, c), nil
		})
		start := func(ctx context.Context) chan outcome {
			done := make(chan outcome, 1)
			go func() {
				report, err := r.Refresh(ctx)
				done <- outcome{report, err}
			}()
			return done
		}
		first := start(t.Context())
		synctest.Wait()
		second := start(t.Context())
		ctx, cancel := context.WithCancel(t.Context())
		impatient := start(ctx)
		synctest.Wait()

		cancel()
		if got := <-impatient; !errors.Is(got.err, context.Canceled) || len(got.report.Feeds) != 0 {
			t.Fatalf("canceled joiner returned %+v", got)
		}
		close(release)
		a, b := <-first, <-second
		if a.err != nil || b.err != nil {
			t.Fatal(a.err, b.err)
		}
		checkReport(t, a.report, feeds, Refreshed)
		checkReport(t, b.report, feeds, Refreshed)
		if n := fetches.Load(); n != 1 {
			t.Fatalf("joined calls fetched %d times", n)
		}
		a.report.Feeds[0].Status = Failing
		if b.report.Feeds[0].Status != Refreshed {
			t.Error("callers of one round share mutable results")
		}

		c.Advance(refreshGap)
		checkReport(t, refresh(t, r), feeds, Refreshed)
		if n := fetches.Load(); n != 2 {
			t.Errorf("a call after the round did not start a new one: %d fetches", n)
		}
	})
}

func TestRefreshRerunsAfterCanceledStarter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		feeds := []config.Feed{{URL: "https://a.example/feed", Name: "alpha"}}
		r, _, c := newReader(t, feeds, nil)
		var fetches atomic.Int32
		r.fetcher = fetchFunc(func(ctx context.Context, feedURL string, _ feed.Validators) (feed.Result, error) {
			if fetches.Add(1) == 1 {
				<-ctx.Done()
				return feed.Result{}, fmt.Errorf("fetch %s: %w", feedURL, ctx.Err())
			}
			return served(feedURL, c), nil
		})
		ctx, cancel := context.WithCancel(t.Context())
		starter, joiner := make(chan outcome, 1), make(chan outcome, 1)
		go func() {
			report, err := r.Refresh(ctx)
			starter <- outcome{report, err}
		}()
		synctest.Wait()
		go func() {
			report, err := r.Refresh(t.Context())
			joiner <- outcome{report, err}
		}()
		synctest.Wait()

		cancel()
		if got := <-starter; !errors.Is(got.err, context.Canceled) {
			t.Fatalf("canceled starter returned %v", got.err)
		}
		got := <-joiner
		if got.err != nil {
			t.Fatal(got.err)
		}
		checkReport(t, got.report, feeds, Refreshed)
		if n := fetches.Load(); n != 2 {
			t.Errorf("joiner with a live context did not rerun the round: %d fetches", n)
		}
	})
}

func TestRefreshRereadsStateAfterQueueing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "openfeed.db")
		var feeds []config.Feed
		for i := range 3 {
			feeds = append(feeds, config.Feed{URL: fmt.Sprintf("https://shared.example/%d", i), Name: "shared"})
		}
		r, _, c := newReaderAt(t, path, feeds, nil)
		elsewhere := openStore(t, path)
		release := make(chan struct{})
		var mu sync.Mutex
		fetched := map[string]int{}
		r.fetcher = fetchFunc(func(_ context.Context, feedURL string, _ feed.Validators) (feed.Result, error) {
			mu.Lock()
			fetched[feedURL]++
			mu.Unlock()
			<-release
			return served(feedURL, c), nil
		})
		done := make(chan outcome, 1)
		go func() {
			report, err := r.Refresh(t.Context())
			done <- outcome{report, err}
		}()
		synctest.Wait() // Two feeds hold the host's slots; the third is queued.

		mu.Lock()
		var queued string
		for _, f := range feeds {
			if fetched[f.URL] == 0 {
				queued = f.URL
			}
		}
		mu.Unlock()
		state, err := elsewhere.FeedState(t.Context(), queued)
		if err == nil {
			err = elsewhere.SaveRefresh(t.Context(), state, store.Refresh{FetchedAt: c.Now()})
		}
		if err != nil {
			t.Fatal(err)
		}
		close(release)
		got := <-done
		for _, result := range got.report.Feeds {
			want := Refreshed
			if result.Feed.URL == queued {
				want = NotDue
			}
			if result.Status != want || result.Err != nil {
				t.Errorf("%s = %+v, want %v", result.Feed.URL, result, want)
			}
		}
		if fetched[queued] != 0 {
			t.Errorf("queued feed saved elsewhere was still fetched")
		}
	})
}

func TestRefreshCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var feeds []config.Feed
		for i := range 3 {
			feeds = append(feeds, config.Feed{URL: fmt.Sprintf("https://shared.example/%d", i), Name: "shared"})
		}
		r, st, c := newReader(t, feeds, nil)
		var fetches atomic.Int32
		var blocking atomic.Bool
		blocking.Store(true)
		r.fetcher = fetchFunc(func(ctx context.Context, feedURL string, _ feed.Validators) (feed.Result, error) {
			fetches.Add(1)
			if blocking.Load() {
				<-ctx.Done()
				return feed.Result{}, fmt.Errorf("fetch %s: %w", feedURL, ctx.Err())
			}
			return served(feedURL, c), nil
		})
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan outcome, 1)
		go func() {
			report, err := r.Refresh(ctx)
			done <- outcome{report, err}
		}()
		synctest.Wait() // Two fetches hold the host's slots; the third waits for one.
		cancel()
		got := <-done
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("canceled round returned %v", got.err)
		}
		checkReport(t, got.report, feeds, Failing, Failing, Failing)
		for _, result := range got.report.Feeds {
			if !errors.Is(result.Err, context.Canceled) {
				t.Errorf("%s failed with %v, want cancellation", result.Feed.URL, result.Err)
			}
		}
		if n := fetches.Load(); n != hostSlots {
			t.Errorf("fetched %d times, want only the %d slot holders", n, hostSlots)
		}
		for _, f := range feeds {
			if state, err := st.FeedState(t.Context(), f.URL); err != nil || !state.FetchedAt.IsZero() {
				t.Errorf("canceled round saved %s: %+v, %v", f.URL, state, err)
			}
		}

		blocking.Store(false)
		checkReport(t, refresh(t, r), feeds, Refreshed, Refreshed, Refreshed)
	})
}

func TestRefreshGap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "openfeed.db")
	feeds := []config.Feed{{URL: "https://a.example/feed", Name: "alpha"}}
	r, _, c := newReaderAt(t, path, feeds, nil)
	var fetches atomic.Int32
	counting := fetchFunc(func(_ context.Context, feedURL string, _ feed.Validators) (feed.Result, error) {
		fetches.Add(1)
		return served(feedURL, c), nil
	})
	r.fetcher = counting
	checkReport(t, refresh(t, r), feeds, Refreshed)
	c.Advance(refreshGap - time.Nanosecond)
	checkReport(t, refresh(t, r), feeds, NotDue)

	other, _, otherClock := newReaderAt(t, path, feeds, counting)
	otherClock.now = c.Now()
	checkReport(t, refresh(t, other), feeds, NotDue)
	if n := fetches.Load(); n != 1 {
		t.Fatalf("fetched %d times within the gap", n)
	}

	c.Advance(time.Nanosecond)
	checkReport(t, refresh(t, r), feeds, Refreshed)
	c.Advance(-time.Hour) // A clock set backwards does not hold the feed until it catches up.
	checkReport(t, refresh(t, r), feeds, Refreshed)
	if n := fetches.Load(); n != 3 {
		t.Errorf("fetched %d times, want 3", n)
	}
}

func TestRefreshBackoff(t *testing.T) {
	feeds := []config.Feed{{URL: "https://a.example/feed", Name: "alpha"}}
	r, _, c := newReader(t, feeds, nil)
	var attempts atomic.Int32
	var failing atomic.Bool
	failing.Store(true)
	r.fetcher = fetchFunc(func(_ context.Context, feedURL string, _ feed.Validators) (feed.Result, error) {
		n := attempts.Add(1)
		if failing.Load() {
			return feed.Result{}, fmt.Errorf("attempt %d failed", n)
		}
		return served(feedURL, c), nil
	})
	expectAttempt := func(n int32) {
		t.Helper()
		report := refresh(t, r)
		checkReport(t, report, feeds, Failing)
		if got := attempts.Load(); got != n || report.Feeds[0].Err.Error() != fmt.Sprintf("attempt %d failed", n) {
			t.Fatalf("attempts = %d with error %v, want attempt %d", got, report.Feeds[0].Err, n)
		}
	}
	expectAttempt(1)
	for i, delay := range []time.Duration{15 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute,
		4 * time.Minute, 8 * time.Minute, 10 * time.Minute, 10 * time.Minute} {
		c.Advance(delay - time.Nanosecond)
		expectAttempt(int32(i + 1)) // Still backing off: reports the latest error without fetching.
		c.Advance(time.Nanosecond)
		expectAttempt(int32(i + 2))
	}

	failing.Store(false)
	c.Advance(maxBackoff)
	checkReport(t, refresh(t, r), feeds, Refreshed)
	failing.Store(true)
	c.Advance(refreshGap)
	before := attempts.Load()
	expectAttempt(before + 1)
	c.Advance(initialBackoff)
	expectAttempt(before + 2) // Success reset the backoff to its initial delay.
}

func TestRefreshBackoffResetsAfterSaveElsewhere(t *testing.T) {
	path := filepath.Join(t.TempDir(), "openfeed.db")
	feeds := []config.Feed{{URL: "https://a.example/feed", Name: "alpha"}}
	r, _, c := newReaderAt(t, path, feeds, nil)
	elsewhere := openStore(t, path)
	var attempts atomic.Int32
	r.fetcher = fetchFunc(func(context.Context, string, feed.Validators) (feed.Result, error) {
		return feed.Result{}, fmt.Errorf("attempt %d failed", attempts.Add(1))
	})
	for _, wait := range []time.Duration{0, 15 * time.Second, 30 * time.Second} {
		c.Advance(wait)
		checkReport(t, refresh(t, r), feeds, Failing)
	}
	c.Advance(time.Minute) // The fourth attempt would now wait a minute.
	state, err := elsewhere.FeedState(t.Context(), feeds[0].URL)
	if err == nil {
		err = elsewhere.SaveRefresh(t.Context(), state, store.Refresh{FetchedAt: c.Now()})
	}
	if err != nil {
		t.Fatal(err)
	}
	checkReport(t, refresh(t, r), feeds, NotDue)

	c.Advance(refreshGap)
	checkReport(t, refresh(t, r), feeds, Failing)
	c.Advance(initialBackoff)
	checkReport(t, refresh(t, r), feeds, Failing)
	if n := attempts.Load(); n != 5 {
		t.Errorf("attempts = %d; a success elsewhere should reset backoff to %s", n, initialBackoff)
	}
}

func TestRefreshConflictCountsAsRefreshed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "openfeed.db")
	feeds := []config.Feed{{URL: "https://a.example/feed", Name: "alpha"}}
	r, _, c := newReaderAt(t, path, feeds, nil)
	elsewhere := openStore(t, path)
	var fetches atomic.Int32
	r.fetcher = fetchFunc(func(ctx context.Context, feedURL string, _ feed.Validators) (feed.Result, error) {
		if fetches.Add(1) == 1 {
			state, err := elsewhere.FeedState(ctx, feedURL)
			if err == nil {
				err = elsewhere.SaveRefresh(ctx, state, store.Refresh{FetchedAt: c.Now(), Items: []store.RefreshItem{
					{Key: "id:elsewhere", Title: "elsewhere"},
				}})
			}
			if err != nil {
				return feed.Result{}, err
			}
		}
		return feed.Result{Items: []feed.Item{post("mine", c.Now())}, FetchedAt: c.Now()}, nil
	})
	checkReport(t, refresh(t, r), feeds, Refreshed)
	if got := titles(river(t, r)); !slices.Equal(got, []string{"elsewhere@alpha"}) {
		t.Errorf("stale response replaced another process's save: %v", got)
	}
	if n := fetches.Load(); n != 1 {
		t.Errorf("conflict was refetched: %d fetches", n)
	}
	c.Advance(refreshGap)
	checkReport(t, refresh(t, r), feeds, Refreshed) // No backoff was recorded.
}

func TestRefreshSavesFetchedOutcomes(t *testing.T) {
	feeds := []config.Feed{{URL: "https://a.example/feed", Name: "alpha"}}
	r, st, c := newReader(t, feeds, nil)
	first, second := post("dup", c.Now()), post("dup", c.Now())
	second.Title = "second"
	var unchanged atomic.Bool
	r.fetcher = fetchFunc(func(_ context.Context, _ string, previous feed.Validators) (feed.Result, error) {
		if unchanged.Load() {
			return feed.Result{Validators: previous, FetchedAt: c.Now(), NotModified: true}, nil
		}
		return feed.Result{
			Items:      []feed.Item{first, second},
			Validators: feed.Validators{ETag: `"e"`, LastModified: "Tue, 02 Sep 2025 13:00:00 GMT"},
			FetchedAt:  c.Now(),
		}, nil
	})
	checkReport(t, refresh(t, r), feeds, Refreshed)
	if got := titles(river(t, r)); !slices.Equal(got, []string{"dup@alpha"}) {
		t.Fatalf("source order was not preserved for the first-occurrence rule: %v", got)
	}

	unchanged.Store(true)
	c.Advance(refreshGap)
	checkReport(t, refresh(t, r), feeds, Refreshed)
	state, err := st.FeedState(t.Context(), feeds[0].URL)
	if err != nil || state.ETag != `"e"` || state.LastModified != "Tue, 02 Sep 2025 13:00:00 GMT" || !state.FetchedAt.Equal(c.Now()) {
		t.Errorf("unchanged outcome saved %+v, %v", state, err)
	}
}
