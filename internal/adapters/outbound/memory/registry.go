// Package memory implements a single-process connection and room registry.
package memory

import (
	"context"
	"sync"
	"time"

	"socketfabric/internal/application/ports"
	"socketfabric/internal/domain"
)

type roomKey struct{ project, room string }

type connection struct {
	id    string
	scope domain.ClientScope
	sink  ports.DeliverySink
	rooms map[string]*subscription
}

// A new membership gets its own identity so an old broadcast snapshot cannot
// become valid again after leave/rejoin on the same connection.
type subscription struct{ conn *connection }

type Registry struct {
	mu          sync.Mutex
	connections map[string]*connection
	rooms       map[roomKey]map[string]*subscription
	stopped     bool
	now         func() time.Time
}

var _ ports.RoomRegistry = (*Registry)(nil)

func NewRegistry() *Registry {
	return &Registry{connections: make(map[string]*connection), rooms: make(map[roomKey]map[string]*subscription), now: time.Now}
}

func (r *Registry) Register(ctx context.Context, id string, scope domain.ClientScope, sink ports.DeliverySink) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if domain.ValidateID(id) != nil || sink == nil {
		return domain.ErrInvalid
	}
	if err := scope.Validate(r.now()); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return domain.ErrUnavailable
	}
	if _, exists := r.connections[id]; exists {
		return domain.ErrInvalid
	}
	r.connections[id] = &connection{id: id, scope: scope.Clone(), sink: sink, rooms: make(map[string]*subscription)}
	return nil
}

// removeLocked only removes the captured registration, protecting a reconnected ID.
func (r *Registry) removeLocked(conn *connection) {
	if r.connections[conn.id] != conn {
		return
	}
	delete(r.connections, conn.id)
	for room := range conn.rooms {
		key := roomKey{conn.scope.ProjectID, room}
		delete(r.rooms[key], conn.id)
		if len(r.rooms[key]) == 0 {
			delete(r.rooms, key)
		}
	}
}

func (r *Registry) Unregister(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if conn := r.connections[id]; conn != nil {
		r.removeLocked(conn)
	}
}

func (r *Registry) Join(ctx context.Context, id, room string) error {
	return r.membership(ctx, id, room, true)
}

func (r *Registry) Leave(ctx context.Context, id, room string) error {
	return r.membership(ctx, id, room, false)
}

func (r *Registry) membership(ctx context.Context, id, room string, join bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if domain.ValidateID(room) != nil {
		return domain.ErrInvalid
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return domain.ErrUnavailable
	}
	conn := r.connections[id]
	if conn == nil {
		return domain.ErrNotFound
	}
	if err := conn.scope.Validate(r.now()); err != nil {
		return err
	}
	if !conn.scope.CanJoin(room) {
		return domain.ErrForbidden
	}
	key := roomKey{conn.scope.ProjectID, room}
	if join {
		if conn.rooms[room] != nil {
			return nil
		}
		if r.rooms[key] == nil {
			r.rooms[key] = make(map[string]*subscription)
		}
		membership := &subscription{conn: conn}
		conn.rooms[room] = membership
		r.rooms[key][id] = membership
	} else {
		delete(conn.rooms, room)
		delete(r.rooms[key], id)
		if len(r.rooms[key]) == 0 {
			delete(r.rooms, key)
		}
	}
	return nil
}

func (r *Registry) Broadcast(ctx context.Context, event domain.Event) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := event.Validate(); err != nil {
		return 0, err
	}
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return 0, domain.ErrUnavailable
	}
	key := roomKey{event.ProjectID, event.RoomID}
	recipients := make([]*subscription, 0, len(r.rooms[key]))
	for _, membership := range r.rooms[key] {
		recipients = append(recipients, membership)
	}
	r.mu.Unlock()
	queued := 0
	for _, membership := range recipients {
		if err := ctx.Err(); err != nil {
			return queued, err
		}
		conn := membership.conn
		r.mu.Lock()
		stopped := r.stopped
		current := r.connections[conn.id] == conn && r.rooms[key][conn.id] == membership
		r.mu.Unlock()
		if stopped {
			return queued, domain.ErrUnavailable
		}
		if !current {
			continue
		}
		// Admission occurs at the membership check above. A leave can still
		// overlap this nonblocking enqueue; already admitted events may arrive.
		// Never hold the registry lock while calling transport-owned methods.
		if !conn.scope.ExpiresAt.After(r.now()) {
			r.remove(conn)
			conn.sink.Disconnect("token_expired")
			continue
		}
		if conn.sink.TryEnqueue(event.Clone()) {
			queued++
		} else {
			r.remove(conn)
			conn.sink.Disconnect("slow_client")
		}
	}
	return queued, nil
}

func (r *Registry) remove(conn *connection) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removeLocked(conn)
}

// Stop prevents new work. Transport owners close existing sessions separately.
func (r *Registry) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopped = true
}

func (r *Registry) Snapshot() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	memberships := 0
	for _, room := range r.rooms {
		memberships += len(room)
	}
	return map[string]any{"registeredConnections": len(r.connections), "rooms": len(r.rooms), "memberships": memberships}
}
