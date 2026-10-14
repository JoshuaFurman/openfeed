package reader

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
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

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type fetchFunc func(ctx context.Context, feedURL string, previous feed.Validators) (feed.Result, error)

func (f fetchFunc) Fetch(ctx context.Context, feedURL string, previous feed.Validators) (feed.Result, error) {
	return f(ctx, feedURL, previous)
}

func openStore(t *testing.T, path string) *store.Store {
	t.Helper()
	st, err := store.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	})
	return st
}

func newReaderAt(t *testing.T, path string, feeds []config.Feed, fetcher Fetcher) (*Reader, *store.Store, *clock) {
	t.Helper()
	st := openStore(t, path)
	c := &clock{now: time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)}
	r := New(feeds, nil, st, fetcher, nil)
	r.now = c.Now
	return r, st, c
}

func newReader(t *testing.T, feeds []config.Feed, fetcher Fetcher) (*Reader, *store.Store, *clock) {
	t.Helper()
	return newReaderAt(t, filepath.Join(t.TempDir(), "openfeed.db"), feeds, fetcher)
}

func post(key string, published time.Time) feed.Item {
	return feed.Item{Key: "id:" + key, Title: key, URL: "https://example.com/" + key, PublishedAt: published}
}

// servePosts fetches the same posts for every URL, stamped with the reader's clock.
func servePosts(c *clock, posts map[string][]feed.Item) fetchFunc {
	return func(_ context.Context, feedURL string, _ feed.Validators) (feed.Result, error) {
		return feed.Result{Items: posts[feedURL], FetchedAt: c.Now()}, nil
	}
}

