package usecase

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"socketfabric/internal/adapters/outbound/memory"
	"socketfabric/internal/domain"
)

type tokenVerifier struct {
	scope domain.ClientScope
	err   error
}

func (v *tokenVerifier) Verify(ctx context.Context, _ string) (domain.ClientScope, error) {
	if err := ctx.Err(); err != nil {
		return domain.ClientScope{}, err
	}
	return v.scope, v.err
}

type publisherAuth struct {
	scope domain.PublisherScope
	err   error
}

func (p *publisherAuth) Authenticate(ctx context.Context, _ string) (domain.PublisherScope, error) {
	if err := ctx.Err(); err != nil {
		return domain.PublisherScope{}, err
	}
	return p.scope, p.err
}

type delivery struct{ events []domain.Event }

func (d *delivery) TryEnqueue(e domain.Event) bool { d.events = append(d.events, e); return true }
func (d *delivery) Disconnect(string)              {}

func validScope() domain.ClientScope {
	now := time.Now()
	return domain.ClientScope{UserID: "user", ProjectID: "project", Rooms: []string{"room"}, IssuedAt: now, ExpiresAt: now.Add(time.Minute)}
}

func newService(t *testing.T) (*Service, *tokenVerifier, *publisherAuth) {
	t.Helper()
	v := &tokenVerifier{scope: validScope()}
	p := &publisherAuth{scope: domain.PublisherScope{ProjectID: "project"}}
	registry := memory.NewRegistry()
	s, err := NewService(registry, v, p, registry)
	if err != nil {
		t.Fatal(err)
	}
	return s, v, p
}

func TestNewServiceRequiresDependencies(t *testing.T) {
	if _, err := NewService(nil, &tokenVerifier{}, &publisherAuth{}, memory.NewRegistry()); err == nil {
		t.Fatal("accepted nil registry")
	}
	if _, err := NewService(memory.NewRegistry(), nil, &publisherAuth{}, memory.NewRegistry()); err == nil {
		t.Fatal("accepted nil verifier")
	}
	if _, err := NewService(memory.NewRegistry(), &tokenVerifier{}, nil, memory.NewRegistry()); err == nil {
		t.Fatal("accepted nil publisher authenticator")
	}
	if _, err := NewService(memory.NewRegistry(), &tokenVerifier{}, &publisherAuth{}, nil); err == nil {
		t.Fatal("accepted nil event broadcaster")
	}
}

func TestAuthenticateScopesAndExpiredClient(t *testing.T) {
	s, v, p := newService(t)
	ctx := context.Background()
	scope, err := s.AuthenticateClient(ctx, "token")
	if err != nil {
		t.Fatal(err)
	}
	scope.Rooms[0] = "changed"
	if v.scope.Rooms[0] != "room" {
		t.Fatal("authenticated scope shares verifier data")
	}
	v.err = domain.ErrUnauthorized
	if _, err := s.AuthenticateClient(ctx, "invalid"); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("invalid client: %v", err)
	}
	v.err = nil
	v.scope.ExpiresAt = time.Now().Add(-time.Second)
	if _, err := s.AuthenticateClient(ctx, "expired"); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("expired client: %v", err)
	}
	if scope, err := s.AuthenticatePublisher(ctx, "key"); err != nil || scope.ProjectID != "project" {
		t.Fatalf("publisher: %+v, %v", scope, err)
	}
	p.err = domain.ErrUnauthorized
	if _, err := s.AuthenticatePublisher(ctx, "invalid"); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("invalid publisher: %v", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.AuthenticateClient(ctx, "token"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled auth: %v", err)
	}
}

func TestSubscriptionsAndPublishFlow(t *testing.T) {
	s, v, p := newService(t)
	ctx := context.Background()
	a, b := &delivery{}, &delivery{}
	for id, sink := range map[string]*delivery{"tab-a": a, "tab-b": b} {
		if err := s.Connect(ctx, id, v.scope, sink); err != nil {
			t.Fatal(err)
		}
		if err := s.SubscribeRoom(ctx, id, v.scope, "room"); err != nil {
			t.Fatal(err)
		}
	}
	input := PublishInput{ProjectID: "project", Type: "resource.updated", RoomID: "room", Data: json.RawMessage(`{"resourceId":"r-1"}`)}
	result, err := s.PublishEvent(ctx, p.scope, input)
	if err != nil {
		t.Fatal(err)
	}
	if result.QueuedConnections != 2 || !strings.HasPrefix(result.Event.EventID, "evt_") || result.Event.Timestamp.Location() != time.UTC {
		t.Fatalf("publish metadata: %+v", result)
	}
	if len(a.events) != 1 || len(b.events) != 1 || a.events[0].EventID != b.events[0].EventID {
		t.Fatal("tabs did not receive one shared event envelope")
	}
	input.Data[0] = '['
	if result.Event.Data[0] != '{' || a.events[0].Data[0] != '{' {
		t.Fatal("publish retained caller payload buffer")
	}
	if err := s.UnsubscribeRoom(ctx, "tab-a", v.scope, "room"); err != nil {
		t.Fatal(err)
	}
	s.Disconnect("tab-b")
	input.Data = json.RawMessage(`null`)
	result, err = s.PublishEvent(ctx, p.scope, input)
	if err != nil || result.QueuedConnections != 0 {
		t.Fatalf("zero subscriber publish: %+v, %v", result, err)
	}
}

