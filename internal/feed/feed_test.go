package feed

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

const emptyFeed = `<rss version="2.0"><channel><title>Empty</title></channel></rss>`

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type trackedBody struct {
	io.Reader
	closed bool
	read   bool
}

func (b *trackedBody) Read(p []byte) (int, error) {
	b.read = true
	return b.Reader.Read(p)
}

func (b *trackedBody) Close() error {
	b.closed = true
	return nil
}

type readFunc func([]byte) (int, error)

func (f readFunc) Read(p []byte) (int, error) { return f(p) }

func TestFetch(t *testing.T) {
	body := &trackedBody{Reader: strings.NewReader(string(fixture(t, "rss.xml")))}
	previous := Validators{ETag: `W/"old"`, LastModified: "Mon, 01 Sep 2025 13:00:00 GMT"}
	wantValidators := Validators{ETag: `"new"`, LastModified: "Tue, 02 Sep 2025 13:00:00 GMT"}
	feedURL := "https://example.com/feeds/index"
	before := time.Now()
	client := NewClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet || req.URL.String() != feedURL {
			t.Errorf("request = %s %s, want GET %s", req.Method, req.URL, feedURL)
		}
		for header, want := range map[string]string{
			"If-None-Match":     previous.ETag,
			"If-Modified-Since": previous.LastModified,
			"User-Agent":        userAgent,
		} {
			if got := req.Header.Get(header); got != want {
				t.Errorf("%s = %q, want %q", header, got, want)
			}
		}
		if !strings.Contains(req.Header.Get("Accept"), "application/feed+json") {
			t.Error("Accept does not advertise JSON Feed")
		}
		if deadline, ok := req.Context().Deadline(); !ok || deadline.Before(before.Add(requestTimeout)) || deadline.After(time.Now().Add(requestTimeout)) {
			t.Errorf("request deadline = %v, present = %v", deadline, ok)
		}
		header := make(http.Header)
		header.Set("ETag", wantValidators.ETag)
		header.Set("Last-Modified", wantValidators.LastModified)
		return &http.Response{StatusCode: http.StatusOK, Body: body, Header: header, Request: req}, nil
	})})

	got, err := client.Fetch(t.Context(), feedURL, previous)
	if err != nil {
		t.Fatal(err)
	}
	if got.NotModified || len(got.Items) != 3 || got.Validators != wantValidators {
		t.Errorf("unexpected result: %+v", got)
	}
	if got.FetchedAt.Before(before) || got.FetchedAt.After(time.Now()) || got.FetchedAt.Location() != time.UTC {
		t.Errorf("invalid fetch timestamp: %v", got.FetchedAt)
	}
	if !body.closed {
		t.Error("response body was not closed")
	}
}

func TestFetchCacheValidators(t *testing.T) {
	old := Validators{ETag: `W/"old"`, LastModified: "Mon, 01 Sep 2025 13:00:00 GMT"}
	for _, tc := range []struct {
		name     string
		status   int
		previous Validators
		header   http.Header
		want     Validators
	}{
		{"first fetch", http.StatusOK, Validators{}, nil, Validators{}},
		{"200 clears stale tokens", http.StatusOK, old, nil, Validators{}},
		{"304 keeps tokens", http.StatusNotModified, old, nil, old},
		{"304 replaces ETag", http.StatusNotModified, old, http.Header{"Etag": {`"new"`}}, Validators{ETag: `"new"`, LastModified: old.LastModified}},
		{"304 replaces modified date", http.StatusNotModified, old, http.Header{"Last-Modified": {"Tue, 02 Sep 2025 13:00:00 GMT"}}, Validators{ETag: old.ETag, LastModified: "Tue, 02 Sep 2025 13:00:00 GMT"}},
		{"ETag only", http.StatusNotModified, Validators{ETag: old.ETag}, nil, Validators{ETag: old.ETag}},
		{"modified date only", http.StatusNotModified, Validators{LastModified: old.LastModified}, nil, Validators{LastModified: old.LastModified}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &trackedBody{Reader: strings.NewReader(emptyFeed)}
			client := NewClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.Header.Get("If-None-Match") != tc.previous.ETag || req.Header.Get("If-Modified-Since") != tc.previous.LastModified {
					t.Errorf("incorrect conditional headers: %v", req.Header)
				}
				return &http.Response{StatusCode: tc.status, Header: tc.header, Body: body, Request: req}, nil
			})})
			got, err := client.Fetch(t.Context(), "https://example.com/feed", tc.previous)
			if err != nil {
				t.Fatal(err)
			}
			unchanged := tc.status == http.StatusNotModified
			if got.NotModified != unchanged || got.Validators != tc.want || len(got.Items) != 0 || got.FetchedAt.IsZero() {
				t.Errorf("unexpected result: %+v", got)
			}
			if unchanged && body.read {
				t.Error("304 body was read")
			}
			if !body.closed {
				t.Error("response body was not closed")
			}
		})
	}
}

