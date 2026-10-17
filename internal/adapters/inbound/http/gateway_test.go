package httpadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"socketfabric/internal/adapters/outbound/auth"
	"socketfabric/internal/adapters/outbound/memory"
	"socketfabric/internal/application/usecase"
	"socketfabric/internal/domain"
)

type gatewayFixture struct {
	gateway *Gateway
	auth    *auth.Authenticator
	server  *httptest.Server
	keyA    string
	keyB    string
}

func newGatewayFixture(t *testing.T, change func(*GatewayOptions)) *gatewayFixture {
	t.Helper()
	keyA, keyB := strings.Repeat("a", 32), strings.Repeat("b", 32)
	credentials, err := auth.New(strings.Repeat("s", 32), map[string]string{"project-a": keyA, "project-b": keyB})
	if err != nil {
		t.Fatal(err)
	}
	registry := memory.NewRegistry()
	service, err := usecase.NewService(registry, credentials, credentials, registry)
	if err != nil {
		t.Fatal(err)
	}
	options := GatewayOptions{QueueCapacity: 32, MaxConnections: 256, MaxMessageBytes: 16384,
		AuthTimeout: time.Second, WriteTimeout: 200 * time.Millisecond, PingInterval: time.Minute, PongTimeout: time.Second}
	if change != nil {
		change(&options)
	}
	gateway, err := NewGateway(service, options, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	router, err := NewRouter(discardLogger(), func() bool { return true }, gateway)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(router)
	fixture := &gatewayFixture{gateway, credentials, server, keyA, keyB}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = gateway.Shutdown(ctx)
		gateway.ForceClose()
		server.Close()
	})
	return fixture
}

