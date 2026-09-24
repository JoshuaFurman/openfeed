package store

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"path"
	"strings"
)

func migrate(ctx context.Context, db *sql.DB, files fs.FS) error {
	names, err := fs.Glob(files, "migrations/*.sql")
	if err != nil {
		return fmt.Errorf("list migrations: %w", err)
	}
	if len(names) == 0 {
		return fmt.Errorf("no embedded migrations")
	}
	for i, name := range names {
		if !strings.HasPrefix(path.Base(name), fmt.Sprintf("%03d_", i+1)) {
			return fmt.Errorf("migration %q is not numbered consecutively from 001", name)
		}
	}

	var tx *sql.Tx
	if err := retryBusy(ctx, func() error {
		var err error
		tx, err = db.BeginTx(ctx, nil)
		return err
	}); err != nil {
		return fmt.Errorf("lock migrations: %w", err)
	}
	defer tx.Rollback()

	var exists bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type = 'table' AND name = 'schema_migrations')").Scan(&exists); err != nil {
		return fmt.Errorf("read migration history: %w", err)
	}
	var version, count int
	if exists {
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*), COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&count, &version); err != nil {
			return fmt.Errorf("read schema version: %w", err)
		}
	}
	if version > len(names) {
		return fmt.Errorf("database schema version %d is newer than supported version %d; upgrade openfeed", version, len(names))
	}
	if version != count {
		return fmt.Errorf("invalid migration history: %d versions recorded through version %d", count, version)
	}

	for i := version; i < len(names); i++ {
		script, err := fs.ReadFile(files, names[i])
		if err != nil {
			return fmt.Errorf("read migration %s: %w", names[i], err)
		}
		if _, err := tx.ExecContext(ctx, string(script)); err != nil {
			return fmt.Errorf("apply migration %s: %w", names[i], err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations (version) VALUES (?)", i+1); err != nil {
			return fmt.Errorf("record migration %s: %w", names[i], err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migrations: %w", err)
	}
	return nil
}
