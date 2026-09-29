// Command openfeed is a TUI-first RSS reader.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/JoshuaFurman/openfeed/internal/config"
	"github.com/JoshuaFurman/openfeed/internal/feed"
	"github.com/JoshuaFurman/openfeed/internal/reader"
	"github.com/JoshuaFurman/openfeed/internal/store"
	"github.com/JoshuaFurman/openfeed/internal/tui"
)

func main() {
	showVersion, err := parseArgs(os.Args[1:], os.Stderr)
	switch {
	case errors.Is(err, flag.ErrHelp):
		return
	case err != nil:
		os.Exit(2)
	case showVersion:
		fmt.Println("openfeed", version())
		return
	}

	ctx, cancel := context.WithCancelCause(context.Background())
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go func() {
		received := <-signals
		signal.Stop(signals) // A second signal during cleanup uses the default behavior and exits.
		cancel(interrupted{received})
	}()
	err = run(ctx, os.Stdout, &http.Client{}, tui.Run)
	signal.Stop(signals)

	code, report := exitStatus(err, context.Cause(ctx))
	if report != nil {
		fmt.Fprintln(os.Stderr, "openfeed:", tui.Sanitize(report.Error()))
	}
	os.Exit(code)
}

const usage = "usage: openfeed [--version]\n\n  --version  print the version and exit\n"

// parseArgs reads the command line, writing usage and mistakes to stderr.
// Asking for help returns flag.ErrHelp; positional arguments are mistakes, not ignored.
func parseArgs(args []string, stderr io.Writer) (showVersion bool, err error) {
	flags := flag.NewFlagSet("openfeed", flag.ContinueOnError)
	flags.SetOutput(io.Discard) // The flag package's own messages would bypass Sanitize.
	flags.Usage = func() {}
	flags.BoolVar(&showVersion, "version", false, "")
	err = flags.Parse(args)
	if err == nil && flags.NArg() > 0 {
		err = fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(stderr, tui.Sanitize(err.Error()))
		}
		fmt.Fprint(stderr, usage)
		return false, err
	}
	return showVersion, nil
}

// version is the module version Go stamped into the build: a release tag, a pseudo-version
// naming the commit, or "(devel)" when the build carries no version control information.
func version() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "(devel)"
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

// present shows the river; tests substitute one that inspects it.
type present func(ctx context.Context, river tui.River) error

func run(ctx context.Context, out io.Writer, httpClient feed.HTTPClient, show present) (err error) {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if len(cfg.Feeds) == 0 {
		fmt.Fprintf(out, "No subscriptions yet. Add a [[feed]] block to %s\n", tui.Sanitize(cfg.FilePath()))
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
	return show(ctx, river)
}
