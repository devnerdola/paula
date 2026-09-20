// Package store keeps the conversation, the media, the turn log and the
// settings Paula saves, in one SQLite database.
package store

import (
	"bytes"
	"compress/gzip"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// applicationID stamps the databases Paula creates: "PAUL" in ASCII.
const applicationID = 0x5041554C

// File is the database inside the data directory.
const File = "paula.db"

//go:embed migrations/*.sql
var migrationFiles embed.FS

// migration is one schema change, in a file named <version>_<what it does>.sql.
type migration struct {
	version int
	name    string
	sql     string
}

// migrations are every schema change Paula carries, oldest first.
var migrations = loadMigrations(migrationFiles, "migrations")

// schemaVersion is the version a set of migrations brings a database to, which
// is what user_version holds once they have all run.
func schemaVersion(list []migration) int { return list[len(list)-1].version }

// loadMigrations reads the migrations of a directory, and panics on a file
// named anything else: the ones Paula carries are fixed when it is built.
func loadMigrations(fsys fs.FS, dir string) []migration {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil || len(entries) == 0 {
		panic("store: no migrations under " + dir)
	}
	out := make([]migration, 0, len(entries))
	for _, e := range entries {
		number, _, ok := strings.Cut(strings.TrimSuffix(e.Name(), ".sql"), "_")
		version, err := strconv.Atoi(number)
		if !ok || err != nil || version < 1 {
			panic("store: " + dir + "/" + e.Name() + " is not named <version>_<name>.sql")
		}
		statements, err := fs.ReadFile(fsys, dir+"/"+e.Name())
		if err != nil {
			panic(err)
		}
		out = append(out, migration{version: version, name: e.Name(), sql: string(statements)})
	}
	slices.SortFunc(out, func(a, b migration) int { return a.version - b.version })
	return out
}

type Store struct {
	db  *sql.DB // writes, one connection: SQLite takes one writer
	ro  *sql.DB // reads, which WAL serves beside the writer
	dir string
}

// Open opens, and creates when it is missing, the database in the data
// directory. It refuses a database Paula did not create.
func Open(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	return openStore(dataDir, true, migrations)
}

// ErrNoConversation says the data directory holds nothing yet, which is what a
// command that only reads finds before the first run, rather than a failure.
var ErrNoConversation = errors.New("holds no conversation yet")

// Read opens a database that is already there, and says so when it is not,
// rather than leaving an empty one behind.
func Read(dataDir string) (*Store, error) {
	return openStore(dataDir, false, migrations)
}

func openStore(dataDir string, writable bool, list []migration) (*Store, error) {
	path, err := filepath.Abs(filepath.Join(dataDir, File))
	if err != nil {
		return nil, err
	}
	// A database that is already there is read before anything is written to
	// it, since setting the journal mode is itself a write.
	if err := inspect(path, dataDir, writable, schemaVersion(list)); err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", file(path)+
		"?_pragma=busy_timeout(5000)"+
		"&_pragma=journal_mode(WAL)"+
		"&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)

	s := &Store{db: db, dir: dataDir}
	if err := s.migrate(list); err != nil {
		db.Close()
		return nil, err
	}

	s.ro, err = sql.Open("sqlite", file(path)+
		"?_pragma=busy_timeout(5000)"+
		"&_pragma=foreign_keys(1)")
	if err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// file is the database as a URI, with whatever a directory name holds escaped:
// a question mark in a path would otherwise read as the pragmas.
func file(path string) string {
	return (&url.URL{Scheme: "file", Path: path}).String()
}

// inspect reads what a database says about itself without writing anything, so
// a file Paula cannot use is left exactly as it was.
func inspect(path, dataDir string, writable bool, newest int) error {
	empty := fmt.Errorf("%s %w", dataDir, ErrNoConversation)
	if _, err := os.Stat(path); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if writable {
			return nil
		}
		return empty
	}
	db, err := sql.Open("sqlite", file(path)+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer db.Close()

	var id, tables, version int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_schema`).Scan(&tables); err != nil {
		return err
	}
	if err := db.QueryRow(`PRAGMA application_id`).Scan(&id); err != nil {
		return err
	}
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	switch {
	case tables == 0 && !writable:
		return empty
	case tables == 0:
		return nil
	case id != applicationID:
		return fmt.Errorf("%s holds a database Paula did not create; point data_dir at a new directory", dataDir)
	case version > newest:
		return fmt.Errorf("%s holds a database of version %d, and this Paula knows up to %d; it was made by a newer Paula",
			dataDir, version, newest)
	case version < newest && !writable:
		return fmt.Errorf("%s holds a database of version %d, and this Paula keeps version %d; run paula serve to bring it up to date",
			dataDir, version, newest)
	}
	return nil
}

// migrate applies the migrations a database has not had yet. inspect has
// already said it is one Paula may write to.
func (s *Store) migrate(list []migration) error {
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	for _, m := range list {
		if m.version <= version {
			continue
		}
		if err := s.apply(m); err != nil {
			return fmt.Errorf("%s: %w", m.name, err)
		}
	}
	return nil
}

// apply runs one migration, its stamp and the version it brings the database
// to together, so a database is always at a version one of them left it at.
func (s *Store) apply(m migration) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(m.sql); err != nil {
		return err
	}
	_, err = tx.Exec(fmt.Sprintf(`PRAGMA application_id = %d; PRAGMA user_version = %d`, applicationID, m.version))
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Close() error {
	return errors.Join(s.ro.Close(), s.db.Close())
}

// Dir is the data directory the database lives in.
func (s *Store) Dir() string { return s.dir }

// ErrNotFound is returned when a row a caller named does not exist.
var ErrNotFound = errors.New("not found")

func ns(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixNano()
}

func at(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return time.Unix(0, v.Int64)
}

// id is a row id that may be null, as a number, where null reads as zero.
func id(v sql.NullInt64) int64 {
	if !v.Valid {
		return 0
	}
	return v.Int64
}

// nullID is a row id as a column, where zero is written as null: a row that
// points at nothing holds null, not the id zero.
func nullID[T ~int64](v T) any {
	if v == 0 {
		return nil
	}
	return int64(v)
}

func compress(b []byte) (any, error) {
	if b == nil {
		return nil, nil
	}
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(b); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func decompress(b []byte) ([]byte, error) {
	if b == nil {
		return nil, nil
	}
	r, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}
