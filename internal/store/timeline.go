package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ItemID identifies one cached item within a database and remains stable across publisher updates.
type ItemID int64

// TimelineCursor is an opaque observed chronological position, not a live reference to an item.
// Obtain it from a Timeline result and reuse it only with the same database and URL set.
type TimelineCursor struct {
	date string
	id   ItemID
}

// TimelineItem is cached publisher data with a stable identity and its position when queried.
// Subscription labels and tags remain config-owned.
type TimelineItem struct {
	ID             ItemID
	FeedURL        string
	Title          string
	URL            string    // Absolute HTTP(S) article URL, or empty.
	Content        string    // Feed-provided content; may contain untrusted HTML.
	PublishedAt    time.Time // UTC; zero means absent.
	FirstFetchedAt time.Time // UTC; immutable for this identity.
	Position       TimelineCursor
}

// TimelineQuery selects positions strictly older than Before and no older than Through.
// Nil boundaries are unrestricted; an empty FeedURLs set selects nothing.
type TimelineQuery struct {
	FeedURLs []string        // Active configured URLs; order and repetitions do not affect selection.
	Before   *TimelineCursor // Exclusive older-page boundary.
	Through  *TimelineCursor // Inclusive oldest boundary.
	Limit    int             // Zero uses 50; otherwise 1–200.
}

// TimelinePage contains current items in newest-first order.
// Next continues the same URL set and Through boundary; nil means that interval is exhausted for now.
type TimelinePage struct {
	Items []TimelineItem
	Next  *TimelineCursor
}

// Timeline reads one current page ordered by publication/fallback date, then ID, both descending.
// Calls do not share a snapshot; date corrections can move items across page boundaries.
func (s *Store) Timeline(ctx context.Context, query TimelineQuery) (_ TimelinePage, err error) {
	defer func() { err = publicError(ctx, "read timeline", err) }()
	if err := ctx.Err(); err != nil {
		return TimelinePage{}, err
	}
	if query.Limit < 0 || query.Limit > 200 {
		return TimelinePage{}, errors.New("limit must be between 0 and 200")
	}
	if query.Limit == 0 {
		query.Limit = 50
	}
	for _, bound := range []struct {
		name   string
		cursor *TimelineCursor
	}{{"before", query.Before}, {"through", query.Through}} {
		if bound.cursor != nil && (bound.cursor.date == "" || bound.cursor.id <= 0) {
			return TimelinePage{}, fmt.Errorf("%s cursor was not obtained from Timeline", bound.name)
		}
	}
	if len(query.FeedURLs) == 0 {
		return TimelinePage{}, nil
	}

	statement, args := timelineSQL(query)
	rows, err := s.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return TimelinePage{}, err
	}
	defer rows.Close()

	var page TimelinePage
	for rows.Next() {
		if len(page.Items) == query.Limit {
			position := page.Items[len(page.Items)-1].Position
			page.Next = &position
			break
		}
		item, err := scanTimelineItem(rows)
		if err != nil {
			return TimelinePage{}, err
		}
		page.Items = append(page.Items, item)
	}
	if err := rows.Err(); err != nil {
		return TimelinePage{}, err
	}
	if err := rows.Close(); err != nil {
		return TimelinePage{}, err
	}
	return page, nil
}

func timelineSQL(query TimelineQuery) (string, []any) {
	feedURLs, _ := json.Marshal(query.FeedURLs) // A slice of strings is always JSON-encodable.
	statement := `
		SELECT id, feed_url, title, url, content, published_at, first_fetched_at
		FROM items
		WHERE feed_url IN (SELECT value FROM json_each(?))
	`
	args := []any{string(feedURLs)}
	// Scalar date bounds enable index seeks; SQLite does not seek with the expression tuple alone.
	if query.Before != nil {
		statement += `
			AND COALESCE(published_at, first_fetched_at) <= ?
			AND (COALESCE(published_at, first_fetched_at), id) < (?, ?)
		`
		args = append(args, query.Before.date, query.Before.date, query.Before.id)
	}
	if query.Through != nil {
		statement += `
			AND COALESCE(published_at, first_fetched_at) >= ?
			AND (COALESCE(published_at, first_fetched_at), id) >= (?, ?)
		`
		args = append(args, query.Through.date, query.Through.date, query.Through.id)
	}
	statement += `ORDER BY COALESCE(published_at, first_fetched_at) DESC, id DESC LIMIT ?`
	args = append(args, query.Limit+1)
	return statement, args
}

func scanTimelineItem(rows *sql.Rows) (TimelineItem, error) {
	var item TimelineItem
	var publishedAt sql.NullString
	var firstFetchedAt string
	if err := rows.Scan(&item.ID, &item.FeedURL, &item.Title, &item.URL, &item.Content, &publishedAt, &firstFetchedAt); err != nil {
		return TimelineItem{}, err
	}
	var err error
	item.FirstFetchedAt, err = time.Parse(timestampLayout, firstFetchedAt)
	if err != nil {
		return TimelineItem{}, fmt.Errorf("item %d first-fetch time: %w", item.ID, err)
	}
	item.Position = TimelineCursor{date: firstFetchedAt, id: item.ID}
	if publishedAt.Valid {
		item.PublishedAt, err = time.Parse(timestampLayout, publishedAt.String)
		if err != nil {
			return TimelineItem{}, fmt.Errorf("item %d publication time: %w", item.ID, err)
		}
		item.Position.date = publishedAt.String
	}
	return item, nil
}
