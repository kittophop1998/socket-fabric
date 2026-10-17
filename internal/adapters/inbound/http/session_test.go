package httpadapter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"socketfabric/internal/domain"
)

// stalledConn can deterministically stop writes after the real HTTP upgrade.
// net.Pipe provides proper deadline and close behavior without filling TCP buffers.
type stalledConn struct {
	net.Conn
	writer, peer net.Conn
	armed        atomic.Bool
	started      chan struct{}
	once         sync.Once
}

func (c *stalledConn) Write(data []byte) (int, error) {
	if !c.armed.Load() {
		return c.Conn.Write(data)
	}
	c.once.Do(func() { close(c.started) })
	return c.writer.Write(data)
}

func (c *stalledConn) Close() error {
	_ = c.writer.Close()
	_ = c.peer.Close()
	return c.Conn.Close()
}

func (c *stalledConn) SetWriteDeadline(deadline time.Time) error {
	_ = c.writer.SetWriteDeadline(deadline)
	return c.Conn.SetWriteDeadline(deadline)
}

func (c *stalledConn) SetDeadline(deadline time.Time) error {
	_ = c.writer.SetDeadline(deadline)
	return c.Conn.SetDeadline(deadline)
}

type sessionHijacker struct {
	http.ResponseWriter
	raw *stalledConn
}

func (w *sessionHijacker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, buffers, err := w.ResponseWriter.(http.Hijacker).Hijack()
	if err != nil {
		return nil, nil, err
	}
	if err := buffers.Writer.Flush(); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	writer, peer := net.Pipe()
	w.raw = &stalledConn{Conn: conn, writer: writer, peer: peer, started: make(chan struct{})}
	buffers.Writer.Reset(w.raw)
	return w.raw, buffers, nil
}

// A standalone session lets tests stop its writer without disrupting a healthy
// gateway connection. It has no gateway WaitGroup lease and never calls run().
func standaloneSession(t *testing.T, gateway *Gateway) (*session, *websocket.Conn, *stalledConn) {
	t.Helper()
	created := make(chan *session, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writer := &sessionHijacker{ResponseWriter: w}
		conn, err := websocket.Accept(writer, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		created <- newSession(gateway, conn, writer.raw, context.Background())
	}))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	client, _, err := websocket.Dial(ctx, strings.Replace(server.URL, "http", "ws", 1), nil)
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	var s *session
	select {
	case s = <-created:
	case <-ctx.Done():
		_ = client.CloseNow()
		server.Close()
		t.Fatal("standalone session not created")
	}
	t.Cleanup(func() {
		_ = s.raw.Close()
		s.cancel()
		s.beginClose(websocket.StatusGoingAway, "test_cleanup")
		awaitSignal(t, s.closeDone, "standalone close")
		_ = client.CloseNow()
		server.Close()
	})
	return s, client, s.raw.(*stalledConn)
}

func awaitSignal(t *testing.T, done <-chan struct{}, operation string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal(operation + " did not complete")
	}
}

func subscribeStandalone(t *testing.T, s *session) {
	t.Helper()
	now := time.Now()
	scope := domain.ClientScope{UserID: "user", ProjectID: "project-a", Rooms: []string{"room-a"}, IssuedAt: now, ExpiresAt: now.Add(time.Minute)}
	s.scope = &scope
	if err := s.gateway.service.Connect(s.ctx, s.id, scope, s); err != nil {
		t.Fatal(err)
	}
	if err := s.gateway.service.SubscribeRoom(s.ctx, s.id, scope, "room-a"); err != nil {
		t.Fatal(err)
	}
}

