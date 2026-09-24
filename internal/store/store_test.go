package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"modernc.org/sqlite"
)

func openStore(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func execSQL(t *testing.T, db *sql.DB, statement string) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), statement); err != nil {
		t.Fatal(err)
	}
}

func queryInt(t *testing.T, db *sql.DB, query string) int {
	t.Helper()
	var value int
	if err := db.QueryRowContext(t.Context(), query).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func schemaVersion(t *testing.T) int {
	t.Helper()
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	return len(names)
}

func TestOpenReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "data", "openfeed.db")
	s := openStore(t, path)
	version := schemaVersion(t)
	if got := queryInt(t, s.db, "SELECT MAX(version) FROM schema_migrations"); got != version {
		t.Errorf("schema version = %d, want %d", got, version)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Dir(path))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Errorf("database directory permissions = %o, want 700", got)
		}
	}
	execSQL(t, s.db, "CREATE TABLE probe (value TEXT); INSERT INTO probe VALUES ('survives reopen')")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if err := s.db.PingContext(t.Context()); err == nil {
		t.Error("closed store still accepts queries")
	}

	again := openStore(t, path)
	var value string
	if err := again.db.QueryRowContext(t.Context(), "SELECT value FROM probe").Scan(&value); err != nil {
		t.Fatal(err)
	}
	if value != "survives reopen" || queryInt(t, again.db, "SELECT COUNT(*) FROM schema_migrations") != version {
		t.Error("reopen changed stored data or reapplied a migration")
	}
}

func TestOpenLiteralPaths(t *testing.T) {
	for _, name := range []string{"space ü #100%.db", "file:cache.db", ":memory:", "feed?_pragma=foreign_keys(0).db"} {
		t.Run(name, func(t *testing.T) {
			if runtime.GOOS == "windows" && strings.ContainsAny(name, ":?") {
				t.Skip("filename contains characters forbidden on Windows")
			}
			t.Chdir(t.TempDir())
			s := openStore(t, filepath.Join("relative", name))
			if _, err := os.Stat(filepath.Join("relative", name)); err != nil {
				t.Fatalf("literal database file not created: %v", err)
			}
			if got := queryInt(t, s.db, "PRAGMA foreign_keys"); got != 1 {
				t.Error("filename was interpreted as connection options")
			}
		})
	}
}

func TestConnectionSettingsSurviveReplacement(t *testing.T) {
	s := openStore(t, filepath.Join(t.TempDir(), "openfeed.db"))
	for _, phase := range []string{"initial", "replacement"} {
		t.Run(phase, func(t *testing.T) {
			for query, want := range map[string]int{
				"PRAGMA foreign_keys": 1,
				"PRAGMA synchronous":  2,
				"PRAGMA busy_timeout": 0,
			} {
				if got := queryInt(t, s.db, query); got != want {
					t.Errorf("%s = %d, want %d", query, got, want)
				}
			}
			var mode string
			if err := s.db.QueryRowContext(t.Context(), "PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
				t.Errorf("journal mode = %q, error = %v", mode, err)
			}
			if got := s.db.Stats().MaxOpenConnections; got != 1 {
				t.Errorf("max connections = %d, want 1", got)
			}
		})
		s.db.SetMaxIdleConns(0)
		s.db.SetMaxIdleConns(1)
	}
	execSQL(t, s.db, "CREATE TABLE parent (id INTEGER PRIMARY KEY); CREATE TABLE child (parent_id INTEGER REFERENCES parent(id))")
	if _, err := s.db.ExecContext(t.Context(), "INSERT INTO child VALUES (42)"); err == nil {
		t.Error("foreign key violation was accepted")
	}
}

