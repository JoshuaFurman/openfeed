// Command openfeed is a TUI-first RSS reader.
// Until the TUI exists, it refreshes subscriptions and prints the newest posts.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"unicode"

	"github.com/JoshuaFurman/openfeed/internal/config"
	"github.com/JoshuaFurman/openfeed/internal/feed"
	"github.com/JoshuaFurman/openfeed/internal/reader"
	"github.com/JoshuaFurman/openfeed/internal/store"
)

const previewPosts = 20

func main() {
	ctx, cancel := context.WithCancelCause(context.Background())
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go func() {
		received := <-signals
		signal.Stop(signals) // A second signal during cleanup uses the default behavior and exits.
		cancel(interrupted{received})
	}()
	err := run(ctx, os.Stdout, &http.Client{})
	signal.Stop(signals)

	code, report := exitStatus(err, context.Cause(ctx))
	if report != nil {
		fmt.Fprintln(os.Stderr, "openfeed:", terminalText(report.Error()))
	}
	os.Exit(code)
}

// interrupted records which signal canceled the run.
type interrupted struct{ os.Signal }

func (i interrupted) Error() string { return i.String() + " received" }

// exitStatus maps run's error to a process status and the error to print, if any.
// An interrupted run exits as 128 plus the signal number, quietly unless something besides cancellation failed.
func exitStatus(err, cause error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var sig interrupted
	if !errors.As(cause, &sig) {
		return 1, err
	}
	code := 130
	if number, ok := sig.Signal.(syscall.Signal); ok {
		code = 128 + int(number)
	}
	if onlyCancellation(err) {
		return code, nil
	}
	return code, err
}

// onlyCancellation reports whether every error joined into err stems from context cancellation.
func onlyCancellation(err error) bool {
	switch wrapped := err.(type) {
	case interface{ Unwrap() []error }:
		for _, e := range wrapped.Unwrap() {
			if !onlyCancellation(e) {
				return false
			}
		}
		return true
	case interface{ Unwrap() error }:
		return onlyCancellation(wrapped.Unwrap())
	default:
		return errors.Is(err, context.Canceled)
	}
}

func run(ctx context.Context, out io.Writer, httpClient feed.HTTPClient) (err error) {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if len(cfg.Feeds) == 0 {
		fmt.Fprintf(out, "No subscriptions yet. Add a [[feed]] block to %s\n", terminalText(cfg.FilePath()))
		return nil
	}

	logFile, err := os.OpenFile(cfg.LogPath(), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open log: %w", err)
	}
	defer logFile.Close()
	// Logs name feeds and failures; keep a log created elsewhere private too.
	if err := logFile.Chmod(0o600); err != nil {
		return fmt.Errorf("secure log: %w", err)
	}
	st, err := store.Open(ctx, cfg.DatabasePath())
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, st.Close()) }()
	river := reader.New(cfg.Feeds, st, feed.NewClient(httpClient), slog.New(slog.NewTextHandler(logFile, nil)))

	noun := "feeds"
	if len(cfg.Feeds) == 1 {
		noun = "feed"
	}
	fmt.Fprintf(out, "Refreshing %d %s…\n", len(cfg.Feeds), noun)
	report, err := river.Refresh(ctx)
	if err != nil {
		return err
	}
	printReport(out, report, cfg.LogPath())
	page, err := river.Timeline(ctx, reader.Query{Limit: previewPosts})
	if err != nil {
		return err
	}
	printPosts(out, page.Items)
	return nil
}

func printReport(out io.Writer, report reader.Report, logPath string) {
	counts := make(map[reader.Status]int)
	for _, result := range report.Feeds {
		counts[result.Status]++
	}
	fmt.Fprintf(out, "%d refreshed, %d already fresh, %d failing\n",
		counts[reader.Refreshed], counts[reader.NotDue], counts[reader.Failing])
	for _, result := range report.Feeds {
		if result.Status == reader.Failing {
			fmt.Fprintf(out, "  %s: %s\n", terminalText(result.Feed.Name), terminalText(result.Err.Error()))
		}
	}
	if counts[reader.Failing] > 0 {
		fmt.Fprintf(out, "Details: %s\n", terminalText(logPath))
	}
	fmt.Fprintln(out)
}

func printPosts(out io.Writer, items []reader.Item) {
	if len(items) == 0 {
		fmt.Fprintln(out, "No posts cached yet.")
		return
	}
	width := 0
	for _, item := range items {
		width = max(width, len([]rune(item.FeedName)))
	}
	indent := strings.Repeat(" ", len("2006-01-02 15:04")+2+width+2)
	for _, item := range items {
		title := item.Title
		if title == "" {
			title = "(untitled)"
		}
		fmt.Fprintf(out, "%s  %-*s  %s\n", item.EffectiveDate().Local().Format("2006-01-02 15:04"), width,
			terminalText(item.FeedName), terminalText(title))
		if item.URL != "" {
			fmt.Fprintf(out, "%s%s\n", indent, terminalText(item.URL))
		}
	}
}

// terminalText neutralizes characters that could drive the terminal or reorder displayed text.
func terminalText(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case unicode.Is(unicode.Bidi_Control, r):
			return -1
		case unicode.IsControl(r), unicode.In(r, unicode.Zl, unicode.Zp):
			return ' '
		}
		return r
	}, s)
}
