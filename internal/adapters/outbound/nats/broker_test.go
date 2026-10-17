package natsadapter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"socketfabric/internal/domain"
)

type recordingBroadcaster struct {
	events chan domain.Event
	count  int
}

func (r *recordingBroadcaster) Broadcast(ctx context.Context, event domain.Event) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	r.events <- event.Clone()
	return r.count, nil
}

func testOptions(address string) Options {
	return Options{URL: address, Token: strings.Repeat("t", 32), SubjectPrefix: "socketfabric", NodeID: "node-a",
		Projects: []string{"project-a", "project.with.dots"}, ConnectTimeout: time.Second, PublishTimeout: time.Second}
}

func testEvent(project string) domain.Event {
	return domain.Event{EventID: "evt_test", ProjectID: project, Type: "test.event", RoomID: "room-a", Data: json.RawMessage(`{"value":"private payload"}`), Timestamp: time.Now().UTC()}
}

func newTestBroker(t *testing.T, options Options, local *recordingBroadcaster) *Broker {
	t.Helper()
	b, err := New(options, local, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	return b
}

func TestConstructorValidationAndProjectCopy(t *testing.T) {
	local := &recordingBroadcaster{events: make(chan domain.Event, 1)}
	opts := testOptions("nats://127.0.0.1:4222")
	b := newTestBroker(t, opts, local)
	opts.Projects[0] = "changed"
	if b.opts.Projects[0] != "project-a" || b.subjects["project.with.dots"] != "socketfabric.v1.events.cHJvamVjdC53aXRoLmRvdHM" {
		t.Fatal("project namespace was mutated or not encoded")
	}
	if b.Ready() {
		t.Fatal("constructor performed startup")
	}
	for _, mutate := range []func(*Options){
		func(o *Options) { o.Token = "short" },
		func(o *Options) { o.URL = "nats://user:secret@127.0.0.1:4222" },
		func(o *Options) { o.URL = "nats://127.0.0.1" },
		func(o *Options) { o.URL = "http://127.0.0.1:4222" },
		func(o *Options) { o.URL = "nats://127.0.0.1:4222/path" },
		func(o *Options) { o.SubjectPrefix = "socketfabric..events" },
		func(o *Options) { o.SubjectPrefix = "socketfabric.*" },
		func(o *Options) { o.NodeID = "node/*" },
		func(o *Options) { o.Projects = nil },
		func(o *Options) { o.Projects = []string{"project-a", "project-a"} },
		func(o *Options) { o.Projects = make([]string, 129) },
		func(o *Options) { o.PublishTimeout = 0 },
	} {
		o := testOptions("nats://127.0.0.1:4222")
		mutate(&o)
		_, err := New(o, local, slog.New(slog.NewJSONHandler(io.Discard, nil)))
		if !errors.Is(err, domain.ErrInvalid) || strings.Contains(err.Error(), o.Token) && len(o.Token) >= 32 {
			t.Fatalf("constructor error = %v", err)
		}
	}
}

func TestDecodeEnforcesNamespaceAndPayload(t *testing.T) {
	b := newTestBroker(t, testOptions("nats://127.0.0.1:4222"), &recordingBroadcaster{events: make(chan domain.Event, 1)})
	event := testEvent("project-a")
	body, _ := json.Marshal(event)
	decoded, err := b.decode("project-a", &nats.Msg{Subject: b.subjects["project-a"], Data: body})
	if err != nil || decoded.EventID != event.EventID || !decoded.Timestamp.Equal(event.Timestamp) {
		t.Fatalf("decode = %#v, %v", decoded, err)
	}
	for _, message := range []*nats.Msg{
		nil,
		{Subject: "socketfabric.v1.events.other", Data: body},
		{Subject: b.subjects["project-a"], Data: append(append([]byte(nil), body...), []byte(" null")...)},
		{Subject: b.subjects["project-a"], Data: []byte(strings.Replace(string(body), `"project-a"`, `"project-b"`, 1))},
		{Subject: b.subjects["project-a"], Data: []byte(strings.Replace(string(body), `"type":`, `"unknown":1,"type":`, 1))},
		{Subject: b.subjects["project-a"], Data: bytes.Repeat([]byte("x"), maxMessageBytes+1)},
	} {
		if _, err := b.decode("project-a", message); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("malformed broker message accepted: %v", err)
		}
	}
	event.Data = json.RawMessage(strconv.Quote(strings.Repeat("x", maxDataBytes)))
	body, _ = json.Marshal(event)
	if _, err := b.decode("project-a", &nats.Msg{Subject: b.subjects["project-a"], Data: body}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("oversized data accepted: %v", err)
	}
}

