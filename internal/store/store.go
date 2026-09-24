// Package store persists openfeed's cached reader state independently of fetching and presentation.
// Subscription intent and labels remain config-owned.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const (
	lockWait          = 5 * time.Second
	lockRetryInterval = 10 * time.Millisecond
)

//go:embed migrations/*.sql
var migrations embed.FS

// Store owns cached reader state at one database path.
// Construct it with Open; a Store is safe for concurrent use.
type Store struct {
	db *sql.DB
}

// Open creates missing parent directories and brings the database up to the supported schema.
// The path is a filesystem path, not a SQLite URI; a newer schema is rejected without downgrading it.
func Open(ctx context.Context, path string) (_ *Store, err error) {
	defer func() {
		if err != nil {
			// Keep driver errors private while preserving caller cancellation.
			cause := ctx.Err()
			if cause == nil {
				cause = errors.New(err.Error())
			}
			err = fmt.Errorf("open store %q: %w", path, cause)
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if path == "" {
		return nil, errors.New("database path is empty")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}

	uriPath := filepath.ToSlash(abs)
	if !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath // Windows drive letters need a leading slash in file URIs.
	}
	dsn := url.URL{Scheme: "file", Path: uriPath}
	// Exclusive locking protects first-run commits before WAL exists; WAL readers remain concurrent.
	params := url.Values{"_txlock": {"exclusive"}, "_pragma": {"foreign_keys(1)", "synchronous(FULL)"}}
	dsn.RawQuery = params.Encode()
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	defer func() {
		if err != nil {
			db.Close()
		}
	}()

	if err := migrate(ctx, db, migrations); err != nil {
		return nil, err
	}
	if err := retryBusy(ctx, func() error {
		var mode string
		if err := db.QueryRowContext(ctx, "PRAGMA journal_mode = WAL").Scan(&mode); err != nil {
			return err
		}
		if mode != "wal" {
			return fmt.Errorf("database did not enable WAL (journal mode %q)", mode)
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("enable concurrent reading: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the database after active operations finish.
// Repeated calls are harmless.
func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("close store: %s", err)
	}
	return nil
}

// retryBusy is only for lock acquisition, never for replaying transaction contents.
func retryBusy(ctx context.Context, acquire func() error) error {
	timeout := time.NewTimer(lockWait)
	defer timeout.Stop()
	retry := time.NewTicker(lockRetryInterval)
	defer retry.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := acquire()
		var sqliteErr *sqlite.Error
		if !errors.As(err, &sqliteErr) || sqliteErr.Code()&0xff != sqlite3.SQLITE_BUSY {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout.C:
			return fmt.Errorf("database remained busy for %s", lockWait)
		case <-retry.C:
		}
	}
}
