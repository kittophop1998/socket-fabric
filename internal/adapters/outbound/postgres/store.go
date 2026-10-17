// Package postgres implements the management persistence ports using pgx/v5.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"socketfabric/internal/application/ports"
	"socketfabric/internal/domain"
)

type queryer interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type queries struct {
	db      queryer
	timeout time.Duration
	inTx    bool
}
type Store struct {
	queries
	pool *pgxpool.Pool
}

var _ ports.ManagementStore = (*Store)(nil)

// Open validates without waiting for connectivity; runtime readiness is separate.
// Errors intentionally exclude connection strings, passwords and driver details.
func Open(ctx context.Context, dsn string, maxConns int32, connectTimeout, queryTimeout time.Duration) (*Store, error) {
	if maxConns < 1 || connectTimeout <= 0 || queryTimeout <= 0 {
		return nil, domain.ErrInvalid
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("invalid database configuration: %w", domain.ErrInvalid)
	}
	cfg.MaxConns, cfg.MinConns, cfg.MinIdleConns = maxConns, 0, 0
	cfg.ConnConfig.ConnectTimeout = connectTimeout
	cfg.HealthCheckPeriod = 5 * time.Second
	cfg.PingTimeout = queryTimeout
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create database pool: %w", domain.ErrUnavailable)
	}
	return &Store{queries: queries{db: pool, timeout: queryTimeout}, pool: pool}, nil
}

func (s *Store) Close() { s.pool.Close() }

func (s *Store) Transact(ctx context.Context, fn func(ports.ManagementQueries) error) error {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
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
	if err := fn(&queries{db: tx, timeout: s.timeout, inTx: true}); err != nil {
		return err
	}
	return safeError(tx.Commit(ctx))
}

func safeError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrResourceNotFound
	}
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("database operation canceled: %w", context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("database operation deadline: %w", context.DeadlineExceeded)
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "23505":
			return domain.ErrConflict
		case "23503", "23514", "22001", "22P02":
			return domain.ErrInvalid
		}
	}
	return fmt.Errorf("database operation failed: %w", domain.ErrUnavailable)
}

const operatorColumns = "id,email,role,status,password_hash,created_at,updated_at"

func scanOperator(row pgx.Row) (domain.Operator, error) {
	var o domain.Operator
	err := row.Scan(&o.ID, &o.Email, &o.Role, &o.Status, &o.PasswordHash, &o.CreatedAt, &o.UpdatedAt)
	return o, safeError(err)
}
func (q *queries) OperatorByEmail(ctx context.Context, email string) (domain.Operator, error) {
	ctx, cancel := context.WithTimeout(ctx, q.timeout)
	defer cancel()
	return scanOperator(q.db.QueryRow(ctx, "SELECT "+operatorColumns+" FROM operators WHERE email=$1", email))
}
func (q *queries) Operator(ctx context.Context, id string) (domain.Operator, error) {
	ctx, cancel := context.WithTimeout(ctx, q.timeout)
	defer cancel()
	suffix := ""
	if q.inTx {
		suffix = " FOR UPDATE"
	}
	return scanOperator(q.db.QueryRow(ctx, "SELECT "+operatorColumns+" FROM operators WHERE id=$1"+suffix, id))
}
func (q *queries) CreateOperator(ctx context.Context, o domain.Operator, bootstrap bool) error {
	ctx, cancel := context.WithTimeout(ctx, q.timeout)
	defer cancel()
	// Provisioning serializes with other provisioning/security changes.
	if _, err := q.db.Exec(ctx, "SELECT pg_advisory_xact_lock(73665341001)"); err != nil {
		return safeError(err)
	}
	var exists bool
	if err := q.db.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM operators)").Scan(&exists); err != nil {
		return safeError(err)
	}
	if bootstrap == exists {
		return domain.ErrConflict
	}
	_, err := q.db.Exec(ctx, "INSERT INTO operators ("+operatorColumns+") VALUES ($1,$2,$3,$4,$5,$6,$7)", o.ID, o.Email, o.Role, o.Status, o.PasswordHash, o.CreatedAt, o.UpdatedAt)
	return safeError(err)
}
func (q *queries) UpdateOperatorSecurity(ctx context.Context, id, passwordHash, status string) error {
	ctx, cancel := context.WithTimeout(ctx, q.timeout)
	defer cancel()
	if _, err := q.db.Exec(ctx, "SELECT pg_advisory_xact_lock(73665341001)"); err != nil {
		return safeError(err)
	}
	// Serialize against session issuance so a password reset cannot leave a new old-password session.
	tag, err := q.db.Exec(ctx, "UPDATE operators SET password_hash=CASE WHEN $2='' THEN password_hash ELSE $2 END,status=CASE WHEN $3='' THEN status ELSE $3 END,updated_at=now() WHERE id=$1", id, passwordHash, status)
	if err == nil && tag.RowsAffected() == 0 {
		return domain.ErrResourceNotFound
	}
	return safeError(err)
}
func (q *queries) CreateSession(ctx context.Context, s domain.OperatorSession) error {
	ctx, cancel := context.WithTimeout(ctx, q.timeout)
	defer cancel()
	_, err := q.db.Exec(ctx, "INSERT INTO operator_sessions (id,operator_id,secret_hash,expires_at) VALUES ($1,$2,$3,$4)", s.ID, s.OperatorID, s.SecretHash, s.ExpiresAt)
	return safeError(err)
}
func (q *queries) Session(ctx context.Context, id string) (domain.OperatorSession, error) {
	ctx, cancel := context.WithTimeout(ctx, q.timeout)
	defer cancel()
	var s domain.OperatorSession
	err := q.db.QueryRow(ctx, "SELECT id,operator_id,secret_hash,expires_at,revoked_at FROM operator_sessions WHERE id=$1", id).Scan(&s.ID, &s.OperatorID, &s.SecretHash, &s.ExpiresAt, &s.RevokedAt)
	return s, safeError(err)
}
func (q *queries) RevokeSession(ctx context.Context, id string, now time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, q.timeout)
	defer cancel()
	_, err := q.db.Exec(ctx, "UPDATE operator_sessions SET revoked_at=COALESCE(revoked_at,$2) WHERE id=$1", id, now)
	return safeError(err)
}
func (q *queries) RevokeOperatorSessions(ctx context.Context, id string, now time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, q.timeout)
	defer cancel()
	_, err := q.db.Exec(ctx, "UPDATE operator_sessions SET revoked_at=COALESCE(revoked_at,$2) WHERE operator_id=$1", id, now)
	return safeError(err)
}