func (f *gatewayFixture) token(t *testing.T, project, user string, rooms []string, ttl time.Duration) string {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	token, err := f.auth.Issue(domain.ClientScope{UserID: user, ProjectID: project, Rooms: rooms, IssuedAt: now, ExpiresAt: now.Add(ttl)})
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func (f *gatewayFixture) dial(t *testing.T, headers http.Header) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, response, err := websocket.Dial(ctx, strings.Replace(f.server.URL, "http", "ws", 1)+"/ws", &websocket.DialOptions{HTTPHeader: headers})
	if err != nil {
		t.Fatalf("dial WebSocket: %v", err)
	}
	if response.Header.Get(requestIDHeader) == "" {
		t.Fatal("upgrade missing request ID")
	}
	conn.SetReadLimit(128 << 10)
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

func wsWrite(t *testing.T, conn *websocket.Conn, value any) {
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

type receivedFrame struct {
	Op           string       `json:"op"`
	ConnectionID string       `json:"connectionId"`
	ProjectID    string       `json:"projectId"`
	UserID       string       `json:"userId"`
	RoomID       string       `json:"roomId"`
	Event        domain.Event `json:"event"`
	Error        apiError     `json:"error"`
}

func wsRead(t *testing.T, conn *websocket.Conn) receivedFrame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	kind, body, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if kind != websocket.MessageText {
		t.Fatal("non-text server frame")
	}
	var frame receivedFrame
	if err := json.Unmarshal(body, &frame); err != nil {
		t.Fatal(err)
	}
	return frame
}

func (f *gatewayFixture) connect(t *testing.T, project, user string, rooms []string) *websocket.Conn {
	t.Helper()
	conn := f.dial(t, nil)
	wsWrite(t, conn, command{Op: "auth", Token: f.token(t, project, user, rooms, time.Minute)})
	frame := wsRead(t, conn)
	if frame.Op != "authenticated" || frame.ProjectID != project || frame.UserID != user || frame.ConnectionID == "" {
		t.Fatalf("authentication frame = %+v", frame)
	}
	return conn
}

func membership(t *testing.T, conn *websocket.Conn, operation, room string) {
	t.Helper()
	wsWrite(t, conn, command{Op: operation, RoomID: room})
	frame := wsRead(t, conn)
	want := "joined"
	if operation == "leave" {
		want = "left"
	}
	if frame.Op != want || frame.RoomID != room {
		t.Fatalf("membership response = %+v, want %s %s", frame, want, room)
	}
}

func (f *gatewayFixture) post(t *testing.T, key, body, mediaType string) (int, []byte) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, f.server.URL+"/v1/events", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	request.Header.Set("Content-Type", mediaType)
	request.Header.Set(requestIDHeader, "publish-test")
	response, err := (&http.Client{Timeout: 2 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.Header.Get("Content-Type") != "application/json; charset=utf-8" || response.Header.Get(requestIDHeader) != "publish-test" {
		t.Fatal("publish response missing JSON/correlation headers")
	}
	return response.StatusCode, data
}

func (f *gatewayFixture) publish(t *testing.T, project, room string) usecase.PublishResult {
	t.Helper()
	key := f.keyA
	if project == "project-b" {
		key = f.keyB
	}
	status, data := f.post(t, key, fmt.Sprintf(`{"projectId":%q,"type":"resource.updated","roomId":%q,"data":{"value":1}}`, project, room), "application/json")
	if status != http.StatusAccepted {
		t.Fatalf("publish = %d %s", status, data)
	}
	var result usecase.PublishResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result.Event.Validate() != nil || !strings.HasPrefix(result.Event.EventID, "evt_") {
		t.Fatalf("invalid server event: %+v", result.Event)
	}
	return result
}

func TestRealtimeFlowAndScopeIsolation(t *testing.T) {
	f := newGatewayFixture(t, nil)
	first := f.connect(t, "project-a", "same-user", []string{"room-a", "room-b"})
	second := f.connect(t, "project-a", "same-user", []string{"room-a"})
	otherRoom := f.connect(t, "project-a", "other-user", []string{"room-b"})
	otherProject := f.connect(t, "project-b", "other-user", []string{"room-a"})
	for _, conn := range []*websocket.Conn{first, second, otherProject} {
		membership(t, conn, "join", "room-a")
	}
	membership(t, otherRoom, "join", "room-b")
	result := f.publish(t, "project-a", "room-a")
	if result.QueuedConnections != 2 {
		t.Fatalf("queued=%d, want 2 independent tabs", result.QueuedConnections)
	}
	for _, conn := range []*websocket.Conn{first, second} {
		frame := wsRead(t, conn)
		if frame.Op != "event" || frame.Event.EventID != result.Event.EventID || frame.Event.ProjectID != "project-a" || !bytes.Equal(frame.Event.Data, result.Event.Data) || !frame.Event.Timestamp.Equal(result.Event.Timestamp) {
			t.Fatalf("unexpected event frame: %+v", frame)
		}
	}
	// Publish completes all queue attempts before returning. An idempotent join
	// ACK is a FIFO barrier: wrongly routed events would arrive before this ACK.
	membership(t, otherRoom, "join", "room-b")
	membership(t, otherProject, "join", "room-a")
	bResult := f.publish(t, "project-b", "room-a")
	if bResult.QueuedConnections != 1 || wsRead(t, otherProject).Event.EventID != bResult.Event.EventID {
		t.Fatal("project-b event did not stay within project-b")
	}
	membership(t, first, "join", "room-a")
	membership(t, second, "join", "room-a")
	membership(t, first, "leave", "room-a")
	membership(t, first, "leave", "room-a")
	result = f.publish(t, "project-a", "room-a")
	if result.QueuedConnections != 1 || wsRead(t, second).Event.EventID != result.Event.EventID {
		t.Fatal("leave did not isolate the remaining tab")
	}
	membership(t, first, "join", "room-b")
}

func TestUnauthorizedJoinPublishAndInvalidPayloads(t *testing.T) {
	f := newGatewayFixture(t, nil)
	conn := f.connect(t, "project-a", "user", []string{"room-a"})
	wsWrite(t, conn, command{Op: "join", RoomID: "private-room"})
	if frame := wsRead(t, conn); frame.Op != "error" || frame.Error.Code != "forbidden" || frame.Error.RequestID == "" {
		t.Fatalf("unauthorized join = %+v", frame)
	}
	membership(t, conn, "join", "room-a")
	valid := `{"projectId":"project-a","type":"resource.updated","roomId":"room-a","data":null}`
	for _, tc := range []struct {
		key, body, media string
		status           int
		code             string
	}{
		{"", valid, "application/json", 401, "unauthorized"},
		{"invalid-key", valid, "application/json", 401, "unauthorized"},
		{f.keyB, valid, "application/json", 403, "forbidden"},
		{f.keyA, valid, "text/plain", 415, "unsupported_media_type"},
		{f.keyA, `{`, "application/json", 400, "invalid_request"},
		{f.keyA, valid + `{}`, "application/json", 400, "invalid_request"},
		{f.keyA, `{"projectId":"project-a","type":"resource.updated","roomId":"room-a"}`, "application/json", 400, "invalid_request"},
		{f.keyA, `{"projectId":"project-a","type":"resource.updated","roomId":"room-a","data":{},"eventId":"publisher-id"}`, "application/json", 400, "invalid_request"},
		{f.keyA, `{"projectId":"project-a","type":"resource.updated","roomId":"room-a","data":"` + strings.Repeat("x", maxPublishBytes) + `"}`, "application/json", 413, "payload_too_large"},
	} {
		status, data := f.post(t, tc.key, tc.body, tc.media)
		var envelope errorResponse
		if err := json.Unmarshal(data, &envelope); err != nil || status != tc.status || envelope.Error.Code != tc.code {
			t.Fatalf("error response = %d %s, want %d %s", status, data, tc.status, tc.code)
		}
	}
	if result := f.publish(t, "project-a", "no-subscribers"); result.QueuedConnections != 0 {
		t.Fatal("empty room should accept with queuedConnections=0")
	}
}

func TestHandshakeErrorsAndOrigins(t *testing.T) {
	f := newGatewayFixture(t, func(options *GatewayOptions) {
		options.AllowedOrigins = []string{"https://app.example.com", "http://[::1]:3000"}
	})
	response, err := http.Get(f.server.URL + "/ws")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	var envelope errorResponse
	if json.Unmarshal(body, &envelope) != nil || response.StatusCode != http.StatusUpgradeRequired || response.Header.Get("Content-Type") != "application/json; charset=utf-8" || envelope.Error.RequestID == "" || response.Header.Get("Upgrade") != "websocket" {
		t.Fatalf("handshake error = %d %s headers=%v", response.StatusCode, body, response.Header)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, response, err := websocket.Dial(ctx, strings.Replace(f.server.URL, "http", "ws", 1)+"/ws", &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {"https://evil.example.com/ORIGIN_SECRET"}}})
	if conn != nil {
		conn.CloseNow()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusForbidden || response.Header.Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatal("unapproved origin not rejected with JSON error")
	}
	for _, origin := range []string{"https://app.example.com", "http://[::1]:3000"} {
		conn := f.dial(t, http.Header{"Origin": {origin}})
		wsWrite(t, conn, command{Op: "auth", Token: f.token(t, "project-a", "user", []string{"room-a"}, time.Minute)})
		if wsRead(t, conn).Op != "authenticated" {
			t.Fatal("allowed origin failed")
		}
	}
}

func TestInvalidAuthenticationAndTimeout(t *testing.T) {
	f := newGatewayFixture(t, func(options *GatewayOptions) { options.AuthTimeout = 40 * time.Millisecond })
	for _, input := range []command{{Op: "join", RoomID: "room-a"}, {Op: "auth", Token: "tampered-token"}} {
		conn := f.dial(t, nil)
		wsWrite(t, conn, input)
		if frame := wsRead(t, conn); frame.Op != "error" || frame.Error.Code != "unauthorized" {
			t.Fatalf("auth error = %+v", frame)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, _, err := conn.Read(ctx)
		cancel()
		if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
			t.Fatalf("invalid auth close = %v", err)
		}
	}
	conn := f.dial(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _, err := conn.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("auth timeout close = %v", err)
	}
}

func TestTokenExpiryClosesAndUnsubscribes(t *testing.T) {
	f := newGatewayFixture(t, nil)
	conn := f.dial(t, nil)
	wsWrite(t, conn, command{Op: "auth", Token: f.token(t, "project-a", "user", []string{"room-a"}, 2*time.Second)})
	if wsRead(t, conn).Op != "authenticated" {
		t.Fatal("authentication failed")
	}
	membership(t, conn, "join", "room-a")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _, err := conn.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("expiry close = %v", err)
	}
	if f.publish(t, "project-a", "room-a").QueuedConnections != 0 {
		t.Fatal("expired connection remained subscribed")
	}
}

