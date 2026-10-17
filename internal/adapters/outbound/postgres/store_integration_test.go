//go:build integration && postgres

package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"socketfabric/internal/adapters/outbound/auth"
	"socketfabric/internal/application/ports"
	"socketfabric/internal/application/usecase"
	"socketfabric/internal/domain"
	"socketfabric/internal/testsupport/postgresdb"
)

func TestManagementConcurrentCredentialRevocationAndLogout(t *testing.T) {
	_, store := postgresdb.New(t)
	ctx := context.Background()
	if err := store.Migrate(ctx, false); err != nil {
		t.Fatal(err)
	}
	m, err := usecase.NewManagement(store, auth.NewPasswords(), nil, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Minute, 4, nil)
	if err != nil {
		t.Fatal(err)
	}
	password := "private concurrent operation passphrase"
	if _, err := m.ProvisionOperator(ctx, "admin@example.com", password, domain.RoleAdmin, true); err != nil {
		t.Fatal(err)
	}
	login, err := m.Login(ctx, "admin@example.com", password, "")
	if err != nil {
		t.Fatal(err)
	}
	actor, err := m.AuthenticateOperator(ctx, login.Token)
	if err != nil {
		t.Fatal(err)
	}
	for _, project := range []string{"project-a", "project-b"} {
		if _, err := m.CreateProject(ctx, actor, domain.Project{ID: project, Name: project}, ""); err != nil {
			t.Fatal(err)
		}
	}
	key, err := m.CreateKey(ctx, actor, "project-a", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	peer, err := m.CreateKey(ctx, actor, "project-b", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.RevokeKey(ctx, actor, "project-b", key.Credential.ID, ""); !errors.Is(err, domain.ErrResourceNotFound) {
		t.Fatal("cross-project key ID accepted")
	}
	start, revoked := make(chan struct{}), make(chan struct{})
	errorsFound := make(chan error, 9)
	var workers sync.WaitGroup
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			for range 24 {
				select {
				case <-revoked:
					if _, err := m.Authenticate(ctx, key.Key); !errors.Is(err, domain.ErrUnauthorized) {
						errorsFound <- fmt.Errorf("key authenticated after committed revocation")
						return
					}
				default:
					scope, err := m.Authenticate(ctx, key.Key)
					if err != nil && !errors.Is(err, domain.ErrUnauthorized) || err == nil && scope.ProjectID != "project-a" {
						errorsFound <- fmt.Errorf("incorrect in-flight credential result")
						return
					}
				}
			}
		}()
	}
	workers.Add(1)
	go func() {
		defer workers.Done()
		<-start
		if err := m.RevokeKey(ctx, actor, "project-a", key.Credential.ID, ""); err != nil {
			errorsFound <- err
		}
		close(revoked)
	}()
	close(start)
	workers.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Error(err)
	}
	for range 8 {
		if _, err := m.Authenticate(ctx, key.Key); !errors.Is(err, domain.ErrUnauthorized) {
			t.Fatal("post-revoke request authenticated")
		}
		if scope, err := m.Authenticate(ctx, peer.Key); err != nil || scope.ProjectID != "project-b" {
			t.Fatal("revocation affected another project")
		}
	}
	if err := m.Logout(ctx, actor, ""); err != nil {
		t.Fatal(err)
	}
	workers = sync.WaitGroup{}
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if _, err := m.AuthenticateOperator(ctx, login.Token); !errors.Is(err, domain.ErrUnauthorized) {
				t.Error("post-logout session authenticated")
			}
		}()
	}
	workers.Wait()
}