const projectColumns = "id,name,status,created_at,updated_at"

func scanProject(row pgx.Row) (domain.Project, error) {
	var p domain.Project
	err := row.Scan(&p.ID, &p.Name, &p.Status, &p.CreatedAt, &p.UpdatedAt)
	return p, safeError(err)
}
func (q *queries) Project(ctx context.Context, id string) (domain.Project, error) {
	ctx, cancel := context.WithTimeout(ctx, q.timeout)
	defer cancel()
	suffix := ""
	if q.inTx {
		suffix = " FOR UPDATE"
	}
	return scanProject(q.db.QueryRow(ctx, "SELECT "+projectColumns+" FROM projects WHERE id=$1"+suffix, id))
}
func (q *queries) ListProjects(ctx context.Context, o domain.ListOptions) ([]domain.Project, error) {
	ctx, cancel := context.WithTimeout(ctx, q.timeout)
	defer cancel()
	rows, err := q.db.Query(ctx, "SELECT "+projectColumns+" FROM projects WHERE id>$1 AND ($2='' OR status=$2) AND ($3='' OR strpos(lower(name),lower($3))>0) ORDER BY id LIMIT $4", o.After, o.Status, o.Name, o.Limit)
	if err != nil {
		return nil, safeError(err)
	}
	defer rows.Close()
	items := make([]domain.Project, 0)
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, p)
	}
	return items, safeError(rows.Err())
}
func (q *queries) CreateProject(ctx context.Context, p domain.Project, maxProjects int) error {
	ctx, cancel := context.WithTimeout(ctx, q.timeout)
	defer cancel()
	if _, err := q.db.Exec(ctx, "SELECT pg_advisory_xact_lock(73665341002)"); err != nil {
		return safeError(err)
	}
	var count int
	if err := q.db.QueryRow(ctx, "SELECT count(*) FROM projects").Scan(&count); err != nil {
		return safeError(err)
	}
	if count >= maxProjects {
		return domain.ErrConflict
	}
	_, err := q.db.Exec(ctx, "INSERT INTO projects ("+projectColumns+") VALUES ($1,$2,$3,$4,$5)", p.ID, p.Name, p.Status, p.CreatedAt, p.UpdatedAt)
	return safeError(err)
}
func (q *queries) UpdateProject(ctx context.Context, p domain.Project) error {
	ctx, cancel := context.WithTimeout(ctx, q.timeout)
	defer cancel()
	tag, err := q.db.Exec(ctx, "UPDATE projects SET name=$2,status=$3,updated_at=$4 WHERE id=$1", p.ID, p.Name, p.Status, p.UpdatedAt)
	if err == nil && tag.RowsAffected() == 0 {
		return domain.ErrResourceNotFound
	}
	return safeError(err)
}
func (s *Store) ProjectStates(ctx context.Context, limit int) ([]domain.ProjectState, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	rows, err := s.pool.Query(ctx, "SELECT id,status FROM projects ORDER BY id LIMIT $1", limit+1)
	if err != nil {
		return nil, safeError(err)
	}
	defer rows.Close()
	items := make([]domain.ProjectState, 0)
	for rows.Next() {
		var p domain.ProjectState
		if err := rows.Scan(&p.ID, &p.Status); err != nil {
			return nil, safeError(err)
		}
		items = append(items, p)
	}
	if len(items) > limit {
		return nil, domain.ErrConflict
	}
	return items, safeError(rows.Err())
}

