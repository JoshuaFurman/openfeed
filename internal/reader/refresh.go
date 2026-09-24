package reader

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/JoshuaFurman/openfeed/internal/config"
	"github.com/JoshuaFurman/openfeed/internal/feed"
	"github.com/JoshuaFurman/openfeed/internal/store"
)

const (
	refreshGap     = 30 * time.Second
	hostSlots      = 2
	initialBackoff = 15 * time.Second
	maxBackoff     = 10 * time.Minute
)

// Status summarizes one subscription's outcome in a refresh round.
type Status int

const (
	// Refreshed means the feed was fetched and saved during the round, possibly by another process first.
	Refreshed Status = iota + 1
	// NotDue means the feed was saved within the refresh gap, so it was not fetched.
	NotDue
	// Failing means the latest attempt failed, including while the feed waits out its backoff.
	Failing
)

// FeedResult is one configured subscription's outcome; Err is set only when Failing.
type FeedResult struct {
	Feed   config.Feed
	Status Status
	Err    error
}

// Report lists every configured subscription's outcome in config order.
// It deliberately carries no count of new posts.
type Report struct {
	Feeds []FeedResult
}

type round struct {
	done     chan struct{}
	report   Report
	canceled bool
}

type failure struct {
	err      error
	attempts int
	retryAt  time.Time
}

// Refresh fetches every due subscription concurrently and saves each success; one feed never fails the round.
// A call during a round joins it, or reruns it if the starter was canceled; the error means only that ctx ended.
func (r *Reader) Refresh(ctx context.Context) (Report, error) {
	for {
		r.mu.Lock()
		current := r.round
		if current == nil {
			current = &round{done: make(chan struct{})}
			r.round = current
			r.mu.Unlock()
			return r.runRound(ctx, current)
		}
		r.mu.Unlock()
		select {
		case <-current.done:
			if !current.canceled {
				return Report{Feeds: slices.Clone(current.report.Feeds)}, nil
			}
		case <-ctx.Done():
			return Report{}, ctx.Err()
		}
	}
}

func (r *Reader) runRound(ctx context.Context, current *round) (Report, error) {
	slots := make(map[string]chan struct{})
	for _, host := range r.hosts {
		if slots[host] == nil {
			slots[host] = make(chan struct{}, hostSlots)
		}
	}
	report := Report{Feeds: make([]FeedResult, len(r.feeds))}
	var wg sync.WaitGroup
	for i, f := range r.feeds {
		wg.Go(func() { report.Feeds[i] = r.refreshFeed(ctx, f, slots[r.hosts[i]]) })
	}
	wg.Wait()

	current.report, current.canceled = report, ctx.Err() != nil
	r.mu.Lock()
	r.round = nil
	r.mu.Unlock()
	close(current.done)
	return report, ctx.Err()
}

func (r *Reader) refreshFeed(ctx context.Context, f config.Feed, slots chan struct{}) FeedResult {
	result := FeedResult{Feed: f}
	if err := r.backingOff(f.URL); err != nil {
		result.Status, result.Err = Failing, err
		return result
	}
	// Checking before queueing keeps feeds that are not due from waiting on a busy host.
	if _, ok, err := r.due(ctx, f.URL); err != nil || !ok {
		return r.notFetched(ctx, result, err)
	}
	select {
	case slots <- struct{}{}:
	case <-ctx.Done():
		return r.failed(ctx, result, ctx.Err())
	}
	// Reread after queueing: another process may have saved the feed while this one waited.
	state, ok, err := r.due(ctx, f.URL)
	var fetched feed.Result
	if err == nil && ok {
		fetched, err = r.fetcher.Fetch(ctx, f.URL, feed.Validators{ETag: state.ETag, LastModified: state.LastModified})
	}
	<-slots
	if err != nil || !ok {
		return r.notFetched(ctx, result, err)
	}
	// A conflict means another process just saved this feed; refetching would only repeat its work.
	if err := r.store.SaveRefresh(ctx, state, storedRefresh(fetched)); err != nil && !errors.Is(err, store.ErrRefreshConflict) {
		return r.failed(ctx, result, err)
	}
	r.forgetFailure(f.URL)
	result.Status = Refreshed
	return result
}

// due reads a feed's latest snapshot, which is not due within the refresh gap of its last save.
func (r *Reader) due(ctx context.Context, feedURL string) (store.FeedState, bool, error) {
	state, err := r.store.FeedState(ctx, feedURL)
	if err != nil {
		return store.FeedState{}, false, err
	}
	// A clock set backwards makes the age negative; fetch rather than wait for it to catch up.
	age := r.now().Sub(state.FetchedAt)
	return state, state.FetchedAt.IsZero() || age < 0 || age >= refreshGap, nil
}

// notFetched reports a failure, or a feed saved so recently that its earlier failures no longer apply.
func (r *Reader) notFetched(ctx context.Context, result FeedResult, err error) FeedResult {
	if err != nil {
		return r.failed(ctx, result, err)
	}
	r.forgetFailure(result.Feed.URL)
	result.Status = NotDue
	return result
}

func (r *Reader) forgetFailure(feedURL string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.failures, feedURL)
}

func (r *Reader) backingOff(feedURL string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if f, ok := r.failures[feedURL]; ok && r.now().Before(f.retryAt) {
		return f.err
	}
	return nil
}

func (r *Reader) failed(ctx context.Context, result FeedResult, err error) FeedResult {
	result.Status, result.Err = Failing, err
	if ctx.Err() != nil {
		return result // The caller canceled; the feed did not fail.
	}
	r.mu.Lock()
	f := r.failures[result.Feed.URL]
	f.err, f.attempts = err, f.attempts+1
	delay := initialBackoff
	for range f.attempts - 1 {
		if delay >= maxBackoff/2 {
			delay = maxBackoff
			break
		}
		delay *= 2
	}
	f.retryAt = r.now().Add(delay)
	r.failures[result.Feed.URL] = f
	r.mu.Unlock()
	r.log.Warn("refresh feed", "feed", result.Feed.URL, "error", err, "retry_after", delay)
	return result
}

func storedRefresh(fetched feed.Result) store.Refresh {
	refresh := store.Refresh{
		ETag:         fetched.Validators.ETag,
		LastModified: fetched.Validators.LastModified,
		FetchedAt:    fetched.FetchedAt,
		Unchanged:    fetched.NotModified,
		Items:        make([]store.RefreshItem, len(fetched.Items)),
	}
	// Source order, including duplicate keys, lets storage apply its first-occurrence rule.
	for i, item := range fetched.Items {
		refresh.Items[i] = store.RefreshItem{
			Key: item.Key, Title: item.Title, URL: item.URL, Content: item.Content, PublishedAt: item.PublishedAt,
		}
	}
	return refresh
}