func TestManagementMigrationsAndRollback(t *testing.T) {
	_, store := postgresdb.New(t)
	ctx := context.Background()
	if err := store.CheckSchema(ctx); err == nil {
		t.Fatal("unmigrated schema reported ready")
	}
	for range 2 {
		if err := store.Migrate(ctx, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.CheckSchema(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	project := domain.Project{ID: "rollback-project", Name: "Rollback", Status: domain.ProjectEnabled, CreatedAt: now, UpdatedAt: now}
	err := store.Transact(ctx, func(q ports.ManagementQueries) error {
		if err := q.CreateProject(ctx, project, 128); err != nil {
			return err
		}
		actor := "nonexistent-operator"
		return q.AppendAudit(ctx, domain.AuditLog{ID: "audit", ActorID: &actor, Action: "project.created", ResourceType: "project", ResourceID: project.ID, Timestamp: now, Metadata: json.RawMessage(`{}`)})
	})
	if !errors.Is(err, domain.ErrInvalid) {
		t.Fatal("expected real audit FK failure")
	}
	if _, err := store.Project(ctx, project.ID); !errors.Is(err, domain.ErrResourceNotFound) {
		t.Fatal("failed transaction left project")
	}
	if err := store.Transact(ctx, func(q ports.ManagementQueries) error { return q.CreateProject(ctx, project, 128) }); err != nil {
		t.Fatal(err)
	}
	if err := store.Transact(ctx, func(q ports.ManagementQueries) error { return q.CreateProject(ctx, project, 128) }); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("duplicate ID was not rejected")
	}
	if err := store.Migrate(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := store.CheckSchema(ctx); err == nil {
		t.Fatal("down schema was ready")
	}
	if err := store.Migrate(ctx, false); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Project(ctx, project.ID); !errors.Is(err, domain.ErrResourceNotFound) {
		t.Fatal("rollback/reapply retained deleted project")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := store.Project(canceled, "project"); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation not propagated")
	}
}

func TestManagementPersistenceAuthCredentialsAndAudit(t *testing.T) {
	_, store := postgresdb.New(t)
	ctx := context.Background()
	if err := store.Migrate(ctx, false); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	m, err := usecase.NewManagement(store, auth.NewPasswords(), nil, logger, time.Minute, 2, []string{"legacy"})
	if err != nil {
		t.Fatal(err)
	}
	password := "a private integration passphrase"
	if _, err := m.ProvisionOperator(ctx, "viewer@example.com", password, domain.RoleViewer, false); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("operator creation bypassed initial bootstrap")
	}
	admin, err := m.ProvisionOperator(ctx, "admin@example.com", password, domain.RoleAdmin, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.ProvisionOperator(ctx, "another@example.com", password, domain.RoleAdmin, true); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("bootstrap allowed twice")
	}
	if _, err := m.ProvisionOperator(ctx, "viewer@example.com", password, domain.RoleViewer, false); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Login(ctx, "admin@example.com", "incorrect", ""); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatal("bad password accepted")
	}
	login, err := m.Login(ctx, "admin@example.com", password, "")
	if err != nil {
		t.Fatal(err)
	}
	actor, err := m.AuthenticateOperator(ctx, login.Token)
	if err != nil {
		t.Fatal(err)
	}
	if actor.Operator.ID != admin.ID || actor.Operator.PasswordHash != "" {
		t.Fatal("operator projection incorrect")
	}
	project, err := m.CreateProject(ctx, actor, domain.Project{ID: "managed-a", Name: "Managed A"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateProject(ctx, actor, domain.Project{ID: "legacy", Name: "Legacy"}, ""); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("reserved ENV project accepted")
	}
	if _, err := m.CreateProject(ctx, actor, domain.Project{ID: "managed-b", Name: "Managed B"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateProject(ctx, actor, domain.Project{ID: "managed-c", Name: "Over capacity"}, ""); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("snapshot capacity exceeded")
	}
	page, err := m.ListProjects(ctx, actor, domain.ListOptions{Limit: 1})
	if err != nil || len(page.Items) != 1 || page.NextCursor == "" {
		t.Fatal("first cursor page")
	}
	next, err := m.ListProjects(ctx, actor, domain.ListOptions{Limit: 1, After: page.NextCursor})
	if err != nil || len(next.Items) != 1 || next.Items[0].ID == page.Items[0].ID {
		t.Fatal("cursor repeated item")
	}
	key, err := m.CreateKey(ctx, actor, project.ID, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	scope, err := m.Authenticate(ctx, key.Key)
	if err != nil || scope.ProjectID != project.ID {
		t.Fatal("managed key not authenticated")
	}
	for _, invalid := range []string{login.Token, key.Key + "corrupt", "client.jwt.token"} {
		if _, err := m.Authenticate(ctx, invalid); !errors.Is(err, domain.ErrUnauthorized) {
			t.Fatal("wrong credential namespace/secret accepted")
		}
	}
	keys, err := m.ListKeys(ctx, actor, project.ID, domain.ListOptions{Limit: 100})
	if err != nil || len(keys.Items) != 1 {
		t.Fatal("credential list")
	}
	encoded, _ := json.Marshal(keys)
	if strings.Contains(string(encoded), key.Key) || strings.Contains(string(encoded), "secretHash") || len(keys.Items[0].SecretHash) != 0 {
		t.Fatal("key/hash exposed again")
	}
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := m.CreateKey(ctx, actor, project.ID, nil, ""); results <- err }()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err := m.RevokeKey(ctx, actor, project.ID, key.Credential.ID, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.Authenticate(ctx, key.Key); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatal("revoked key still authenticated")
	}
	if err := m.RevokeKey(ctx, actor, "managed-b", key.Credential.ID, ""); !errors.Is(err, domain.ErrResourceNotFound) {
		t.Fatal("cross-project revocation accepted")
	}
	expires := time.Now().Add(150 * time.Millisecond)
	expired, err := m.CreateKey(ctx, actor, project.ID, &expires, "")
	if err != nil {
		t.Fatal(err)
	}
	<-time.After(time.Until(expires) + time.Millisecond)
	if _, err := m.Authenticate(ctx, expired.Key); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatal("expired key authenticated")
	}
	status := domain.StatusDisabled
	if _, err := m.UpdateProject(ctx, actor, project.ID, nil, &status, ""); err != nil {
		t.Fatal(err)
	}
	valid, err := m.CreateKey(ctx, actor, project.ID, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Authenticate(ctx, valid.Key); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal("disabled project published")
	}
	audits, err := m.ListAudit(ctx, actor, domain.ListOptions{Limit: 100})
	if err != nil || len(audits.Items) < 15 {
		t.Fatal("audit mutations missing")
	}
	auditJSON, _ := json.Marshal(audits)
	if strings.Contains(string(auditJSON), password) || strings.Contains(string(auditJSON), key.Key) || strings.Contains(string(auditJSON), login.Token) {
		t.Fatal("audit contained credential")
	}
	if err := m.Logout(ctx, actor, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AuthenticateOperator(ctx, login.Token); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatal("logout did not revoke")
	}
	login, err = m.Login(ctx, "admin@example.com", password, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.UpdateOperatorSecurity(ctx, "admin@example.com", "a replacement private passphrase", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AuthenticateOperator(ctx, login.Token); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatal("password reset left old session")
	}
	short, err := usecase.NewManagement(store, auth.NewPasswords(), nil, logger, 250*time.Millisecond, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	shortLogin, err := short.Login(ctx, "viewer@example.com", password, "")
	if err != nil {
		t.Fatal(err)
	}
	<-time.After(time.Until(shortLogin.ExpiresAt) + time.Millisecond)
	if _, err := short.AuthenticateOperator(ctx, shortLogin.Token); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatal("expired operator session accepted")
	}
	viewerLogin, err := m.Login(ctx, "viewer@example.com", password, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.UpdateOperatorSecurity(ctx, "viewer@example.com", "", domain.StatusDisabled); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AuthenticateOperator(ctx, viewerLogin.Token); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatal("disabled operator session remained valid")
	}
	if _, err := m.Login(ctx, "viewer@example.com", password, ""); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatal("disabled operator login accepted")
	}
}
