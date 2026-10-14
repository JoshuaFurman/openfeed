// Package reader orchestrates configured subscriptions, fetching, and cached state behind the river API
// that interfaces use. Config owns which feeds exist and their names; the store owns posts and seen state.
package reader

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/JoshuaFurman/openfeed/internal/config"
	"github.com/JoshuaFurman/openfeed/internal/feed"
	"github.com/JoshuaFurman/openfeed/internal/store"
)

// Position is an observed place in the river; obtain it from Timeline or SeenPosition.
type Position = store.TimelineCursor

// ItemID identifies a cached post across reloads and publisher updates.
type ItemID = store.ItemID

// Fetcher retrieves one feed using previous cache validators; feed.Client implements it.
type Fetcher interface {
	Fetch(ctx context.Context, feedURL string, previous feed.Validators) (feed.Result, error)
}

// Subscriptions durably records a new subscription and resolves it as config.Load would;
// *config.Config implements it.
type Subscriptions interface {
	Add(feed config.Feed) (config.Feed, error)
}

// Item is a cached post labeled with its subscription's configured name.
type Item struct {
	store.TimelineItem
	FeedName string
}

// Query selects river positions strictly older than Before and no older than Through, or else the posts
// nearest above After, which cannot combine with the others. Nil boundaries are unrestricted.
// A traversal keeps one Tag; positions from another still compare in river order.
type Query struct {
	Tag     string // Narrows the river to subscriptions carrying it, ignoring case; empty selects all.
	Before  *Position
	Through *Position
	After   *Position
	Limit   int // Zero uses 50; otherwise 1–200.
}

// Page contains current posts in newest-first order; nil continuations mean exhausted for now.
// Continue older with Before: Next and the same Through, or newer with After: Newer.
type Page struct {
	Items []Item
	Next  *Position
	Newer *Position
}

// Reader serves one river across the configured subscriptions, which Subscribe extends.
// Construct it with New; a Reader is safe for concurrent use.
type Reader struct {
	subs    Subscriptions
	store   *store.Store
	fetcher Fetcher
	log     *slog.Logger
	now     func() time.Time

	adding   sync.Mutex // Serializes Subscribe's duplicate check, config write, and feed set swap.
	mu       sync.Mutex
	set      *feedSet // Replaced, never modified, so reads and rounds use a consistent snapshot.
	round    *round
	failures map[string]failure
	reached  *Position
	saved    *Position
}

// feedSet is the configured subscriptions, in config order, with what reads and rounds derive from them.
type feedSet struct {
	feeds  []config.Feed
	hosts  []string // Request-slot key for each feed.
	urls   []string
	names  map[string]string
	tags   []string            // Distinct tags in config order, as first spelled.
	tagged map[string][]string // URLs by tagKey.
}

func newFeedSet(feeds []config.Feed) *feedSet {
	set := &feedSet{feeds: feeds, names: make(map[string]string, len(feeds)), tagged: make(map[string][]string)}
	for _, f := range feeds {
		host := f.URL
		if u, err := url.Parse(f.URL); err == nil {
			host = u.Hostname()
		}
		set.hosts = append(set.hosts, host)
		set.urls = append(set.urls, f.URL)
		set.names[f.URL] = f.Name
		for _, tag := range f.Tags {
			key := tagKey(tag)
			if key == "" {
				continue
			}
			if set.tagged[key] == nil {
				set.tags = append(set.tags, strings.TrimSpace(tag))
			}
			set.tagged[key] = append(set.tagged[key], f.URL)
		}
	}
	return set
}

// tagKey is the form in which tags match, so case and surrounding space never split one tag in two.
func tagKey(tag string) string { return strings.ToLower(strings.TrimSpace(tag)) }

// New serves feeds from st, refreshing them through fetcher, and records Subscribe's additions in subs.
// Feeds must satisfy config.Load's invariants; a nil logger discards failures.
func New(feeds []config.Feed, subs Subscriptions, st *store.Store, fetcher Fetcher, log *slog.Logger) *Reader {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Reader{
		subs:     subs,
		store:    st,
		fetcher:  fetcher,
		log:      log,
		now:      time.Now,
		set:      newFeedSet(slices.Clone(feeds)),
		failures: make(map[string]failure),
	}
}

func (r *Reader) current() *feedSet {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.set
}

// Feeds lists the configured subscriptions in config order; the caller may modify the result.
func (r *Reader) Feeds() []config.Feed { return slices.Clone(r.current().feeds) }

// Tags lists the subscriptions' distinct tags in config order, each as first spelled, for Query.Tag.
// The caller may modify the result.
func (r *Reader) Tags() []string { return slices.Clone(r.current().tags) }

// Subscribe checks that f fetches as a feed, appends it to the configuration, and adds it to the river
// with the posts it fetched. A failure leaves the configuration and river as they were.
func (r *Reader) Subscribe(ctx context.Context, f config.Feed) (config.Feed, error) {
	resolved := f
	if err := resolved.Normalize(); err != nil {
		return config.Feed{}, err
	}
	if err := r.unsubscribed(resolved.URL); err != nil {
		return config.Feed{}, err
	}
	// Read before fetching, as a refresh does, so a save elsewhere meanwhile is not overwritten.
	state, err := r.store.FeedState(ctx, resolved.URL)
	if err != nil {
		return config.Feed{}, err
	}
	fetched, err := r.fetcher.Fetch(ctx, resolved.URL, feed.Validators{ETag: state.ETag, LastModified: state.LastModified})
	if err != nil {
		return config.Feed{}, err
	}

	r.adding.Lock()
	defer r.adding.Unlock()
	if err := r.unsubscribed(resolved.URL); err != nil {
		return config.Feed{}, err
	}
	added, err := r.subs.Add(f)
	if err != nil {
		return config.Feed{}, err
	}
	// Saved before joining the set, so no round refetches it; after a failure, the next round does.
	err = r.store.SaveRefresh(ctx, state, storedRefresh(fetched))
	if err != nil && !errors.Is(err, store.ErrRefreshConflict) && ctx.Err() == nil {
		r.log.Warn("save new subscription", "feed", added.URL, "error", err)
	}
	r.mu.Lock()
	r.set = newFeedSet(append(slices.Clone(r.set.feeds), added))
	r.mu.Unlock()
	return added, nil
}

func (r *Reader) unsubscribed(feedURL string) error {
	if _, ok := r.current().names[feedURL]; ok {
		return fmt.Errorf("already subscribed to %s", feedURL)
	}
	return nil
}

// Timeline reads one page of the river across every configured subscription, or those tagged query.Tag.
// Calls do not share a snapshot; refreshes and date corrections can move posts between pages.
func (r *Reader) Timeline(ctx context.Context, query Query) (Page, error) {
	set := r.current()
	urls := set.urls
	if query.Tag != "" {
		urls = set.tagged[tagKey(query.Tag)] // An unknown tag selects nothing.
	}
	page, err := r.store.Timeline(ctx, store.TimelineQuery{
		FeedURLs: urls, Before: query.Before, Through: query.Through, After: query.After, Limit: query.Limit,
	})
	if err != nil {
		return Page{}, err
	}
	items := make([]Item, len(page.Items))
	for i, item := range page.Items {
		items[i] = Item{TimelineItem: item, FeedName: set.names[item.FeedURL]}
	}
	return Page{Items: items, Next: page.Next, Newer: page.Newer}, nil
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
