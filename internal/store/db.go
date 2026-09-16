package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// DB bundles the two connection pools SQLite wants: a single-connection writer
// (modernc.org/sqlite serializes writers) and a small reader pool that WAL lets
// run concurrently with writes.
type DB struct {
	Write *sql.DB
	Read  *sql.DB
}

// Open creates the data directory, opens both pools with the right pragmas,
// and verifies connectivity. The caller owns closing via Close.
func Open(dataDir, dbPath string) (*DB, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	// SQLite only reads the file; restrict to the owner.
	if err := os.Chmod(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("chmod data dir: %w", err)
	}

	dsn := "file:" + filepath.ToSlash(dbPath) +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(10000)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=synchronous(NORMAL)"

	write, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open write pool: %w", err)
	}
	write.SetMaxOpenConns(1)
	write.SetMaxIdleConns(1)

	read, err := sql.Open("sqlite", dsn)
	if err != nil {
		write.Close()
		return nil, fmt.Errorf("open read pool: %w", err)
	}
	read.SetMaxOpenConns(4)
	read.SetMaxIdleConns(4)

	db := &DB{Write: write, Read: read}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// Ping verifies both pools are usable.
func (db *DB) Ping() error {
	if err := db.Write.Ping(); err != nil {
		return fmt.Errorf("write pool ping: %w", err)
	}
	return db.Read.Ping()
}

// Close releases both pools.
func (db *DB) Close() error {
	err1 := db.Read.Close()
	err2 := db.Write.Close()
	if err1 != nil {
		return err1
	}
	return err2
}
