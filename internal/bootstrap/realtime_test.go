package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"socketfabric/internal/adapters/outbound/auth"
	"socketfabric/internal/domain"
)

func dialBootstrapWebSocket(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, strings.Replace(url, "http://", "ws://", 1)+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

func writeBootstrapCommand(t *testing.T, conn *websocket.Conn, value any) {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, body); err != nil {
		t.Fatal(err)
	}
}

func readBootstrapOperation(t *testing.T, conn *websocket.Conn, expected string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	kind, body, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var message struct {
		Op string `json:"op"`
	}
	if kind != websocket.MessageText || json.Unmarshal(body, &message) != nil || message.Op != expected {
		t.Fatalf("expected %s text message, got %s", expected, body)
	}
}

func TestGracefulShutdownClosesActiveWebSocket(t *testing.T) {
	s := testServer(t)
	url, cancel, result := startServer(t, s)
	conn := dialBootstrapWebSocket(t, url)
	credentials, err := auth.New(s.config.Auth.ClientTokenSecret, s.config.Auth.PublisherKeys)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	token, err := credentials.Issue(domain.ClientScope{
		UserID: "user-a", ProjectID: "project-a", Rooms: []string{"room-a"},
		IssuedAt: now, ExpiresAt: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	writeBootstrapCommand(t, conn, map[string]string{"op": "auth", "token": token})
	readBootstrapOperation(t, conn, "authenticated")
	writeBootstrapCommand(t, conn, map[string]string{"op": "join", "roomId": "room-a"})
	readBootstrapOperation(t, conn, "joined")

	readResult := make(chan error, 1)
	readCtx, cancelRead := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelRead()
	go func() {
		_, _, err := conn.Read(readCtx)
		readResult <- err
	}()
	cancel()
	if err := awaitResult(t, readResult); websocket.CloseStatus(err) != websocket.StatusGoingAway {
		t.Fatalf("WebSocket shutdown status = %v, error %v", websocket.CloseStatus(err), err)
	}
	if err := awaitResult(t, result); err != nil {
		t.Fatal(err)
	}
	if s.ready.Load() {
		t.Fatal("server remained ready after WebSocket drain")
	}
}

func TestShutdownDeadlineForcesUnresponsiveWebSocket(t *testing.T) {
	s := testServer(t)
	s.config.ShutdownTimeout = 30 * time.Millisecond
	url, cancel, result := startServer(t, s)
	// Leave the upgraded client unauthenticated and never read its close frame.
	// This prevents the client library from completing the server close handshake.
	_ = dialBootstrapWebSocket(t, url)
	started := time.Now()
	cancel()
	if err := awaitResult(t, result); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown error = %v, expected deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("WebSocket exceeded shutdown budget: %s", elapsed)
	}
	if s.ready.Load() {
		t.Fatal("server remained ready after forced WebSocket shutdown")
	}
	// Confirm every tracked session finished, rather than merely returning from
	// the bootstrap deadline while a close goroutine remained blocked.
	ctx, cancelDrain := context.WithTimeout(context.Background(), time.Second)
	defer cancelDrain()
	if err := s.gateway.Shutdown(ctx); err != nil {
		t.Fatalf("tracked WebSocket session survived force-close: %v", err)
	}
}
