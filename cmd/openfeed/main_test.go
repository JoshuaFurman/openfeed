package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/JoshuaFurman/openfeed/internal/config"
	"github.com/JoshuaFurman/openfeed/internal/reader"
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

// presented is what the TUI would see: a refresh report and the whole river, labeled.
type presented struct {
	statuses []reader.Status
	river    []string
}

// capture exercises the river run hands to the TUI: a refresh through the configured client, then a read.
// The TUI's own launch refresh is covered by the tui tests; these check run's wiring.
func capture(t *testing.T, into *presented) present {
	return func(ctx context.Context, river *reader.Reader, _ *slog.Logger) error {
		launch, err := river.Refresh(ctx)
		if err != nil {
			return err
		}
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

// A first launch opens the empty river, and a feed added there is appended to the starter config.
func TestRunFirstLaunchSubscribes(t *testing.T) {
	configPath, _ := isolate(t)
	const jsonFeed = `{"version": "https://jsonfeed.org/version/1.1", "title": "New", "items": [
		{"id": "1", "title": "First post", "url": "https://new.example/1", "date_published": "2025-09-01T10:00:00Z"}
	]}`
	client := &http.Client{Transport: transport(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(jsonFeed)), Request: req}, nil
	})}
	var after presented
	err := run(t.Context(), client, func(ctx context.Context, river *reader.Reader, _ *slog.Logger) error {
		if feeds := river.Feeds(); len(feeds) != 0 {
			t.Errorf("first launch feeds = %+v", feeds)
		}
		if _, err := river.Subscribe(ctx, config.Feed{URL: "https://new.example/feed", Name: "new"}); err != nil {
			return err
		}
		return capture(t, &after)(ctx, river, nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"First post@new"}; !slices.Equal(after.river, want) {
		t.Errorf("river = %v, want %v", after.river, want)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); !strings.HasPrefix(got, "# openfeed configuration") ||
		!strings.HasSuffix(got, "\n\n[[feed]]\nurl = \"https://new.example/feed\"\nname = \"new\"\n") {
		t.Errorf("config.toml = %q, want the starter config with the feed appended", got)
	}
}

func TestRunWiresClientStoreAndLog(t *testing.T) {
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

	var first presented
	if err := run(t.Context(), client, capture(t, &first)); err != nil {
		t.Fatal(err)
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
	if err := run(t.Context(), client, capture(t, &second)); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(second.statuses, []reader.Status{reader.NotDue, reader.Failing}) || !slices.Equal(second.river, first.river) {
		t.Errorf("second run = %+v; want a fresh feed and the same cached river", second)
	}
}

func TestRunReturnsCancellationAndLeavesTheStoreReopenable(t *testing.T) {
	configPath, _ := isolate(t)
	writeConfig(t, configPath, "[[feed]]\nurl = \"https://slow.example/feed\"\n")
	ctx, cancel := context.WithCancel(t.Context())
	client := &http.Client{Transport: transport(func(req *http.Request) (*http.Response, error) {
		cancel()
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}
	var canceled presented
	if err := run(ctx, client, capture(t, &canceled)); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled run returned %v", err)
	}
	unavailable := &http.Client{Transport: transport(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: http.NoBody, Request: req}, nil
	})}
	var again presented
	err := run(t.Context(), unavailable, capture(t, &again))
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
	err := run(t.Context(), unavailable, func(context.Context, *reader.Reader, *slog.Logger) error { return failure })
	if !errors.Is(err, failure) {
		t.Errorf("run returned %v, want the presenter's error", err)
	}
}

func TestParseArgs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		want    command
		wantErr bool
		help    bool
		stderr  string
	}{
		{"no arguments", nil, command{}, false, false, ""},
		{"double-dash version", []string{"--version"}, command{version: true}, false, false, ""},
		{"single-dash version", []string{"-version"}, command{version: true}, false, false, ""},
		{"help", []string{"--help"}, command{}, true, true, "usage: openfeed"},
		{"short help", []string{"-h"}, command{}, true, true, "usage: openfeed"},
		{"unknown flag", []string{"--verbose"}, command{}, true, false, "flag provided but not defined: -verbose\nusage: openfeed"},
		{"serve", []string{"serve"}, command{serve: true, addr: ":80"}, false, false, ""},
		{"serve on an address", []string{"serve", "--addr", "127.0.0.1:8080"}, command{serve: true, addr: "127.0.0.1:8080"}, false, false, ""},
		{"serve help", []string{"serve", "-h"}, command{}, true, true, "openfeed serve [--addr host:port]"},
		{"serve with version", []string{"serve", "--version"}, command{}, true, false, "flag provided but not defined: -version"},
		{"addr without serve", []string{"--addr", ":80"}, command{}, true, false, "flag provided but not defined: -addr"},
		{"serve after a flag", []string{"--version", "serve"}, command{}, true, false, `unexpected argument "serve"`},
		{"serve with an argument", []string{"serve", "now"}, command{}, true, false, `unexpected argument "now"`},
		{"unknown subcommand", []string{"sync"}, command{}, true, false, `unexpected argument "sync"`},
		{"escape sequence in an argument", []string{"\x1b[2J"}, command{}, true, false, `unexpected argument "\x1b[2J"`},
		{"escape sequence in a flag", []string{"--\x1b[2J"}, command{}, true, false, "flag provided but not defined: - [2J"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			cmd, err := parseArgs(tc.args, &stderr)
			if cmd != tc.want || (err != nil) != tc.wantErr || errors.Is(err, flag.ErrHelp) != tc.help {
				t.Errorf("parseArgs(%q) = %+v, %v", tc.args, cmd, err)
			}
			if (tc.stderr == "" && stderr.Len() > 0) || !strings.Contains(stderr.String(), tc.stderr) {
				t.Errorf("parseArgs(%q) wrote %q, want %q", tc.args, stderr.String(), tc.stderr)
			}
			if strings.ContainsRune(stderr.String(), '\x1b') {
				t.Errorf("parseArgs(%q) wrote an escape sequence: %q", tc.args, stderr.String())
			}
		})
	}
}

