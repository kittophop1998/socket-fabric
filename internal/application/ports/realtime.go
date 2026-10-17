// Package ports describes the dependencies required by realtime use cases.
package ports

import (
	"context"

	"socketfabric/internal/domain"
)

type ClientTokenVerifier interface {
	Verify(context.Context, string) (domain.ClientScope, error)
}

type PublisherAuthenticator interface {
	Authenticate(context.Context, string) (domain.PublisherScope, error)
}

// DeliverySink never blocks on network I/O. Disconnect initiates session close.
type DeliverySink interface {
	TryEnqueue(domain.Event) bool
	Disconnect(reason string)
}

// EventBroadcaster distributes an event and reports only locally queued clients.
// Implementations may cross a transport boundary before local delivery.
type EventBroadcaster interface {
	Broadcast(context.Context, domain.Event) (int, error)
}

type RoomRegistry interface {
	EventBroadcaster
	Register(context.Context, string, domain.ClientScope, DeliverySink) error
	Unregister(string)
	Join(context.Context, string, string) error
	Leave(context.Context, string, string) error
	Stop()
}
