package socketfabric

import (
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	maxRooms        = 64
	defaultTokenTTL = 5 * time.Minute
	maxTokenTTL     = 15 * time.Minute
)

// TokenSigner holds a trusted, installation-wide signing secret. Anyone with
// this secret can issue tokens for any configured project. It belongs only in
// an authorized backend and must never be sent to a browser. A publisher key
// cannot replace the signing secret.
type TokenSigner struct{ secret []byte }

// ClientTokenInput describes an already authenticated user and authorized rooms.
// The caller supplies application policy; the signer does not decide permission.
type ClientTokenInput struct {
	ProjectID string
	UserID    string
	Rooms     []string
	// TTL defaults to 5 minutes; 1 second through 15 minutes are accepted.
	// Fractional seconds are truncated to match server JWT precision.
	TTL time.Duration
}

// NewTokenSigner copies the secret. The signer is safe for concurrent use and
// does not load environment variables or require publisher credentials.
func NewTokenSigner(secret []byte) (*TokenSigner, error) {
	if len(secret) < 32 {
		return nil, fmt.Errorf("signing secret must contain at least 32 bytes: %w", ErrInvalidInput)
	}
	return &TokenSigner{secret: append([]byte(nil), secret...)}, nil
}

// IssueToken signs a fresh, room-scoped HS256 client token. The caller must
// authenticate its user and authorize every requested room before calling it.
func (s *TokenSigner) IssueToken(input ClientTokenInput) (string, error) {
	if s == nil || len(s.secret) < 32 || !validID(input.ProjectID) || !validID(input.UserID) || len(input.Rooms) == 0 || len(input.Rooms) > maxRooms {
		return "", fmt.Errorf("client identity or rooms: %w", ErrInvalidInput)
	}
	if input.TTL == 0 {
		input.TTL = defaultTokenTTL
	}
	if input.TTL < time.Second || input.TTL > maxTokenTTL {
		return "", fmt.Errorf("token TTL must be 1 second through 15 minutes: %w", ErrInvalidInput)
	}
	rooms := append([]string(nil), input.Rooms...)
	seen := make(map[string]struct{}, len(rooms))
	for _, room := range rooms {
		if !validID(room) {
			return "", fmt.Errorf("client room: %w", ErrInvalidInput)
		}
		if _, duplicate := seen[room]; duplicate {
			return "", fmt.Errorf("duplicate client room: %w", ErrInvalidInput)
		}
		seen[room] = struct{}{}
	}
	now := time.Now().UTC().Truncate(time.Second)
	claims := struct {
		jwt.RegisteredClaims
		ProjectID string   `json:"projectId"`
		Rooms     []string `json:"rooms"`
	}{jwt.RegisteredClaims{
		Issuer: "socketfabric", Audience: jwt.ClaimStrings{"socketfabric-ws"}, Subject: input.UserID,
		IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(input.TTL.Truncate(time.Second))),
	}, input.ProjectID, rooms}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.secret)
	if err != nil {
		return "", ErrRequestFailed
	}
	return token, nil
}