func TestDisconnectCleanupReconnectAndShutdown(t *testing.T) {
	f := newGatewayFixture(t, nil)
	conn := f.connect(t, "project-a", "user", []string{"room-a"})
	membership(t, conn, "join", "room-a")
	if err := conn.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatal(err)
	}
	if f.publish(t, "project-a", "room-a").QueuedConnections != 0 {
		t.Fatal("closed connection accepted queued event")
	}
	conn = f.connect(t, "project-a", "user", []string{"room-a"})
	membership(t, conn, "join", "room-a")
	if f.publish(t, "project-a", "room-a").QueuedConnections != 1 || wsRead(t, conn).Op != "event" {
		t.Fatal("reconnect did not receive live event")
	}
	closed := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, _, err := conn.Read(ctx)
		closed <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := f.gateway.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if websocket.CloseStatus(<-closed) != websocket.StatusGoingAway {
		t.Fatal("shutdown did not send going-away close")
	}
	status, _ := f.post(t, f.keyA, `{}`, "application/json")
	if status != http.StatusServiceUnavailable {
		t.Fatal("publish accepted after drain")
	}
}

func TestShutdownDeadlineForcesUnresponsiveSocket(t *testing.T) {
	f := newGatewayFixture(t, func(options *GatewayOptions) { options.WriteTimeout = 5 * time.Second })
	// Client never reads, so it will not answer the close handshake.
	conn := f.dial(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := f.gateway.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second {
		t.Fatalf("forced shutdown = %v, elapsed %v", err, time.Since(started))
	}
	conn.CloseNow()
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Second)
	defer cleanupCancel()
	if err := f.gateway.Shutdown(cleanupCtx); err != nil {
		t.Fatal("forced socket did not release session lease", err)
	}
}

