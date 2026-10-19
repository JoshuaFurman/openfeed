package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JoshuaFurman/openfeed/internal/config"
	"github.com/JoshuaFurman/openfeed/internal/feed"
	"github.com/JoshuaFurman/openfeed/internal/reader"
	"github.com/JoshuaFurman/openfeed/internal/store"
)

// fakeFeeds serves each URL's current posts, or fails URLs listed in failing.
type fakeFeeds struct {
	mu      sync.Mutex
	posts   map[string][]feed.Item
	failing map[string]bool
}

func (f *fakeFeeds) Fetch(_ context.Context, feedURL string, _ feed.Validators) (feed.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failing[feedURL] {
		return feed.Result{}, errors.New("connection refused")
	}
	return feed.Result{Items: slices.Clone(f.posts[feedURL]), FetchedAt: time.Now().UTC()}, nil
}

const (
	alpha = "https://alpha.example/feed"
	beta  = "https://beta.example/feed"
)

var base = time.Date(2025, 9, 1, 12, 0, 0, 0, time.UTC)

func entry(key string, hoursAfterBase int) feed.Item {
	return feed.Item{
		Key: "id:" + key, Title: key, URL: "https://example.com/" + key,
		Content: "<p>About " + key + "</p>", PublishedAt: base.Add(time.Duration(hoursAfterBase) * time.Hour),
	}
}

type fixture struct {
	river   *reader.Reader
	store   *store.Store
	feeds   *fakeFeeds
	handler http.Handler
}

func newFixture(t *testing.T, feeds []config.Feed, posts map[string][]feed.Item) *fixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "openfeed.db")
	st, err := store.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	fake := &fakeFeeds{posts: posts, failing: map[string]bool{}}
	river := reader.New(feeds, nil, st, fake, nil)
	return &fixture{river: river, store: st, feeds: fake, handler: New(river, nil)}
}

func defaultFeeds() []config.Feed {
	return []config.Feed{
		{URL: alpha, Name: "Alpha", Tags: []string{"tech", "go"}},
		{URL: beta, Name: "Beta"},
	}
}

