package store

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
)

// ItemID identifies one cached item within a database and remains stable across publisher updates.
type ItemID int64

// TimelineCursor is an opaque observed chronological position, not a live reference to an item.
// Obtain it from Timeline or SeenPosition and use it only with the same database; continuing a traversal
// also requires the same URL set.
type TimelineCursor struct {
	date string
	id   ItemID
}

// Compare orders positions chronologically, breaking date ties by ItemID as the timeline does.
// It returns a negative value when c is older than other and zero when they are equal.
func (c TimelineCursor) Compare(other TimelineCursor) int {
	return cmp.Or(cmp.Compare(c.date, other.date), cmp.Compare(c.id, other.id))
}

// TimelineItem is cached publisher data with a stable identity and its position when queried.
// Subscription labels and tags remain config-owned.
type TimelineItem struct {
	ID             ItemID
	FeedURL        string
	Title          string
	URL            string    // Absolute HTTP(S) article URL, or empty.
	Content        string    // Feed-provided content; may contain untrusted HTML.
	PublishedAt    time.Time // UTC publisher date, even when later than FirstFetchedAt; zero means absent.
	FirstFetchedAt time.Time // UTC; immutable for this identity.
	Position       TimelineCursor
}

// EffectiveDate is the time the river orders this item by: the earlier of publication and first fetch.
// Display it rather than PublishedAt wherever dates should read in river order.
func (i TimelineItem) EffectiveDate() time.Time {
	if i.PublishedAt.IsZero() || i.PublishedAt.After(i.FirstFetchedAt) {
		return i.FirstFetchedAt
	}
	return i.PublishedAt
}

// TimelineQuery selects positions strictly older than Before and no older than Through, or else the
// positions nearest above After. Nil boundaries are unrestricted; an empty FeedURLs set selects nothing.
type TimelineQuery struct {
	FeedURLs []string        // Active configured URLs; order and repetitions do not affect selection.
	Before   *TimelineCursor // Exclusive older-page boundary.
	Through  *TimelineCursor // Inclusive oldest boundary.
	After    *TimelineCursor // Selects the positions nearest above it; cannot combine with Before or Through.
	Limit    int             // Zero uses 50; otherwise 1–200.
}

// TimelinePage contains current items in newest-first order; nil continuations mean exhausted for now.
// Continue older with Before: Next and the same URL set and Through, or newer with After: Newer.
type TimelinePage struct {
	Items []TimelineItem
	Next  *TimelineCursor
	Newer *TimelineCursor
}

// Timeline reads one current page ordered by the earlier of publication and first-fetch time, then ID,
// both descending. Calls do not share a snapshot; date corrections can move items across page boundaries.
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
	}{{"before", query.Before}, {"through", query.Through}, {"after", query.After}} {
		if bound.cursor != nil && (bound.cursor.date == "" || bound.cursor.id <= 0) {
			return TimelinePage{}, fmt.Errorf("%s cursor was not obtained from Timeline or SeenPosition", bound.name)
		}
	}
	if query.After != nil && (query.Before != nil || query.Through != nil) {
		return TimelinePage{}, errors.New("after cannot be combined with before or through")
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
	more := false
	for rows.Next() {
		if len(page.Items) == query.Limit {
			more = true
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
	if query.After != nil {
		slices.Reverse(page.Items) // After reads scan upward from the bound.
		if more {
			newest := page.Items[0].Position
			page.Newer = &newest
		}
	} else if more {
		oldest := page.Items[len(page.Items)-1].Position
		page.Next = &oldest
	}
	return page, nil
}

// effectiveDate must match the items_timeline index expression exactly, or queries stop using it.
const effectiveDate = "COALESCE(MIN(published_at, first_fetched_at), first_fetched_at)"

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
			AND ` + effectiveDate + ` <= ?
			AND (` + effectiveDate + `, id) < (?, ?)
		`
		args = append(args, query.Before.date, query.Before.date, query.Before.id)
	}
	if query.Through != nil {
		statement += `
			AND ` + effectiveDate + ` >= ?
			AND (` + effectiveDate + `, id) >= (?, ?)
		`
		args = append(args, query.Through.date, query.Through.date, query.Through.id)
	}
	order := "DESC"
	if query.After != nil {
		statement += `
			AND ` + effectiveDate + ` >= ?
			AND (` + effectiveDate + `, id) > (?, ?)
		`
		args = append(args, query.After.date, query.After.date, query.After.id)
		order = "ASC"
	}
	statement += `ORDER BY ` + effectiveDate + ` ` + order + `, id ` + order + ` LIMIT ?`
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
		// Canonical timestamps compare chronologically as text.
		item.Position.date = min(publishedAt.String, firstFetchedAt)
	}
	return item, nil
}