func TestCapacityAndConcurrentAdmissionDrain(t *testing.T) {
	f := newGatewayFixture(t, func(options *GatewayOptions) { options.MaxConnections = 1 })
	_ = f.dial(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, response, err := websocket.Dial(ctx, strings.Replace(f.server.URL, "http", "ws", 1)+"/ws", nil)
	if conn != nil {
		conn.CloseNow()
	}
	if err == nil || response == nil || response.StatusCode != 503 {
		t.Fatal("unauthenticated sockets did not count toward capacity")
	}

	f = newGatewayFixture(t, nil)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for range 16 {
		workers.Go(func() {
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			conn, _, err := websocket.Dial(ctx, strings.Replace(f.server.URL, "http", "ws", 1)+"/ws", nil)
			if err == nil {
				conn.CloseNow()
			}
		})
	}
	close(start)
	f.gateway.BeginDrain()
	workers.Wait()
	if err := f.gateway.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestBinaryOversizeAndDemo(t *testing.T) {
	f := newGatewayFixture(t, func(options *GatewayOptions) { options.DemoEnabled = true; options.MaxMessageBytes = 1024 })
	for _, tc := range []struct {
		kind   websocket.MessageType
		data   []byte
		status websocket.StatusCode
	}{
		{websocket.MessageBinary, []byte("binary"), websocket.StatusUnsupportedData},
		{websocket.MessageText, bytes.Repeat([]byte("x"), 1025), websocket.StatusMessageTooBig},
	} {
		conn := f.dial(t, nil)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		if err := conn.Write(ctx, tc.kind, tc.data); err != nil {
			t.Fatal(err)
		}
		_, _, err := conn.Read(ctx)
		cancel()
		if websocket.CloseStatus(err) != tc.status {
			t.Fatalf("message close = %v, want %d", err, tc.status)
		}
	}
	response, err := http.Get(f.server.URL + "/demo")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || !bytes.Contains(body, []byte(`src="/demo.js"`)) || bytes.Contains(body, []byte(f.keyA)) {
		t.Fatal("demo not served safely")
	}
	response, err = http.Get(f.server.URL + "/demo.js")
	if err != nil {
		t.Fatal(err)
	}
	script, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "text/javascript; charset=utf-8" || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("X-Content-Type-Options") != "nosniff" || !bytes.Contains(script, []byte("textContent")) || bytes.Contains(script, []byte(f.keyA)) {
		t.Fatal("demo script not served safely")
	}
	f = newGatewayFixture(t, nil)
	for _, path := range []string{"/demo", "/demo.js"} {
		response, err = http.Get(f.server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusNotFound {
			t.Fatal("demo enabled by default")
		}
	}
}

func TestReconnectAuthenticatesAgainAndDoesNotRestoreOldRooms(t *testing.T) {
	f := newGatewayFixture(t, nil)
	first := f.dial(t, nil)
	wsWrite(t, first, command{Op: "auth", Token: f.token(t, "project-a", "user", []string{"room-a"}, time.Minute)})
	previous := wsRead(t, first)
	membership(t, first, "join", "room-a")
	initial := f.publish(t, "project-a", "room-a")
	if wsRead(t, first).Event.EventID != initial.Event.EventID {
		t.Fatal("initial event missing")
	}
	f.gateway.mu.Lock()
	old := f.gateway.sessions[previous.ConnectionID]
	f.gateway.mu.Unlock()
	if old == nil {
		t.Fatal("initial connection missing")
	}
	_ = first.CloseNow()
	awaitSignal(t, old.closing, "abnormal disconnect cleanup")
	if f.publish(t, "project-a", "room-a").QueuedConnections != 0 {
		t.Fatal("disconnected membership retained")
	}

	unauthenticated := f.dial(t, nil)
	wsWrite(t, unauthenticated, command{Op: "join", RoomID: "room-a"})
	if frame := wsRead(t, unauthenticated); frame.Op != "error" || frame.Error.Code != "unauthorized" {
		t.Fatalf("reconnect skipped authentication: %+v", frame)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	_, _, err := unauthenticated.Read(ctx)
	cancel()
	if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("unauthenticated reconnect close: %v", err)
	}

	// Same user, new token with narrower/different permissions: nothing from the
	// previous connection may grant room-a access or restore its membership.
	second := f.connect(t, "project-a", "user", []string{"room-b"})
	if f.publish(t, "project-a", "room-a").QueuedConnections != 0 || f.publish(t, "project-a", "room-b").QueuedConnections != 0 {
		t.Fatal("fresh connection implicitly subscribed")
	}
	wsWrite(t, second, command{Op: "join", RoomID: "room-a"})
	if frame := wsRead(t, second); frame.Op != "error" || frame.Error.Code != "forbidden" {
		t.Fatalf("old token room permission reused: %+v", frame)
	}
	for range 2 {
		membership(t, second, "join", "room-b")
	}
	result := f.publish(t, "project-a", "room-b")
	if result.QueuedConnections != 1 || wsRead(t, second).Event.EventID != result.Event.EventID {
		t.Fatal("idempotent resubscription failed")
	}
	for range 2 {
		membership(t, second, "leave", "room-b")
	}
	if f.publish(t, "project-a", "room-b").QueuedConnections != 0 {
		t.Fatal("unsubscribe retained membership")
	}
}
