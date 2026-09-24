package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrRefreshConflict means another refresh committed since the supplied snapshot was read.
// Read fresh state and fetch again; do not replay the rejected response.
const ErrRefreshConflict = conflictError("feed changed while refresh was in flight")

type conflictError string

// Error supplies a diagnostic; callers should branch with errors.Is.
func (e conflictError) Error() string { return string(e) }

const timestampLayout = "2006-01-02T15:04:05.000000000Z"

// FeedState is a snapshot of one subscription's cached refresh metadata.
// Obtain it from Store.FeedState and use it only with the same database.
type FeedState struct {
	ETag         string
	LastModified string
	FetchedAt    time.Time // UTC request-start time of the last accepted refresh; zero if uncached.

	feedURL  string
	revision int64
}

// RefreshItem is normalized publisher data keyed within one configured feed URL.
// Key must be nonempty, and at least one of Title, URL, or Content must be nonempty.
type RefreshItem struct {
	Key         string
	Title       string
	URL         string    // Absolute HTTP(S) article URL, or empty.
	Content     string    // Feed-provided content; may contain untrusted HTML.
	PublishedAt time.Time // Zero means absent, including a previously supplied date's removal.
}

// Refresh is a successful fetch outcome; validators replace cached tokens even when empty.
// Unchanged requires cached feed state and no items; a full outcome may contain zero items.
type Refresh struct {
	Items        []RefreshItem // Source order; the first occurrence of each key wins as a whole item.
	ETag         string
	LastModified string
	FetchedAt    time.Time // Nonzero request-start time; becomes newly inserted items' first-fetch time.
	Unchanged    bool
}

// FeedState returns cached metadata for a configured URL without creating a subscription.
// An uncached URL returns empty metadata and a usable snapshot, not an error.
func (s *Store) FeedState(ctx context.Context, feedURL string) (_ FeedState, err error) {
	defer func() { err = publicError(ctx, fmt.Sprintf("read feed state %q", feedURL), err) }()
	if err := ctx.Err(); err != nil {
		return FeedState{}, err
	}
	if feedURL == "" {
		return FeedState{}, errors.New("feed URL is empty")
	}
	state := FeedState{feedURL: feedURL}
	var fetchedAt string
	err = s.db.QueryRowContext(ctx, `
		SELECT etag, last_modified, fetched_at, revision FROM feeds WHERE url = ?
	`, feedURL).Scan(&state.ETag, &state.LastModified, &fetchedAt, &state.revision)
	if errors.Is(err, sql.ErrNoRows) {
		return state, nil
	}
	if err != nil {
		return FeedState{}, err
	}
	state.FetchedAt, err = time.Parse(timestampLayout, fetchedAt)
	if err != nil {
		return FeedState{}, fmt.Errorf("decode fetch time: %w", err)
	}
	return state, nil
}

// SaveRefresh commits items and metadata together, preserving existing identities' first-fetch times.
// A stale snapshot returns ErrRefreshConflict without changes; omitted items are never deleted.
// Nonzero timestamps must have UTC years in 0000–9999.
func (s *Store) SaveRefresh(ctx context.Context, previous FeedState, refresh Refresh) (err error) {
	defer func() { err = publicError(ctx, fmt.Sprintf("save refresh %q", previous.feedURL), err) }()
	if err := ctx.Err(); err != nil {
		return err
	}
	if previous.feedURL == "" {
		return errors.New("feed snapshot was not obtained from FeedState")
	}
	if refresh.Unchanged && len(refresh.Items) != 0 {
		return errors.New("unchanged refresh contains items")
	}
	fetchedAt, err := encodeTime(refresh.FetchedAt)
	if err != nil {
		return fmt.Errorf("fetch time: %w", err)
	}

	tx, err := beginWrite(ctx, s.db)
	if err != nil {
		return fmt.Errorf("lock refresh: %w", err)
	}
	defer tx.Rollback()

	var revision int64
	err = tx.QueryRowContext(ctx, "SELECT revision FROM feeds WHERE url = ?", previous.feedURL).Scan(&revision)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read feed revision: %w", err)
	}
	if revision != previous.revision {
		return ErrRefreshConflict
	}
	if refresh.Unchanged && revision == 0 {
		return errors.New("unchanged refresh has no cached feed state")
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO feeds (url, etag, last_modified, fetched_at, revision)
		VALUES (?, ?, ?, ?, 1)
		ON CONFLICT (url) DO UPDATE SET
			etag = excluded.etag,
			last_modified = excluded.last_modified,
			fetched_at = excluded.fetched_at,
			revision = feeds.revision + 1
	`, previous.feedURL, refresh.ETag, refresh.LastModified, fetchedAt); err != nil {
		return fmt.Errorf("save feed metadata: %w", err)
	}
	if err := saveItems(ctx, tx, previous.feedURL, fetchedAt, refresh.Items); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit refresh: %w", err)
	}
	return nil
}

func saveItems(ctx context.Context, tx *sql.Tx, feedURL, fetchedAt string, items []RefreshItem) error {
	if len(items) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO items (feed_url, key, title, url, content, published_at, first_fetched_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (feed_url, key) DO UPDATE SET
			title = excluded.title,
			url = excluded.url,
			content = excluded.content,
			published_at = excluded.published_at
	`)
	if err != nil {
		return fmt.Errorf("prepare item persistence: %w", err)
	}
	defer stmt.Close()

	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, duplicate := seen[item.Key]; duplicate {
			continue
		}
		seen[item.Key] = struct{}{}
		if item.Key == "" {
			return errors.New("item key is empty")
		}
		if item.Title == "" && item.URL == "" && item.Content == "" {
			return fmt.Errorf("item %q has no title, URL, or content", item.Key)
		}
		var publishedAt any
		if !item.PublishedAt.IsZero() {
			publishedAt, err = encodeTime(item.PublishedAt)
			if err != nil {
				return fmt.Errorf("item %q publication time: %w", item.Key, err)
			}
		}
		if _, err := stmt.ExecContext(ctx, feedURL, item.Key, item.Title, item.URL, item.Content, publishedAt, fetchedAt); err != nil {
			return fmt.Errorf("save item %q: %w", item.Key, err)
		}
	}
	return nil
}

func encodeTime(value time.Time) (string, error) {
	if value.IsZero() {
		return "", errors.New("time is zero")
	}
	value = value.UTC()
	if value.Year() < 0 || value.Year() > 9999 {
		return "", errors.New("UTC year is outside 0000–9999")
	}
	return value.Format(timestampLayout), nil
}
