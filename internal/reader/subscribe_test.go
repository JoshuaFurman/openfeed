package reader

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JoshuaFurman/openfeed/internal/config"
	"github.com/JoshuaFurman/openfeed/internal/feed"
	"github.com/JoshuaFurman/openfeed/internal/store"
)

// memorySubs records added feeds, resolved as config.Load would, or fails with err.
type memorySubs struct {
	mu    sync.Mutex
	added []config.Feed
	err   error
}

func (m *memorySubs) Add(f config.Feed) (config.Feed, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return config.Feed{}, m.err
	}
	if err := f.Normalize(); err != nil {
		return config.Feed{}, err
	}
	m.added = append(m.added, f)
	return f, nil
}

// countingFetches serves posts like servePosts and counts fetches per URL.
func countingFetches(c *clock, posts map[string][]feed.Item) (fetchFunc, func(string) int) {
	var mu sync.Mutex
	calls := map[string]int{}
	serve := servePosts(c, posts)
	return func(ctx context.Context, feedURL string, previous feed.Validators) (feed.Result, error) {
			mu.Lock()
			calls[feedURL]++
			mu.Unlock()
			return serve(ctx, feedURL, previous)
		}, func(feedURL string) int {
			mu.Lock()
			defer mu.Unlock()
			return calls[feedURL]
		}
}

func TestSubscribe(t *testing.T) {
	at := time.Date(2025, 9, 1, 0, 0, 0, 0, time.UTC)
	alpha := config.Feed{URL: "https://a.example/feed", Name: "alpha"}
	r, _, c := newReader(t, []config.Feed{alpha}, nil)
	subs := &memorySubs{}
	r.subs = subs
	fetch, calls := countingFetches(c, map[string][]feed.Item{
		alpha.URL:               {post("a-old", at.Add(time.Hour))},
		"https://b.example/rss": {post("b-new", at.Add(2*time.Hour))},
	})
	r.fetcher = fetch
	refresh(t, r)

	added, err := r.Subscribe(t.Context(), config.Feed{URL: " https://B.example/rss ", Tags: []string{"tech"}})
	if err != nil {
		t.Fatal(err)
	}
	beta := config.Feed{URL: "https://b.example/rss", Name: "b.example", Tags: []string{"tech"}}
	if !reflect.DeepEqual(added, beta) || !reflect.DeepEqual(subs.added, []config.Feed{beta}) {
		t.Errorf("Subscribe = %+v, recorded %+v; want %+v", added, subs.added, beta)
	}
	if got := r.Feeds(); !reflect.DeepEqual(got, []config.Feed{alpha, beta}) {
		t.Errorf("Feeds = %+v, want the new feed last, in config order", got)
	}
	if got := titles(river(t, r)); !slices.Equal(got, []string{"b-new@b.example", "a-old@alpha"}) {
		t.Errorf("river = %v, want the new feed's posts without a refresh", got)
	}

	// The subscription's fetch counts as its refresh, so the next round does not repeat it.
	report := refresh(t, r)
	if got := statuses(report); !slices.Equal(got, []Status{NotDue, NotDue}) || report.Feeds[1].Feed.URL != beta.URL {
		t.Errorf("round after subscribing = %+v", report)
	}
	if calls(beta.URL) != 1 {
		t.Errorf("new feed fetched %d times, want once", calls(beta.URL))
	}
}

func TestSubscribeRejectsWithoutChanges(t *testing.T) {
	alpha := config.Feed{URL: "https://a.example/feed", Name: "alpha"}
	tests := []struct {
		name    string
		feed    config.Feed
		fetch   error
		add     error
		wantErr string
		fetches int
	}{
		{name: "invalid url", feed: config.Feed{URL: "ftp://b.example/rss"}, wantErr: "scheme must be http or https"},
		{name: "already subscribed", feed: config.Feed{URL: "https://A.example/feed"}, wantErr: "already subscribed to https://a.example/feed"},
		{name: "fetch fails", feed: config.Feed{URL: "https://b.example/rss"}, fetch: errors.New("parse feed: not a feed"), wantErr: "not a feed", fetches: 1},
		{name: "config rejects", feed: config.Feed{URL: "https://b.example/rss"}, add: errors.New("config.toml: feed 2 duplicates feed 1"), wantErr: "duplicates", fetches: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, st, c := newReader(t, []config.Feed{alpha}, nil)
			subs := &memorySubs{err: tc.add}
			r.subs = subs
			fetches := 0
			r.fetcher = fetchFunc(func(context.Context, string, feed.Validators) (feed.Result, error) {
				fetches++
				return feed.Result{Items: []feed.Item{post("b", time.Time{})}, FetchedAt: c.Now()}, tc.fetch
			})

			if _, err := r.Subscribe(t.Context(), tc.feed); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Subscribe error = %v, want it to contain %q", err, tc.wantErr)
			}
			if fetches != tc.fetches {
				t.Errorf("fetched %d times, want %d", fetches, tc.fetches)
			}
			if got := r.Feeds(); !reflect.DeepEqual(got, []config.Feed{alpha}) || len(subs.added) != 0 {
				t.Errorf("Feeds = %+v and recorded %+v, want no change", got, subs.added)
			}
			if state, err := st.FeedState(t.Context(), "https://b.example/rss"); err != nil || !state.FetchedAt.IsZero() {
				t.Errorf("rejected feed was cached: %+v, %v", state, err)
			}
		})
	}
}

