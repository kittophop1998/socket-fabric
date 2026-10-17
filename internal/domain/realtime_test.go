package domain

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestValidateID(t *testing.T) {
	for _, value := range []string{"room-1", ".:_-", "123", strings.Repeat("a", 128)} {
		if err := ValidateID(value); err != nil {
			t.Errorf("ValidateID(%q): %v", value, err)
		}
	}
	for _, value := range []string{"", "*", "room a", "ห้อง", "room/a", strings.Repeat("a", 129)} {
		if !errors.Is(ValidateID(value), ErrInvalid) {
			t.Errorf("accepted invalid ID %q", value)
		}
	}
}

func TestEventValidation(t *testing.T) {
	for _, data := range []string{`null`, `{"resourceId":"r-1"}`, `[]`, `"value"`, `1`, `false`} {
		e := Event{EventID: "evt_1", ProjectID: "project", Type: "resource.updated", RoomID: "room", Data: json.RawMessage(data), Timestamp: time.Now()}
		if err := e.Validate(); err != nil {
			t.Fatalf("valid event %s: %v", data, err)
		}
	}
	for _, data := range []json.RawMessage{nil, {}, json.RawMessage(`{`)} {
		if err := ValidateEventInput("project", "event", "room", data); !errors.Is(err, ErrInvalid) {
			t.Errorf("accepted invalid data %s", data)
		}
	}
	if err := (Event{}).Validate(); !errors.Is(err, ErrInvalid) {
		t.Errorf("accepted empty event")
	}
}

func TestClientScopeValidation(t *testing.T) {
	now := time.Now()
	valid := ClientScope{UserID: "user", ProjectID: "project", Rooms: []string{"room-a", "room-b"}, IssuedAt: now, ExpiresAt: now.Add(MaxTokenTTL)}
	if err := valid.Validate(now); err != nil {
		t.Fatal(err)
	}
	if !valid.CanJoin("room-a") || valid.CanJoin("room-c") {
		t.Fatal("room authorization must use exact membership")
	}
	cases := map[string]func(*ClientScope){
		"expired":       func(s *ClientScope) { s.ExpiresAt = now },
		"future issue":  func(s *ClientScope) { s.IssuedAt = now.Add(time.Second) },
		"long TTL":      func(s *ClientScope) { s.ExpiresAt = now.Add(MaxTokenTTL + time.Second) },
		"missing issue": func(s *ClientScope) { s.IssuedAt = time.Time{} },
		"missing user":  func(s *ClientScope) { s.UserID = "" },
		"bad project":   func(s *ClientScope) { s.ProjectID = "../project" },
		"no rooms":      func(s *ClientScope) { s.Rooms = nil },
		"wildcard":      func(s *ClientScope) { s.Rooms = []string{"*"} },
		"duplicate":     func(s *ClientScope) { s.Rooms = []string{"room", "room"} },
		"many rooms": func(s *ClientScope) {
			s.Rooms = make([]string, MaxRooms+1)
		},
	}
	for name, modify := range cases {
		t.Run(name, func(t *testing.T) {
			scope := valid.Clone()
			modify(&scope)
			if err := scope.Validate(now); !errors.Is(err, ErrUnauthorized) {
				t.Errorf("wanted unauthorized, got %v", err)
			}
		})
	}
}

func TestCloneDoesNotShareMutableData(t *testing.T) {
	e := Event{Data: json.RawMessage(`{"a":1}`)}
	clone := e.Clone()
	clone.Data[0] = '['
	if e.Data[0] != '{' {
		t.Fatal("event clone shares data")
	}
	s := ClientScope{Rooms: []string{"room"}}
	copy := s.Clone()
	copy.Rooms[0] = "changed"
	if s.Rooms[0] != "room" {
		t.Fatal("scope clone shares rooms")
	}
}
