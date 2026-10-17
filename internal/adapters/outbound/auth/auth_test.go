package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"socketfabric/internal/domain"
)

const (
	testSecret  = "signing-secret-12345678901234567890"
	projectAKey = "project-a-key-1234567890123456789012"
	projectBKey = "project-b-key-1234567890123456789012"
)

func newTestAuthenticator(t *testing.T) *Authenticator {
	t.Helper()
	authenticator, err := New(testSecret, map[string]string{"project-a": projectAKey, "project-b": projectBKey})
	if err != nil {
		t.Fatal(err)
	}
	return authenticator
}

func validScope() domain.ClientScope {
	now := time.Now().UTC().Truncate(time.Second)
	return domain.ClientScope{
		UserID: "user-a", ProjectID: "project-a", Rooms: []string{"room-a", "room-b"},
		IssuedAt: now, ExpiresAt: now.Add(5 * time.Minute),
	}
}

func TestIssueAndVerify(t *testing.T) {
	authenticator := newTestAuthenticator(t)
	expected := validScope()
	raw, err := authenticator.Issue(expected)
	if err != nil {
		t.Fatal(err)
	}
	scope, err := authenticator.Verify(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if scope.UserID != expected.UserID || scope.ProjectID != expected.ProjectID || !scope.IssuedAt.Equal(expected.IssuedAt) || !scope.ExpiresAt.Equal(expected.ExpiresAt) {
		t.Fatalf("unexpected client scope: %#v", scope)
	}
	if !scope.CanJoin("room-a") || !scope.CanJoin("room-b") || scope.CanJoin("room-c") {
		t.Fatalf("unexpected room authorization: %#v", scope.Rooms)
	}
	parsed, _, err := new(jwt.Parser).ParseUnverified(raw, &clientClaims{})
	if err != nil || parsed.Method.Alg() != "HS256" {
		t.Fatalf("issued token is not HS256: %v", err)
	}
}

func TestVerifyRejectsInvalidTokens(t *testing.T) {
	authenticator := newTestAuthenticator(t)
	for _, test := range []struct {
		name   string
		mutate func(*clientClaims)
		method jwt.SigningMethod
		secret any
	}{
		{name: "bad signature", secret: []byte("other-signing-secret-1234567890123")},
		{name: "wrong algorithm", method: jwt.SigningMethodHS384},
		{name: "unsigned", method: jwt.SigningMethodNone, secret: jwt.UnsafeAllowNoneSignatureType},
		{name: "wrong issuer", mutate: func(c *clientClaims) { c.Issuer = "other" }},
		{name: "wrong audience", mutate: func(c *clientClaims) { c.Audience = jwt.ClaimStrings{"other"} }},
		{name: "missing issuer", mutate: func(c *clientClaims) { c.Issuer = "" }},
		{name: "missing audience", mutate: func(c *clientClaims) { c.Audience = nil }},
		{name: "missing subject", mutate: func(c *clientClaims) { c.Subject = "" }},
		{name: "missing issued at", mutate: func(c *clientClaims) { c.IssuedAt = nil }},
		{name: "missing expiry", mutate: func(c *clientClaims) { c.ExpiresAt = nil }},
		{name: "future issued at", mutate: func(c *clientClaims) { c.IssuedAt = jwt.NewNumericDate(time.Now().Add(time.Minute)) }},
		{name: "expired", mutate: func(c *clientClaims) {
			c.IssuedAt = jwt.NewNumericDate(time.Now().Add(-2 * time.Minute))
			c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Minute))
		}},
		{name: "excess lifetime", mutate: func(c *clientClaims) {
			c.ExpiresAt = jwt.NewNumericDate(c.IssuedAt.Add(domain.MaxTokenTTL + time.Second))
		}},
		{name: "unknown project", mutate: func(c *clientClaims) { c.ProjectID = "unknown" }},
		{name: "invalid project", mutate: func(c *clientClaims) { c.ProjectID = "project/*" }},
		{name: "wildcard room", mutate: func(c *clientClaims) { c.Rooms = []string{"*"} }},
		{name: "empty room", mutate: func(c *clientClaims) { c.Rooms = []string{""} }},
		{name: "too many rooms", mutate: func(c *clientClaims) {
			c.Rooms = make([]string, domain.MaxRooms+1)
			for i := range c.Rooms {
				c.Rooms[i] = "room"
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			scope := validScope()
			claims := clientClaims{
				RegisteredClaims: jwt.RegisteredClaims{
					Issuer: issuer, Audience: jwt.ClaimStrings{audience}, Subject: scope.UserID,
					IssuedAt: jwt.NewNumericDate(scope.IssuedAt), ExpiresAt: jwt.NewNumericDate(scope.ExpiresAt),
				}, ProjectID: scope.ProjectID, Rooms: scope.Rooms,
			}
			if test.mutate != nil {
				test.mutate(&claims)
			}
			method := test.method
			if method == nil {
				method = jwt.SigningMethodHS256
			}
			key := test.secret
			if key == nil {
				key = []byte(testSecret)
			}
			raw, err := jwt.NewWithClaims(method, claims).SignedString(key)
			if err != nil {
				t.Fatal(err)
			}
			_, err = authenticator.Verify(context.Background(), raw)
			if !errors.Is(err, domain.ErrUnauthorized) {
				t.Fatalf("expected unauthorized, got %v", err)
			}
			if strings.Contains(err.Error(), raw) || strings.Contains(err.Error(), testSecret) || strings.Contains(err.Error(), "signature") {
				t.Fatalf("verification error disclosed token internals: %v", err)
			}
		})
	}
	for _, raw := range []string{"", "not-a-jwt", "a.b.c"} {
		if _, err := authenticator.Verify(context.Background(), raw); !errors.Is(err, domain.ErrUnauthorized) {
			t.Fatalf("malformed token accepted: %v", err)
		}
	}
}