func TestConsumerPreservesEnvelopeAndSanitizesLogs(t *testing.T) {
	local := &recordingBroadcaster{events: make(chan domain.Event, 1), count: 2}
	var output bytes.Buffer
	b, err := New(testOptions("nats://127.0.0.1:4222"), local, slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	event := testEvent("project-a")
	body, _ := json.Marshal(event)
	b.consume("project-a", &nats.Msg{Subject: b.subjects["project-a"], Data: body, Header: nats.Header{
		requestHeader: []string{"safe-request"}, nodeHeader: []string{"remote-node"}, "Authorization": []string{"do-not-log"},
	}})
	received := <-local.events
	if received.EventID != event.EventID || !received.Timestamp.Equal(event.Timestamp) || !bytes.Equal(received.Data, event.Data) {
		t.Fatal("consumer changed event envelope")
	}
	b.consume("project-a", &nats.Msg{Subject: b.subjects["project-a"], Data: []byte("invalid secret payload")})
	logs := output.String()
	if !strings.Contains(logs, "safe-request") || !strings.Contains(logs, "remote-node") || strings.Contains(logs, "private payload") || strings.Contains(logs, "secret payload") || strings.Contains(logs, "do-not-log") || strings.Contains(logs, b.opts.Token) {
		t.Fatalf("broker logs violate correlation/sanitization: %s", logs)
	}
}

// protocolServer exercises real NATS client parsing without an external binary.
// In particular -ERR followed by PONG must never become successful publication.
type protocolServer struct {
	listener                   net.Listener
	denySubscribe, denyPublish bool
	holdPongs                  atomic.Bool
	publications               chan *nats.Msg
	echoEnabled                atomic.Bool
	mu                         sync.Mutex
	connections                []net.Conn
	stopped                    bool
	wg                         sync.WaitGroup
}

func startProtocolServer(t *testing.T, denySubscribe, denyPublish bool) *protocolServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &protocolServer{listener: listener, denySubscribe: denySubscribe, denyPublish: denyPublish, publications: make(chan *nats.Msg, 128)}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			if s.stopped {
				s.mu.Unlock()
				_ = conn.Close()
				continue
			}
			s.connections = append(s.connections, conn)
			s.mu.Unlock()
			s.wg.Add(1)
			go func() { defer s.wg.Done(); s.serve(conn) }()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		s.mu.Lock()
		s.stopped = true
		for _, conn := range s.connections {
			_ = conn.Close()
		}
		s.mu.Unlock()
		s.wg.Wait()
	})
	return s
}

func (s *protocolServer) serve(conn net.Conn) {
	defer conn.Close()
	_, _ = io.WriteString(conn, "INFO {\"server_id\":\"test\",\"version\":\"2.15.1\",\"proto\":1,\"headers\":true,\"max_payload\":1048576}\r\n")
	reader := bufio.NewReader(conn)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			return
		}
		switch fields[0] {
		case "CONNECT":
			var opts struct {
				Echo bool `json:"echo"`
			}
			if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "CONNECT"))), &opts) == nil {
				s.echoEnabled.Store(opts.Echo)
			}
		case "PING":
			if !s.holdPongs.Load() {
				_, _ = io.WriteString(conn, "PONG\r\n")
			}
		case "SUB":
			if s.denySubscribe {
				_, _ = fmt.Fprintf(conn, "-ERR 'Permissions Violation for Subscription to \"%s\"'\r\n", fields[1])
			}
		case "PUB", "HPUB":
			size, err := strconv.Atoi(fields[len(fields)-1])
			if err != nil || size < 0 || size > maxMessageBytes+4096 {
				return
			}
			payload := make([]byte, size+2)
			if _, err := io.ReadFull(reader, payload); err != nil {
				return
			}
			message := &nats.Msg{Subject: fields[1], Data: payload[:size]}
			if fields[0] == "HPUB" {
				headerSize, err := strconv.Atoi(fields[len(fields)-2])
				if err != nil || headerSize < 0 || headerSize > size {
					return
				}
				message.Header, _ = nats.DecodeHeadersMsg(payload[:headerSize])
				message.Data = payload[headerSize:size]
			}
			s.publications <- message
			if s.denyPublish {
				_, _ = fmt.Fprintf(conn, "-ERR 'Permissions Violation for Publish to \"%s\"'\r\n", fields[1])
			}
		}
	}
}

