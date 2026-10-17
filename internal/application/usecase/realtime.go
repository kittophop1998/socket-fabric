// Package usecase orchestrates authentication, subscriptions, and publishing.
package usecase

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"socketfabric/internal/application/ports"
	"socketfabric/internal/domain"
)

type PublishInput struct {
	ProjectID string          `json:"projectId"`
	Type      string          `json:"type"`
	RoomID    string          `json:"roomId"`
	Data      json.RawMessage `json:"data"`
}

type PublishResult struct {
	Event             domain.Event `json:"event"`
	QueuedConnections int          `json:"queuedConnections"`
}

type Service struct {
	registry    ports.RoomRegistry
	verifier    ports.ClientTokenVerifier
	publishers  ports.PublisherAuthenticator
	broadcaster ports.EventBroadcaster
	projects    ports.ProjectAccess
	now         func() time.Time
}

func NewService(registry ports.RoomRegistry, verifier ports.ClientTokenVerifier, publishers ports.PublisherAuthenticator, broadcaster ports.EventBroadcaster, projects ...ports.ProjectAccess) (*Service, error) {
	if registry == nil || verifier == nil || publishers == nil || broadcaster == nil {
		return nil, errors.New("realtime service dependencies are required")
	}
	if len(projects) > 1 || len(projects) == 1 && projects[0] == nil {
		return nil, domain.ErrInvalid
	}
	s := &Service{registry: registry, verifier: verifier, publishers: publishers, broadcaster: broadcaster, now: time.Now}
	if len(projects) == 1 {
		s.projects = projects[0]
	}
	return s, nil
}

// CheckProject uses only local policy; the transport may also call it on expiry.
func (s *Service) CheckProject(ctx context.Context, id string) error {
	if s.projects != nil {
		return s.projects.CheckProject(ctx, id)
	}
	return ctx.Err()
}

func (s *Service) AuthenticateClient(ctx context.Context, token string) (domain.ClientScope, error) {
	if err := ctx.Err(); err != nil {
		return domain.ClientScope{}, err
	}
	scope, err := s.verifier.Verify(ctx, token)
	if err != nil {
		return domain.ClientScope{}, fmt.Errorf("authenticate client: %w", err)
	}
	if err := scope.Validate(s.now()); err != nil {
		return domain.ClientScope{}, err
	}
	return scope.Clone(), nil
}

func (s *Service) AuthenticatePublisher(ctx context.Context, key string) (domain.PublisherScope, error) {
	if err := ctx.Err(); err != nil {
		return domain.PublisherScope{}, err
	}
	scope, err := s.publishers.Authenticate(ctx, key)
	if err != nil {
		return domain.PublisherScope{}, fmt.Errorf("authenticate publisher: %w", err)
	}
	if domain.ValidateID(scope.ProjectID) != nil {
		return domain.PublisherScope{}, domain.ErrUnauthorized
	}
	return scope, nil
}

func (s *Service) Connect(ctx context.Context, id string, scope domain.ClientScope, sink ports.DeliverySink) error {
	if err := scope.Validate(s.now()); err != nil {
		return err
	}
	if err := s.CheckProject(ctx, scope.ProjectID); err != nil {
		return err
	}
	return s.registry.Register(ctx, id, scope, sink)
}

func (s *Service) Disconnect(id string) { s.registry.Unregister(id) }

// Stop prevents new realtime work while transport owners drain connections.
func (s *Service) Stop() { s.registry.Stop() }

func (s *Service) SubscribeRoom(ctx context.Context, id string, scope domain.ClientScope, room string) error {
	if err := validateSubscription(scope, room, s.now()); err != nil {
		return err
	}
	if err := s.CheckProject(ctx, scope.ProjectID); err != nil {
		return err
	}
	return s.registry.Join(ctx, id, room)
}

func (s *Service) UnsubscribeRoom(ctx context.Context, id string, scope domain.ClientScope, room string) error {
	if err := validateSubscription(scope, room, s.now()); err != nil {
		return err
	}
	return s.registry.Leave(ctx, id, room)
}

func validateSubscription(scope domain.ClientScope, room string, now time.Time) error {
	if err := scope.Validate(now); err != nil {
		return err
	}
	if err := domain.ValidateID(room); err != nil {
		return err
	}
	if !scope.CanJoin(room) {
		return domain.ErrForbidden
	}
	return nil
}

func (s *Service) PublishEvent(ctx context.Context, publisher domain.PublisherScope, input PublishInput) (PublishResult, error) {
	if err := ctx.Err(); err != nil {
		return PublishResult{}, err
	}
	if domain.ValidateID(publisher.ProjectID) != nil {
		return PublishResult{}, domain.ErrUnauthorized
	}
	if publisher.ProjectID != input.ProjectID {
		return PublishResult{}, domain.ErrForbidden
	}
	if err := domain.ValidateEventInput(input.ProjectID, input.Type, input.RoomID, input.Data); err != nil {
		return PublishResult{}, err
	}
	if err := s.CheckProject(ctx, publisher.ProjectID); err != nil {
		// Publisher authentication already established the project. A new DB
		// project may await local policy refresh; this is availability, not a bad key.
		if errors.Is(err, domain.ErrUnauthorized) {
			return PublishResult{}, domain.ErrUnavailable
		}
		return PublishResult{}, err
	}
	event := domain.Event{EventID: "evt_" + rand.Text(), ProjectID: publisher.ProjectID, Type: input.Type, RoomID: input.RoomID, Data: append(json.RawMessage(nil), input.Data...), Timestamp: s.now().UTC()}
	queued, err := s.broadcaster.Broadcast(ctx, event)
	if err != nil {
		return PublishResult{}, fmt.Errorf("broadcast event: %w", err)
	}
	return PublishResult{Event: event, QueuedConnections: queued}, nil
}