func TestOpenFailures(t *testing.T) {
	for _, name := range []string{"empty path", "directory", "parent is a file", "corrupt database", "canceled", "expired deadline"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "nested", "openfeed.db")
			ctx := t.Context()
			var wantIs error
			var want string
			switch name {
			case "empty path":
				path, want = "", "path is empty"
			case "directory":
				path = root
			case "parent is a file":
				if err := os.WriteFile(filepath.Dir(path), []byte("not a directory"), 0o600); err != nil {
					t.Fatal(err)
				}
				want = "create database directory"
			case "corrupt database":
				path = filepath.Join(root, "corrupt.db")
				if err := os.WriteFile(path, []byte("not a SQLite database"), 0o600); err != nil {
					t.Fatal(err)
				}
				want = "not a database"
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				wantIs = context.Canceled
			case "expired deadline":
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer cancel()
				wantIs = context.DeadlineExceeded
			}
			s, err := Open(ctx, path)
			if s != nil {
				s.Close()
				t.Fatal("failed Open returned a store")
			}
			if err == nil || !strings.Contains(err.Error(), "open store") || !strings.Contains(err.Error(), want) {
				t.Fatalf("error = %v, want open context and %q", err, want)
			}
			var driverErr *sqlite.Error
			if errors.As(err, &driverErr) {
				t.Errorf("driver error escaped the store: %v", err)
			}
			if wantIs != nil {
				if !errors.Is(err, wantIs) {
					t.Errorf("error = %v, want %v", err, wantIs)
				}
				if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("canceled Open created directories: %v", err)
				}
			}
			if name == "corrupt database" {
				data, err := os.ReadFile(path)
				if err != nil || string(data) != "not a SQLite database" {
					t.Errorf("corrupt database was modified: %q, %v", data, err)
				}
			}
		})
	}
}

func TestOpenRejectsNewerSchemaWithoutChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "openfeed.db")
	s := openStore(t, path)
	execSQL(t, s.db, "INSERT INTO schema_migrations SELECT MAX(version) + 1 FROM schema_migrations; PRAGMA journal_mode = DELETE")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Open(t.Context(), path)
	if got != nil {
		got.Close()
		t.Fatal("newer database was accepted")
	}
	if err == nil || !strings.Contains(err.Error(), "newer than supported") || !strings.Contains(err.Error(), "upgrade openfeed") {
		t.Fatalf("error = %v, want actionable newer-schema rejection", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("rejected database was changed")
	}
}

func TestConcurrentOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "openfeed.db")
	type result struct {
		store *Store
		err   error
	}
	const count = 12
	start := make(chan struct{})
	results := make(chan result, count)
	for range count {
		go func() {
			<-start
			s, err := Open(t.Context(), path)
			results <- result{s, err}
		}()
	}
	close(start)
	var stores []*Store
	for range count {
		r := <-results
		if r.err != nil {
			t.Error(r.err)
			continue
		}
		t.Cleanup(func() {
			if err := r.store.Close(); err != nil {
				t.Error(err)
			}
		})
		stores = append(stores, r.store)
	}
	version := schemaVersion(t)
	for _, s := range stores {
		if got := queryInt(t, s.db, "SELECT COUNT(*) FROM schema_migrations"); got != version {
			t.Errorf("recorded %d migrations, want %d", got, version)
		}
	}
}

func TestWALReaderDuringWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "openfeed.db")
	writer, reader := openStore(t, path), openStore(t, path)
	execSQL(t, writer.db, "CREATE TABLE probe (value INTEGER); INSERT INTO probe VALUES (1)")
	tx, err := writer.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(t.Context(), "UPDATE probe SET value = 2"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	var value int
	if err := reader.db.QueryRowContext(ctx, "SELECT value FROM probe").Scan(&value); err != nil || value != 1 {
		t.Fatalf("reader during uncommitted write = %d, %v; want 1", value, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if got := queryInt(t, reader.db, "SELECT value FROM probe"); got != 2 {
		t.Errorf("reader after commit = %d, want 2", got)
	}
}

func TestOpenLockWait(t *testing.T) {
	for _, deadline := range []bool{true, false} {
		name := "busy limit"
		if deadline {
			name = "caller deadline"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "openfeed.db")
			s := openStore(t, path)
			tx, err := s.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			ctx := t.Context()
			if deadline {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
				defer cancel()
			}
			start := time.Now()
			got, err := Open(ctx, path)
			elapsed := time.Since(start)
			if got != nil {
				got.Close()
				t.Fatal("Open succeeded while another writer held the lock")
			}
			if deadline {
				if !errors.Is(err, context.DeadlineExceeded) || elapsed >= lockWait/2 {
					t.Errorf("deadline returned %v after %s", err, elapsed)
				}
			} else if err == nil || !strings.Contains(err.Error(), "remained busy") || elapsed < lockWait || elapsed > 2*lockWait {
				t.Errorf("busy limit returned %v after %s", err, elapsed)
			}
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			openStore(t, path)
		})
	}
}

func TestOpenWaitsForReleasedLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "openfeed.db")
	s := openStore(t, path)
	tx, err := s.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	result := make(chan error, 1)
	go func() {
		other, err := Open(t.Context(), path)
		if err == nil {
			err = other.Close()
		}
		result <- err
	}()
	select {
	case err := <-result:
		t.Fatalf("Open returned before lock release: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Errorf("Open did not recover after lock release: %v", err)
	}
}
