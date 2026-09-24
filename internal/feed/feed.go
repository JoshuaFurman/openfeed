// Package feed fetches subscriptions into source items, independent of storage and presentation.
// Subscription names remain config-owned; fetched content is untrusted and may contain HTML.
package feed

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	requestTimeout = 30 * time.Second
	maxBodyBytes   = 10 << 20 // 10 MiB, after HTTP decompression.
	userAgent      = "openfeed (+https://github.com/JoshuaFurman/openfeed)"
)

// Item is publisher data, not a stored timeline entry; it has no database ID or subscription label.
// Key identifies an item only within its configured feed URL; fetched items have a nonempty title, content, or usable URL.
type Item struct {
	Key     string
	Title   string
	URL     string // Absolute HTTP(S) URL, or empty if no usable article link exists.
	Content string // Feed-provided content, falling back to the description; may contain HTML.

	// PublishedAt is UTC, or zero when missing/invalid; storage should then retain the first fetch time.
	PublishedAt time.Time
}

// Validators are opaque HTTP cache tokens for one subscription URL.
// Persist them only alongside the items from a successful fetch.
type Validators struct {
	ETag         string
	LastModified string
}

// Result distinguishes an unchanged subscription from a successfully fetched empty feed.
// FetchedAt is UTC and supplies the initial timeline date for undated items, never their identity.
type Result struct {
	Items       []Item // Source order, including duplicate keys; nil when NotModified.
	Validators  Validators
	FetchedAt   time.Time
	NotModified bool
}

// HTTPClient performs requests with net/http.Client semantics, including request-context cancellation.
type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

// Client fetches feeds without retaining items or cache validators between requests.
// Construct it with NewClient; it can be shared if its HTTP client supports concurrent use.
type Client struct {
	http HTTPClient
}

// NewClient uses the supplied HTTP client for connection reuse and transport policy.
// The client must be non-nil; each Fetch applies its own 30-second deadline and 10 MiB body limit.
func NewClient(client HTTPClient) *Client {
	return &Client{http: client}
}

// Fetch retrieves one configured feed URL, using previous cache validators when available.
// Failures return no result to persist; a 304 retains prior validators unless the server replaces them.
func (c *Client) Fetch(ctx context.Context, feedURL string, previous Validators) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, feedURL, nil)
	if err != nil {
		return Result{}, fmt.Errorf("fetch %s: %w", feedURL, err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/atom+xml, application/rss+xml, application/feed+json, application/xml, text/xml, application/json")
	if previous.ETag != "" {
		req.Header.Set("If-None-Match", previous.ETag)
	}
	if previous.LastModified != "" {
		req.Header.Set("If-Modified-Since", previous.LastModified)
	}

	fetchedAt := time.Now().UTC()
	resp, err := c.http.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("fetch %s: %w", feedURL, err)
	}
	defer resp.Body.Close()

	validators := Validators{
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
	}
	switch resp.StatusCode {
	case http.StatusNotModified:
		if previous == (Validators{}) {
			return Result{}, fmt.Errorf("fetch %s: unexpected HTTP 304 without cache validators", feedURL)
		}
		if validators.ETag == "" {
			validators.ETag = previous.ETag
		}
		if validators.LastModified == "" {
			validators.LastModified = previous.LastModified
		}
		return Result{Validators: validators, FetchedAt: fetchedAt, NotModified: true}, nil
	case http.StatusOK:
	default:
		return Result{}, fmt.Errorf("fetch %s: HTTP %d %s", feedURL, resp.StatusCode, http.StatusText(resp.StatusCode))
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return Result{}, fmt.Errorf("read feed %s: %w", feedURL, err)
	}
	if len(body) > maxBodyBytes {
		return Result{}, fmt.Errorf("read feed %s: body exceeds %d bytes", feedURL, maxBodyBytes)
	}

	base := req.URL
	if resp.Request != nil {
		base = resp.Request.URL
	}
	items, err := parse(body, base)
	if err != nil {
		return Result{}, fmt.Errorf("parse feed %s: %w", feedURL, err)
	}
	if err := ctx.Err(); err != nil {
		return Result{}, fmt.Errorf("fetch %s: %w", feedURL, err)
	}
	return Result{Items: items, Validators: validators, FetchedAt: fetchedAt}, nil
}
