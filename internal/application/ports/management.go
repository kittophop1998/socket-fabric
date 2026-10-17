package ports

import (
	"context"
	"time"

	"socketfabric/internal/domain"
)

// ManagementQueries describes actual control-plane persistence operations.
// Implementations never expose driver transactions to application/domain code.
type ManagementQueries interface {
	OperatorByEmail(context.Context, string) (domain.Operator, error)
	Operator(context.Context, string) (domain.Operator, error)
	CreateOperator(context.Context, domain.Operator, bool) error
	UpdateOperatorSecurity(context.Context, string, string, string) error
	CreateSession(context.Context, domain.OperatorSession) error
	Session(context.Context, string) (domain.OperatorSession, error)
	RevokeSession(context.Context, string, time.Time) error
	RevokeOperatorSessions(context.Context, string, time.Time) error
	ListProjects(context.Context, domain.ListOptions) ([]domain.Project, error)
	Project(context.Context, string) (domain.Project, error)
	CreateProject(context.Context, domain.Project, int) error
	UpdateProject(context.Context, domain.Project) error
	ListCredentials(context.Context, string, domain.ListOptions) ([]domain.ProjectCredential, error)
	Credential(context.Context, string) (domain.ProjectCredential, error)
	CreateCredential(context.Context, domain.ProjectCredential) error
	RevokeCredential(context.Context, string, string, time.Time) error
	AppendAudit(context.Context, domain.AuditLog) error
	ListAudit(context.Context, domain.ListOptions) ([]domain.AuditLog, error)
}

type ManagementStore interface {
	ManagementQueries
	Transact(context.Context, func(ManagementQueries) error) error
	CheckSchema(context.Context) error
	ProjectStates(context.Context, int) ([]domain.ProjectState, error)
}

type PasswordHasher interface {
	Hash(context.Context, string) (string, error)
	Verify(context.Context, string, string) error
}

// ProjectAccess is a constant-time local policy check, never a DB/network call.
type ProjectAccess interface {
	CheckProject(context.Context, string) error
}

type Diagnostics interface {
	Snapshot(context.Context) (map[string]any, error)
}
