//go:build integration && postgres

// Package postgresdb creates isolated schemas in the explicitly supplied test DB.
package postgresdb

import (
	"context"
	"crypto/rand"
	"net/url"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"socketfabric/internal/adapters/outbound/postgres"
)

func New(t testing.TB) (string, *postgres.Store) {
	t.Helper()
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		t.Fatal("set TEST_DATABASE_URL to a disposable PostgreSQL database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, raw)
	if err != nil {
		t.Fatal("cannot connect to test PostgreSQL")
	}
	schema := "sf_" + rand.Text()
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		_ = admin.Close(ctx)
		t.Fatal("cannot create isolated schema")
	}
	_ = admin.Close(ctx)
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal("invalid test DSN")
	}
	q := u.Query()
	q.Set("search_path", quoted)
	u.RawQuery = q.Encode()
	dsn := u.String()
	store, err := postgres.Open(context.Background(), dsn, 8, time.Second, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		store.Close()
		cleanup, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		admin, err := pgx.Connect(cleanup, raw)
		if err != nil {
			t.Error("cannot reconnect for schema cleanup")
			return
		}
		defer admin.Close(cleanup)
		if _, err := admin.Exec(cleanup, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error("isolated schema cleanup failed")
		}
	})
	return dsn, store
}

// Stop/Restart act only on the runner-owned container, never on arbitrary DSNs.
func Stop(t testing.TB) {
	t.Helper()
	container := os.Getenv("TEST_POSTGRES_CONTAINER")
	if container == "" {
		t.Fatal("outage test requires runner-owned TEST_POSTGRES_CONTAINER")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "stop", "--time", "1", container).Run(); err != nil {
		t.Fatal("stop test PostgreSQL")
	}
}
func Restart(t testing.TB) {
	t.Helper()
	container := os.Getenv("TEST_POSTGRES_CONTAINER")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "start", container).Run(); err != nil {
		t.Fatal("restart test PostgreSQL")
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		conn, err := pgx.Connect(ctx, os.Getenv("TEST_DATABASE_URL"))
		if err == nil {
			_ = conn.Close(ctx)
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("test PostgreSQL recovery deadline")
		case <-ticker.C:
		}
	}
}
