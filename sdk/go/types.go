// Package socketfabric provides a project-scoped HTTP publisher and a separate
// trusted client-token signer for SocketFabric. It does not retry publishes.
package socketfabric

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var (
	ErrInvalidInput    = errors.New("socketfabric: invalid input")
	ErrInvalidResponse = errors.New("socketfabric: invalid response")
	ErrRequestFailed   = errors.New("socketfabric: request failed")
)

// Event is the unchanged SocketFabric wire envelope. Data is an independent
// JSON value, including null; applications choose how to decode its contents.
type Event struct {
	EventID   string          `json:"eventId"`
	ProjectID string          `json:"projectId"`
	Type      string          `json:"type"`
	RoomID    string          `json:"roomId"`
	Data      json.RawMessage `json:"data"`
	Timestamp time.Time       `json:"timestamp"`
}

// PublishInput does not expose a project override. Each Client owns one scope.
// A nil Data value is encoded as JSON null. Do not mutate Data while publishing.
type PublishInput struct {
	RoomID    string
	Type      string
	Data      any
	RequestID string
}

// PublishResult counts only local queues on the gateway accepting the request.
// Success does not guarantee delivery to a browser or any particular gateway.
type PublishResult struct {
	Event             Event `json:"event"`
	QueuedConnections int   `json:"queuedConnections"`
}

// APIError exposes the structured error returned by the HTTP API. Error omits
// Message because even a misconfigured upstream must not leak body contents
// through ordinary error logging. Inspect fields explicitly when appropriate.
type APIError struct {
	StatusCode int
	Code       string
	Message    string
	RequestID  string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("socketfabric: HTTP status %d", e.StatusCode)
}

func validID(value string) bool {
	if len(value) < 1 || len(value) > 128 {
		return false
	}
	for i := range len(value) {
		ch := value[i]
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '.' || ch == '_' || ch == ':' || ch == '-') {
			return false
		}
	}
	return true
}

func validRequestID(value string) bool {
	if !validID(value) {
		return false
	}
	for i := range len(value) {
		if value[i] == ':' {
			return false
		}
	}
	return true
}
