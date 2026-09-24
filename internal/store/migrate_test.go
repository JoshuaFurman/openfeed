package store

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// openMigrationStore isolates migration tests from the evolving domain schema.
func openMigrationStore(t *testing.T, path string) *Store {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := migrate(t.Context(), db, migrationFiles(t)); err != nil {
		t.Fatal(err)
	}
	return s
}

func migrationFiles(t *testing.T, scripts ...string) fstest.MapFS {
	t.Helper()
	initial, err := fs.ReadFile(migrations, "migrations/001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	files := fstest.MapFS{"migrations/001_init.sql": &fstest.MapFile{Data: initial}}
	for i, script := range scripts {
		files[fmt.Sprintf("migrations/%03d_test.sql", i+2)] = &fstest.MapFile{Data: []byte(script)}
	}
	return files
}

func TestMigrateUpgrade(t *testing.T) {
	s := openMigrationStore(t, filepath.Join(t.TempDir(), "openfeed.db"))
	files := migrationFiles(t,
		"CREATE TABLE probe (value INTEGER); INSERT INTO probe VALUES (42)",
		"ALTER TABLE probe ADD COLUMN label TEXT NOT NULL DEFAULT 'retained'",
	)
	for range 2 {
		if err := migrate(t.Context(), s.db, files); err != nil {
			t.Fatal(err)
		}
		if got := queryInt(t, s.db, "SELECT MAX(version) FROM schema_migrations"); got != 3 {
			t.Errorf("version = %d, want 3", got)
		}
		if got := queryInt(t, s.db, "SELECT COUNT(*) FROM probe WHERE value = 42 AND label = 'retained'"); got != 1 {
			t.Errorf("upgrade lost data or replayed an applied migration: %d rows", got)
		}
	}
}

func TestMigrateRollback(t *testing.T) {
	for _, failure := range []string{"SQL", "version record", "canceled", "read file"} {
		t.Run(failure, func(t *testing.T) {
			s := openMigrationStore(t, filepath.Join(t.TempDir(), "openfeed.db"))
			execSQL(t, s.db, "CREATE TABLE existing (value INTEGER); INSERT INTO existing VALUES (7)")
			files := migrationFiles(t,
				"UPDATE existing SET value = 8; CREATE TABLE probe (value INTEGER)",
				"INSERT INTO probe VALUES (9)",
			)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var source fs.FS = files
			switch failure {
			case "SQL":
				files["migrations/003_test.sql"].Data = []byte("INSERT INTO probe VALUES (9); INVALID SQL")
			case "version record":
				files["migrations/003_test.sql"].Data = []byte("INSERT INTO schema_migrations VALUES (3)")
			case "canceled":
				source = cancelOnRead{FS: files, cancel: cancel, name: "migrations/003_test.sql"}
			case "read file":
				files["migrations/003_test.sql"].Mode = fs.ModeDir
			}
			if err := migrate(ctx, s.db, source); err == nil {
				t.Fatal("broken migration succeeded")
			}
			if got := queryInt(t, s.db, "SELECT MAX(version) FROM schema_migrations"); got != 1 {
				t.Errorf("rollback left version %d, want 1", got)
			}
			if got := queryInt(t, s.db, "SELECT COUNT(*) FROM sqlite_schema WHERE name = 'probe'"); got != 0 {
				t.Error("rollback left a table from an earlier pending migration")
			}
			if got := queryInt(t, s.db, "SELECT value FROM existing"); got != 7 {
				t.Errorf("rollback changed existing data to %d", got)
			}
			if err := migrate(t.Context(), s.db, migrationFiles(t, "CREATE TABLE probe (value INTEGER)")); err != nil {
				t.Errorf("could not retry after failed migration: %v", err)
			}
		})
	}
}

type cancelOnRead struct {
	fs.FS
	cancel context.CancelFunc
	name   string
}

func (f cancelOnRead) ReadFile(name string) ([]byte, error) {
	data, err := fs.ReadFile(f.FS, name)
	if name == f.name {
		f.cancel()
	}
	return data, err
}

func TestMigrateFirstRunRollback(t *testing.T) {
	s := openMigrationStore(t, filepath.Join(t.TempDir(), "openfeed.db"))
	execSQL(t, s.db, "DROP TABLE schema_migrations")
	if err := migrate(t.Context(), s.db, migrationFiles(t, "INVALID SQL")); err == nil {
		t.Fatal("broken migration succeeded")
	}
	if got := queryInt(t, s.db, "SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table'"); got != 0 {
		t.Errorf("failed first migration left %d tables", got)
	}
	if err := migrate(t.Context(), s.db, migrations); err != nil {
		t.Errorf("could not migrate after rollback: %v", err)
	}
}

func TestMigrateRejectsInvalidSequence(t *testing.T) {
	for _, failure := range []string{"no files", "file numbering gap", "history gap"} {
		t.Run(failure, func(t *testing.T) {
			s := openMigrationStore(t, filepath.Join(t.TempDir(), "openfeed.db"))
			files := migrationFiles(t, "CREATE TABLE probe (value INTEGER)")
			want := "consecutively"
			switch failure {
			case "no files":
				files = fstest.MapFS{}
				want = "no embedded migrations"
			case "file numbering gap":
				delete(files, "migrations/001_init.sql")
			case "history gap":
				execSQL(t, s.db, "UPDATE schema_migrations SET version = 2")
				want = "invalid migration history"
			}
			if err := migrate(t.Context(), s.db, files); err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("error = %v, want %q", err, want)
			}
			if got := queryInt(t, s.db, "SELECT COUNT(*) FROM sqlite_schema WHERE name = 'probe'"); got != 0 {
				t.Error("invalid migration sequence changed schema")
			}
		})
	}
}