func TestFetchFailures(t *testing.T) {
	readErr := errors.New("broken response body")
	transportErr := errors.New("connection failed")
	for _, tc := range []struct {
		name         string
		status       int
		body         io.Reader
		transportErr error
		want         string
		wantIs       error
	}{
		{"transport", 0, nil, transportErr, "connection failed", transportErr},
		{"body read", http.StatusOK, readFunc(func([]byte) (int, error) { return 0, readErr }), nil, "read feed", readErr},
		{"no content", http.StatusNoContent, strings.NewReader(""), nil, "HTTP 204", nil},
		{"partial content", http.StatusPartialContent, strings.NewReader(emptyFeed), nil, "HTTP 206", nil},
		{"not found", http.StatusNotFound, strings.NewReader("Not found"), nil, "HTTP 404", nil},
		{"rate limited", http.StatusTooManyRequests, strings.NewReader("Try later"), nil, "HTTP 429", nil},
		{"server failure", http.StatusInternalServerError, strings.NewReader("Error"), nil, "HTTP 500", nil},
		{"unsolicited 304", http.StatusNotModified, strings.NewReader(""), nil, "without cache validators", nil},
		{"not a feed", http.StatusOK, strings.NewReader("<html>Not a feed</html>"), nil, "parse feed", nil},
		{"broken XML", http.StatusOK, strings.NewReader(`<rss version="2.0"><channel><item><title>broken`), nil, "parse feed", nil},
		{"broken JSON", http.StatusOK, strings.NewReader(`{"version":"https://jsonfeed.org/version/1.1", "items": [`), nil, "parse feed", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &trackedBody{Reader: tc.body}
			client := NewClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if tc.transportErr != nil {
					return nil, tc.transportErr
				}
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Etag": {`"must-not-persist"`}}, Body: body, Request: req}, nil
			})})
			feedURL := "https://example.com/feed"
			got, err := client.Fetch(t.Context(), feedURL, Validators{})
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), feedURL) {
				t.Fatalf("error = %v, want %q and feed URL", err, tc.want)
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Errorf("error = %v, does not wrap %v", err, tc.wantIs)
			}
			if !reflect.DeepEqual(got, Result{}) {
				t.Errorf("failed fetch returned data to persist: %+v", got)
			}
			if tc.transportErr == nil && !body.closed {
				t.Error("response body was not closed on failure")
			}
		})
	}
}

func TestFetchInvalidRequest(t *testing.T) {
	client := NewClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid request reached the transport")
		return nil, nil
	})})
	if _, err := client.Fetch(t.Context(), "://invalid", Validators{}); err == nil {
		t.Error("expected request construction error")
	}
}

