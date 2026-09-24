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
	"strings"
	"syscall"
	"testing"
	"time"
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

func TestRunFirstLaunch(t *testing.T) {
	configPath, _ := isolate(t)
	var out bytes.Buffer
	if err := run(t.Context(), &out, offline(t)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "No subscriptions yet") || !strings.Contains(out.String(), configPath) {
		t.Errorf("first launch printed %q", out.String())
	}
	if _, err := os.Stat(configPath); err != nil {
		t.Errorf("starter config not written: %v", err)
	}
}

func TestRunRefreshesAndPrintsRiver(t *testing.T) {
	configPath, logPath := isolate(t)
	writeConfig(t, configPath, `
[[feed]]
url  = "https://good.example/feed"
name = "good"

[[feed]]
url  = "https://broken.example/feed"
name = "broken"
`)
	// JSON Feed, unlike XML 1.0, can carry the ESC character that starts terminal control sequences.
	const jsonFeed = `{"version": "https://jsonfeed.org/version/1.1", "title": "Good", "items": [
		{"id": "1", "title": "Older post", "url": "https://good.example/1", "date_published": "2025-09-01T10:00:00Z"},
		{"id": "2", "title": "Newer \u001b[2Jpost", "url": "https://good.example/2", "date_published": "2025-09-02T10:00:00Z"},
		{"id": "3", "title": "Scheduled \u202egnp.exe\u2028post", "url": "https://good.example/3", "date_published": "2099-01-01T00:00:00Z"}
	]}`
	client := &http.Client{Transport: transport(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "broken.example" {
			return &http.Response{StatusCode: http.StatusInternalServerError, Body: http.NoBody, Request: req}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(jsonFeed)), Request: req}, nil
	})}

	var out bytes.Buffer
	if err := run(t.Context(), &out, client); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"1 refreshed, 0 already fresh, 1 failing", "broken: fetch https://broken.example/feed: HTTP 500", "Details: " + logPath} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	newer, older := strings.Index(got, "good  Newer  [2Jpost"), strings.Index(got, "good  Older post")
	if newer < 0 || older < newer || !strings.Contains(got, "https://good.example/2") {
		t.Errorf("river is missing, out of order, or unsanitized:\n%q", got)
	}
	if strings.ContainsAny(got, "\x1b\u202e\u2028") {
		t.Error("feed text wrote a terminal escape, bidi override, or line separator")
	}
	scheduled := strings.Index(got, "good  Scheduled gnp.exe post")
	if scheduled < 0 {
		t.Fatalf("future-dated post is missing or unsanitized:\n%s", got)
	}
	line := got[strings.LastIndex(got[:scheduled], "\n")+1:]
	// A post dated 2099 is shown at its first fetch, which is now, in river order.
	if shown, err := time.ParseInLocation("2006-01-02 15:04", line[:16], time.Local); err != nil || time.Since(shown).Abs() > time.Hour {
		t.Errorf("future-dated post shown at %q, not its river date", line[:16])
	}
	if logs, err := os.ReadFile(logPath); err != nil || !strings.Contains(string(logs), "https://broken.example/feed") {
		t.Errorf("failure was not logged: %q, %v", logs, err)
	}

	out.Reset()
	if err := run(t.Context(), &out, client); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "0 refreshed, 1 already fresh, 1 failing") || !strings.Contains(out.String(), "Older post") {
		t.Errorf("second run did not show cached posts with a fresh feed:\n%s", out.String())
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
	if err := run(ctx, &out, client); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled run returned %v", err)
	}
	if strings.Contains(out.String(), "refreshed") {
		t.Errorf("canceled run printed results:\n%s", out.String())
	}
	unavailable := &http.Client{Transport: transport(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: http.NoBody, Request: req}, nil
	})}
	var again bytes.Buffer
	err := run(t.Context(), &again, unavailable)
	if err != nil || !strings.Contains(again.String(), "0 refreshed, 0 already fresh, 1 failing") {
		t.Errorf("database did not reopen cleanly, or the canceled round saved something: %v\n%s", err, again.String())
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

func TestTerminalText(t *testing.T) {
	for in, want := range map[string]string{
		"plain ASCII":                     "plain ASCII",
		"日本語 and emoji 👩\u200d💻":          "日本語 and emoji 👩\u200d💻",
		"clear\x1b[2Jscreen":              "clear [2Jscreen",
		"bell\a tab\t newline\n C1\u009b": "bell  tab  newline  C1 ",
		"evil\u202eexe.txt\u2066":         "evilexe.txt",
		"line\u2028para\u2029end":         "line para end",
	} {
		if got := terminalText(in); got != want {
			t.Errorf("terminalText(%q) = %q, want %q", in, got, want)
		}
	}
}
