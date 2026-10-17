package socketfabric

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func tokenInput() ClientTokenInput {
	return ClientTokenInput{ProjectID: "project.one", UserID: "user:one", Rooms: []string{"room:one", "room.two"}}
}

func TestTokenSignerClaimsAndSecretOwnership(t *testing.T) {
	secret := []byte("sdk-signing-secret-at-least-32-bytes")
	original := append([]byte(nil), secret...)
	signer, err := NewTokenSigner(secret)
	if err != nil {
		t.Fatal(err)
	}
	for i := range secret {
		secret[i] = 'x'
	}
	before := time.Now().UTC().Truncate(time.Second)
	raw, err := signer.IssueToken(tokenInput())
	if err != nil {
		t.Fatal(err)
	}
	claims := struct {
		jwt.RegisteredClaims
		ProjectID string   `json:"projectId"`
		Rooms     []string `json:"rooms"`
	}{}
	parsed, err := jwt.ParseWithClaims(raw, &claims, func(t *jwt.Token) (any, error) {
		if t.Method != jwt.SigningMethodHS256 {
			t2 := t.Method.Alg()
			return nil, fmt.Errorf("unexpected algorithm %s", t2)
		}
		return original, nil
	}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithIssuer("socketfabric"), jwt.WithAudience("socketfabric-ws"), jwt.WithIssuedAt(), jwt.WithExpirationRequired())
	if err != nil || !parsed.Valid {
		t.Fatalf("token invalid: %v", err)
	}
	if claims.ProjectID != "project.one" || claims.Subject != "user:one" || len(claims.Rooms) != 2 || claims.Rooms[0] != "room:one" || claims.IssuedAt == nil || claims.ExpiresAt == nil {
		t.Fatalf("claims = %+v", claims)
	}
	if claims.ExpiresAt.Sub(claims.IssuedAt.Time) != 5*time.Minute || claims.IssuedAt.Before(before) || claims.IssuedAt.After(time.Now()) || claims.IssuedAt.Nanosecond() != 0 || claims.ExpiresAt.Nanosecond() != 0 {
		t.Fatal("incorrect lifetime precision/default")
	}
}

func TestTokenSignerInputValidation(t *testing.T) {
	if _, err := NewTokenSigner([]byte("short")); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("short secret: %v", err)
	}
	signer, _ := NewTokenSigner([]byte("sdk-signing-secret-at-least-32-bytes"))
	tooMany := make([]string, 65)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("room%d", i)
	}
	for _, change := range []func(*ClientTokenInput){
		func(i *ClientTokenInput) { i.ProjectID = "*" }, func(i *ClientTokenInput) { i.UserID = "bad user" }, func(i *ClientTokenInput) { i.Rooms = nil }, func(i *ClientTokenInput) { i.Rooms = []string{"duplicate", "duplicate"} }, func(i *ClientTokenInput) { i.Rooms = []string{"invalid/*"} }, func(i *ClientTokenInput) { i.Rooms = tooMany }, func(i *ClientTokenInput) { i.TTL = -time.Second }, func(i *ClientTokenInput) { i.TTL = time.Millisecond }, func(i *ClientTokenInput) { i.TTL = 15*time.Minute + time.Nanosecond },
	} {
		input := tokenInput()
		change(&input)
		if _, err := signer.IssueToken(input); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("invalid token input error: %v", err)
		}
	}
	for _, ttl := range []time.Duration{time.Second, 1500 * time.Millisecond, 15 * time.Minute} {
		input := tokenInput()
		input.TTL = ttl
		if _, err := signer.IssueToken(input); err != nil {
			t.Fatalf("valid lifetime %s: %v", ttl, err)
		}
	}
}

func TestTokenSignerConcurrent(t *testing.T) {
	signer, _ := NewTokenSigner([]byte("sdk-signing-secret-at-least-32-bytes"))
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			if _, err := signer.IssueToken(tokenInput()); err != nil {
				t.Errorf("concurrent issue: %v", err)
			}
		})
	}
	wg.Wait()
}

func TestUninitializedSignerCannotMintToken(t *testing.T) {
	for _, signer := range []*TokenSigner{nil, {}} {
		if token, err := signer.IssueToken(tokenInput()); token != "" || !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("uninitialized signer token = %q, error = %v", token, err)
		}
	}
}
