//go:build integration

package natsadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"socketfabric/internal/domain"
	"socketfabric/internal/testsupport/natsserver"
)

func startIntegrationBroker(t *testing.T, server *natsserver.Server, local *recordingBroadcaster) *Broker {
	t.Helper()
	opts := testOptions(server.URL)
	opts.Token = server.Token
	// Deliberately duplicate operator labels: NoEcho depends on the connection,
	// and a remote message must not be discarded merely because labels match.
	opts.NodeID = "shared-label"
	b := newTestBroker(t, opts, local)
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return b
}

func integrationEvent(t *testing.T, local *recordingBroadcaster) domain.Event {
	t.Helper()
	select {
	case event := <-local.events:
		return event
	case <-time.After(3 * time.Second):
		t.Fatal("broker event did not reach local subscribers")
		return domain.Event{}
	}
}

func waitBrokerCondition(t *testing.T, condition func() bool) {
	t.Helper()
	timer := time.NewTimer(8 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if condition() {
			return
		}
		select {
		case <-timer.C:
			t.Fatal("broker state did not reach expected condition")
		case <-ticker.C:
		}
	}
}

func flushConsumer(t *testing.T, b *Broker) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := b.conn.FlushWithContext(ctx); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	if err := b.conn.Barrier(func() { close(done) }); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("subscription callbacks did not finish")
	}
}

func TestIntegrationCrossNodeEnvelopeAndNoEcho(t *testing.T) {
	server := natsserver.Start(t, strings.Repeat("t", 32))
	localA := &recordingBroadcaster{events: make(chan domain.Event, 8), count: 2}
	localB := &recordingBroadcaster{events: make(chan domain.Event, 8), count: 3}
	a, b := startIntegrationBroker(t, server, localA), startIntegrationBroker(t, server, localB)
	event := testEvent("project.with.dots")
	count, err := a.Broadcast(context.Background(), event)
	if err != nil || count != 2 {
		t.Fatalf("origin publish = %d, %v", count, err)
	}
	for _, local := range []*recordingBroadcaster{localA, localB} {
		received := integrationEvent(t, local)
		if received.EventID != event.EventID || received.ProjectID != event.ProjectID || received.RoomID != event.RoomID || !received.Timestamp.Equal(event.Timestamp) || !bytes.Equal(received.Data, event.Data) {
			t.Fatalf("envelope changed: %#v", received)
		}
	}
	flushConsumer(t, a)
	flushConsumer(t, b)
	for _, local := range []*recordingBroadcaster{localA, localB} {
		select {
		case <-local.events:
			t.Fatal("event was echoed or delivered twice")
		default:
		}
	}
}

func TestIntegrationMalformedAndWrongProjectMessagesDropped(t *testing.T) {
	server := natsserver.Start(t, strings.Repeat("t", 32))
	local := &recordingBroadcaster{events: make(chan domain.Event, 8)}
	b := startIntegrationBroker(t, server, local)
	publisher, err := nats.Connect(server.URL, nats.Token(server.Token), nats.NoReconnect())
	if err != nil {
		t.Fatal(err)
	}
	defer publisher.Close()
	wrongProject, _ := json.Marshal(testEvent("project.with.dots"))
	for _, payload := range [][]byte{[]byte("malformed secret payload"), wrongProject, bytes.Repeat([]byte("x"), maxMessageBytes+1)} {
		if err := publisher.Publish(b.subjects["project-a"], payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := publisher.Publish("socketfabric.v1.events.unknown", wrongProject); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := publisher.FlushWithContext(ctx); err != nil {
		t.Fatal(err)
	}
	flushConsumer(t, b)
	select {
	case <-local.events:
		t.Fatal("malformed, oversized, or mismatched-project event was delivered")
	default:
	}
	if !b.Ready() {
		t.Fatal("malformed messages changed broker readiness")
	}
}

func TestIntegrationDisconnectAndReconnectRevalidate(t *testing.T) {
	server := natsserver.Start(t, strings.Repeat("t", 32))
	localA := &recordingBroadcaster{events: make(chan domain.Event, 8), count: 1}
	localB := &recordingBroadcaster{events: make(chan domain.Event, 8), count: 2}
	a, b := startIntegrationBroker(t, server, localA), startIntegrationBroker(t, server, localB)
	server.Stop()
	waitBrokerCondition(t, func() bool { return !a.Ready() && !b.Ready() })
	if _, err := a.Broadcast(context.Background(), testEvent("project-a")); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("disconnected publish = %v", err)
	}
	select {
	case <-localA.events:
		t.Fatal("disconnected publish used local fallback")
	default:
	}
	server.Restart()
	waitBrokerCondition(t, func() bool { return a.Ready() && b.Ready() })
	event := testEvent("project-a")
	count, err := a.Broadcast(context.Background(), event)
	if err != nil || count != 1 {
		t.Fatalf("reconnected publish = %d, %v", count, err)
	}
	for _, local := range []*recordingBroadcaster{localA, localB} {
		if received := integrationEvent(t, local); received.EventID != event.EventID {
			t.Fatal("reconnection regenerated event metadata")
		}
	}
	a.BeginDrain()
	if a.Ready() {
		t.Fatal("reconnected worker restored draining broker readiness")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := a.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}
