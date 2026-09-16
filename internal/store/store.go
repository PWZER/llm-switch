// Package store provides SQLite persistence: connection pools, embedded
// migrations, and small repositories per table group.
package store

import (
	"context"
	"database/sql"
	"time"
)

// Store aggregates the repositories. Write statements go through db.Write
// (single connection); read statements go through db.Read (WAL readers).
type Store struct {
	db *DB
	Settings  *SettingsRepo
	Admin     *AdminRepo
	Providers *ProviderRepo
	Keys      *ProviderKeyRepo
	Channels  *ChannelRepo
	Models    *ModelRepo
	Aliases   *AliasRepo
	APIKeys   *APIKeyRepo
	Logs      *LogsRepo
}

// New wraps an opened DB and runs migrations.
func New(db *DB) (*Store, error) {
	if err := db.Migrate(); err != nil {
		return nil, err
	}
	s := &Store{db: db}
	s.Settings = &SettingsRepo{db: db}
	s.Admin = &AdminRepo{db: db}
	s.Providers = &ProviderRepo{db: db}
	s.Keys = &ProviderKeyRepo{db: db}
	s.Channels = &ChannelRepo{db: db}
	s.Models = &ModelRepo{db: db}
	s.Aliases = &AliasRepo{db: db}
	s.APIKeys = &APIKeyRepo{db: db}
	s.Logs = &LogsRepo{db: db}
	return s, nil
}

// Close releases the underlying pools.
func (s *Store) Close() error { return s.db.Close() }

// now returns the current unix second; overridable in tests via setClock.
func now() int64 { return time.Now().Unix() }

// execer abstracts *sql.DB / *sql.Tx for repository helpers.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}