func TestFetchBodyLimit(t *testing.T) {
	for _, size := range []int{maxBodyBytes, maxBodyBytes + 1} {
		t.Run(fmt.Sprintf("%d bytes", size), func(t *testing.T) {
			content := emptyFeed + strings.Repeat(" ", size-len(emptyFeed))
			body := &trackedBody{Reader: strings.NewReader(content)}
			client := NewClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Body: body, ContentLength: -1, Request: req}, nil
			})})
			got, err := client.Fetch(t.Context(), "https://example.com/feed", Validators{})
			if size > maxBodyBytes {
				if err == nil || !strings.Contains(err.Error(), "body exceeds") || !reflect.DeepEqual(got, Result{}) {
					t.Errorf("oversized feed returned %+v, %v", got, err)
				}
			} else if err != nil || len(got.Items) != 0 || got.NotModified {
				t.Errorf("feed at limit returned %+v, %v", got, err)
			}
			if !body.closed {
				t.Error("response body was not closed")
			}
		})
	}
}

func TestFetchContext(t *testing.T) {
	for _, phase := range []string{"transport", "body", "after body"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			body := &trackedBody{}
			client := NewClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				switch phase {
				case "transport":
					cancel()
					return nil, req.Context().Err()
				case "body":
					body.Reader = readFunc(func([]byte) (int, error) {
						cancel()
						return 0, req.Context().Err()
					})
				case "after body":
					reader := strings.NewReader(emptyFeed)
					body.Reader = readFunc(func(p []byte) (int, error) {
						n, err := reader.Read(p)
						if err == io.EOF {
							cancel()
						}
						return n, err
					})
				}
				return &http.Response{StatusCode: http.StatusOK, Body: body, Request: req}, nil
			})})
			got, err := client.Fetch(ctx, "https://example.com/feed", Validators{})
			if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(got, Result{}) {
				t.Errorf("canceled fetch returned %+v, %v", got, err)
			}
			if phase != "transport" && !body.closed {
				t.Error("response body was not closed after cancellation")
			}
		})
	}
}

func TestFetchPreservesEarlierDeadline(t *testing.T) {
	deadline := time.Now().Add(5 * time.Second)
	ctx, cancel := context.WithDeadline(t.Context(), deadline)
	defer cancel()
	client := NewClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if got, ok := req.Context().Deadline(); !ok || !got.Equal(deadline) {
			t.Errorf("request deadline = %v, want %v", got, deadline)
		}
		return nil, context.DeadlineExceeded
	})})
	if _, err := client.Fetch(ctx, "https://example.com/feed", Validators{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want wrapped deadline exceeded", err)
	}
}

func TestFetchResolvesLinksAfterRedirect(t *testing.T) {
	client := NewClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.String() {
		case "https://example.com/feed":
			return &http.Response{StatusCode: http.StatusMovedPermanently, Header: http.Header{"Location": {"https://other.example/archive/feed"}}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
		case "https://other.example/archive/feed":
			body := `<rss version="2.0"><channel><item><title>Relative</title><link>post</link></item></channel></rss>`
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
		default:
			t.Fatalf("unexpected request: %s", req.URL)
			return nil, nil
		}
	})})
	got, err := client.Fetch(t.Context(), "https://example.com/feed", Validators{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 1 || got.Items[0].URL != "https://other.example/archive/post" || got.Items[0].Key != "url:https://other.example/archive/post" {
		t.Errorf("incorrect relative link after redirect: %+v", got.Items)
	}
}

func TestFetchIdentityIgnoresFetchTime(t *testing.T) {
	body := string(fixture(t, "rss.xml"))
	client := NewClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})})
	first, err := client.Fetch(t.Context(), "https://example.com/feed", Validators{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.Fetch(t.Context(), "https://example.com/feed", Validators{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first.Items, second.Items) {
		t.Error("refetch changed normalized items")
	}
	if !first.Items[2].PublishedAt.IsZero() || !strings.HasPrefix(first.Items[2].Key, "sha256:") {
		t.Error("undated fallback identity case was not exercised")
	}
}

func TestClientConcurrentFetch(t *testing.T) {
	body := string(fixture(t, "rss.xml"))
	client := NewClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})})
	for range 8 {
		t.Run("fetch", func(t *testing.T) {
			t.Parallel()
			if got, err := client.Fetch(t.Context(), "https://example.com/feed", Validators{}); err != nil || len(got.Items) != 3 {
				t.Errorf("concurrent fetch returned %+v, %v", got, err)
			}
		})
	}
}