// serve listens, announces the address, serves the river, and stops when canceled.
func TestServe(t *testing.T) {
	configPath, _ := isolate(t)
	writeConfig(t, configPath, "[[feed]]\nurl = \"https://a.example/feed\"\nname = \"alpha\"\n")
	const jsonFeed = `{"version": "https://jsonfeed.org/version/1.1", "items": [
		{"id": "1", "title": "Served post", "url": "https://a.example/1", "date_published": "2025-09-01T10:00:00Z"}
	]}`
	client := &http.Client{Transport: transport(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(jsonFeed)), Request: req}, nil
	})}
	ctx, cancel := context.WithCancel(t.Context())
	announced := make(chan string, 1)
	done := make(chan error, 1)
	go func() { done <- run(ctx, client, serve("127.0.0.1:0", lineWriter(announced))) }()

	var url string
	select {
	case line := <-announced:
		url = "http://" + strings.TrimSpace(strings.TrimPrefix(line, "openfeed listening on "))
	case err := <-done:
		t.Fatalf("serve stopped before listening: %v", err)
	}
	refresh, err := http.Post(url+"/refresh", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	refresh.Body.Close()
	page, err := http.Get(url + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(page.Body)
	page.Body.Close()
	if !strings.Contains(string(body), "Served post") || !strings.Contains(string(body), "alpha") {
		t.Errorf("page does not show the refreshed river:\n%s", body)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("serve returned %v, want cancellation", err)
	}
}

func TestServeReportsListenFailures(t *testing.T) {
	configPath, _ := isolate(t)
	writeConfig(t, configPath, "")
	var out bytes.Buffer
	if err := run(t.Context(), http.DefaultClient, serve("256.0.0.1:0", &out)); err == nil || out.Len() > 0 {
		t.Errorf("serve on an invalid address = %v, wrote %q", err, out.String())
	}
}

// lineWriter sends each write to lines.
type lineWriter chan<- string

func (w lineWriter) Write(p []byte) (int, error) {
	w <- string(p)
	return len(p), nil
}

func TestVersionIsNeverBlank(t *testing.T) {
	if version() == "" {
		t.Error("version is blank")
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