func (s *protocolServer) url() string { return "nats://" + s.listener.Addr().String() }

func TestStartupRejectsSubscriptionPermissionViolation(t *testing.T) {
	server := startProtocolServer(t, true, false)
	b := newTestBroker(t, testOptions(server.url()), &recordingBroadcaster{events: make(chan domain.Event, 1)})
	if err := b.Start(context.Background()); !errors.Is(err, domain.ErrUnavailable) || b.Ready() {
		t.Fatalf("startup permission failure = %v, ready %v", err, b.Ready())
	}
}

func TestPublishRejectsAsyncPermissionFailure(t *testing.T) {
	server := startProtocolServer(t, false, true)
	local := &recordingBroadcaster{events: make(chan domain.Event, 1), count: 2}
	b := newTestBroker(t, testOptions(server.url()), local)
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	count, err := b.Broadcast(context.Background(), testEvent("project-a"))
	if count != 0 || !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("denied publication = %d, %v", count, err)
	}
	select {
	case <-local.events:
		t.Fatal("denied event reached local clients")
	default:
	}
	if b.Ready() {
		t.Fatal("permission failure remained ready")
	}
}

func TestPublishThenLocalDeliveryAndCorrelation(t *testing.T) {
	server := startProtocolServer(t, false, false)
	local := &recordingBroadcaster{events: make(chan domain.Event, 2), count: 3}
	opts := testOptions(server.url())
	opts.RequestID = func(context.Context) string { return "request-123" }
	b := newTestBroker(t, opts, local)
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !b.Ready() || server.echoEnabled.Load() {
		t.Fatal("broker not ready or self echo enabled")
	}
	for _, sub := range b.subs {
		messages, bytes, err := sub.PendingLimits()
		if err != nil || messages != 64 || bytes != 1<<20 {
			t.Fatalf("pending limits = %d, %d, %v", messages, bytes, err)
		}
	}
	event := testEvent("project-a")
	count, err := b.Broadcast(context.Background(), event)
	if count != 3 || err != nil {
		t.Fatalf("publish = %d, %v", count, err)
	}
	wire := <-server.publications
	if wire.Subject != b.subjects[event.ProjectID] || wire.Header.Get(requestHeader) != "request-123" || wire.Header.Get(nodeHeader) != opts.NodeID {
		t.Fatalf("publication metadata = %#v", wire)
	}
	if received := <-local.events; received.EventID != event.EventID || !received.Timestamp.Equal(event.Timestamp) {
		t.Fatal("origin envelope changed")
	}
	b.BeginDrain()
	if b.Ready() {
		t.Fatal("draining broker remained ready")
	}
	if _, err := b.Broadcast(context.Background(), event); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("draining publication = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := b.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestPublishAdmissionIsBoundedAndShutdownInterruptsFlush(t *testing.T) {
	server := startProtocolServer(t, false, false)
	b := newTestBroker(t, testOptions(server.url()), &recordingBroadcaster{events: make(chan domain.Event, 128)})
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	server.holdPongs.Store(true)
	results := make(chan error, maxPublishes)
	for range maxPublishes {
		go func() { _, err := b.Broadcast(context.Background(), testEvent("project-a")); results <- err }()
	}
	for range maxPublishes {
		select {
		case <-server.publications:
		case <-time.After(time.Second):
			t.Fatal("publication did not reach blocked flush")
		}
	}
	if _, err := b.Broadcast(context.Background(), testEvent("project-a")); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("overflow publication = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := b.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked flush shutdown = %v", err)
	}
	for range maxPublishes {
		select {
		case err := <-results:
			if !errors.Is(err, domain.ErrUnavailable) {
				t.Fatalf("interrupted publish = %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("publish survived shutdown")
		}
	}
}

func TestCanceledStartupAndPublication(t *testing.T) {
	b := newTestBroker(t, testOptions("nats://127.0.0.1:4222"), &recordingBroadcaster{events: make(chan domain.Event, 1)})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.Start(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled startup = %v", err)
	}
	if _, err := b.Broadcast(ctx, testEvent("project-a")); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled publication = %v", err)
	}
}
