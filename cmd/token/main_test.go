package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"socketfabric/internal/adapters/outbound/auth"
	"socketfabric/internal/config"
)

func tokenConfig() config.Auth {
	return config.Auth{
		ClientTokenSecret: "client-secret-12345678901234567890",
		PublisherKeys:     map[string]string{"project-a": "publisher-key-123456789012345678901"},
	}
}

func TestRunIssuesTokenWithDefaultLifetime(t *testing.T) {
	var stdout, stderr bytes.Buffer
	now := time.Now().UTC().Truncate(time.Second)
	load := func() (config.Auth, error) { return tokenConfig(), nil }
	code := run([]string{"--project", "project-a", "--user", "user-a", "--rooms", "room-a, room-b"}, &stdout, &stderr, load, func() time.Time { return now })
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("code %d, stderr %s", code, stderr.String())
	}
	cfg := tokenConfig()
	authenticator, err := auth.New(cfg.ClientTokenSecret, cfg.PublisherKeys)
	if err != nil {
		t.Fatal(err)
	}
	scope, err := authenticator.Verify(context.Background(), strings.TrimSpace(stdout.String()))
	if err != nil {
		t.Fatal(err)
	}
	if scope.UserID != "user-a" || scope.ProjectID != "project-a" || !scope.CanJoin("room-b") || scope.ExpiresAt.Sub(scope.IssuedAt) != 5*time.Minute {
		t.Fatalf("unexpected CLI scope %#v", scope)
	}
	if strings.Count(stdout.String(), "\n") != 1 || strings.Contains(stdout.String(), cfg.ClientTokenSecret) || strings.Contains(stdout.String(), cfg.PublisherKeys["project-a"]) {
		t.Fatal("stdout must contain only the token and newline")
	}
}

func TestRunRejectsInvalidArgumentsBeforeLoadingSecrets(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"--project", "project-a", "--user", "user-a"},
		{"--project", "project-a", "--user", "user-a", "--rooms", "*"},
		{"--project", "project-a", "--user", "user-a", "--rooms", "room-a", "--ttl", "0s"},
		{"--project", "project-a", "--user", "user-a", "--rooms", "room-a", "--ttl", "16m"},
		{"--project", "project-a", "--user", "user-a", "--rooms", "room-a", "--ttl", "100ms"},
		{"--project", "project-a", "--user", "user-a", "--rooms", "room-a", "extra"},
	} {
		var stdout, stderr bytes.Buffer
		loaded := false
		code := run(args, &stdout, &stderr, func() (config.Auth, error) { loaded = true; return tokenConfig(), nil }, time.Now)
		if code != 2 || loaded || stdout.Len() != 0 || stderr.Len() == 0 {
			t.Fatalf("args %v returned code %d, loaded %v, stdout %s, stderr %s", args, code, loaded, stdout.String(), stderr.String())
		}
	}
}

func TestRunSanitizesConfigurationErrors(t *testing.T) {
	var stdout, stderr bytes.Buffer
	secret := tokenConfig().ClientTokenSecret
	load := func() (config.Auth, error) { return config.Auth{}, errors.New(secret) }
	code := run([]string{"--project", "project-a", "--user", "user-a", "--rooms", "room-a"}, &stdout, &stderr, load, time.Now)
	if code != 1 || stdout.Len() != 0 || strings.Contains(stderr.String(), secret) {
		t.Fatalf("unexpected CLI failure code %d, stderr %s", code, stderr.String())
	}
}

func TestRunHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"--help"}, &stdout, &stderr, func() (config.Auth, error) { t.Fatal("unexpected config load"); return config.Auth{}, nil }, time.Now)
	if code != 0 || !strings.Contains(stderr.String(), "--") && !strings.Contains(stderr.String(), "-project") {
		t.Fatalf("unexpected help code %d, stderr %s", code, stderr.String())
	}
}