func refresh(t *testing.T, r *Reader) Report {
	t.Helper()
	report, err := r.Refresh(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func statuses(report Report) []Status {
	var got []Status
	for _, result := range report.Feeds {
		got = append(got, result.Status)
	}
	return got
}

func river(t *testing.T, r *Reader) []Item {
	t.Helper()
	var items []Item
	query := Query{Limit: 1}
	for range 1000 {
		page, err := r.Timeline(t.Context(), query)
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, page.Items...)
		if page.Next == nil {
			return items
		}
		query.Before = page.Next
	}
	t.Fatal("river did not end")
	return nil
}

func titles(items []Item) []string {
	var got []string
	for _, item := range items {
		got = append(got, item.Title+"@"+item.FeedName)
	}
	return got
}

type transport func(*http.Request) (*http.Response, error)

func (f transport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestRefreshEndToEnd(t *testing.T) {
	const rss = `<?xml version="1.0"?><rss version="2.0"><channel><title>A</title>
		<item><guid>a-1</guid><title>A one</title><link>https://a.example/1</link><pubDate>Mon, 01 Sep 2025 10:00:00 GMT</pubDate></item>
		<item><guid>a-2</guid><title>A two</title><link>https://a.example/2</link><pubDate>Wed, 03 Sep 2025 10:00:00 GMT</pubDate></item>
		</channel></rss>`
	const atom = `<?xml version="1.0" encoding="utf-8"?><feed xmlns="http://www.w3.org/2005/Atom">
		<title>B</title><id>urn:b</id><updated>2025-09-02T10:00:00Z</updated>
		<entry><id>urn:b:1</id><title>B one</title><link href="https://b.example/1"/>
		<published>2025-09-02T10:00:00Z</published><updated>2025-09-02T10:00:00Z</updated></entry>
		</feed>`
	bodies := map[string]string{"https://a.example/feed": rss, "https://b.example/feed": atom}
	var mu sync.Mutex
	var conditional []string
	client := feed.NewClient(&http.Client{Transport: transport(func(req *http.Request) (*http.Response, error) {
		body, ok := bodies[req.URL.String()]
		if !ok {
			return nil, errors.New("no route")
		}
		header := http.Header{"Etag": {`"v1"`}}
		if req.Header.Get("If-None-Match") == `"v1"` {
			mu.Lock()
			conditional = append(conditional, req.URL.String())
			mu.Unlock()
			return &http.Response{StatusCode: http.StatusNotModified, Header: header, Body: http.NoBody, Request: req}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})})
	feeds := []config.Feed{{URL: "https://a.example/feed", Name: "alpha"}, {URL: "https://b.example/feed", Name: "beta"}}
	r, st, c := newReader(t, feeds, client)
	c.now = time.Now() // feed.Client stamps real request times.

	checkReport(t, refresh(t, r), feeds, Refreshed, Refreshed)
	first := river(t, r)
	if got := titles(first); !slices.Equal(got, []string{"A two@alpha", "B one@beta", "A one@alpha"}) {
		t.Fatalf("river = %v", got)
	}

	c.Advance(refreshGap + time.Second)
	checkReport(t, refresh(t, r), feeds, Refreshed, Refreshed)
	slices.Sort(conditional)
	if !slices.Equal(conditional, []string{"https://a.example/feed", "https://b.example/feed"}) {
		t.Errorf("second round did not send saved validators: %v", conditional)
	}
	if got := river(t, r); !reflect.DeepEqual(got, first) {
		t.Error("unchanged feeds changed the river")
	}
	if state, err := st.FeedState(t.Context(), feeds[0].URL); err != nil || state.ETag != `"v1"` {
		t.Errorf("validators not saved: %+v, %v", state, err)
	}
}

func TestTimelineLabelsAndSelection(t *testing.T) {
	at := time.Date(2025, 9, 1, 0, 0, 0, 0, time.UTC)
	feeds := []config.Feed{{URL: "https://a.example/feed", Name: "alpha"}, {URL: "https://b.example/feed", Name: "beta"}}
	r, st, c := newReader(t, feeds, nil)
	r.fetcher = servePosts(c, map[string][]feed.Item{
		feeds[0].URL: {post("a-new", at.Add(3*time.Hour)), post("a-old", at.Add(time.Hour))},
		feeds[1].URL: {post("b-mid", at.Add(2*time.Hour))},
	})
	const removed = "https://removed.example/feed"
	state, err := st.FeedState(t.Context(), removed)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveRefresh(t.Context(), state, store.Refresh{FetchedAt: c.Now(), Items: []store.RefreshItem{
		{Key: "id:removed", Title: "removed", PublishedAt: at.Add(10 * time.Hour)},
	}}); err != nil {
		t.Fatal(err)
	}
	refresh(t, r)
	all := river(t, r)
	if got := titles(all); !slices.Equal(got, []string{"a-new@alpha", "b-mid@beta", "a-old@alpha"}) {
		t.Errorf("river = %v, want configured feeds only, labeled", got)
	}
	var upward []Item
	query := Query{After: &all[2].Position, Limit: 1}
	for pages := 0; ; pages++ {
		page, err := r.Timeline(t.Context(), query)
		if err != nil {
			t.Fatal(err)
		}
		if page.Next != nil || pages > len(all) {
			t.Fatalf("upward read returned an older continuation or did not end: %+v", page)
		}
		upward = append(slices.Clone(page.Items), upward...)
		if page.Newer == nil {
			break
		}
		query.After = page.Newer
	}
	if got := titles(upward); !slices.Equal(got, []string{"a-new@alpha", "b-mid@beta"}) {
		t.Errorf("upward pages = %v, want labeled posts above the oldest", got)
	}

	listed := r.Feeds()
	listed[0].Name = "changed"
	if got := r.Feeds(); !reflect.DeepEqual(got, feeds) {
		t.Errorf("Feeds = %v, want the configured feeds unchanged", got)
	}

	empty := New(nil, nil, st, fetchFunc(func(context.Context, string, feed.Validators) (feed.Result, error) {
		t.Error("empty config fetched")
		return feed.Result{}, nil
	}), nil)
	if report, err := empty.Refresh(t.Context()); err != nil || len(report.Feeds) != 0 {
		t.Errorf("empty refresh = %+v, %v", report, err)
	}
	if page, err := empty.Timeline(t.Context(), Query{}); err != nil || len(page.Items) != 0 || page.Next != nil {
		t.Errorf("empty river = %+v, %v", page, err)
	}
}

// tagged reads every post carrying tag, a page at a time, older and then back up.
func tagged(t *testing.T, r *Reader, tag string) (down, up []string) {
	t.Helper()
	var items []Item
	for query := (Query{Tag: tag, Limit: 1}); len(items) <= 100; {
		page, err := r.Timeline(t.Context(), query)
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, page.Items...)
		if page.Next == nil {
			break
		}
		query.Before = page.Next
	}
	if len(items) < 2 {
		return titles(items), titles(items)
	}
	upward := items[len(items)-1:]
	for query := (Query{Tag: tag, After: &upward[0].Position, Limit: 1}); len(upward) <= 100; {
		page, err := r.Timeline(t.Context(), query)
		if err != nil {
			t.Fatal(err)
		}
		upward = append(slices.Clone(page.Items), upward...)
		if page.Newer == nil {
			break
		}
		query.After = page.Newer
	}
	return titles(items), titles(upward)
}

func TestTagFilters(t *testing.T) {
	at := time.Date(2025, 9, 1, 0, 0, 0, 0, time.UTC)
	feeds := []config.Feed{
		{URL: "https://a.example/feed", Name: "alpha", Tags: []string{"Tech", "news", "TECH"}},
		{URL: "https://b.example/feed", Name: "beta", Tags: []string{" tech ", " "}},
		{URL: "https://c.example/feed", Name: "gamma", Tags: []string{"friends"}},
		{URL: "https://d.example/feed", Name: "delta"},
	}
	r, _, c := newReader(t, feeds, nil)
	fetch, _ := countingFetches(c, map[string][]feed.Item{
		feeds[0].URL:           {post("a-new", at.Add(5*time.Hour)), post("a-old", at.Add(time.Hour))},
		feeds[1].URL:           {post("b-mid", at.Add(3*time.Hour))},
		feeds[2].URL:           {post("c", at.Add(4*time.Hour))},
		feeds[3].URL:           {post("d", at.Add(2*time.Hour))},
		"https://e.example/rx": {post("e", at.Add(6*time.Hour))},
	})
	r.fetcher = fetch
	r.subs = &memorySubs{}
	refresh(t, r)

	listed := r.Tags()
	if !slices.Equal(listed, []string{"Tech", "news", "friends"}) {
		t.Errorf("Tags = %q, want each once, in config order, as first spelled", listed)
	}
	listed[0] = "changed"
	if got := r.Tags(); got[0] != "Tech" {
		t.Errorf("Tags = %q after modifying a copy", got)
	}

	tech := []string{"a-new@alpha", "b-mid@beta", "a-old@alpha"}
	for _, test := range []struct {
		tag  string
		want []string
	}{
		{"tech", tech},
		{" TECH ", tech},
		{"news", []string{"a-new@alpha", "a-old@alpha"}},
		{"friends", []string{"c@gamma"}},
		{"missing", nil},
		{" ", nil},
	} {
		down, up := tagged(t, r, test.tag)
		if !slices.Equal(down, test.want) || !slices.Equal(up, test.want) {
			t.Errorf("tag %q: older pages %v, newer pages %v; want %v", test.tag, down, up, test.want)
		}
	}
	all := river(t, r)
	if got := titles(all); len(got) != 5 {
		t.Errorf("unfiltered river = %v, want every configured feed", got)
	}
	labels := map[string][]string{}
	for _, item := range all {
		labels[item.FeedName] = item.FeedTags
	}
	want := map[string][]string{"alpha": {"Tech", "news"}, "beta": {"Tech"}, "gamma": {"friends"}, "delta": nil}
	if !reflect.DeepEqual(labels, want) {
		t.Errorf("item tags = %q, want each feed's once, spelled as Tags lists them", labels)
	}
	all[0].FeedTags[0] = "changed"
	feeds[0].Tags[0] = "changed" // The configuration New was given.
	configured := r.Feeds()
	if !slices.Equal(configured[0].Tags, []string{"Tech", "news"}) || configured[3].Tags != nil {
		t.Errorf("Feeds tags = %q and %q, want them as items carry them", configured[0].Tags, configured[3].Tags)
	}
	configured[0].Tags[0] = "changed"
	if got := titles(river(t, r)); got[0] != "a-new@alpha" || r.Feeds()[0].Tags[0] != "Tech" {
		t.Errorf("modifying results changed the reader: %v, %q", got, r.Feeds()[0].Tags)
	}
	for _, item := range river(t, r) {
		if item.FeedName == "alpha" && item.FeedTags[0] != "Tech" {
			t.Errorf("modifying an item's tags changed the next read: %q", item.FeedTags)
		}
	}

	if _, err := r.Subscribe(t.Context(), config.Feed{URL: "https://e.example/rx", Name: "epsilon", Tags: []string{"tech", "Video"}}); err != nil {
		t.Fatal(err)
	}
	if got := r.Tags(); !slices.Equal(got, []string{"Tech", "news", "friends", "Video"}) {
		t.Errorf("Tags after subscribing = %q, want the new tag last", got)
	}
	if down, _ := tagged(t, r, "tech"); !slices.Equal(down, append([]string{"e@epsilon"}, tech...)) {
		t.Errorf("tech after subscribing = %v, want the new feed's post", down)
	}
}

func TestSeenBookkeeping(t *testing.T) {
	path := filepath.Join(t.TempDir(), "openfeed.db")
	at := time.Date(2025, 9, 1, 0, 0, 0, 0, time.UTC)
	feeds := []config.Feed{{URL: "https://a.example/feed", Name: "alpha"}}
	r, st, c := newReaderAt(t, path, feeds, nil)
	var logs bytes.Buffer
	r.log = slog.New(slog.NewTextHandler(&logs, nil))
	r.fetcher = servePosts(c, map[string][]feed.Item{
		feeds[0].URL: {post("newest", at.Add(3*time.Hour)), post("middle", at.Add(2*time.Hour)), post("oldest", at.Add(time.Hour))},
	})
	refresh(t, r)
	items := river(t, r)
	newest, middle, oldest := items[0].Position, items[1].Position, items[2].Position
	seen := func() *Position {
		t.Helper()
		position, err := r.SeenPosition(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return position
	}

	if err := r.SaveSeen(t.Context()); err != nil || seen() != nil {
		t.Fatalf("saving with nothing reached wrote %+v, %v", seen(), err)
	}
	r.Reached(Position{})
	if err := r.SaveSeen(t.Context()); err != nil || seen() != nil {
		t.Fatalf("zero position was recorded: %+v, %v", seen(), err)
	}
	r.Reached(middle)
	r.Reached(oldest)
	if err := r.SaveSeen(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := seen(); got == nil || *got != middle {
		t.Fatalf("saved %+v, want furthest reached %+v", got, middle)
	}

	r.Reached(newest)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := r.SaveSeen(ctx); !errors.Is(err, context.Canceled) || logs.Len() != 0 {
		t.Fatalf("canceled save returned %v and logged %q", err, logs.String())
	}
	if got := seen(); got == nil || *got != middle {
		t.Fatalf("failed save changed seen position to %+v", got)
	}
	if err := r.SaveSeen(t.Context()); err != nil {
		t.Fatal(err)
	}
	other, _, _ := newReaderAt(t, path, feeds, nil)
	if got, err := other.SeenPosition(t.Context()); err != nil || got == nil || *got != newest {
		t.Errorf("retried save not visible to another reader: %+v, %v", got, err)
	}

	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	closed := New(feeds, nil, st, nil, r.log)
	closed.Reached(middle)
	if err := closed.SaveSeen(t.Context()); err == nil || !strings.Contains(logs.String(), "save seen position") {
		t.Errorf("store failure returned %v and logged %q", err, logs.String())
	}
}
