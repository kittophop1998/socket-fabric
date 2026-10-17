package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"socketfabric/internal/domain"
)

type testSink struct {
	mu           sync.Mutex
	events       []domain.Event
	disconnected []string
	full         bool
	onEnqueue    func()
}

func (s *testSink) TryEnqueue(event domain.Event) bool {
	if s.onEnqueue != nil {
		s.onEnqueue()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.full {
		return false
	}
	s.events = append(s.events, event)
	return true
}

func (s *testSink) Disconnect(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.disconnected = append(s.disconnected, reason)
}

func scope(project string) domain.ClientScope {
	now := time.Now()
	return domain.ClientScope{UserID: "user", ProjectID: project, Rooms: []string{"room-a", "room-b"}, IssuedAt: now, ExpiresAt: now.Add(time.Minute)}
}

func event() domain.Event {
	return domain.Event{EventID: "evt_1", ProjectID: "project", Type: "resource.updated", RoomID: "room-a", Data: json.RawMessage(`{"value":1}`), Timestamp: time.Now().UTC()}
}

func register(t *testing.T, r *Registry, id, project, room string, sink *testSink) {
	t.Helper()
	if err := r.Register(context.Background(), id, scope(project), sink); err != nil {
		t.Fatal(err)
	}
	if err := r.Join(context.Background(), id, room); err != nil {
		t.Fatal(err)
	}
}

func TestRoomProjectIsolationAndMultipleConnections(t *testing.T) {
	r := NewRegistry()
	sinks := []*testSink{{}, {}, {}, {}}
	register(t, r, "tab-1", "project", "room-a", sinks[0])
	register(t, r, "tab-2", "project", "room-a", sinks[1])
	register(t, r, "other-room", "project", "room-b", sinks[2])
	register(t, r, "other-project", "project-2", "room-a", sinks[3])
	if n, err := r.Broadcast(context.Background(), event()); err != nil || n != 2 {
		t.Fatalf("broadcast = %d, %v", n, err)
	}
	for i, sink := range sinks {
		want := 0
		if i < 2 {
			want = 1
		}
		if len(sink.events) != want {
			t.Errorf("sink %d got %d events", i, len(sink.events))
		}
	}
	sinks[0].events[0].Data[0] = '['
	if sinks[1].events[0].Data[0] != '{' {
		t.Fatal("recipient payloads share mutable storage")
	}
}

func TestMembershipAuthorizationCleanupAndReconnect(t *testing.T) {
	r := NewRegistry()
	sink := &testSink{}
	register(t, r, "tab", "project", "room-a", sink)
	if err := r.Join(context.Background(), "tab", "room-c"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("unauthorized join: %v", err)
	}
	if err := r.Leave(context.Background(), "tab", "room-c"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("unauthorized leave: %v", err)
	}
	for range 2 {
		if err := r.Join(context.Background(), "tab", "room-a"); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := r.Broadcast(context.Background(), event()); n != 1 {
		t.Fatalf("duplicate join gives %d recipients", n)
	}
	for range 2 {
		if err := r.Leave(context.Background(), "tab", "room-a"); err != nil {
			t.Fatal(err)
		}
	}
	if len(r.rooms) != 0 {
		t.Fatal("empty room retained")
	}
	if n, _ := r.Broadcast(context.Background(), event()); n != 0 {
		t.Fatalf("left room still has %d recipients", n)
	}
	r.Unregister("tab")
	r.Unregister("tab")
	register(t, r, "tab", "project", "room-a", sink)
	r.Unregister("tab")
	if len(r.connections) != 0 || len(r.rooms) != 0 {
		t.Fatal("disconnect left connection or membership")
	}
}

func TestSlowClientOnlyRemovesAffectedConnectionAndNoLockDuringDelivery(t *testing.T) {
	r := NewRegistry()
	slow := &testSink{full: true}
	fast := &testSink{onEnqueue: func() { r.Unregister("missing") }}
	register(t, r, "slow-tab", "project", "room-a", slow)
	register(t, r, "fast-tab", "project", "room-a", fast)
	if n, err := r.Broadcast(context.Background(), event()); err != nil || n != 1 {
		t.Fatalf("broadcast: %d, %v", n, err)
	}
	if len(slow.disconnected) != 1 || slow.disconnected[0] != "slow_client" {
		t.Fatalf("slow close: %v", slow.disconnected)
	}
	if len(r.connections) != 1 || len(fast.disconnected) != 0 {
		t.Fatal("slow tab disconnected healthy tab")
	}
}

func TestExpiredScopeAndDefensiveCopy(t *testing.T) {
	r := NewRegistry()
	now := time.Now()
	r.now = func() time.Time { return now }
	s := scope("project")
	s.IssuedAt = now
	s.ExpiresAt = now.Add(time.Second)
	sink := &testSink{}
	if err := r.Register(context.Background(), "tab", s, sink); err != nil {
		t.Fatal(err)
	}
	s.Rooms[0] = "room-c"
	if err := r.Join(context.Background(), "tab", "room-a"); err != nil {
		t.Fatal("registry retained caller's mutable scope", err)
	}
	now = now.Add(time.Second)
	if err := r.Join(context.Background(), "tab", "room-a"); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("expired join: %v", err)
	}
	if n, err := r.Broadcast(context.Background(), event()); err != nil || n != 0 {
		t.Fatalf("expired broadcast: %d, %v", n, err)
	}
	if len(r.connections) != 0 || len(sink.disconnected) != 1 || sink.disconnected[0] != "token_expired" {
		t.Fatal("expired connection not cleaned up")
	}
}

func TestStopAndCancelledContext(t *testing.T) {
	r := NewRegistry()
	register(t, r, "tab", "project", "room-a", &testSink{})
	r.Stop()
	ctx := context.Background()
	checks := []error{r.Register(ctx, "new", scope("project"), &testSink{}), r.Join(ctx, "tab", "room-a"), r.Leave(ctx, "tab", "room-a")}
	_, err := r.Broadcast(ctx, event())
	checks = append(checks, err)
	for _, err := range checks {
		if !errors.Is(err, domain.ErrUnavailable) {
			t.Errorf("stopped operation returned %v", err)
		}
	}
	r.Unregister("tab")
	if len(r.rooms) != 0 || len(r.connections) != 0 {
		t.Fatal("stopped registry cannot clean up")
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := r.Broadcast(ctx, event()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled broadcast: %v", err)
	}
}

type countingSink struct{ count atomic.Int64 }

func (s *countingSink) TryEnqueue(domain.Event) bool { s.count.Add(1); return true }
func (s *countingSink) Disconnect(string)            {}

func TestConcurrentJoinLeavePublishAndDisconnect(t *testing.T) {
	r := NewRegistry()
	ctx := context.Background()
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range 16 {
		wg.Go(func() {
			<-start
			id := fmt.Sprintf("tab-%d", i)
			sink := &countingSink{}
			for range 40 {
				if err := r.Register(ctx, id, scope("project"), sink); err != nil {
					t.Error(err)
					return
				}
				if err := r.Join(ctx, id, "room-a"); err != nil {
					t.Error(err)
				}
				if _, err := r.Broadcast(ctx, event()); err != nil {
					t.Error(err)
				}
				if err := r.Leave(ctx, id, "room-a"); err != nil {
					t.Error(err)
				}
				r.Unregister(id)
			}
		})
	}
	close(start)
	wg.Wait()
	if len(r.rooms) != 0 || len(r.connections) != 0 {
		t.Fatal("concurrent cleanup leaked state")
	}
}

func TestBroadcastRechecksCapturedMembership(t *testing.T) {
	for _, action := range []string{"leave", "leave-rejoin", "disconnect", "reconnect"} {
		t.Run(action, func(t *testing.T) {
			r := NewRegistry()
			entered := make(chan int, 1)
			release := make(chan struct{})
			var releaseOnce sync.Once
			t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
			var first atomic.Bool
			sinks := []*testSink{{}, {}}
			for index, sink := range sinks {
				sink.onEnqueue = func() {
					if first.CompareAndSwap(false, true) {
						entered <- index
						<-release
					}
				}
				register(t, r, fmt.Sprintf("tab-%d", index), "project", "room-a", sink)
			}
			type result struct {
				count int
				err   error
			}
			done := make(chan result, 1)
			go func() {
				count, err := r.Broadcast(context.Background(), event())
				done <- result{count, err}
			}()
			var index int
			select {
			case index = <-entered:
			case <-time.After(time.Second):
				t.Fatal("broadcast did not snapshot recipients")
			}
			// Map iteration order is arbitrary. Mutate the other recipient while
			// the first callback holds the broadcast between snapshot and delivery.
			other := 1 - index
			id := fmt.Sprintf("tab-%d", other)
			var replacement *testSink
			switch action {
			case "leave", "leave-rejoin":
				if err := r.Leave(context.Background(), id, "room-a"); err != nil {
					t.Fatal(err)
				}
				if action == "leave-rejoin" {
					if err := r.Join(context.Background(), id, "room-a"); err != nil {
						t.Fatal(err)
					}
				}
			case "disconnect", "reconnect":
				r.Unregister(id)
				if action == "reconnect" {
					replacement = &testSink{}
					register(t, r, id, "project", "room-a", replacement)
				}
			}
			releaseOnce.Do(func() { close(release) })
			select {
			case result := <-done:
				if result.err != nil || result.count != 1 {
					t.Fatalf("stale snapshot delivery: %+v", result)
				}
			case <-time.After(time.Second):
				t.Fatal("membership mutation blocked event routing")
			}
			if len(sinks[other].events) != 0 || replacement != nil && len(replacement.events) != 0 {
				t.Fatal("old snapshot delivered to a removed or newly created membership")
			}
			want := 1
			if action == "leave-rejoin" || action == "reconnect" {
				want = 2
			}
			if count, err := r.Broadcast(context.Background(), event()); err != nil || count != want {
				t.Fatalf("future event count=%d want=%d: %v", count, want, err)
			}
		})
	}
}
