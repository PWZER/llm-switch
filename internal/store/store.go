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
	db        *DB
	Settings  *SettingsRepo
	Admin     *AdminRepo
	Providers *ProviderRepo
	Accounts  *AccountRepo
	Channels  *ChannelRepo
	Models    *ModelRepo
	Routes    *ModelRouteRepo
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
	s.Accounts = &AccountRepo{db: db}
	s.Channels = &ChannelRepo{db: db}
	s.Models = &ModelRepo{db: db}
	s.Routes = &ModelRouteRepo{db: db}
	s.APIKeys = &APIKeyRepo{db: db}
	s.Logs = &LogsRepo{db: db}
	return s, nil
}

// Close releases the underlying pools.
func (s *Store) Close() error { return s.db.Close() }

// DeleteChannel removes a channel and degrades every route target pinned to
// it into a provider-scoped auto-select target (the provider's remaining
// live endpoints take over). The model_routes table carries no FK to the
// JSON target list, so the cleanup is application-level.
func (s *Store) DeleteChannel(ctx context.Context, id int64) error {
	ch, err := s.Channels.Get(ctx, id)
	if err != nil {
		return err
	}
	if err := s.Channels.Delete(ctx, id); err != nil {
		return err
	}
	return s.Routes.RemoveChannel(ctx, id, ch.ProviderID)
}

// DeleteProvider removes a provider (accounts, channels, and their model
// rows cascade via FK) and strips every one of its targets from model route
// chains — a channel pin on a deleted provider has nothing to degrade to.
func (s *Store) DeleteProvider(ctx context.Context, id int64) error {
	channels, err := s.Channels.List(ctx)
	if err != nil {
		return err
	}
	channelIDs := make([]int64, 0, len(channels))
	for _, c := range channels {
		if c.ProviderID == id {
			channelIDs = append(channelIDs, c.ID)
		}
	}
	if err := s.Providers.Delete(ctx, id); err != nil {
		return err
	}
	return s.Routes.RemoveProvider(ctx, id, channelIDs)
}

// now returns the current unix second; overridable in tests via setClock.
func now() int64 { return time.Now().Unix() }

// execer abstracts *sql.DB / *sql.Tx for repository helpers.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}