func TestAuthorizationAndValidation(t *testing.T) {
	s, v, p := newService(t)
	ctx := context.Background()
	if err := s.Connect(ctx, "tab", v.scope, &delivery{}); err != nil {
		t.Fatal(err)
	}
	if err := s.SubscribeRoom(ctx, "tab", v.scope, "other-room"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("unauthorized join: %v", err)
	}
	if err := s.UnsubscribeRoom(ctx, "tab", v.scope, "other-room"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("unauthorized leave: %v", err)
	}
	input := PublishInput{ProjectID: "other-project", Type: "event", RoomID: "room", Data: json.RawMessage(`null`)}
	if _, err := s.PublishEvent(ctx, p.scope, input); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("wrong project publish: %v", err)
	}
	input.ProjectID, input.Data = "project", nil
	if _, err := s.PublishEvent(ctx, p.scope, input); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("missing data: %v", err)
	}
	input.Data, input.RoomID = json.RawMessage(`null`), "*"
	if _, err := s.PublishEvent(ctx, p.scope, input); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid target: %v", err)
	}
	v.scope.ExpiresAt = time.Now().Add(-time.Second)
	if err := s.SubscribeRoom(ctx, "tab", v.scope, "room"); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("expired join: %v", err)
	}
	if err := s.Connect(ctx, "expired-tab", v.scope, &delivery{}); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("expired connect: %v", err)
	}
}

type recordingBroadcaster struct {
	ctx   context.Context
	event domain.Event
	calls int
	err   error
}

func (b *recordingBroadcaster) Broadcast(ctx context.Context, event domain.Event) (int, error) {
	b.ctx, b.event = ctx, event
	b.calls++
	return 7, b.err
}

func TestPublishUsesInjectedBroadcasterWithoutLocalFallback(t *testing.T) {
	registry := memory.NewRegistry()
	broadcaster := &recordingBroadcaster{}
	scope := validScope()
	publisher := domain.PublisherScope{ProjectID: scope.ProjectID}
	service, err := NewService(registry, &tokenVerifier{scope: scope}, &publisherAuth{scope: publisher}, broadcaster)
	if err != nil {
		t.Fatal(err)
	}
	sink := &delivery{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := service.Connect(ctx, "local", scope, sink); err != nil {
		t.Fatal(err)
	}
	if err := service.SubscribeRoom(ctx, "local", scope, "room"); err != nil {
		t.Fatal(err)
	}
	input := PublishInput{ProjectID: scope.ProjectID, RoomID: "room", Type: "resource.updated", Data: json.RawMessage(`{"value":42}`)}
	result, err := service.PublishEvent(ctx, publisher, input)
	if err != nil || result.QueuedConnections != 7 {
		t.Fatalf("publish result: %+v, %v", result, err)
	}
	if broadcaster.calls != 1 || broadcaster.ctx != ctx || !reflect.DeepEqual(result.Event, broadcaster.event) {
		t.Fatal("publish did not pass one unchanged envelope and request context to the broadcaster")
	}
	if len(sink.events) != 0 {
		t.Fatal("service also broadcast locally, which would duplicate distributed delivery")
	}
	broadcaster.err = domain.ErrUnavailable
	if _, err := service.PublishEvent(ctx, publisher, input); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("broker error was not preserved: %v", err)
	}
	if broadcaster.calls != 2 || len(sink.events) != 0 {
		t.Fatal("failed broker publish retried or fell back to local delivery")
	}
	input.ProjectID = "other-project"
	if _, err := service.PublishEvent(ctx, publisher, input); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("unauthorized publish: %v", err)
	}
	if broadcaster.calls != 2 {
		t.Fatal("unauthorized publish reached the event transport")
	}
}
