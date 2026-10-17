//go:build integration

package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"socketfabric/internal/adapters/outbound/auth"
	"socketfabric/internal/application/usecase"
	"socketfabric/internal/config"
	"socketfabric/internal/domain"
	"socketfabric/internal/testsupport/natsserver"
)

func TestDistributedRepeatedOutageConcurrentPublishAndCleanup(t *testing.T) {
	broker := natsserver.Start(t, strings.Repeat("n", 32))
	a := startDistributedNode(t, distributedConfig(t, broker))
	b := startDistributedNode(t, distributedConfig(t, broker))
	sockets := []*websocket.Conn{distributedConnect(t, a, "project-a", "room-a"), distributedConnect(t, b, "project-a", "room-a")}
	isolated := distributedConnect(t, b, "project-b", "room-a")
	readerCtx, stopReaders := context.WithCancel(context.Background())
	var readers sync.WaitGroup
	var mu sync.Mutex
	observed := []map[string]int{{}, {}}
	readErrors := make(chan error, 2)
	for index, socket := range sockets {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				_, body, err := socket.Read(readerCtx)
				if err != nil {
					if readerCtx.Err() == nil {
						readErrors <- fmt.Errorf("authorized client lost during broker-only outage")
					}
					return
				}
				var frame distributedFrame
				if json.Unmarshal(body, &frame) != nil || frame.Op != "event" || frame.Event.ProjectID != "project-a" || frame.Event.RoomID != "room-a" {
					readErrors <- fmt.Errorf("invalid or unscoped failure-test frame")
					return
				}
				mu.Lock()
				observed[index][frame.Event.EventID]++
				mu.Unlock()
			}
		}()
	}
	t.Cleanup(func() {
		stopReaders()
		for _, socket := range sockets {
			_ = socket.CloseNow()
		}
		readers.Wait()
	})
	client := &http.Client{Timeout: 2 * time.Second}
	defer client.CloseIdleConnections()
	for cycle := range 3 {
		start := make(chan struct{})
		var publishers sync.WaitGroup
		publishErrors := make(chan error, 4)
		for worker := range 4 {
			publishers.Add(1)
			go func() {
				defer publishers.Done()
				<-start
				for range 24 {
					node := []*distributedNode{a, b}[worker%2]
					req, _ := http.NewRequest("POST", node.url+"/v1/events", strings.NewReader(`{"projectId":"project-a","roomId":"room-a","type":"failure.test","data":null}`))
					req.Header.Set("Authorization", "Bearer "+node.server.config.Auth.PublisherKeys["project-a"])
					req.Header.Set("Content-Type", "application/json")
					response, err := client.Do(req)
					if err != nil {
						publishErrors <- fmt.Errorf("publish exceeded transport failure budget")
						return
					}
					_, _ = io.Copy(io.Discard, response.Body)
					response.Body.Close()
					if response.StatusCode != 202 && response.StatusCode != 503 {
						publishErrors <- fmt.Errorf("unexpected failure-time status %d", response.StatusCode)
						return
					}
				}
			}()
		}
		close(start)
		broker.Stop()
		for _, node := range []*distributedNode{a, b} {
			waitHTTPStatus(t, node.url+"/readyz", 503)
			waitHTTPStatus(t, node.url+"/healthz", 200)
		}
		publishers.Wait()
		close(publishErrors)
		for err := range publishErrors {
			t.Fatal(err)
		}
		broker.Restart()
		for _, node := range []*distributedNode{a, b} {
			waitHTTPStatus(t, node.url+"/readyz", 200)
		}
		event := distributedPublish(t, a, "project-a", "room-a", 1)
		deadline := time.Now().Add(3 * time.Second)
		for {
			mu.Lock()
			complete := observed[0][event.EventID] == 1 && observed[1][event.EventID] == 1
			mu.Unlock()
			if complete {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("cycle %d recovery did not deliver a new event", cycle)
			}
			select {
			case err := <-readErrors:
				t.Fatal(err)
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	// A membership response is a barrier behind any wrongly routed event.
	distributedJoin(t, isolated, "room-a")
	stopReaders()
	for _, socket := range sockets {
		_ = socket.CloseNow()
	}
	_ = isolated.CloseNow()
	readers.Wait()
	select {
	case err := <-readErrors:
		t.Fatal(err)
	default:
	}
	mu.Lock()
	for _, events := range observed {
		for _, count := range events {
			if count != 1 {
				t.Error("broker recovery produced a routing echo")
			}
		}
	}
	mu.Unlock()
	for _, node := range []*distributedNode{a, b} {
		node.cancel()
		select {
		case <-node.done:
			if node.err != nil {
				t.Fatal(node.err)
			}
		case <-time.After(4 * time.Second):
			t.Fatal("failure-test gateway did not join its workers")
		}
		if node.server.registry.Snapshot()["registeredConnections"].(int) != 0 || node.server.gateway.Snapshot()["connections"].(int) != 0 {
			t.Fatal("failure cycles left local connections/memberships")
		}
	}
}

type distributedNode struct {
	server *Server
	url    string
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

func distributedConfig(t *testing.T, broker *natsserver.Server) config.Config {
	t.Helper()
	cfg := testServer(t).config
	cfg.Mode, cfg.NodeID = "distributed", "same-operator-label"
	cfg.NATS = config.NATS{URL: broker.URL, Token: broker.Token, SubjectPrefix: "socketfabric.v1.events", ConnectTimeout: 500 * time.Millisecond, PublishTimeout: 500 * time.Millisecond}
	cfg.Auth.PublisherKeys = map[string]string{"project-a": strings.Repeat("a", 32), "project-b": strings.Repeat("b", 32), "project.a": strings.Repeat("c", 32), "project:a": strings.Repeat("d", 32)}
	cfg.ShutdownTimeout = 2 * time.Second
	cfg.WebSocket.WriteTimeout = 200 * time.Millisecond
	return cfg
}

func startDistributedNode(t *testing.T, cfg config.Config) *distributedNode {
	t.Helper()
	s, err := NewServer(cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	url, cancel, result := startServer(t, s)
	node := &distributedNode{server: s, url: url, cancel: cancel, done: make(chan struct{})}
	go func() {
		node.err = <-result
		close(node.done)
	}()
	t.Cleanup(func() {
		node.cancel()
		select {
		case <-node.done:
			if node.err != nil {
				t.Error("distributed node cleanup", node.err)
			}
		case <-time.After(3 * time.Second):
			t.Error("distributed node did not stop within its shutdown budget")
			s.gateway.ForceClose()
		}
	})
	waitHTTPStatus(t, url+"/readyz", http.StatusOK)
	return node
}

// Polls use a bounded context because broker startup/reconnection are external
// process transitions rather than operations with a test-owned completion channel.
func waitHTTPStatus(t testing.TB, url string, want int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	client := &http.Client{Timeout: 100 * time.Millisecond}
	for {
		response, err := client.Get(url)
		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode == want {
				return
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("HTTP endpoint never reached status %d", want)
		case <-ticker.C:
		}
	}
}

type distributedFrame struct {
	Op    string       `json:"op"`
	Event domain.Event `json:"event"`
}

func distributedWrite(t *testing.T, conn *websocket.Conn, message any) {
	t.Helper()
	body, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, body); err != nil {
		t.Fatal(err)
	}
}

func distributedRead(t *testing.T, conn *websocket.Conn) distributedFrame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, body, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var frame distributedFrame
	if err := json.Unmarshal(body, &frame); err != nil {
		t.Fatal(err)
	}
	return frame
}

func distributedJoin(t *testing.T, conn *websocket.Conn, room string) {
	t.Helper()
	distributedWrite(t, conn, map[string]string{"op": "join", "roomId": room})
	if distributedRead(t, conn).Op != "joined" {
		t.Fatal("unexpected message before room ACK; a scoped event may have leaked")
	}
}

func distributedConnect(t *testing.T, node *distributedNode, project, room string) *websocket.Conn {
	t.Helper()
	credentials, err := auth.New(node.server.config.Auth.ClientTokenSecret, node.server.config.Auth.PublisherKeys)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	token, err := credentials.Issue(domain.ClientScope{UserID: "user", ProjectID: project, Rooms: []string{room}, IssuedAt: now, ExpiresAt: now.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, response, err := websocket.Dial(ctx, strings.Replace(node.url, "http", "ws", 1)+"/ws", nil)
	if err != nil {
		t.Fatal("connect authorized distributed client", err)
	}
	if response.Header.Get("X-Request-ID") == "" {
		t.Fatal("WebSocket upgrade missing request ID")
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	distributedWrite(t, conn, map[string]string{"op": "auth", "token": token})
	if distributedRead(t, conn).Op != "authenticated" {
		t.Fatal("distributed client authentication failed")
	}
	distributedJoin(t, conn, room)
	return conn
}

func distributedPost(t *testing.T, node *distributedNode, keyProject, eventProject, room string) (int, usecase.PublishResult) {
	t.Helper()
	body, _ := json.Marshal(usecase.PublishInput{ProjectID: eventProject, RoomID: room, Type: "resource.updated", Data: json.RawMessage(`{"value":1}`)})
	request, err := http.NewRequest(http.MethodPost, node.url+"/v1/events", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+node.server.config.Auth.PublisherKeys[keyProject])
	response, err := (&http.Client{Timeout: 2 * time.Second}).Do(request)
	if err != nil {
		t.Fatal("publish request failed", err)
	}
	defer response.Body.Close()
	var result usecase.PublishResult
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal("invalid JSON publish response", err)
	}
	return response.StatusCode, result
}

func distributedPublish(t *testing.T, node *distributedNode, project, room string, localCount int) domain.Event {
	t.Helper()
	status, result := distributedPost(t, node, project, project, room)
	if status != http.StatusAccepted || result.QueuedConnections != localCount || result.Event.Validate() != nil {
		t.Fatalf("publish status=%d localQueued=%d, want 202 and %d", status, result.QueuedConnections, localCount)
	}
	return result.Event
}

func expectDistributedEvent(t *testing.T, conn *websocket.Conn, want domain.Event) {
	t.Helper()
	frame := distributedRead(t, conn)
	if frame.Op != "event" || frame.Event.EventID != want.EventID || frame.Event.ProjectID != want.ProjectID || frame.Event.RoomID != want.RoomID || frame.Event.Type != want.Type || !frame.Event.Timestamp.Equal(want.Timestamp) || !bytes.Equal(frame.Event.Data, want.Data) {
		t.Fatal("subscriber did not receive the identical published envelope")
	}
}

func TestDistributedFanoutCountsIsolationAndOriginDuplication(t *testing.T) {
	broker := natsserver.Start(t, strings.Repeat("n", 32))
	a := startDistributedNode(t, distributedConfig(t, broker))
	b := startDistributedNode(t, distributedConfig(t, broker))
	if a.server.nodeID == "" || b.server.nodeID == "" || a.server.nodeID == b.server.nodeID {
		t.Fatal("duplicate operator labels must still produce unique runtime identities")
	}
	local := distributedConnect(t, a, "project-a", "room-a")
	remote := distributedConnect(t, b, "project-a", "room-a")
	otherRoom := distributedConnect(t, b, "project-a", "room-b")
	otherProject := distributedConnect(t, b, "project-b", "room-a")
	dotted := distributedConnect(t, b, "project.a", "room.a")
	colon := distributedConnect(t, b, "project:a", "room:a")
	for range 2 {
		event := distributedPublish(t, a, "project-a", "room-a", 1)
		expectDistributedEvent(t, local, event)
		expectDistributedEvent(t, remote, event)
	}
	// The second event is a FIFO marker for the same project subscription. Its
	// delivery proves the prior callback finished; unrelated clients' ACKs must
	// therefore have no leaked event ahead of them in their outbound queues.
	distributedJoin(t, otherRoom, "room-b")
	distributedJoin(t, otherProject, "room-a")
	distributedJoin(t, dotted, "room.a")
	distributedJoin(t, colon, "room:a")
	event := distributedPublish(t, a, "project-b", "room-a", 0)
	expectDistributedEvent(t, otherProject, event)
	event = distributedPublish(t, a, "project.a", "room.a", 0)
	expectDistributedEvent(t, dotted, event)
	event = distributedPublish(t, a, "project:a", "room:a", 0)
	expectDistributedEvent(t, colon, event)
	if status, _ := distributedPost(t, a, "project-a", "project-b", "room-a"); status != http.StatusForbidden {
		t.Fatal("cross-project publisher credential accepted")
	}
}

func TestDistributedBrokerOutageRecoversExistingClientsWithoutReplay(t *testing.T) {
	broker := natsserver.Start(t, strings.Repeat("n", 32))
	a := startDistributedNode(t, distributedConfig(t, broker))
	b := startDistributedNode(t, distributedConfig(t, broker))
	first := distributedConnect(t, a, "project-a", "room-a")
	second := distributedConnect(t, b, "project-a", "room-a")
	event := distributedPublish(t, a, "project-a", "room-a", 1)
	expectDistributedEvent(t, first, event)
	expectDistributedEvent(t, second, event)
	broker.Stop()
	for _, node := range []*distributedNode{a, b} {
		waitHTTPStatus(t, node.url+"/readyz", http.StatusServiceUnavailable)
		waitHTTPStatus(t, node.url+"/healthz", http.StatusOK)
		if status, _ := distributedPost(t, node, "project-a", "project-a", "room-a"); status != http.StatusServiceUnavailable {
			t.Fatal("publisher accepted while broker was unavailable")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	conn, response, err := websocket.Dial(ctx, strings.Replace(a.url, "http", "ws", 1)+"/ws", nil)
	cancel()
	if conn != nil {
		_ = conn.CloseNow()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatal("new WebSocket admitted during broker outage")
	}
	distributedJoin(t, first, "room-a")
	distributedJoin(t, second, "room-a")
	broker.Restart()
	waitHTTPStatus(t, a.url+"/readyz", http.StatusOK)
	waitHTTPStatus(t, b.url+"/readyz", http.StatusOK)
	event = distributedPublish(t, b, "project-a", "room-a", 1)
	// The first queued frame must be this new event, never an outage replay.
	expectDistributedEvent(t, first, event)
	expectDistributedEvent(t, second, event)
}

func TestDistributedOneNodeShutdownLeavesPeerOperational(t *testing.T) {
	broker := natsserver.Start(t, strings.Repeat("n", 32))
	a := startDistributedNode(t, distributedConfig(t, broker))
	b := startDistributedNode(t, distributedConfig(t, broker))
	first := distributedConnect(t, a, "project-a", "room-a")
	second := distributedConnect(t, b, "project-a", "room-a")
	closed := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, _, err := first.Read(ctx)
		closed <- err
	}()
	a.cancel()
	select {
	case <-a.done:
		if a.err != nil {
			t.Fatal("first node shutdown", a.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first node shutdown did not complete")
	}
	if websocket.CloseStatus(<-closed) != websocket.StatusGoingAway {
		t.Fatal("node shutdown did not close its client gracefully")
	}
	waitHTTPStatus(t, b.url+"/readyz", http.StatusOK)
	event := distributedPublish(t, b, "project-a", "room-a", 1)
	expectDistributedEvent(t, second, event)
}

func TestDistributedStartupFailureOpensNoHTTPListener(t *testing.T) {
	broker := natsserver.Start(t, strings.Repeat("n", 32))
	for _, failure := range []string{"invalid authentication", "missing URL", "unreachable broker"} {
		t.Run(failure, func(t *testing.T) {
			cfg := distributedConfig(t, broker)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			cfg.Port = listener.Addr().(*net.TCPAddr).Port
			_ = listener.Close()
			switch failure {
			case "invalid authentication":
				cfg.NATS.Token = strings.Repeat("x", 32)
			case "missing URL":
				cfg.NATS.URL = ""
			case "unreachable broker":
				cfg.NATS.URL = "nats://" + cfg.Address()
			}
			s, err := NewServer(cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)))
			if err == nil {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				err = s.Run(ctx)
				cancel()
			}
			if err == nil {
				t.Fatal("invalid broker startup succeeded")
			}
			conn, dialErr := net.DialTimeout("tcp", cfg.Address(), 50*time.Millisecond)
			if conn != nil {
				_ = conn.Close()
			}
			if dialErr == nil {
				t.Fatal("HTTP listener opened before broker initialization succeeded")
			}
		})
	}
}

func TestDistributedShutdownRacingBrokerReconnect(t *testing.T) {
	broker := natsserver.Start(t, strings.Repeat("n", 32))
	a := startDistributedNode(t, distributedConfig(t, broker))
	b := startDistributedNode(t, distributedConfig(t, broker))
	_ = distributedConnect(t, a, "project-a", "room-a")
	peer := distributedConnect(t, b, "project-a", "room-a")
	broker.Stop()
	waitHTTPStatus(t, a.url+"/readyz", http.StatusServiceUnavailable)
	waitHTTPStatus(t, b.url+"/readyz", http.StatusServiceUnavailable)
	restarted := make(chan struct{})
	go func() {
		defer close(restarted)
		broker.Restart()
	}()
	a.cancel()
	select {
	case <-a.done:
		if a.err != nil && !errors.Is(a.err, context.DeadlineExceeded) {
			t.Fatal("shutdown/reconnect failure", a.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown raced reconnect beyond its budget")
	}
	select {
	case <-restarted:
	case <-time.After(6 * time.Second):
		t.Fatal("broker restart did not complete")
	}
	if a.server.isReady() {
		t.Fatal("stale reconnect restored readiness after shutdown")
	}
	waitHTTPStatus(t, b.url+"/readyz", http.StatusOK)
	event := distributedPublish(t, b, "project-a", "room-a", 1)
	expectDistributedEvent(t, peer, event)
}

func TestDistributedShutdownWhileBrokerRemainsOffline(t *testing.T) {
	broker := natsserver.Start(t, strings.Repeat("n", 32))
	node := startDistributedNode(t, distributedConfig(t, broker))
	client := distributedConnect(t, node, "project-a", "room-a")
	broker.Stop()
	waitHTTPStatus(t, node.url+"/readyz", http.StatusServiceUnavailable)
	closed := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, _, err := client.Read(ctx)
		closed <- err
	}()
	node.cancel()
	select {
	case <-node.done:
		if node.err != nil {
			t.Fatal("shutdown with broker remaining offline", node.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("offline broker prevented shutdown within its budget")
	}
	if websocket.CloseStatus(<-closed) != websocket.StatusGoingAway {
		t.Fatal("offline broker shutdown did not close its client gracefully")
	}
	if node.server.isReady() {
		t.Fatal("stopped node remained ready while broker was offline")
	}
}