const credentialColumns = "id,project_id,key_identifier,secret_hash,status,created_at,expires_at,revoked_at"

func scanCredential(row pgx.Row) (domain.ProjectCredential, error) {
	var k domain.ProjectCredential
	err := row.Scan(&k.ID, &k.ProjectID, &k.KeyIdentifier, &k.SecretHash, &k.Status, &k.CreatedAt, &k.ExpiresAt, &k.RevokedAt)
	return k, safeError(err)
}
func (q *queries) Credential(ctx context.Context, id string) (domain.ProjectCredential, error) {
	ctx, cancel := context.WithTimeout(ctx, q.timeout)
	defer cancel()
	return scanCredential(q.db.QueryRow(ctx, "SELECT "+credentialColumns+" FROM project_credentials WHERE key_identifier=$1", id))
}
func (q *queries) ListCredentials(ctx context.Context, project string, o domain.ListOptions) ([]domain.ProjectCredential, error) {
	ctx, cancel := context.WithTimeout(ctx, q.timeout)
	defer cancel()
	rows, err := q.db.Query(ctx, "SELECT "+credentialColumns+" FROM project_credentials WHERE project_id=$1 AND id>$2 ORDER BY id LIMIT $3", project, o.After, o.Limit)
	if err != nil {
		return nil, safeError(err)
	}
	defer rows.Close()
	items := make([]domain.ProjectCredential, 0)
	for rows.Next() {
		k, err := scanCredential(rows)
		if err != nil {
			return nil, err
		}
		k.SecretHash = nil
		if k.Status == domain.StatusActive && !k.ValidAt(time.Now()) {
			k.Status = "expired"
		}
		items = append(items, k)
	}
	return items, safeError(rows.Err())
}
func (q *queries) CreateCredential(ctx context.Context, k domain.ProjectCredential) error {
	ctx, cancel := context.WithTimeout(ctx, q.timeout)
	defer cancel()
	_, err := q.db.Exec(ctx, "INSERT INTO project_credentials ("+credentialColumns+") VALUES ($1,$2,$3,$4,$5,$6,$7,$8)", k.ID, k.ProjectID, k.KeyIdentifier, k.SecretHash, k.Status, k.CreatedAt, k.ExpiresAt, k.RevokedAt)
	return safeError(err)
}
func (q *queries) RevokeCredential(ctx context.Context, project, id string, now time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, q.timeout)
	defer cancel()
	tag, err := q.db.Exec(ctx, "UPDATE project_credentials SET status='revoked',revoked_at=COALESCE(revoked_at,$3) WHERE project_id=$1 AND id=$2", project, id, now)
	if err == nil && tag.RowsAffected() == 0 {
		return domain.ErrResourceNotFound
	}
	return safeError(err)
}
func (q *queries) AppendAudit(ctx context.Context, a domain.AuditLog) error {
	ctx, cancel := context.WithTimeout(ctx, q.timeout)
	defer cancel()
	_, err := q.db.Exec(ctx, "INSERT INTO audit_logs (id,actor_id,action,resource_type,resource_id,timestamp,metadata) VALUES ($1,$2,$3,$4,$5,$6,$7)", a.ID, a.ActorID, a.Action, a.ResourceType, a.ResourceID, a.Timestamp, a.Metadata)
	return safeError(err)
}
func (q *queries) ListAudit(ctx context.Context, o domain.ListOptions) ([]domain.AuditLog, error) {
	ctx, cancel := context.WithTimeout(ctx, q.timeout)
	defer cancel()
	rows, err := q.db.Query(ctx, "SELECT id,actor_id,action,resource_type,resource_id,timestamp,metadata FROM audit_logs WHERE id>$1 AND ($2='' OR action=$2) AND ($3='' OR actor_id=$3) ORDER BY id LIMIT $4", o.After, o.Action, o.ActorID, o.Limit)
	if err != nil {
		return nil, safeError(err)
	}
	defer rows.Close()
	items := make([]domain.AuditLog, 0)
	for rows.Next() {
		var a domain.AuditLog
		if err := rows.Scan(&a.ID, &a.ActorID, &a.Action, &a.ResourceType, &a.ResourceID, &a.Timestamp, &a.Metadata); err != nil {
			return nil, safeError(err)
		}
		items = append(items, a)
	}
	return items, safeError(rows.Err())
}
