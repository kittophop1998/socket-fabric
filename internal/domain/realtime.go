// Package domain contains transport-independent realtime rules and identities.
package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var (
	ErrInvalid      = errors.New("invalid input")
	ErrUnauthorized = errors.New("unauthorized")
	ErrForbidden    = errors.New("forbidden")
	ErrUnavailable  = errors.New("service unavailable")
	ErrNotFound     = errors.New("connection not found")
)

const MaxRooms = 64
const MaxTokenTTL = 15 * time.Minute

// Event is the immutable envelope distributed to matching room subscribers.
type Event struct {
	EventID   string          `json:"eventId"`
	ProjectID string          `json:"projectId"`
	Type      string          `json:"type"`
	RoomID    string          `json:"roomId"`
	Data      json.RawMessage `json:"data"`
	Timestamp time.Time       `json:"timestamp"`
}

// Clone keeps a recipient from mutating another recipient's payload.
func (e Event) Clone() Event {
	e.Data = append(json.RawMessage(nil), e.Data...)
	return e
}

func (e Event) Validate() error {
	if err := ValidateID(e.EventID); err != nil {
		return fmt.Errorf("event ID: %w", err)
	}
	if e.Timestamp.IsZero() {
		return fmt.Errorf("event timestamp: %w", ErrInvalid)
	}
	return ValidateEventInput(e.ProjectID, e.Type, e.RoomID, e.Data)
}

// ValidateEventInput accepts any present JSON value, including null.
func ValidateEventInput(projectID, eventType, roomID string, data json.RawMessage) error {
	for _, field := range []struct{ name, value string }{{"project ID", projectID}, {"event type", eventType}, {"room ID", roomID}} {
		if err := ValidateID(field.value); err != nil {
			return fmt.Errorf("%s: %w", field.name, err)
		}
	}
	if len(data) == 0 || !json.Valid(data) {
		return fmt.Errorf("event data: %w", ErrInvalid)
	}
	return nil
}

// ValidateID limits identifiers to a bounded, literal ASCII namespace.
func ValidateID(value string) error {
	if len(value) < 1 || len(value) > 128 {
		return ErrInvalid
	}
	for i := range len(value) {
		ch := value[i]
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '.' || ch == '_' || ch == ':' || ch == '-') {
			return ErrInvalid
		}
	}
	return nil
}

type PublisherScope struct {
	ProjectID string
}

// ClientScope is granted by a verified token, never by a subscription message.
type ClientScope struct {
	UserID    string
	ProjectID string
	Rooms     []string
	IssuedAt  time.Time
	ExpiresAt time.Time
}

func (s ClientScope) Validate(now time.Time) error {
	if ValidateID(s.UserID) != nil || ValidateID(s.ProjectID) != nil {
		return fmt.Errorf("client identity: %w", ErrUnauthorized)
	}
	if s.IssuedAt.IsZero() || s.ExpiresAt.IsZero() || s.IssuedAt.After(now) || !s.ExpiresAt.After(now) || !s.ExpiresAt.After(s.IssuedAt) || s.ExpiresAt.Sub(s.IssuedAt) > MaxTokenTTL {
		return fmt.Errorf("client token lifetime: %w", ErrUnauthorized)
	}
	if len(s.Rooms) == 0 || len(s.Rooms) > MaxRooms {
		return fmt.Errorf("client rooms: %w", ErrUnauthorized)
	}
	seen := make(map[string]struct{}, len(s.Rooms))
	for _, room := range s.Rooms {
		if ValidateID(room) != nil {
			return fmt.Errorf("client room: %w", ErrUnauthorized)
		}
		if _, exists := seen[room]; exists {
			return fmt.Errorf("duplicate client room: %w", ErrUnauthorized)
		}
		seen[room] = struct{}{}
	}
	return nil
}

func (s ClientScope) CanJoin(room string) bool {
	for _, allowed := range s.Rooms {
		if allowed == room {
			return true
		}
	}
	return false
}

func (s ClientScope) Clone() ClientScope {
	s.Rooms = append([]string(nil), s.Rooms...)
	return s
}
