package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/JoshuaFurman/openfeed/internal/reader"
	"github.com/JoshuaFurman/openfeed/internal/tui"
)

type transport func(*http.Request) (*http.Response, error)

func (f transport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// isolate points every XDG directory at a temporary tree and returns the config file path.
func isolate(t *testing.T) (configPath, logPath string) {
	t.Helper()
	root := t.TempDir()
	for env, dir := range map[string]string{"XDG_CONFIG_HOME": "config", "XDG_DATA_HOME": "data", "XDG_STATE_HOME": "state"} {
		t.Setenv(env, filepath.Join(root, dir))
	}
	return filepath.Join(root, "config", "openfeed", "config.toml"), filepath.Join(root, "state", "openfeed", "openfeed.log")
}

func writeConfig(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func offline(t *testing.T) *http.Client {
	return &http.Client{Transport: transport(func(req *http.Request) (*http.Response, error) {
		t.Errorf("unexpected request to %s", req.URL)
		return nil, errors.New("offline")
	})}
}

func never(t *testing.T) present {
	return func(context.Context, tui.River, reader.Report) error {
		t.Error("the river was presented")
		return nil
	}
}

// presented is what run handed to the TUI: the launch report and the whole river, labeled.
type presented struct {
	statuses []reader.Status
	river    []string
}

func capture(t *testing.T, into *presented) present {
	return func(ctx context.Context, river tui.River, launch reader.Report) error {
		for _, result := range launch.Feeds {
			into.statuses = append(into.statuses, result.Status)
		}
		query := reader.Query{}
		for range 100 {
			page, err := river.Timeline(ctx, query)
			if err != nil {
				return err
			}
			for _, item := range page.Items {
				into.river = append(into.river, item.Title+"@"+item.FeedName)
			}
			if page.Next == nil {
				return nil
			}
			query.Before = page.Next
		}
		t.Fatal("river did not end")
		return nil
	}
}

func TestRunFirstLaunch(t *testing.T) {
	configPath, _ := isolate(t)
	var out bytes.Buffer
	if err := run(t.Context(), &out, offline(t), never(t)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "No subscriptions yet") || !strings.Contains(out.String(), configPath) {
		t.Errorf("first launch printed %q", out.String())
	}
	if _, err := os.Stat(configPath); err != nil {
		t.Errorf("starter config not written: %v", err)
	}
}

func TestRunRefreshesThenPresentsRiver(t *testing.T) {
	configPath, logPath := isolate(t)
	writeConfig(t, configPath, `
[[feed]]
url  = "https://good.example/feed"
name = "good"

[[feed]]
url  = "https://broken.example/feed"
name = "broken"
`)
	const jsonFeed = `{"version": "https://jsonfeed.org/version/1.1", "title": "Good", "items": [
		{"id": "1", "title": "Older post", "url": "https://good.example/1", "date_published": "2025-09-01T10:00:00Z"},
		{"id": "2", "title": "Newer post", "url": "https://good.example/2", "date_published": "2025-09-02T10:00:00Z"},
		{"id": "3", "title": "Scheduled post", "url": "https://good.example/3", "date_published": "2099-01-01T00:00:00Z"}
	]}`
	client := &http.Client{Transport: transport(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "broken.example" {
			return &http.Response{StatusCode: http.StatusInternalServerError, Body: http.NoBody, Request: req}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(jsonFeed)), Request: req}, nil
	})}

	var out bytes.Buffer
	var first presented
	if err := run(t.Context(), &out, client, capture(t, &first)); err != nil {
		t.Fatal(err)
	}
	if out.String() != "Refreshing 2 feeds…\n" {
		t.Errorf("output = %q", out.String())
	}
	if !slices.Equal(first.statuses, []reader.Status{reader.Refreshed, reader.Failing}) {
		t.Errorf("launch statuses = %v", first.statuses)
	}
	// The 2099 post sorts at its first fetch, which is now.
	if want := []string{"Scheduled post@good", "Newer post@good", "Older post@good"}; !slices.Equal(first.river, want) {
		t.Errorf("river = %v, want %v", first.river, want)
	}
	if logs, err := os.ReadFile(logPath); err != nil || !strings.Contains(string(logs), "https://broken.example/feed") {
		t.Errorf("failure was not logged: %q, %v", logs, err)
	}

	var second presented
	if err := run(t.Context(), &out, client, capture(t, &second)); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(second.statuses, []reader.Status{reader.NotDue, reader.Failing}) || !slices.Equal(second.river, first.river) {
		t.Errorf("second run = %+v; want a fresh feed and the same cached river", second)
	}
}

func TestRunCanceledDuringRefresh(t *testing.T) {
	configPath, _ := isolate(t)
	writeConfig(t, configPath, "[[feed]]\nurl = \"https://slow.example/feed\"\n")
	ctx, cancel := context.WithCancel(t.Context())
	client := &http.Client{Transport: transport(func(req *http.Request) (*http.Response, error) {
		cancel()
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}
	var out bytes.Buffer
	if err := run(ctx, &out, client, never(t)); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled run returned %v", err)
	}
	unavailable := &http.Client{Transport: transport(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: http.NoBody, Request: req}, nil
	})}
	var again presented
	err := run(t.Context(), &out, unavailable, capture(t, &again))
	if err != nil || !slices.Equal(again.statuses, []reader.Status{reader.Failing}) {
		t.Errorf("database did not reopen cleanly, or the canceled round saved something: %v, %+v", err, again)
	}
}

func TestRunReturnsPresenterErrors(t *testing.T) {
	configPath, _ := isolate(t)
	writeConfig(t, configPath, "[[feed]]\nurl = \"https://a.example/feed\"\n")
	unavailable := &http.Client{Transport: transport(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: http.NoBody, Request: req}, nil
	})}
	failure := errors.New("terminal lost")
	err := run(t.Context(), io.Discard, unavailable, func(context.Context, tui.River, reader.Report) error { return failure })
	if !errors.Is(err, failure) {
		t.Errorf("run returned %v, want the presenter's error", err)
	}
}

func TestExitStatus(t *testing.T) {
	canceled := fmt.Errorf("read timeline: %w", context.Canceled)
	failure := errors.New("disk I/O error")
	for _, tc := range []struct {
		name       string
		err, cause error
		code       int
		report     error
	}{
		{"success", nil, nil, 0, nil},
		{"failure", failure, nil, 1, failure},
		{"cancellation without a signal", canceled, context.Canceled, 1, canceled},
		{"interrupt", canceled, interrupted{os.Interrupt}, 130, nil},
		{"terminate", canceled, interrupted{syscall.SIGTERM}, 143, nil},
		{"interrupt hides no other failure", errors.Join(canceled, failure), interrupted{os.Interrupt}, 130, errors.Join(canceled, failure)},
		{"interrupt after completion", nil, interrupted{os.Interrupt}, 0, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, report := exitStatus(tc.err, tc.cause)
			if code != tc.code || (report == nil) != (tc.report == nil) || (report != nil && report.Error() != tc.report.Error()) {
				t.Errorf("exitStatus = %d, %v; want %d, %v", code, report, tc.code, tc.report)
			}
		})
	}
}
