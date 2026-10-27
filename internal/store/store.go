// Package store owns triage-bot's SQLite database: the source of truth for
// what has been triaged, what each verdict was, what the maintainer did about
// it, and everything else we know about argo-workflows items — notes, links
// between items, and security advisories.
//
// Beads are disposable work tickets; this database is not. Everything durable
// lives here, so a lost beads database costs at most the in-flight work.
//
// The daemon, the CLI, `review` and the MCP server all open the same file at
// once. WAL mode lets readers carry on while one writer works, and every write
// transaction is BEGIN IMMEDIATE, so a read-modify-write takes the write lock
// before it reads and two writers cannot clobber each other.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// busyTimeoutMS is how long a statement waits on another process's lock before
// failing. Writes are short, so anything near this means something is wrong.
const busyTimeoutMS = 10_000

// ErrNotFound is returned when a lookup matches nothing.
var ErrNotFound = errors.New("not found")

// Store is an open database.
type Store struct {
	db *sql.DB
	// Path is the database file.
	Path string
}

// DefaultPath is where the database lives unless --db says otherwise: under
// the user's data directory, outside any checkout so it cannot be committed by
// accident.
func DefaultPath() (string, error) {
	dir := os.Getenv("XDG_DATA_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("failed to find the home directory: %w", err)
		}
		dir = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dir, "triage-bot", "argo-workflows.db"), nil
}

// Open opens the database at path, creating it (mode 0600, in a 0700 directory
// if that is missing too) and bringing its schema up to date.
//
// A database whose schema is newer than this build understands is refused, so
// an older binary cannot write rows a newer one would misread.
func Open(ctx context.Context, path string) (*Store, error) {
	if strings.ContainsRune(path, '?') {
		return nil, fmt.Errorf("database path %q may not contain '?'", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("failed to create the database directory: %w", err)
	}
	// Create the file ourselves so it starts private, and keep it private if
	// it was loosened: it holds advisory notes. SQLite gives the -wal and
	// -shm files the same mode as the database.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // path is from a command-line flag
	if err != nil {
		return nil, fmt.Errorf("failed to open the database file: %w", err)
	}
	_ = f.Close()
	if cerr := os.Chmod(path, 0o600); cerr != nil {
		return nil, fmt.Errorf("failed to make the database private: %w", cerr)
	}

	dsn := fmt.Sprintf("%s?_pragma=busy_timeout(%d)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_txlock=immediate",
		path, busyTimeoutMS)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open %s: %w", path, err)
	}

	s := &Store{db: db, Path: path}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database.
func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("failed to close the database: %w", err)
	}
	return nil
}

// Tx is one transaction. Its methods are the only way rows are read or
// written, so the rules about what a row may say live in one place.
type Tx struct {
	tx *sql.Tx
}

// Update runs fn in a write transaction, committing if it returns nil and
// rolling back otherwise. The transaction takes the write lock as it begins,
// so everything fn reads is current until it commits.
func (s *Store) Update(ctx context.Context, fn func(*Tx) error) error {
	return s.run(ctx, &sql.TxOptions{}, fn)
}

// View runs fn in a read transaction: a consistent snapshot that never blocks
// a writer.
func (s *Store) View(ctx context.Context, fn func(*Tx) error) error {
	return s.run(ctx, &sql.TxOptions{ReadOnly: true}, fn)
}

func (s *Store) run(ctx context.Context, opts *sql.TxOptions, fn func(*Tx) error) error {
	tx, err := s.db.BeginTx(ctx, opts)
	if err != nil {
		return fmt.Errorf("failed to begin a transaction: %w", err)
	}
	if err := fn(&Tx{tx: tx}); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit: %w", err)
	}
	return nil
}

// queryAll runs a query and scans every row, closing the rows however it
// ends. what names the rows in errors.
func queryAll[T any](ctx context.Context, tx *Tx, what string, scan func(scanner) (T, error),
	query string, args ...any) ([]T, error) {
	rows, err := tx.tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", what, err)
	}
	defer func() { _ = rows.Close() }()

	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", what, err)
	}
	return out, nil
}

// SchemaVersion is the newest schema this build understands: the number of
// embedded migrations.
func SchemaVersion() int {
	names, err := migrationNames()
	if err != nil {
		panic(err) // the embedded directory is fixed at build time
	}
	return len(names)
}

func migrationNames() ([]string, error) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return nil, fmt.Errorf("failed to read embedded migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	slices.Sort(names)
	return names, nil
}

// migrate applies whichever migrations the database has not had, recording
// the schema version in SQLite's user_version. It runs under the write lock,
// so two processes opening a new database at once cannot both migrate it.
func (s *Store) migrate(ctx context.Context) error {
	names, err := migrationNames()
	if err != nil {
		return err
	}
	return s.Update(ctx, func(tx *Tx) error {
		var version int
		if err := tx.tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
			return fmt.Errorf("failed to read the schema version: %w", err)
		}
		if version > len(names) {
			return fmt.Errorf(
				"database is schema version %d but this build understands only %d - upgrade triage-bot",
				version, len(names))
		}
		for i := version; i < len(names); i++ {
			body, err := migrationFiles.ReadFile("migrations/" + names[i])
			if err != nil {
				return fmt.Errorf("failed to read migration %s: %w", names[i], err)
			}
			if _, err := tx.tx.ExecContext(ctx, string(body)); err != nil {
				return fmt.Errorf("failed to apply migration %s: %w", names[i], err)
			}
		}
		// PRAGMA takes no bound parameters; the value is an int we computed.
		if _, err := tx.tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", len(names))); err != nil {
			return fmt.Errorf("failed to record the schema version: %w", err)
		}
		return nil
	})
}
