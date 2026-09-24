// Package reader orchestrates configured subscriptions, fetching, and cached state behind the river API
// that interfaces use. Config owns which feeds exist and their names; the store owns posts and seen state.
package reader

import (
	"context"
	"log/slog"
	"net/url"
	"slices"
	"sync"
	"time"

	"github.com/JoshuaFurman/openfeed/internal/config"
	"github.com/JoshuaFurman/openfeed/internal/feed"
	"github.com/JoshuaFurman/openfeed/internal/store"
)

// Position is an observed place in the river; obtain it from Timeline or SeenPosition.
type Position = store.TimelineCursor

// Fetcher retrieves one feed using previous cache validators; feed.Client implements it.
type Fetcher interface {
	Fetch(ctx context.Context, feedURL string, previous feed.Validators) (feed.Result, error)
}

// Item is a cached post labeled with its subscription's configured name.
type Item struct {
	store.TimelineItem
	FeedName string
}

// Query selects river positions strictly older than Before and no older than Through.
// Nil boundaries are unrestricted; Limit zero uses 50, otherwise 1–200.
type Query struct {
	Before  *Position
	Through *Position
	Limit   int
}

// Page contains current posts in newest-first order.
// Next continues the same Through boundary; nil means that interval is exhausted for now.
type Page struct {
	Items []Item
	Next  *Position
}

// Reader serves one river across a fixed set of configured subscriptions.
// Construct it with New; a Reader is safe for concurrent use.
type Reader struct {
	feeds   []config.Feed
	hosts   []string // Request-slot key for each feed.
	urls    []string
	names   map[string]string
	store   *store.Store
	fetcher Fetcher
	log     *slog.Logger
	now     func() time.Time

	mu       sync.Mutex
	round    *round
	failures map[string]failure
	reached  *Position
	saved    *Position
}

// New serves feeds from st, refreshing them through fetcher; a nil logger discards failures.
// Feeds must satisfy config.Load's invariants; later config changes need a new Reader.
func New(feeds []config.Feed, st *store.Store, fetcher Fetcher, log *slog.Logger) *Reader {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	r := &Reader{
		feeds:    slices.Clone(feeds),
		names:    make(map[string]string, len(feeds)),
		store:    st,
		fetcher:  fetcher,
		log:      log,
		now:      time.Now,
		failures: make(map[string]failure),
	}
	for _, f := range r.feeds {
		host := f.URL
		if u, err := url.Parse(f.URL); err == nil {
			host = u.Hostname()
		}
		r.hosts = append(r.hosts, host)
		r.urls = append(r.urls, f.URL)
		r.names[f.URL] = f.Name
	}
	return r
}

// Timeline reads one page of the river across every configured subscription.
// Calls do not share a snapshot; refreshes and date corrections can move posts between pages.
func (r *Reader) Timeline(ctx context.Context, query Query) (Page, error) {
	page, err := r.store.Timeline(ctx, store.TimelineQuery{
		FeedURLs: r.urls, Before: query.Before, Through: query.Through, Limit: query.Limit,
	})
	if err != nil {
		return Page{}, err
	}
	items := make([]Item, len(page.Items))
	for i, item := range page.Items {
		items[i] = Item{TimelineItem: item, FeedName: r.names[item.FeedURL]}
	}
	return Page{Items: items, Next: page.Next}, nil
}

// SeenPosition returns the saved divider position for a session to display, or nil before any.
func (r *Reader) SeenPosition(ctx context.Context) (*Position, error) {
	return r.store.SeenPosition(ctx)
}

// Reached records that the user navigated to a post; only the furthest position is kept.
// Call it for user navigation only, never for launch placement, refreshes, or loading.
func (r *Reader) Reached(position Position) {
	if position == (Position{}) {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.reached == nil || position.Compare(*r.reached) > 0 {
		r.reached = &position
	}
}

// SaveSeen persists the furthest reached position if it moved forward since the last save.
// A failure is logged and retried by the next call; it never affects reading.
func (r *Reader) SaveSeen(ctx context.Context) error {
	r.mu.Lock()
	pending := r.reached
	if pending == nil || (r.saved != nil && pending.Compare(*r.saved) <= 0) {
		r.mu.Unlock()
		return nil
	}
	r.mu.Unlock()

	if err := r.store.AdvanceSeen(ctx, *pending); err != nil {
		if ctx.Err() == nil {
			r.log.Warn("save seen position", "error", err)
		}
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.saved == nil || pending.Compare(*r.saved) > 0 {
		r.saved = pending
	}
	return nil
}
