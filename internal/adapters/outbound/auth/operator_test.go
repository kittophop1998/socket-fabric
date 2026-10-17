package auth

import (
	"context"
	"errors"
	"strings"
	"testing"

	"socketfabric/internal/domain"
)

func TestOperatorPasswords(t *testing.T) {
	p := NewPasswords()
	password := "a long private passphrase"
	hash, err := p.Hash(context.Background(), password)
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.Hash(context.Background(), password)
	if err != nil || hash == second || strings.Contains(hash, password) {
		t.Fatal("password salt/storage")
	}
	if err := p.Verify(context.Background(), hash, password); err != nil {
		t.Fatal(err)
	}
	if err := p.Verify(context.Background(), hash, "wrong password"); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatal("wrong password accepted")
	}
	if _, err := p.Hash(context.Background(), "short"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatal("short password accepted")
	}
	if err := p.Verify(context.Background(), strings.Replace(hash, "m=19456", "m=999999999", 1), password); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatal("unbounded parameters accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Hash(ctx, password); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation lost")
	}
	for range 4 {
		p.slots <- struct{}{}
	}
	if _, err := p.Hash(context.Background(), password); !errors.Is(err, domain.ErrRateLimited) {
		t.Fatal("hash work not bounded")
	}
}