func TestPublisherAuthenticationCopiesKeysAndScopesProject(t *testing.T) {
	keys := map[string]string{"project-a": projectAKey, "project-b": projectBKey}
	authenticator, err := New(testSecret, keys)
	if err != nil {
		t.Fatal(err)
	}
	keys["project-a"] = projectBKey
	delete(keys, "project-b")
	for key, project := range map[string]string{projectAKey: "project-a", projectBKey: "project-b"} {
		scope, err := authenticator.Authenticate(context.Background(), key)
		if err != nil || scope.ProjectID != project {
			t.Fatalf("credential for %s returned scope %#v, error %v", project, scope, err)
		}
	}
	for _, key := range []string{"", "invalid-key", projectAKey + "x", testSecret} {
		_, err := authenticator.Authenticate(context.Background(), key)
		if !errors.Is(err, domain.ErrUnauthorized) || strings.Contains(err.Error(), key) && key != "" {
			t.Fatalf("unexpected credential error: %v", err)
		}
	}
}

func TestNewRejectsInvalidCredentialsWithoutSecrets(t *testing.T) {
	for _, test := range []struct {
		name   string
		secret string
		keys   map[string]string
	}{
		{"short signing secret", "short-secret", map[string]string{"project-a": projectAKey}},
		{"missing publishers", testSecret, nil},
		{"invalid project", testSecret, map[string]string{"project/*": projectAKey}},
		{"short publisher key", testSecret, map[string]string{"project-a": "short-key"}},
		{"duplicate publisher key", testSecret, map[string]string{"project-a": projectAKey, "project-b": projectAKey}},
		{"reused signing secret", testSecret, map[string]string{"project-a": testSecret}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := New(test.secret, test.keys)
			if !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("expected invalid config: %v", err)
			}
			if strings.Contains(err.Error(), test.secret) {
				t.Fatalf("error disclosed signing secret: %v", err)
			}
			for _, key := range test.keys {
				if strings.Contains(err.Error(), key) {
					t.Fatalf("error disclosed publisher key: %v", err)
				}
			}
		})
	}
}

func TestAuthenticationHonorsCanceledContext(t *testing.T) {
	authenticator := newTestAuthenticator(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := authenticator.Verify(ctx, "invalid"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Verify returned %v", err)
	}
	if _, err := authenticator.Authenticate(ctx, projectAKey); !errors.Is(err, context.Canceled) {
		t.Fatalf("Authenticate returned %v", err)
	}
}

func TestIssueRejectsUnknownProjectAndInvalidScope(t *testing.T) {
	authenticator := newTestAuthenticator(t)
	for _, scope := range []domain.ClientScope{
		{UserID: "user-a", ProjectID: "unknown", Rooms: []string{"room-a"}, IssuedAt: validScope().IssuedAt, ExpiresAt: validScope().ExpiresAt},
		{UserID: "user-a", ProjectID: "project-a", Rooms: []string{"*"}, IssuedAt: validScope().IssuedAt, ExpiresAt: validScope().ExpiresAt},
		{UserID: "user-a", ProjectID: "project-a", Rooms: []string{"room-a"}, IssuedAt: validScope().IssuedAt, ExpiresAt: validScope().IssuedAt.Add(domain.MaxTokenTTL + time.Second)},
	} {
		if _, err := authenticator.Issue(scope); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("Issue returned %v", err)
		}
	}
}
