package usecase

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"socketfabric/internal/application/ports"
	"socketfabric/internal/domain"
)

type Management struct {
	store       ports.ManagementStore
	passwords   ports.PasswordHasher
	diagnostics ports.Diagnostics
	logger      *slog.Logger
	sessionTTL  time.Duration
	maxProjects int
	reserved    map[string]bool
	dummyHash   string
}

func NewManagement(store ports.ManagementStore, passwords ports.PasswordHasher, diagnostics ports.Diagnostics, logger *slog.Logger, sessionTTL time.Duration, maxProjects int, reserved []string) (*Management, error) {
	if store == nil || passwords == nil || logger == nil || sessionTTL <= 0 || sessionTTL > time.Hour || maxProjects < 1 {
		return nil, domain.ErrInvalid
	}
	dummy, err := passwords.Hash(context.Background(), "dummy-"+rand.Text())
	if err != nil {
		return nil, err
	}
	m := &Management{store: store, passwords: passwords, diagnostics: diagnostics, logger: logger, sessionTTL: sessionTTL, maxProjects: maxProjects, reserved: map[string]bool{}, dummyHash: dummy}
	for _, id := range reserved {
		m.reserved[id] = true
	}
	return m, nil
}

type LoginResult struct {
	Token     string          `json:"token"`
	TokenType string          `json:"tokenType"`
	ExpiresAt time.Time       `json:"expiresAt"`
	Operator  domain.Operator `json:"operator"`
}
type Page[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"nextCursor,omitempty"`
}
type CreateKeyResult struct {
	Credential domain.ProjectCredential `json:"credential"`
	Key        string                   `json:"key"`
}

func newOpaque(prefix string) (id, token string, digest []byte) {
	id = prefix + rand.Text()
	var secret [32]byte
	_, _ = rand.Read(secret[:])
	verifier := base64.RawURLEncoding.EncodeToString(secret[:])
	sum := sha256.Sum256([]byte(verifier))
	return id, id + "." + verifier, sum[:]
}
func parseOpaque(raw, prefix string) (string, []byte, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 2 || !strings.HasPrefix(parts[0], prefix) || len(parts[0]) != len(prefix)+26 || domain.ValidateID(parts[0]) != nil || len(parts[1]) != 43 {
		return "", nil, domain.ErrUnauthorized
	}
	decoded, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(decoded) != 32 || base64.RawURLEncoding.EncodeToString(decoded) != parts[1] {
		return "", nil, domain.ErrUnauthorized
	}
	digest := sha256.Sum256([]byte(parts[1]))
	return parts[0], digest[:], nil
}

func (m *Management) audit(ctx context.Context, q ports.ManagementQueries, actorID *string, action, kind, id, requestID string) error {
	metadata := map[string]string{}
	if requestID != "" && domain.ValidateID(requestID) == nil {
		metadata["requestId"] = requestID
	}
	body, _ := json.Marshal(metadata)
	now := time.Now().UTC()
	return q.AppendAudit(ctx, domain.AuditLog{ID: fmt.Sprintf("aud_%020d_%s", now.UnixNano(), rand.Text()), ActorID: actorID, Action: action, ResourceType: kind, ResourceID: id, Timestamp: now, Metadata: body})
}
func (m *Management) failedAuth(ctx context.Context, actor *string, requestID string) {
	if err := m.audit(ctx, m.store, actor, "auth.login_failed", "operator", "", requestID); err != nil {
		m.logger.WarnContext(ctx, "management audit unavailable", "category", "login_failed")
	}
}
func (m *Management) authorize(ctx context.Context, actor domain.OperatorActor, admin bool, requestID string) error {
	if err := actor.Authorize(admin); err != nil {
		if actor.Operator.ID != "" {
			if auditErr := m.audit(ctx, m.store, &actor.Operator.ID, "auth.denied", "operator", actor.Operator.ID, requestID); auditErr != nil {
				m.logger.WarnContext(ctx, "management audit unavailable", "category", "access_denied")
			}
		}
		return err
	}
	return nil
}

// Authorize applies the same permission/audit policy to the transport gate.
func (m *Management) Authorize(ctx context.Context, actor domain.OperatorActor, admin bool, requestID string) error {
	return m.authorize(ctx, actor, admin, requestID)
}

func (m *Management) Login(ctx context.Context, email, password, requestID string) (LoginResult, error) {
	email, err := domain.NormalizeEmail(email)
	if err != nil {
		m.failedAuth(ctx, nil, requestID)
		return LoginResult{}, domain.ErrUnauthorized
	}
	o, err := m.store.OperatorByEmail(ctx, email)
	if err != nil && !errors.Is(err, domain.ErrResourceNotFound) {
		return LoginResult{}, err
	}
	found := err == nil
	hash := m.dummyHash
	if found {
		hash = o.PasswordHash
	}
	verifyErr := m.passwords.Verify(ctx, hash, password)
	if errors.Is(verifyErr, domain.ErrRateLimited) || errors.Is(verifyErr, context.Canceled) || errors.Is(verifyErr, context.DeadlineExceeded) {
		return LoginResult{}, verifyErr
	}
	if !found || verifyErr != nil || o.Status != domain.StatusActive {
		var actor *string
		if found {
			actor = &o.ID
		}
		m.failedAuth(ctx, actor, requestID)
		return LoginResult{}, domain.ErrUnauthorized
	}
	id, token, digest := newOpaque("sfop_")
	expires := time.Now().UTC().Add(m.sessionTTL)
	err = m.store.Transact(ctx, func(q ports.ManagementQueries) error {
		current, err := q.Operator(ctx, o.ID)
		if err != nil {
			return err
		}
		if current.Status != domain.StatusActive || current.PasswordHash != hash {
			return domain.ErrUnauthorized
		}
		o = current
		if err := q.CreateSession(ctx, domain.OperatorSession{ID: id, OperatorID: o.ID, SecretHash: digest, ExpiresAt: expires}); err != nil {
			return err
		}
		return m.audit(ctx, q, &o.ID, "auth.login", "operator", o.ID, requestID)
	})
	if err != nil {
		return LoginResult{}, err
	}
	o.PasswordHash = ""
	return LoginResult{Token: token, TokenType: "Bearer", ExpiresAt: expires, Operator: o}, nil
}
func (m *Management) AuthenticateOperator(ctx context.Context, raw string) (domain.OperatorActor, error) {
	id, hash, err := parseOpaque(raw, "sfop_")
	if err != nil {
		return domain.OperatorActor{}, err
	}
	s, err := m.store.Session(ctx, id)
	if errors.Is(err, domain.ErrResourceNotFound) {
		err = domain.ErrUnauthorized
	}
	if err != nil {
		return domain.OperatorActor{}, err
	}
	if subtle.ConstantTimeCompare(hash, s.SecretHash) != 1 || s.RevokedAt != nil || !time.Now().Before(s.ExpiresAt) {
		return domain.OperatorActor{}, domain.ErrUnauthorized
	}
	o, err := m.store.Operator(ctx, s.OperatorID)
	if err != nil {
		return domain.OperatorActor{}, err
	}
	o.PasswordHash = ""
	a := domain.OperatorActor{Operator: o, SessionID: s.ID, ExpiresAt: s.ExpiresAt}
	if err := a.Authorize(false); err != nil {
		return domain.OperatorActor{}, domain.ErrUnauthorized
	}
	return a, nil
}
func (m *Management) Me(ctx context.Context, a domain.OperatorActor) (domain.Operator, error) {
	if err := m.authorize(ctx, a, false, ""); err != nil {
		return domain.Operator{}, err
	}
	return a.Operator, nil
}
func (m *Management) Logout(ctx context.Context, a domain.OperatorActor, requestID string) error {
	if err := m.authorize(ctx, a, false, requestID); err != nil {
		return err
	}
	return m.store.Transact(ctx, func(q ports.ManagementQueries) error {
		if err := q.RevokeSession(ctx, a.SessionID, time.Now().UTC()); err != nil {
			return err
		}
		return m.audit(ctx, q, &a.Operator.ID, "auth.logout", "operator", a.Operator.ID, requestID)
	})
}

// ProvisionOperator is reachable only from the explicitly invoked trusted local
// command, never from an HTTP endpoint. Database access is administrative power.
func (m *Management) ProvisionOperator(ctx context.Context, email, password, role string, bootstrap bool) (domain.Operator, error) {
	email, err := domain.NormalizeEmail(email)
	if err != nil {
		return domain.Operator{}, err
	}
	if role != domain.RoleAdmin && role != domain.RoleViewer || bootstrap && role != domain.RoleAdmin {
		return domain.Operator{}, domain.ErrInvalid
	}
	hash, err := m.passwords.Hash(ctx, password)
	if err != nil {
		return domain.Operator{}, err
	}
	now := time.Now().UTC()
	o := domain.Operator{ID: "opr_" + rand.Text(), Email: email, PasswordHash: hash, Role: role, Status: domain.StatusActive, CreatedAt: now, UpdatedAt: now}
	err = m.store.Transact(ctx, func(q ports.ManagementQueries) error {
		if err := q.CreateOperator(ctx, o, bootstrap); err != nil {
			return err
		}
		action := "operator.created"
		if bootstrap {
			action = "operator.bootstrap"
		}
		return m.audit(ctx, q, &o.ID, action, "operator", o.ID, "")
	})
	o.PasswordHash = ""
	return o, err
}
func (m *Management) UpdateOperatorSecurity(ctx context.Context, email, password, status string) error {
	email, err := domain.NormalizeEmail(email)
	if err != nil {
		return err
	}
	if status != "" && status != domain.StatusActive && status != domain.StatusDisabled {
		return domain.ErrInvalid
	}
	hash := ""
	if password != "" {
		hash, err = m.passwords.Hash(ctx, password)
		if err != nil {
			return err
		}
	}
	if hash == "" && status == "" {
		return domain.ErrInvalid
	}
	return m.store.Transact(ctx, func(q ports.ManagementQueries) error {
		o, err := q.OperatorByEmail(ctx, email)
		if err != nil {
			return err
		}
		if err := q.UpdateOperatorSecurity(ctx, o.ID, hash, status); err != nil {
			return err
		}
		if err := q.RevokeOperatorSessions(ctx, o.ID, time.Now().UTC()); err != nil {
			return err
		}
		return m.audit(ctx, q, &o.ID, "operator.security_updated", "operator", o.ID, "")
	})
}

func validateList(o domain.ListOptions) error {
	if o.Limit < 1 || o.Limit > 100 || o.After != "" && domain.ValidateID(o.After) != nil || len(o.Name) > 200 || len(o.Action) > 128 || o.ActorID != "" && domain.ValidateID(o.ActorID) != nil {
		return domain.ErrInvalid
	}
	return nil
}
func (m *Management) ListProjects(ctx context.Context, a domain.OperatorActor, o domain.ListOptions) (Page[domain.Project], error) {
	if err := m.authorize(ctx, a, false, ""); err != nil {
		return Page[domain.Project]{}, err
	}
	if err := validateList(o); err != nil {
		return Page[domain.Project]{}, err
	}
	if o.Status != "" && o.Status != domain.ProjectEnabled && o.Status != domain.StatusDisabled {
		return Page[domain.Project]{}, domain.ErrInvalid
	}
	items, err := m.store.ListProjects(ctx, o)
	page := Page[domain.Project]{Items: items}
	if len(items) == o.Limit {
		page.NextCursor = items[len(items)-1].ID
	}
	return page, err
}
func (m *Management) Project(ctx context.Context, a domain.OperatorActor, id string) (domain.Project, error) {
	if err := m.authorize(ctx, a, false, ""); err != nil {
		return domain.Project{}, err
	}
	if domain.ValidateID(id) != nil {
		return domain.Project{}, domain.ErrInvalid
	}
	return m.store.Project(ctx, id)
}
func (m *Management) CreateProject(ctx context.Context, a domain.OperatorActor, p domain.Project, requestID string) (domain.Project, error) {
	if err := m.authorize(ctx, a, true, requestID); err != nil {
		return domain.Project{}, err
	}
	p.Name = strings.TrimSpace(p.Name)
	if p.Status == "" {
		p.Status = domain.ProjectEnabled
	}
	if err := domain.ValidateProject(p); err != nil {
		return domain.Project{}, err
	}
	if m.reserved[p.ID] {
		return domain.Project{}, domain.ErrConflict
	}
	p.CreatedAt = time.Now().UTC()
	p.UpdatedAt = p.CreatedAt
	err := m.store.Transact(ctx, func(q ports.ManagementQueries) error {
		if err := q.CreateProject(ctx, p, m.maxProjects); err != nil {
			return err
		}
		return m.audit(ctx, q, &a.Operator.ID, "project.created", "project", p.ID, requestID)
	})
	return p, err
}
func (m *Management) UpdateProject(ctx context.Context, a domain.OperatorActor, id string, name, status *string, requestID string) (domain.Project, error) {
	if err := m.authorize(ctx, a, true, requestID); err != nil {
		return domain.Project{}, err
	}
	if domain.ValidateID(id) != nil || name == nil && status == nil {
		return domain.Project{}, domain.ErrInvalid
	}
	var p domain.Project
	err := m.store.Transact(ctx, func(q ports.ManagementQueries) error {
		var err error
		p, err = q.Project(ctx, id)
		if err != nil {
			return err
		}
		if name != nil {
			p.Name = strings.TrimSpace(*name)
		}
		if status != nil {
			p.Status = *status
		}
		if err := domain.ValidateProject(p); err != nil {
			return err
		}
		p.UpdatedAt = time.Now().UTC()
		if err := q.UpdateProject(ctx, p); err != nil {
			return err
		}
		return m.audit(ctx, q, &a.Operator.ID, "project.updated", "project", p.ID, requestID)
	})
	return p, err
}
func (m *Management) ListKeys(ctx context.Context, a domain.OperatorActor, project string, o domain.ListOptions) (Page[domain.ProjectCredential], error) {
	if err := m.authorize(ctx, a, true, ""); err != nil {
		return Page[domain.ProjectCredential]{}, err
	}
	if domain.ValidateID(project) != nil {
		return Page[domain.ProjectCredential]{}, domain.ErrInvalid
	}
	if err := validateList(o); err != nil {
		return Page[domain.ProjectCredential]{}, err
	}
	if _, err := m.store.Project(ctx, project); err != nil {
		return Page[domain.ProjectCredential]{}, err
	}
	items, err := m.store.ListCredentials(ctx, project, o)
	page := Page[domain.ProjectCredential]{Items: items}
	if len(items) == o.Limit {
		page.NextCursor = items[len(items)-1].ID
	}
	return page, err
}
func (m *Management) CreateKey(ctx context.Context, a domain.OperatorActor, project string, expires *time.Time, requestID string) (CreateKeyResult, error) {
	if err := m.authorize(ctx, a, true, requestID); err != nil {
		return CreateKeyResult{}, err
	}
	if domain.ValidateID(project) != nil || expires != nil && !time.Now().Before(*expires) {
		return CreateKeyResult{}, domain.ErrInvalid
	}
	id, key, hash := newOpaque("sfpk_")
	k := domain.ProjectCredential{ID: "key_" + rand.Text(), ProjectID: project, KeyIdentifier: id, SecretHash: hash, Status: domain.StatusActive, CreatedAt: time.Now().UTC(), ExpiresAt: expires}
	err := m.store.Transact(ctx, func(q ports.ManagementQueries) error {
		if _, err := q.Project(ctx, project); err != nil {
			return err
		}
		if err := q.CreateCredential(ctx, k); err != nil {
			return err
		}
		return m.audit(ctx, q, &a.Operator.ID, "credential.created", "credential", k.ID, requestID)
	})
	if err != nil {
		return CreateKeyResult{}, err
	}
	k.SecretHash = nil
	return CreateKeyResult{Credential: k, Key: key}, nil
}
func (m *Management) RevokeKey(ctx context.Context, a domain.OperatorActor, project, id, requestID string) error {
	if err := m.authorize(ctx, a, true, requestID); err != nil {
		return err
	}
	if domain.ValidateID(project) != nil || domain.ValidateID(id) != nil {
		return domain.ErrInvalid
	}
	return m.store.Transact(ctx, func(q ports.ManagementQueries) error {
		if err := q.RevokeCredential(ctx, project, id, time.Now().UTC()); err != nil {
			return err
		}
		return m.audit(ctx, q, &a.Operator.ID, "credential.revoked", "credential", id, requestID)
	})
}
func (m *Management) ListAudit(ctx context.Context, a domain.OperatorActor, o domain.ListOptions) (Page[domain.AuditLog], error) {
	if err := m.authorize(ctx, a, true, ""); err != nil {
		return Page[domain.AuditLog]{}, err
	}
	if err := validateList(o); err != nil {
		return Page[domain.AuditLog]{}, err
	}
	items, err := m.store.ListAudit(ctx, o)
	page := Page[domain.AuditLog]{Items: items}
	if len(items) == o.Limit {
		page.NextCursor = items[len(items)-1].ID
	}
	return page, err
}
func (m *Management) Diagnostics(ctx context.Context, a domain.OperatorActor) (map[string]any, error) {
	if err := m.authorize(ctx, a, false, ""); err != nil {
		return nil, err
	}
	if m.diagnostics == nil {
		return nil, domain.ErrUnavailable
	}
	return m.diagnostics.Snapshot(ctx)
}

// Authenticate implements the managed publisher port. There is intentionally
// no credential cache: every new HTTP publish verifies revocation at the DB.
func (m *Management) Authenticate(ctx context.Context, raw string) (domain.PublisherScope, error) {
	id, hash, err := parseOpaque(raw, "sfpk_")
	if err != nil {
		return domain.PublisherScope{}, err
	}
	k, err := m.store.Credential(ctx, id)
	if errors.Is(err, domain.ErrResourceNotFound) {
		err = domain.ErrUnauthorized
	}
	if err != nil {
		return domain.PublisherScope{}, err
	}
	if subtle.ConstantTimeCompare(hash, k.SecretHash) != 1 || !k.ValidAt(time.Now()) {
		return domain.PublisherScope{}, domain.ErrUnauthorized
	}
	if m.reserved[k.ProjectID] {
		return domain.PublisherScope{}, domain.ErrUnauthorized
	}
	p, err := m.store.Project(ctx, k.ProjectID)
	if err != nil {
		return domain.PublisherScope{}, err
	}
	if p.Status != domain.ProjectEnabled {
		return domain.PublisherScope{}, domain.ErrForbidden
	}
	return domain.PublisherScope{ProjectID: k.ProjectID}, nil
}

type PublisherCredentials struct {
	Legacy  ports.PublisherAuthenticator
	Managed ports.PublisherAuthenticator
}

func (p PublisherCredentials) Authenticate(ctx context.Context, key string) (domain.PublisherScope, error) {
	scope, err := p.Legacy.Authenticate(ctx, key)
	if err == nil || !errors.Is(err, domain.ErrUnauthorized) {
		return scope, err
	}
	return p.Managed.Authenticate(ctx, key)
}
