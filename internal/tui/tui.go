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
	Refresh(ctx context.Context) (reader.Report, error)
	SeenPosition(ctx context.Context) (*reader.Position, error)
	Reached(position reader.Position)
	SaveSeen(ctx context.Context) error
}

// exitSave bounds saving the reading position at exit, which runs even after a signal.
const exitSave = 5 * time.Second

// Run shows the river until the user quits or ctx ends, resuming where the reader left off and saving
// the position they reach. It refreshes at launch and on pull.
// Signals are the caller's to handle: cancel ctx to stop, and Run returns ctx's error.
func Run(ctx context.Context, river River) error {
	return run(ctx, newModel(ctx, river, openBrowser, time.Now))
}

func run(ctx context.Context, m *model, options ...tea.ProgramOption) error {
	options = append([]tea.ProgramOption{tea.WithContext(ctx), tea.WithoutSignalHandler()}, options...)
	defer func() {
		// Deferred, so even a panic stops the commands and saves the reading position.
		m.finish()
		saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), exitSave)
		defer cancel()
		m.river.SaveSeen(saveCtx) // The reader logs a failure; there is nothing more to do at exit.
	}()
	_, err := tea.NewProgram(m, options...).Run()
	if errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil {
		return ctx.Err() // Cancellation is the caller's decision, not a program failure.
	}
	return err
}
