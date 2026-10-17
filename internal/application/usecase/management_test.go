package usecase

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"socketfabric/internal/application/ports"
	"socketfabric/internal/domain"
)

type policyStore struct {
	ports.ManagementStore
	mu     sync.Mutex
	states []domain.ProjectState
	err    error
}

func (s *policyStore) CheckSchema(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}
func (s *policyStore) ProjectStates(ctx context.Context, limit int) ([]domain.ProjectState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.ProjectState(nil), s.states...), s.err
}
func (s *policyStore) AppendAudit(context.Context, domain.AuditLog) error { return nil }

func TestPolicyAgeIsolationAndOutage(t *testing.T) {
	store := &policyStore{states: []domain.ProjectState{{ID: "managed-a", Status: domain.ProjectEnabled}, {ID: "managed-b", Status: domain.StatusDisabled}}}
	p, err := NewProjectPolicy(store, []string{"legacy"}, 128, time.Second, 5*time.Second, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(p.CheckProject(context.Background(), "managed-a"), domain.ErrUnavailable) || p.CheckProject(context.Background(), "legacy") != nil {
		t.Fatal("startup policy")
	}
	if err := p.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.CheckProject(context.Background(), "managed-a") != nil || !errors.Is(p.CheckProject(context.Background(), "managed-b"), domain.ErrForbidden) || !errors.Is(p.CheckProject(context.Background(), "unknown"), domain.ErrUnauthorized) {
		t.Fatal("project isolation")
	}
	store.mu.Lock()
	store.err = domain.ErrUnavailable
	store.mu.Unlock()
	if err := p.Refresh(context.Background()); err == nil || p.Ready() {
		t.Fatal("DB failure was hidden")
	}
	if p.CheckProject(context.Background(), "managed-a") != nil {
		t.Fatal("fresh snapshot should remain valid")
	}
	p.mu.Lock()
	p.readAt = time.Now().Add(-6 * time.Second)
	p.mu.Unlock()
	if !errors.Is(p.CheckProject(context.Background(), "managed-a"), domain.ErrUnavailable) || p.CheckProject(context.Background(), "legacy") != nil {
		t.Fatal("stale managed policy did not fail closed")
	}
	store.mu.Lock()
	store.err = nil
	store.states = []domain.ProjectState{{ID: "legacy", Status: domain.ProjectEnabled}}
	store.mu.Unlock()
	if err := p.Refresh(context.Background()); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("reserved ID collision accepted")
	}
}

func TestOpaqueTokensAreScopedAndHashed(t *testing.T) {
	id, token, hash := newOpaque("sfop_")
	parsed, got, err := parseOpaque(token, "sfop_")
	if err != nil || parsed != id || string(got) != string(hash) || strings.Contains(token, string(hash)) {
		t.Fatal("opaque encoding")
	}
	if _, _, err := parseOpaque(token, "sfpk_"); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatal("operator token accepted as publisher")
	}
	for _, raw := range []string{"", token + "extra", token + ".extra", "Bearer " + token, "eyJ.fake.jwt"} {
		if _, _, err := parseOpaque(raw, "sfop_"); err == nil {
			t.Fatal("malformed token accepted")
		}
	}
	body, err := json.Marshal(domain.ProjectCredential{SecretHash: hash})
	if err != nil || strings.Contains(string(body), "SecretHash") || strings.Contains(string(body), "secretHash") {
		t.Fatal("hash exposed by JSON")
	}
}

func TestManagementUseCasesDenyViewerBeforePersistence(t *testing.T) {
	m := &Management{store: &policyStore{}, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	viewer := domain.OperatorActor{Operator: domain.Operator{ID: "operator", Role: domain.RoleViewer, Status: domain.StatusActive}, ExpiresAt: time.Now().Add(time.Minute)}
	if _, err := m.CreateProject(context.Background(), viewer, domain.Project{ID: "project", Name: "Project"}, ""); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal("viewer could create project")
	}
	if _, err := m.CreateKey(context.Background(), viewer, "project", nil, ""); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal("viewer could create key")
	}
	if err := m.RevokeKey(context.Background(), viewer, "project", "key", ""); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal("viewer could revoke key")
	}
	if _, err := m.ListAudit(context.Background(), viewer, domain.ListOptions{Limit: 25}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal("viewer could read audit")
	}
	viewer.ExpiresAt = time.Now().Add(-time.Second)
	if _, err := m.Me(context.Background(), viewer); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatal("expired actor accepted")
	}
}

func TestPolicyWorkerStopsAndSignalsExpiry(t *testing.T) {
	store := &policyStore{states: []domain.ProjectState{{ID: "p", Status: domain.ProjectEnabled}}}
	p, err := NewProjectPolicy(store, nil, 128, 20*time.Millisecond, 100*time.Millisecond, 10*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	changed := make(chan struct{}, 20)
	done := make(chan struct{})
	go func() {
		p.Run(ctx, func() {
			select {
			case changed <- struct{}{}:
			default:
			}
		})
		close(done)
	}()
	select {
	case <-changed:
	case <-time.After(time.Second):
		t.Fatal("initial refresh missing")
	}
	store.mu.Lock()
	store.err = domain.ErrUnavailable
	store.mu.Unlock()
	deadline := time.After(time.Second)
	for !errors.Is(p.CheckProject(context.Background(), "p"), domain.ErrUnavailable) {
		select {
		case <-changed:
		case <-deadline:
			t.Fatal("policy did not expire")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("policy worker leaked")
	}
}
