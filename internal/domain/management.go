package domain

import (
	"encoding/json"
	"errors"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	ErrConflict         = errors.New("resource conflict")
	ErrResourceNotFound = errors.New("resource not found")
	ErrRateLimited      = errors.New("rate limited")
)

const (
	RoleAdmin      = "admin"
	RoleViewer     = "viewer"
	StatusActive   = "active"
	StatusDisabled = "disabled"
	ProjectEnabled = "enabled"
)

type Operator struct {
	ID           string    `json:"id"`
	Email        string    `json:"email"`
	Role         string    `json:"role"`
	Status       string    `json:"status"`
	PasswordHash string    `json:"-"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

type OperatorSession struct {
	ID         string
	OperatorID string
	SecretHash []byte
	ExpiresAt  time.Time
	RevokedAt  *time.Time
}

// OperatorActor is authenticated afresh for each management request.
type OperatorActor struct {
	Operator  Operator
	SessionID string
	ExpiresAt time.Time
}

func (a OperatorActor) Authorize(admin bool) error {
	if a.Operator.ID == "" || a.Operator.Status != StatusActive || !time.Now().Before(a.ExpiresAt) {
		return ErrUnauthorized
	}
	if a.Operator.Role != RoleAdmin && a.Operator.Role != RoleViewer || admin && a.Operator.Role != RoleAdmin {
		return ErrForbidden
	}
	return nil
}

type Project struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type ProjectState struct{ ID, Status string }

type ProjectCredential struct {
	ID            string     `json:"id"`
	ProjectID     string     `json:"projectId"`
	KeyIdentifier string     `json:"keyIdentifier"`
	SecretHash    []byte     `json:"-"`
	Status        string     `json:"status"`
	CreatedAt     time.Time  `json:"createdAt"`
	ExpiresAt     *time.Time `json:"expiresAt"`
	RevokedAt     *time.Time `json:"revokedAt"`
}

func (k ProjectCredential) ValidAt(now time.Time) bool {
	return k.Status == StatusActive && k.RevokedAt == nil && (k.ExpiresAt == nil || now.Before(*k.ExpiresAt))
}

type AuditLog struct {
	ID           string          `json:"id"`
	ActorID      *string         `json:"actorId"`
	Action       string          `json:"action"`
	ResourceType string          `json:"resourceType"`
	ResourceID   string          `json:"resourceId"`
	Timestamp    time.Time       `json:"timestamp"`
	Metadata     json.RawMessage `json:"metadata"`
}

// ListOptions is shared only by the implemented bounded management lists.
// Cursor is a stable ascending ID; audit logs also use generated sortable IDs.
type ListOptions struct {
	Limit   int
	After   string
	Status  string
	Name    string
	Action  string
	ActorID string
}

func NormalizeEmail(value string) (string, error) {
	email := strings.ToLower(strings.TrimSpace(value))
	address, err := mail.ParseAddress(email)
	if err != nil || address.Address != email || len(email) > 254 || strings.ContainsAny(email, "\r\n") {
		return "", ErrInvalid
	}
	return email, nil
}

func ValidateProject(p Project) error {
	if ValidateID(p.ID) != nil || strings.TrimSpace(p.Name) == "" || !utf8.ValidString(p.Name) || utf8.RuneCountInString(p.Name) > 200 || strings.ContainsAny(p.Name, "\r\n\x00") {
		return ErrInvalid
	}
	if p.Status != ProjectEnabled && p.Status != StatusDisabled {
		return ErrInvalid
	}
	return nil
}
