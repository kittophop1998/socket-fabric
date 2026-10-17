package postgres

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"time"

	"socketfabric/internal/domain"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

func migrationChecksum() string {
	body, _ := migrationFiles.ReadFile("migrations/001_management.up.sql")
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func (s *Store) CheckSchema(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	var count, version int
	var checksum string
	err := s.pool.QueryRow(ctx, "SELECT count(*),COALESCE(max(version),0),COALESCE(max(checksum),'') FROM socketfabric_schema_migrations").Scan(&count, &version, &checksum)
	if err != nil {
		return safeError(err)
	}
	if count != 1 || version != 1 || checksum != migrationChecksum() {
		return domain.ErrUnavailable
	}
	return nil
}

// Migrate is an explicit operator action. Down is destructive; callers require
// an explicit confirmation flag and tests use disposable databases only.
func (s *Store) Migrate(ctx context.Context, down bool) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return safeError(err)
	}
	defer func() {
		cleanup, done := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer done()
		_ = tx.Rollback(cleanup)
	}()
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(73665341000)"); err != nil {
		return safeError(err)
	}
	if _, err := tx.Exec(ctx, "CREATE TABLE IF NOT EXISTS socketfabric_schema_migrations (version integer PRIMARY KEY,checksum text NOT NULL,applied_at timestamptz NOT NULL DEFAULT now())"); err != nil {
		return safeError(err)
	}
	var count, version int
	var checksum string
	if err := tx.QueryRow(ctx, "SELECT count(*),COALESCE(max(version),0),COALESCE(max(checksum),'') FROM socketfabric_schema_migrations").Scan(&count, &version, &checksum); err != nil {
		return safeError(err)
	}
	if count > 1 || count == 1 && (version != 1 || checksum != migrationChecksum()) {
		return domain.ErrConflict
	}
	if down && count == 1 {
		body, _ := migrationFiles.ReadFile("migrations/001_management.down.sql")
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			return safeError(err)
		}
		if _, err := tx.Exec(ctx, "DELETE FROM socketfabric_schema_migrations WHERE version=1"); err != nil {
			return safeError(err)
		}
	} else if !down && count == 0 {
		body, _ := migrationFiles.ReadFile("migrations/001_management.up.sql")
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			return safeError(err)
		}
		if _, err := tx.Exec(ctx, "INSERT INTO socketfabric_schema_migrations (version,checksum) VALUES (1,$1)", migrationChecksum()); err != nil {
			return safeError(err)
		}
	}
	return safeError(tx.Commit(ctx))
}
