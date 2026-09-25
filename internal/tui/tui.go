// Package tui renders the river in the terminal with Bubble Tea.
// Every string it draws passes Sanitize, because feed text is untrusted.
package tui

import (
	"context"
	"errors"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/JoshuaFurman/openfeed/internal/config"
	"github.com/JoshuaFurman/openfeed/internal/reader"
)

// River is the reader behavior the TUI depends on; *reader.Reader implements it.
type River interface {
	Feeds() []config.Feed
	Timeline(ctx context.Context, query reader.Query) (reader.Page, error)
}

// Run shows the river until the user quits or ctx ends; launch reports the refresh before it opened.
// Signals are the caller's to handle: cancel ctx to stop, and Run returns ctx's error.
func Run(ctx context.Context, river River, launch reader.Report) error {
	return run(ctx, newModel(ctx, river, launch, openBrowser, time.Now))
}

func run(ctx context.Context, m *model, options ...tea.ProgramOption) error {
	options = append([]tea.ProgramOption{tea.WithContext(ctx), tea.WithoutSignalHandler()}, options...)
	_, err := tea.NewProgram(m, options...).Run()
	if errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil {
		return ctx.Err() // Cancellation is the caller's decision, not a program failure.
	}
	return err
}
