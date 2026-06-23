package db

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

// DB wraps a *sql.DB with logai-specific query helpers.
type DB struct {
	sql *sql.DB
}

// Open opens (creating if necessary) the SQLite database at path and applies
// pragmatic connection settings for a concurrent server workload.
func Open(ctx context.Context, path string) (*DB, error) {
	// modernc.org/sqlite driver name is "sqlite".
	// _pragma options enable WAL + busy timeout for better concurrency.
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)", path)
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("db: open: %w", err)
	}

	// SQLite handles writes serially; a small pool avoids "database is locked".
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetConnMaxLifetime(time.Hour)

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(pingCtx); err != nil {
		return nil, fmt.Errorf("db: ping: %w", err)
	}

	return &DB{sql: sqlDB}, nil
}

// Migrate applies the embedded schema (idempotent).
func (d *DB) Migrate(ctx context.Context) error {
	if _, err := d.sql.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("db: migrate: %w", err)
	}
	return nil
}

// Close closes the underlying database.
func (d *DB) Close() error {
	if d.sql == nil {
		return nil
	}
	if err := d.sql.Close(); err != nil {
		return fmt.Errorf("db: close: %w", err)
	}
	return nil
}

// Ping verifies connectivity (used by /health).
func (d *DB) Ping(ctx context.Context) error {
	if err := d.sql.PingContext(ctx); err != nil {
		return fmt.Errorf("db: ping: %w", err)
	}
	return nil
}