func (f *fixture) do(t *testing.T, method, target string, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, target, nil)
	for k, v := range header {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func (f *fixture) get(t *testing.T, target string) string {
	t.Helper()
	rec := f.do(t, http.MethodGet, target, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", target, rec.Code, rec.Body)
	}
	return rec.Body.String()
}

// refresh posts the page's launch refresh, sent same-origin as htmx would.
func (f *fixture) refresh(t *testing.T, target string) string {
	t.Helper()
	rec := f.do(t, http.MethodPost, target, http.Header{"Sec-Fetch-Site": {"same-origin"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST %s = %d: %s", target, rec.Code, rec.Body)
	}
	return rec.Body.String()
}

var (
	titlePattern = regexp.MustCompile(`<h2 class="title">([^<]*)</h2>`)
	olderPattern = regexp.MustCompile(`hx-get="(/older\?before=[^"]+)"`)
	headPattern  = regexp.MustCompile(`hx-post="(/refresh[^"]*)"`)
)

func titles(body string) []string {
	var out []string
	for _, m := range titlePattern.FindAllStringSubmatch(body, -1) {
		out = append(out, m[1])
	}
	return out
}

func older(body string) string {
	m := olderPattern.FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return strings.ReplaceAll(m[1], "&#43;", "+")
}

func refreshTarget(t *testing.T, body string) string {
	t.Helper()
	m := headPattern.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("page has no launch refresh:\n%s", body)
	}
	return strings.ReplaceAll(m[1], "&#43;", "+")
}

func populate(t *testing.T, f *fixture) {
	t.Helper()
	if _, err := f.river.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestPageShowsTheRiver(t *testing.T) {
	f := newFixture(t, defaultFeeds(), map[string][]feed.Item{
		alpha: {entry("a-new", 3), {Key: "id:untitled", URL: "https://example.com/untitled", PublishedAt: base.Add(2 * time.Hour)}},
		beta:  {entry("b-old", 1), {Key: "id:bare", Title: "Same", Content: "Same", PublishedAt: base}},
	})
	populate(t, f)
	body := f.get(t, "/")
	if got, want := titles(body), []string{"a-new", "(untitled)", "b-old", "Same"}; !slices.Equal(got, want) {
		t.Errorf("titles = %v, want %v", got, want)
	}
	for _, want := range []string{
		`<span class="feed">Alpha</span> <span class="tag tag-0">tech</span> <span class="tag tag-1">go</span>`,
		`<p class="excerpt">About a-new</p>`,
		`href="https://example.com/a-new" target="_blank" rel="noopener noreferrer"`,
		`<time datetime="` + base.Add(3*time.Hour).Format(time.RFC3339) + `">`,
		`<script src="/assets/htmx.min.js?v=`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(body, `<p class="excerpt">Same</p>`) {
		t.Error("an excerpt repeating its title is shown")
	}
	if strings.Count(body, `class="hit"`) != 3 {
		t.Error("the post without a URL has a link, or another post lacks one")
	}
	if older(body) != "" {
		t.Error("a short river offers older posts")
	}
}

func TestPageEscapesFeedText(t *testing.T) {
	f := newFixture(t, []config.Feed{{URL: alpha, Name: `<b>Alpha</b>`, Tags: []string{`"><script>`}}}, map[string][]feed.Item{
		alpha: {
			{Key: "id:x", Title: `<img src=x onerror=alert(1)>`, URL: "https://example.com/?a=1&b=\"2\"", Content: `<script>alert(1)</script>&lt;b&gt;`, PublishedAt: base},
			{Key: "id:js", Title: "javascript link", URL: "https://example.com/js", PublishedAt: base.Add(-time.Hour)},
		},
	})
	populate(t, f)
	body := f.get(t, "/")
	for _, bad := range []string{"<img src=x", "<script>alert", "<b>Alpha", `"><script>`} {
		if strings.Contains(body, bad) {
			t.Errorf("page contains unescaped %q", bad)
		}
	}
	for _, want := range []string{"&lt;img src=x onerror=alert(1)&gt;", "&lt;b&gt;Alpha&lt;/b&gt;", `<p class="excerpt">&lt;b&gt;</p>`, `href="https://example.com/?a=1&amp;b=%222%22"`} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks escaped %q", want)
		}
	}
}

func TestOlderPagesReachTheEnd(t *testing.T) {
	var posts []feed.Item
	for i := range 120 {
		posts = append(posts, entry(fmt.Sprintf("p%d", i), i))
	}
	f := newFixture(t, defaultFeeds(), map[string][]feed.Item{alpha: posts})
	populate(t, f)
	body := f.get(t, "/")
	seen := titles(body)
	for next := older(body); next != ""; next = older(body) {
		body = f.get(t, next)
		if strings.Contains(body, "<html") {
			t.Fatal("an older page renders the whole document")
		}
		seen = append(seen, titles(body)...)
	}
	if len(seen) != 120 || seen[0] != "p119" || seen[119] != "p0" {
		t.Errorf("paged through %d posts, from %v to %v", len(seen), seen[0], seen[len(seen)-1])
	}
	if !strings.Contains(f.get(t, "/"), `hx-trigger="intersect once"`) {
		t.Error("older posts do not load when scrolled to")
	}
}

func TestOlderRejectsBadPositions(t *testing.T) {
	f := newFixture(t, defaultFeeds(), nil)
	for _, target := range []string{"/older", "/older?before=", "/older?before=nonsense", "/older?before=2025-09-01T12:00:00.000000000Z~0"} {
		if rec := f.do(t, http.MethodGet, target, nil); rec.Code != http.StatusBadRequest {
			t.Errorf("GET %s = %d", target, rec.Code)
		}
	}
}

func TestRefreshOffersNewerPosts(t *testing.T) {
	f := newFixture(t, defaultFeeds(), map[string][]feed.Item{alpha: {entry("first", 1)}})
	populate(t, f)
	body := f.get(t, "/")
	target := refreshTarget(t, body)
	if notice := f.refresh(t, target); strings.Contains(notice, "pill") {
		t.Errorf("nothing new, but the notice offers newer posts: %s", notice)
	}

	// Another process saves a newer post; this reader's own round finds the feed not due.
	state, err := f.store.FeedState(t.Context(), alpha)
	if err != nil {
		t.Fatal(err)
	}
	second := entry("second", 2)
	if err := f.store.SaveRefresh(t.Context(), state, store.Refresh{
		Items:     []store.RefreshItem{{Key: second.Key, Title: second.Title, URL: second.URL, PublishedAt: second.PublishedAt}},
		FetchedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	notice := f.refresh(t, target)
	if !strings.Contains(notice, `<a class="pill" href="/">New posts ↑</a>`) {
		t.Errorf("a newer post exists, but the notice is %s", notice)
	}
	if got := titles(f.get(t, "/")); !slices.Equal(got, []string{"second", "first"}) {
		t.Errorf("after the pill, titles = %v", got)
	}
}

func TestRefreshNamesFailingFeeds(t *testing.T) {
	f := newFixture(t, defaultFeeds(), map[string][]feed.Item{})
	f.feeds.failing[beta] = true
	if notice := f.refresh(t, "/refresh"); !strings.Contains(notice, "Beta is failing to refresh") {
		t.Errorf("notice = %s", notice)
	}
	f = newFixture(t, defaultFeeds(), map[string][]feed.Item{})
	f.feeds.failing[alpha], f.feeds.failing[beta] = true, true
	if notice := f.refresh(t, "/refresh"); !strings.Contains(notice, "2 feeds failing: Alpha, Beta") {
		t.Errorf("notice = %s", notice)
	}
}

func TestRefreshReachesTheShownHead(t *testing.T) {
	f := newFixture(t, defaultFeeds(), map[string][]feed.Item{alpha: {entry("old", 1), entry("new", 2)}})
	populate(t, f)
	body := f.get(t, "/")
	if seen, err := f.store.SeenPosition(t.Context()); err != nil || seen != nil {
		t.Fatalf("rendering saved a position: %v, %v", seen, err)
	}
	f.refresh(t, refreshTarget(t, body))
	seen, err := f.store.SeenPosition(t.Context())
	if err != nil || seen == nil {
		t.Fatalf("seen position = %v, %v", seen, err)
	}
	head, _ := f.river.Timeline(t.Context(), reader.Query{Limit: 1})
	if *seen != head.Items[0].Position {
		t.Errorf("saved %+v, want the newest post %+v", *seen, head.Items[0].Position)
	}
}

func TestRefreshCapsAForgedHead(t *testing.T) {
	f := newFixture(t, defaultFeeds(), map[string][]feed.Item{alpha: {entry("only", 1)}})
	populate(t, f)
	forged := "2999-01-01T00:00:00.000000000Z~999"
	f.refresh(t, "/refresh?head="+url.QueryEscape(forged))
	seen, _ := f.store.SeenPosition(t.Context())
	head, _ := f.river.Timeline(t.Context(), reader.Query{Limit: 1})
	if seen == nil || *seen != head.Items[0].Position {
		t.Errorf("a forged head saved %+v, want the real head", seen)
	}
	if rec := f.do(t, http.MethodPost, "/refresh?head=junk", http.Header{"Sec-Fetch-Site": {"same-origin"}}); rec.Code != http.StatusBadRequest {
		t.Errorf("a malformed head = %d", rec.Code)
	}
}

func TestEmptyStates(t *testing.T) {
	if body := newFixture(t, nil, nil).get(t, "/"); !strings.Contains(body, "No subscriptions yet") {
		t.Errorf("no feeds: %s", body)
	}
	f := newFixture(t, defaultFeeds(), nil)
	body := f.get(t, "/")
	if !strings.Contains(body, "No posts yet") || refreshTarget(t, body) != "/refresh" {
		t.Errorf("uncached feeds: %s", body)
	}
}

func TestCrossOriginPostsAreRejected(t *testing.T) {
	f := newFixture(t, defaultFeeds(), map[string][]feed.Item{alpha: {entry("only", 1)}})
	for _, header := range []http.Header{
		{"Sec-Fetch-Site": {"cross-site"}},
		{"Origin": {"https://evil.example"}},
	} {
		if rec := f.do(t, http.MethodPost, "/refresh", header); rec.Code != http.StatusForbidden {
			t.Errorf("cross-origin refresh with %v = %d", header, rec.Code)
		}
	}
	if state, _ := f.store.FeedState(t.Context(), alpha); !state.FetchedAt.IsZero() {
		t.Error("a rejected refresh fetched")
	}
	if rec := f.do(t, http.MethodGet, "/refresh", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /refresh = %d", rec.Code)
	}
}

func TestSecurityHeadersAndAssets(t *testing.T) {
	f := newFixture(t, defaultFeeds(), nil)
	page := f.do(t, http.MethodGet, "/", nil)
	if csp := page.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe") {
		t.Errorf("CSP = %q", csp)
	}
	if page.Header().Get("Referrer-Policy") != "no-referrer" || page.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("page headers = %v", page.Header())
	}
	for _, asset := range []string{"/assets/htmx.min.js", "/assets/style.css"} {
		rec := f.do(t, http.MethodGet, asset+"?v="+assetVersion(), nil)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") || rec.Body.Len() == 0 {
			t.Errorf("GET %s = %d, %v", asset, rec.Code, rec.Header())
		}
	}
	for _, target := range []string{"/templates/page.html", "/assets/../templates/page.html", "/nope"} {
		if rec := f.do(t, http.MethodGet, target, nil); rec.Code == http.StatusOK {
			t.Errorf("GET %s = %d", target, rec.Code)
		}
	}
}

func TestServeStopsAndSaves(t *testing.T) {
	f := newFixture(t, defaultFeeds(), map[string][]feed.Item{alpha: {entry("only", 1)}})
	populate(t, f)
	listener := httptest.NewUnstartedServer(nil).Listener
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, listener, f.river, nil) }()
	resp, err := http.Get("http://" + listener.Addr().String() + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	head, _ := f.river.Timeline(t.Context(), reader.Query{Limit: 1})
	f.river.Reached(head.Items[0].Position)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("Serve returned %v", err)
	}
	if seen, _ := f.store.SeenPosition(t.Context()); seen == nil {
		t.Error("Serve did not save the reached position at exit")
	}
}

func TestAge(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.Local)
	for _, tc := range []struct {
		date time.Time
		want string
	}{
		{now.Add(-30 * time.Second), "now"},
		{now.Add(-5 * time.Minute), "5m"},
		{now.Add(-3 * time.Hour), "3h"},
		{now.Add(-2 * 24 * time.Hour), "2d"},
		{time.Date(2026, 3, 4, 0, 0, 0, 0, time.Local), "Mar 4"},
		{time.Date(2024, 3, 4, 0, 0, 0, 0, time.Local), "Mar 4, 2024"},
	} {
		if got := age(tc.date, now); got != tc.want {
			t.Errorf("age(%v) = %q, want %q", tc.date, got, tc.want)
		}
	}
}