func TestSessionQueueOverflowDisconnectsOnlySlowConnection(t *testing.T) {
	f := newGatewayFixture(t, func(options *GatewayOptions) { options.QueueCapacity = 1 })
	healthy := f.connect(t, "project-a", "user", []string{"room-a"})
	membership(t, healthy, "join", "room-a")
	slow, client, _ := standaloneSession(t, f.gateway)
	subscribeStandalone(t, slow)
	// No writer is running for this session: one pending control message fills
	// its bounded queue while the healthy connection continues writing normally.
	if !slow.sendControl(map[string]string{"op": "queued"}) {
		t.Fatal("could not preload outbound queue")
	}
	result := f.publish(t, "project-a", "room-a")
	if result.QueuedConnections != 1 || wsRead(t, healthy).Event.EventID != result.Event.EventID {
		t.Fatal("overflowing connection disrupted the healthy subscriber")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _, err := client.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusTryAgainLater {
		t.Fatalf("queue overflow close = %v", err)
	}
	awaitSignal(t, slow.closeDone, "slow connection close")
	if f.publish(t, "project-a", "room-a").QueuedConnections != 1 || wsRead(t, healthy).Op != "event" {
		t.Fatal("slow connection was not removed independently")
	}
}

func TestSessionWriteTimeoutRemovesSubscription(t *testing.T) {
	f := newGatewayFixture(t, func(options *GatewayOptions) { options.WriteTimeout = 40 * time.Millisecond })
	s, _, raw := standaloneSession(t, f.gateway)
	subscribeStandalone(t, s)
	raw.armed.Store(true)
	if !s.sendControl(map[string]string{"op": "blocked"}) {
		t.Fatal("could not enqueue pending write")
	}
	go s.writeLoop()
	awaitSignal(t, raw.started, "blocked write")
	awaitSignal(t, s.writerDone, "writer timeout")
	awaitSignal(t, s.closeDone, "write-timeout close")
	if f.publish(t, "project-a", "room-a").QueuedConnections != 0 {
		t.Fatal("timed-out writer remained subscribed")
	}
}

func TestSessionMissingPongClosesAndCleansMembership(t *testing.T) {
	f := newGatewayFixture(t, func(options *GatewayOptions) {
		options.PingInterval = 20 * time.Millisecond
		options.PongTimeout = 30 * time.Millisecond
		options.WriteTimeout = 100 * time.Millisecond
	})
	client := f.connect(t, "project-a", "user", []string{"room-a"})
	membership(t, client, "join", "room-a")
	f.gateway.mu.Lock()
	var s *session
	for _, current := range f.gateway.sessions {
		s = current
	}
	f.gateway.mu.Unlock()
	if s == nil {
		t.Fatal("authenticated session missing")
	}
	// coder clients answer pings only while Read is active. Stop reading until
	// the server has independently detected the absent pong and started close.
	awaitSignal(t, s.closing, "missing-pong detection")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _, err := client.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusGoingAway {
		t.Fatalf("missing-pong close = %v", err)
	}
	awaitSignal(t, s.closeDone, "heartbeat close")
	if f.publish(t, "project-a", "room-a").QueuedConnections != 0 {
		t.Fatal("unresponsive connection remained subscribed")
	}
}

func TestSessionRemainsHealthyPastHTTPDeadlines(t *testing.T) {
	f := newGatewayFixture(t, func(options *GatewayOptions) {
		options.PingInterval = 20 * time.Millisecond
		options.PongTimeout = 200 * time.Millisecond
	})
	handler := f.server.Config.Handler
	f.server.Close()
	server := httptest.NewUnstartedServer(handler)
	server.Config.ReadHeaderTimeout = 30 * time.Millisecond
	server.Config.ReadTimeout = 30 * time.Millisecond
	server.Config.WriteTimeout = 30 * time.Millisecond
	server.Config.IdleTimeout = 30 * time.Millisecond
	server.Start()
	f.server = server
	t.Cleanup(server.Close)
	var pings atomic.Int32
	threePings := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	client, _, err := websocket.Dial(ctx, strings.Replace(server.URL, "http", "ws", 1)+"/ws", &websocket.DialOptions{
		OnPingReceived: func(context.Context, []byte) bool {
			if pings.Add(1) == 3 {
				close(threePings)
			}
			return true
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.CloseNow() })
	wsWrite(t, client, command{Op: "auth", Token: f.token(t, "project-a", "user", []string{"room-a"}, time.Minute)})
	if wsRead(t, client).Op != "authenticated" {
		t.Fatal("client authentication failed")
	}
	membership(t, client, "join", "room-a")
	frames := make(chan receivedFrame, 1)
	readErrors := make(chan error, 1)
	readCtx, cancelReads := context.WithCancel(context.Background())
	defer cancelReads()
	go func() {
		for {
			_, body, err := client.Read(readCtx)
			if err != nil {
				readErrors <- err
				return
			}
			var frame receivedFrame
			if err := json.Unmarshal(body, &frame); err != nil {
				readErrors <- err
				return
			}
			frames <- frame
		}
	}()
	select {
	case <-threePings:
	case err := <-readErrors:
		t.Fatalf("HTTP timeout affected idle WebSocket: %v", err)
	case <-ctx.Done():
		t.Fatal("healthy client did not exchange heartbeats")
	}
	result := f.publish(t, "project-a", "room-a")
	select {
	case frame := <-frames:
		if result.QueuedConnections != 1 || frame.Event.EventID != result.Event.EventID {
			t.Fatal("healthy idle WebSocket did not receive published event")
		}
	case err := <-readErrors:
		t.Fatal("healthy idle WebSocket disconnected", err)
	case <-ctx.Done():
		t.Fatal("event did not arrive after HTTP deadlines")
	}
}

func sessionEvent(index int) domain.Event {
	return domain.Event{EventID: fmt.Sprintf("event-%d", index), ProjectID: "project-a", RoomID: "room-a",
		Type: "test.event", Data: json.RawMessage(fmt.Sprintf(`{"index":%d}`, index)), Timestamp: time.Now().UTC()}
}

func TestSessionAcceptedEventsKeepFIFOAndWrittenAccounting(t *testing.T) {
	f := newGatewayFixture(t, nil)
	s, client, _ := standaloneSession(t, f.gateway)
	subscribeStandalone(t, s)
	const count = 12
	for index := range count {
		if !s.TryEnqueue(sessionEvent(index)) {
			t.Fatal("bounded queue rejected event before capacity")
		}
	}
	go s.writeLoop()
	for index := range count {
		frame := wsRead(t, client)
		if frame.Op != "event" || frame.Event.EventID != fmt.Sprintf("event-%d", index) {
			t.Fatalf("out-of-order accepted event at %d: %+v", index, frame)
		}
	}
	// Wait for a later control write so accounting for every preceding event
	// is complete, rather than racing the writer after receiving the last byte.
	written := make(chan error, 1)
	if !s.enqueue(outboundMessage{data: []byte(`{"op":"complete"}`), written: written}) {
		t.Fatal("completion control was rejected")
	}
	if wsRead(t, client).Op != "complete" {
		t.Fatal("completion frame missing")
	}
	select {
	case err := <-written:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("completion write did not finish")
	}
	s.mu.Lock()
	accepted, rejected, discarded, completed, highWater := s.eventsAccepted, s.eventsRejected, s.eventsDiscarded, s.eventsWritten, s.queueHighWater
	s.mu.Unlock()
	if accepted != count || completed != count || rejected != 0 || discarded != 0 || highWater != count {
		t.Fatalf("event accounting accepted=%d written=%d rejected=%d discarded=%d highwater=%d", accepted, completed, rejected, discarded, highWater)
	}
	s.beginClose(websocket.StatusGoingAway, "test_cleanup")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _, _ = client.Read(ctx)
	awaitSignal(t, s.writerDone, "ordered writer cleanup")
}

func TestSessionConcurrentOverflowRejectsAllLaterEventsAndDiscardsQueue(t *testing.T) {
	f := newGatewayFixture(t, func(options *GatewayOptions) { options.QueueCapacity = 1 })
	s, client, _ := standaloneSession(t, f.gateway)
	subscribeStandalone(t, s)
	if !s.TryEnqueue(sessionEvent(0)) {
		t.Fatal("could not fill queue")
	}
	const attempts = 64
	start := make(chan struct{})
	var workers sync.WaitGroup
	var accepted atomic.Uint64
	for index := range attempts {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			if s.TryEnqueue(sessionEvent(index + 1)) {
				accepted.Add(1)
			}
		}()
	}
	close(start)
	workers.Wait()
	if accepted.Load() != 0 {
		t.Fatal("an event was admitted after queue overflow")
	}
	s.mu.Lock()
	closed, reason, count, rejected, discarded, depth := s.closed, s.closeReason, s.eventsAccepted, s.eventsRejected, s.eventsDiscarded, len(s.outbound)
	s.mu.Unlock()
	if !closed || reason != "slow_client" || count != 1 || rejected != attempts || discarded != 1 || depth != 0 {
		t.Fatalf("overflow state closed=%t reason=%s accepted=%d rejected=%d discarded=%d depth=%d", closed, reason, count, rejected, discarded, depth)
	}
	if f.publish(t, "project-a", "room-a").QueuedConnections != 0 {
		t.Fatal("slow client membership survived closure")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _, err := client.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusTryAgainLater {
		t.Fatalf("overflow close = %v", err)
	}
	awaitSignal(t, s.closeDone, "overflow close")
}

func TestSessionHeartbeatProgressesDuringBlockedDataWrite(t *testing.T) {
	f := newGatewayFixture(t, func(options *GatewayOptions) {
		options.PingInterval = 20 * time.Millisecond
		options.PongTimeout = 40 * time.Millisecond
		options.WriteTimeout = 500 * time.Millisecond
	})
	s, _, raw := standaloneSession(t, f.gateway)
	subscribeStandalone(t, s)
	raw.armed.Store(true)
	if !s.TryEnqueue(sessionEvent(0)) {
		t.Fatal("could not enqueue blocked event")
	}
	go s.writeLoop()
	awaitSignal(t, raw.started, "blocked data write")
	go s.heartbeatLoop()
	select {
	case <-s.closing:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("data writer starved heartbeat until its longer write timeout")
	}
	s.mu.Lock()
	reason := s.closeReason
	s.mu.Unlock()
	if reason != "heartbeat_timeout" {
		t.Fatalf("blocked-writer detection reason = %q", reason)
	}
	if f.publish(t, "project-a", "room-a").QueuedConnections != 0 {
		t.Fatal("heartbeat failure did not immediately remove membership")
	}
	_ = raw.Close()
	awaitSignal(t, s.heartbeatDone, "heartbeat cleanup")
	awaitSignal(t, s.writerDone, "blocked writer cleanup")
}

func gatewaySession(t *testing.T, gateway *Gateway) *session {
	t.Helper()
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	if len(gateway.sessions) != 1 {
		t.Fatalf("gateway has %d sessions, want one", len(gateway.sessions))
	}
	for _, s := range gateway.sessions {
		return s
	}
	panic("unreachable")
}

func TestSessionIdleTimeoutIgnoresPongsAndOutboundEvents(t *testing.T) {
	f := newGatewayFixture(t, func(options *GatewayOptions) {
		options.IdleTimeout = 200 * time.Millisecond
		options.PingInterval = 20 * time.Millisecond
		options.PongTimeout = 100 * time.Millisecond
	})
	client := f.connect(t, "project-a", "user", []string{"room-a"})
	membership(t, client, "join", "room-a")
	s := gatewaySession(t, f.gateway)
	var events atomic.Int32
	readDone := make(chan error, 1)
	go func() {
		for {
			_, body, err := client.Read(context.Background())
			if err != nil {
				readDone <- err
				return
			}
			var frame receivedFrame
			if json.Unmarshal(body, &frame) == nil && frame.Op == "event" {
				events.Add(1)
			}
		}
	}()
	ticker := time.NewTicker(15 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
loop:
	for {
		select {
		case <-s.closing:
			break loop
		case <-ticker.C:
			f.publish(t, "project-a", "room-a")
		case <-deadline.C:
			t.Fatal("outbound events or heartbeat pongs kept an idle client alive")
		}
	}
	select {
	case err := <-readDone:
		if websocket.CloseStatus(err) != websocket.StatusGoingAway {
			t.Fatalf("idle close = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("idle close handshake did not finish")
	}
	awaitSignal(t, s.closeDone, "idle close")
	s.mu.Lock()
	reason := s.closeReason
	s.mu.Unlock()
	if reason != "idle_timeout" || events.Load() < 3 {
		t.Fatalf("idle detection reason=%s delivered events=%d", reason, events.Load())
	}
	if f.publish(t, "project-a", "room-a").QueuedConnections != 0 {
		t.Fatal("idle connection membership was not removed")
	}
}

func TestSessionApplicationTextResetsIdleTimeout(t *testing.T) {
	f := newGatewayFixture(t, func(options *GatewayOptions) {
		options.IdleTimeout = 200 * time.Millisecond
		options.PingInterval = 20 * time.Millisecond
		options.PongTimeout = 100 * time.Millisecond
	})
	client := f.connect(t, "project-a", "user", []string{"room-a"})
	s := gatewaySession(t, f.gateway)
	ticker := time.NewTicker(60 * time.Millisecond)
	defer ticker.Stop()
	for range 5 {
		<-ticker.C
		// Repeated joins exercise idle reset without changing authorization or
		// creating duplicate memberships over more than one idle interval.
		membership(t, client, "join", "room-a")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _, err := client.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusGoingAway {
		t.Fatalf("post-activity idle close = %v", err)
	}
	awaitSignal(t, s.closeDone, "post-activity idle close")
	s.mu.Lock()
	reason := s.closeReason
	s.mu.Unlock()
	if reason != "idle_timeout" {
		t.Fatalf("post-activity close reason = %q", reason)
	}
}

func TestSessionClosingLogCountsControlOverflowWithoutPayloads(t *testing.T) {
	f := newGatewayFixture(t, func(options *GatewayOptions) { options.QueueCapacity = 1 })
	var output bytes.Buffer
	f.gateway.logger = slog.New(slog.NewJSONHandler(&output, nil))
	s, client, _ := standaloneSession(t, f.gateway)
	if !s.sendControl(map[string]string{"op": "private-control-payload"}) || s.sendControl(map[string]string{"op": "overflow"}) {
		t.Fatal("control queue did not enforce its capacity")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _, err := client.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusTryAgainLater {
		t.Fatalf("control overflow close: %v", err)
	}
	awaitSignal(t, s.closeDone, "control overflow close")
	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if record["messages_rejected"] != float64(1) || record["messages_discarded"] != float64(1) || record["events_rejected"] != float64(0) || record["slow_client_disconnect"] != true || record["close_code"] != float64(1013) {
		t.Fatalf("incomplete overflow observation: %v", record)
	}
	if strings.Contains(output.String(), "private-control-payload") || strings.Contains(output.String(), f.keyA) {
		t.Fatal("closing log exposed payload or credentials")
	}
}
