// Package auth verifies client tokens and trusted publisher credentials.
package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"socketfabric/internal/application/ports"
	"socketfabric/internal/domain"
)

const (
	issuer   = "socketfabric"
	audience = "socketfabric-ws"
)

type publisherCredential struct {
	projectID string
	digest    [sha256.Size]byte
}

// Authenticator is immutable after construction and safe for concurrent use.
type Authenticator struct {
	secret     []byte
	publishers []publisherCredential
	projects   map[string]struct{}
	managed    ports.ProjectAccess
}

var (
	_ ports.ClientTokenVerifier    = (*Authenticator)(nil)
	_ ports.PublisherAuthenticator = (*Authenticator)(nil)
)

type clientClaims struct {
	jwt.RegisteredClaims
	ProjectID string   `json:"projectId"`
	Rooms     []string `json:"rooms"`
}

func New(secret string, publisherKeys map[string]string, managed ...ports.ProjectAccess) (*Authenticator, error) {
	if len(secret) < 32 {
		return nil, fmt.Errorf("client signing secret must contain at least 32 bytes: %w", domain.ErrInvalid)
	}
	if len(managed) > 1 || len(managed) == 1 && managed[0] == nil {
		return nil, domain.ErrInvalid
	}
	if len(publisherKeys) == 0 && len(managed) == 0 {
		return nil, fmt.Errorf("publisher keys must not be empty: %w", domain.ErrInvalid)
	}
	authenticator := &Authenticator{
		secret:   []byte(secret),
		projects: make(map[string]struct{}, len(publisherKeys)),
	}
	if len(managed) == 1 {
		authenticator.managed = managed[0]
	}
	seen := make(map[[sha256.Size]byte]struct{}, len(publisherKeys))
	for projectID, key := range publisherKeys {
		if err := domain.ValidateID(projectID); err != nil {
			return nil, fmt.Errorf("publisher project identifier is invalid: %w", domain.ErrInvalid)
		}
		if len(key) < 32 {
			return nil, fmt.Errorf("publisher keys must contain at least 32 bytes: %w", domain.ErrInvalid)
		}
		if key == secret {
			return nil, fmt.Errorf("publisher keys and signing secret must differ: %w", domain.ErrInvalid)
		}
		digest := sha256.Sum256([]byte(key))
		if _, duplicate := seen[digest]; duplicate {
			return nil, fmt.Errorf("publisher keys must be distinct: %w", domain.ErrInvalid)
		}
		seen[digest] = struct{}{}
		authenticator.publishers = append(authenticator.publishers, publisherCredential{projectID, digest})
		authenticator.projects[projectID] = struct{}{}
	}
	return authenticator, nil
}

func (a *Authenticator) Authenticate(ctx context.Context, key string) (domain.PublisherScope, error) {
	if err := ctx.Err(); err != nil {
		return domain.PublisherScope{}, err
	}
	digest := sha256.Sum256([]byte(key))
	projectID := ""
	// Compare fixed-size hashes and inspect every credential even after a match.
	for _, credential := range a.publishers {
		if subtle.ConstantTimeCompare(digest[:], credential.digest[:]) == 1 {
			projectID = credential.projectID
		}
	}
	if err := ctx.Err(); err != nil {
		return domain.PublisherScope{}, err
	}
	if projectID == "" {
		return domain.PublisherScope{}, fmt.Errorf("invalid publisher credential: %w", domain.ErrUnauthorized)
	}
	return domain.PublisherScope{ProjectID: projectID}, nil
}

func (a *Authenticator) Verify(ctx context.Context, rawToken string) (domain.ClientScope, error) {
	if err := ctx.Err(); err != nil {
		return domain.ClientScope{}, err
	}
	now := time.Now()
	claims := &clientClaims{}
	token, err := jwt.ParseWithClaims(rawToken, claims, func(token *jwt.Token) (any, error) {
		if token.Method != jwt.SigningMethodHS256 {
			return nil, domain.ErrUnauthorized
		}
		return a.secret, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(issuer), jwt.WithAudience(audience), jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(), jwt.WithTimeFunc(func() time.Time { return now }))
	if err := ctx.Err(); err != nil {
		return domain.ClientScope{}, err
	}
	if err != nil || token == nil || !token.Valid || claims.IssuedAt == nil || claims.ExpiresAt == nil {
		return domain.ClientScope{}, invalidToken()
	}
	scope := domain.ClientScope{
		UserID: claims.Subject, ProjectID: claims.ProjectID, Rooms: append([]string(nil), claims.Rooms...),
		IssuedAt: claims.IssuedAt.Time, ExpiresAt: claims.ExpiresAt.Time,
	}
	if err := scope.Validate(now); err != nil {
		return domain.ClientScope{}, invalidToken()
	}
	if _, configured := a.projects[scope.ProjectID]; !configured {
		if a.managed == nil {
			return domain.ClientScope{}, invalidToken()
		}
		if err := a.managed.CheckProject(ctx, scope.ProjectID); err != nil {
			return domain.ClientScope{}, err
		}
	}
	return scope, nil
}

// Issue signs a client scope for the local token CLI and trusted callers.
func (a *Authenticator) Issue(scope domain.ClientScope) (string, error) {
	scope.IssuedAt = scope.IssuedAt.UTC().Truncate(time.Second)
	scope.ExpiresAt = scope.ExpiresAt.UTC().Truncate(time.Second)
	if err := scope.Validate(time.Now()); err != nil {
		return "", fmt.Errorf("invalid client token scope: %w", domain.ErrInvalid)
	}
	if _, configured := a.projects[scope.ProjectID]; !configured {
		return "", fmt.Errorf("client project is not configured: %w", domain.ErrInvalid)
	}
	claims := clientClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: issuer, Audience: jwt.ClaimStrings{audience}, Subject: scope.UserID,
			IssuedAt: jwt.NewNumericDate(scope.IssuedAt), ExpiresAt: jwt.NewNumericDate(scope.ExpiresAt),
		},
		ProjectID: scope.ProjectID, Rooms: append([]string(nil), scope.Rooms...),
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(a.secret)
	if err != nil {
		return "", fmt.Errorf("sign client token: %w", domain.ErrUnavailable)
	}
	return token, nil
}

func invalidToken() error {
	return fmt.Errorf("invalid client token: %w", domain.ErrUnauthorized)
}