// A feed removed from config keeps its cache; subscribing again revalidates it rather than refetching.
func TestSubscribeRevivesAHiddenCache(t *testing.T) {
	const url = "https://b.example/rss"
	r, st, c := newReader(t, nil, nil)
	r.subs = &memorySubs{}
	state, err := st.FeedState(t.Context(), url)
	if err == nil {
		err = st.SaveRefresh(t.Context(), state, store.Refresh{ETag: `"v1"`, FetchedAt: c.Now(), Items: []store.RefreshItem{
			{Key: "id:kept", Title: "kept", PublishedAt: c.Now().Add(-time.Hour)},
		}})
	}
	if err != nil {
		t.Fatal(err)
	}
	c.Advance(time.Hour)
	r.fetcher = fetchFunc(func(_ context.Context, _ string, previous feed.Validators) (feed.Result, error) {
		if previous.ETag != `"v1"` {
			t.Errorf("subscribing sent validators %+v, want the cached ones", previous)
		}
		return feed.Result{NotModified: true, Validators: previous, FetchedAt: c.Now()}, nil
	})

	if _, err := r.Subscribe(t.Context(), config.Feed{URL: url, Name: "bee"}); err != nil {
		t.Fatal(err)
	}
	if got := titles(river(t, r)); !slices.Equal(got, []string{"kept@bee"}) {
		t.Errorf("river = %v, want the cached post under its new name", got)
	}
	if state, err := st.FeedState(t.Context(), url); err != nil || !state.FetchedAt.Equal(c.Now()) {
		t.Errorf("unchanged outcome not saved: %+v, %v", state, err)
	}
}

// Once the configuration has the feed, failing to save its posts does not undo the subscription.
func TestSubscribeStandsWhenSavingFails(t *testing.T) {
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "openfeed.db"))
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	subs := &memorySubs{}
	r := New(nil, subs, st, fetchFunc(func(context.Context, string, feed.Validators) (feed.Result, error) {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
		return feed.Result{FetchedAt: time.Now()}, nil
	}), slog.New(slog.NewTextHandler(&logs, nil)))

	if _, err := r.Subscribe(t.Context(), config.Feed{URL: "https://b.example/rss"}); err != nil {
		t.Fatal(err)
	}
	if len(r.Feeds()) != 1 || len(subs.added) != 1 {
		t.Errorf("Feeds = %+v, recorded %+v; want the subscription kept", r.Feeds(), subs.added)
	}
	if !strings.Contains(logs.String(), "save new subscription") {
		t.Errorf("save failure not logged: %q", logs.String())
	}
}

func TestConcurrentSubscribesAddOnce(t *testing.T) {
	r, _, c := newReader(t, nil, nil)
	subs := &memorySubs{}
	r.subs = subs
	var fetched sync.WaitGroup
	fetched.Add(2)
	r.fetcher = fetchFunc(func(context.Context, string, feed.Validators) (feed.Result, error) {
		fetched.Done()
		fetched.Wait() // Both calls pass the first duplicate check before either adds.
		return feed.Result{Items: []feed.Item{post("b", time.Time{})}, FetchedAt: c.Now()}, nil
	})

	errs := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := r.Subscribe(t.Context(), config.Feed{URL: "https://b.example/rss"})
			errs <- err
		}()
	}
	var failures []error
	for range 2 {
		if err := <-errs; err != nil {
			failures = append(failures, err)
		}
		river(t, r) // Reads run safely alongside the set swap.
	}
	if len(failures) != 1 || !strings.Contains(failures[0].Error(), "already subscribed") {
		t.Errorf("failures = %v, want one duplicate", failures)
	}
	if len(subs.added) != 1 || len(r.Feeds()) != 1 {
		t.Errorf("recorded %+v, Feeds = %+v; want the feed once", subs.added, r.Feeds())
	}
}
