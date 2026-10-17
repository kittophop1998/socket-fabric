package httpadapter

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/coder/websocket"

	"socketfabric/internal/domain"
)

type command struct {
	Op     string `json:"op"`
	Token  string `json:"token,omitempty"`
	RoomID string `json:"roomId,omitempty"`
}

type outboundMessage struct {
	data    []byte
	written chan error
	event   bool
}

type session struct {
	gateway           *Gateway
	conn              *websocket.Conn
	raw               net.Conn
	id                string
	requestID         string
	ctx               context.Context
	cancel            context.CancelFunc
	outbound          chan outboundMessage
	closing           chan struct{}
	closeDone         chan struct{}
	writerDone        chan struct{}
	mu                sync.Mutex
	closed            bool
	scope             *domain.ClientScope
	heartbeatDone     chan struct{}
	idleDone          chan struct{}
	activity          chan struct{}
	idleActive        bool
	lastActivity      time.Time
	closeStatus       websocket.StatusCode
	closeReason       string
	queueAtClose      int
	queueHighWater    int
	eventsAccepted    uint64
	eventsRejected    uint64
	eventsDiscarded   uint64
	eventsWritten     uint64
	messagesRejected  uint64
	messagesDiscarded uint64
}

func newSession(g *Gateway, conn *websocket.Conn, raw net.Conn, parent context.Context) *session {
	ctx, cancel := context.WithCancel(parent)
	return &session{
		gateway: g, conn: conn, raw: raw, id: "conn_" + rand.Text(), requestID: RequestID(parent), ctx: ctx, cancel: cancel,
		outbound: make(chan outboundMessage, g.options.QueueCapacity), closing: make(chan struct{}),
		closeDone: make(chan struct{}), writerDone: make(chan struct{}),
		heartbeatDone: make(chan struct{}), idleDone: make(chan struct{}), activity: make(chan struct{}, 1),
	}
}

func (s *session) run() {
	go s.writeLoop()
	go s.heartbeatLoop()
	go s.idleLoop()
	defer func() {
		if recover() != nil {
			s.gateway.logger.Error("WebSocket session panic recovered", "connection_id", s.id, "request_id", s.requestID)
			s.beginClose(websocket.StatusInternalError, "internal_error")
		}
		s.beginClose(websocket.StatusNormalClosure, "connection_closed")
		<-s.closeDone
		s.cancel()
		<-s.writerDone
		<-s.heartbeatDone
		<-s.idleDone
		s.gateway.service.Disconnect(s.id)
		s.gateway.remove(s)
		s.gateway.logger.Info("WebSocket disconnected", s.logAttributes()...)
	}()

	scope, ok := s.authenticate()
	if !ok {
		return
	}
	s.mu.Lock()
	s.scope = &scope
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return
	}
	if err := s.gateway.service.Connect(s.ctx, s.id, scope, s); err != nil {
		status := websocket.StatusGoingAway
		if errors.Is(err, domain.ErrUnavailable) {
			status = websocket.StatusTryAgainLater
		} else if errors.Is(err, domain.ErrForbidden) {
			status = websocket.StatusPolicyViolation
		}
		s.sendErrorAndClose(err, status)
		return
	}
	// A close may race registration; remove any membership created after the
	// close started, before returning to the reader.
	s.mu.Lock()
	closed = s.closed
	s.mu.Unlock()
	if closed {
		s.gateway.service.Disconnect(s.id)
		return
	}
	s.mu.Lock()
	s.idleActive = true
	s.mu.Unlock()
	s.signalActivity()
	expiry := time.AfterFunc(time.Until(scope.ExpiresAt), func() {
		s.beginClose(websocket.StatusPolicyViolation, "token_expired")
	})
	defer expiry.Stop()
	if !s.sendControl(map[string]any{
		"op": "authenticated", "connectionId": s.id, "projectId": scope.ProjectID, "userId": scope.UserID,
	}) {
		return
	}
	s.gateway.logger.Info("WebSocket authenticated", "connection_id", s.id, "request_id", s.requestID,
		"project_id", scope.ProjectID, "user_id", scope.UserID)

	for {
		kind, body, err := s.conn.Read(s.ctx)
		if err != nil {
			if code := websocket.CloseStatus(err); code == -1 {
				s.beginClose(websocket.StatusGoingAway, "read_failed")
			} else if code == websocket.StatusMessageTooBig {
				s.beginClose(code, "message_too_big")
			} else {
				s.beginClose(code, "peer_closed")
			}
			return
		}
		if kind != websocket.MessageText {
			s.beginClose(websocket.StatusUnsupportedData, "text_messages_required")
			return
		}
		s.recordActivity()
		var input command
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		if err := decodeOne(decoder, &input); err != nil || input.Token != "" {
			if !s.sendError(domain.ErrInvalid) {
				return
			}
			continue
		}
		var response string
		switch input.Op {
		case "join":
			err = s.gateway.service.SubscribeRoom(s.ctx, s.id, scope, input.RoomID)
			response = "joined"
		case "leave":
			err = s.gateway.service.UnsubscribeRoom(s.ctx, s.id, scope, input.RoomID)
			response = "left"
		default:
			err = domain.ErrInvalid
		}
		if err != nil {
			if errors.Is(err, domain.ErrUnauthorized) {
				s.sendErrorAndClose(err, websocket.StatusPolicyViolation)
				return
			}
			if !s.sendError(err) {
				return
			}
			continue
		}
		if !s.sendControl(map[string]string{"op": response, "roomId": input.RoomID}) {
			return
		}
	}
}

func (s *session) authenticate() (domain.ClientScope, bool) {
	ctx, cancel := context.WithTimeout(s.ctx, s.gateway.options.AuthTimeout)
	defer cancel()
	// Canceling a library Read context force-closes its socket. Initiate the
	// protocol close at the deadline instead, so browsers see a policy failure.
	authDeadline := time.AfterFunc(s.gateway.options.AuthTimeout, func() {
		s.beginClose(websocket.StatusPolicyViolation, "authentication_timeout")
	})
	defer authDeadline.Stop()
	kind, body, err := s.conn.Read(s.ctx)
	if err != nil {
		s.beginClose(websocket.StatusPolicyViolation, "authentication_timeout")
		return domain.ClientScope{}, false
	}
	if kind != websocket.MessageText {
		s.beginClose(websocket.StatusUnsupportedData, "text_messages_required")
		return domain.ClientScope{}, false
	}
	s.recordActivity()
	var input command
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decodeOne(decoder, &input); err != nil || input.Op != "auth" || input.Token == "" || input.RoomID != "" {
		s.sendErrorAndClose(domain.ErrUnauthorized, websocket.StatusPolicyViolation)
		return domain.ClientScope{}, false
	}
	scope, err := s.gateway.service.AuthenticateClient(ctx, input.Token)
	if err != nil {
		if errors.Is(err, domain.ErrUnavailable) || errors.Is(err, context.DeadlineExceeded) {
			s.sendErrorAndClose(domain.ErrUnavailable, websocket.StatusTryAgainLater)
			return domain.ClientScope{}, false
		}
		if errors.Is(err, domain.ErrForbidden) {
			s.sendErrorAndClose(err, websocket.StatusPolicyViolation)
			return domain.ClientScope{}, false
		}
		s.sendErrorAndClose(domain.ErrUnauthorized, websocket.StatusPolicyViolation)
		return domain.ClientScope{}, false
	}
	return scope, true
}

func (s *session) sendControl(value any) bool {
	body, err := json.Marshal(value)
	if err != nil {
		s.beginClose(websocket.StatusInternalError, "internal_error")
		return false
	}
	return s.enqueue(outboundMessage{data: body})
}

func (s *session) sendError(err error) bool {
	_, code, message := applicationError(err)
	return s.sendControl(struct {
		Op    string   `json:"op"`
		Error apiError `json:"error"`
	}{"error", apiError{Code: code, Message: message, RequestID: s.requestID}})
}

func (s *session) sendErrorAndClose(err error, status websocket.StatusCode) {
	_, code, message := applicationError(err)
	body, _ := json.Marshal(struct {
		Op    string   `json:"op"`
		Error apiError `json:"error"`
	}{"error", apiError{Code: code, Message: message, RequestID: s.requestID}})
	written := make(chan error, 1)
	if s.enqueue(outboundMessage{data: body, written: written}) {
		select {
		case <-written:
		case <-s.closing:
		case <-s.ctx.Done():
		}
	}
	s.beginClose(status, code)
}

// TryEnqueue implements the application delivery port. Only JSON serialization
// and a nonblocking queue operation happen on the publisher's path.
func (s *session) TryEnqueue(event domain.Event) bool {
	body, err := json.Marshal(struct {
		Op    string       `json:"op"`
		Event domain.Event `json:"event"`
	}{"event", event})
	if err != nil {
		s.mu.Lock()
		s.eventsRejected++
		s.messagesRejected++
		s.mu.Unlock()
		return false
	}
	return s.enqueue(outboundMessage{data: body, event: true})
}

func (s *session) enqueue(message outboundMessage) bool {
	s.mu.Lock()
	if s.closed {
		s.messagesRejected++
		if message.event {
			s.eventsRejected++
		}
		s.mu.Unlock()
		return false
	}
	if s.scope != nil && !time.Now().Before(s.scope.ExpiresAt) {
		s.messagesRejected++
		if message.event {
			s.eventsRejected++
		}
		s.claimCloseLocked(websocket.StatusPolicyViolation, "token_expired")
		s.mu.Unlock()
		s.finishClose()
		return false
	}
	select {
	case s.outbound <- message:
		if message.event {
			s.eventsAccepted++
		}
		s.queueHighWater = max(s.queueHighWater, len(s.outbound))
		s.mu.Unlock()
		return true
	default:
		s.messagesRejected++
		if message.event {
			s.eventsRejected++
		}
		// Claim closure before releasing the admission lock. A concurrent
		// publisher must never enqueue after the first overflow rejection.
		s.claimCloseLocked(websocket.StatusTryAgainLater, "slow_client")
		s.mu.Unlock()
		s.finishClose()
		return false
	}
}

func (s *session) Disconnect(reason string) {
	// Reasons are application lifecycle categories, never peer supplied text.
	if reason == "token_expired" {
		s.beginClose(websocket.StatusPolicyViolation, "token_expired")
	} else if reason == "slow_client" {
		s.beginClose(websocket.StatusTryAgainLater, "slow_client")
	} else {
		s.beginClose(websocket.StatusGoingAway, "connection_closed")
	}
}

func (s *session) beginClose(status websocket.StatusCode, reason string) {
	s.mu.Lock()
	claimed := s.claimCloseLocked(status, reason)
	s.mu.Unlock()
	if claimed {
		s.finishClose()
	}
}

// claimCloseLocked closes admission and releases queued payloads under the
// same lock used by enqueue. The outbound channel stays open permanently.
func (s *session) claimCloseLocked(status websocket.StatusCode, reason string) bool {
	if s.closed {
		return false
	}
	s.closed, s.closeStatus, s.closeReason = true, status, reason
	s.queueAtClose = len(s.outbound)
	close(s.closing)
	for {
		select {
		case message := <-s.outbound:
			s.messagesDiscarded++
			if message.event {
				s.eventsDiscarded++
			}
		default:
			return true
		}
	}
}

// Only the caller that claims closure starts cleanup and the close handshake.
func (s *session) finishClose() {
	s.gateway.service.Disconnect(s.id)
	s.gateway.logger.Info("WebSocket closing", s.logAttributes()...)
	s.mu.Lock()
	status, reason := s.closeStatus, s.closeReason
	s.mu.Unlock()
	go func() {
		// The library's close handshake has its own timeout. Keep it within
		// the configured write budget and allow global shutdown to force it.
		force := time.AfterFunc(s.gateway.options.WriteTimeout, func() {
			_ = s.raw.Close()
			s.cancel()
		})
		_ = s.conn.Close(status, reason)
		force.Stop()
		s.cancel()
		close(s.closeDone)
	}()
}

func (s *session) logAttributes() []any {
	s.mu.Lock()
	defer s.mu.Unlock()
	attributes := []any{
		"connection_id", s.id, "request_id", s.requestID,
		"close_code", int(s.closeStatus), "reason", s.closeReason,
		"queue_capacity", cap(s.outbound), "queue_depth", len(s.outbound),
		"queue_depth_at_close", s.queueAtClose, "queue_high_water", s.queueHighWater,
		"events_accepted", s.eventsAccepted, "events_rejected", s.eventsRejected,
		"events_discarded", s.eventsDiscarded, "events_written", s.eventsWritten,
		"messages_rejected", s.messagesRejected, "messages_discarded", s.messagesDiscarded,
		"slow_client_disconnect", s.closeReason == "slow_client",
	}
	if s.scope != nil {
		attributes = append(attributes, "project_id", s.scope.ProjectID, "user_id", s.scope.UserID)
	}
	return attributes
}

func (s *session) writeLoop() {
	defer close(s.writerDone)
	defer func() {
		if recover() != nil {
			s.gateway.logger.Error("WebSocket writer panic recovered", "connection_id", s.id, "request_id", s.requestID)
			s.beginClose(websocket.StatusInternalError, "internal_error")
		}
	}()
	for {
		select {
		case <-s.closing:
			return
		case <-s.ctx.Done():
			return
		case message := <-s.outbound:
			s.mu.Lock()
			closed := s.closed
			expired := s.scope != nil && !time.Now().Before(s.scope.ExpiresAt)
			if closed || expired {
				s.messagesDiscarded++
			}
			if (closed || expired) && message.event {
				s.eventsDiscarded++
			}
			s.mu.Unlock()
			if closed {
				return
			}
			if expired {
				s.beginClose(websocket.StatusPolicyViolation, "token_expired")
				return
			}
			ctx, cancel := context.WithTimeout(s.ctx, s.gateway.options.WriteTimeout)
			err := s.conn.Write(ctx, websocket.MessageText, message.data)
			cancel()
			if err != nil {
				s.mu.Lock()
				s.messagesDiscarded++
				s.mu.Unlock()
			}
			if message.event {
				s.mu.Lock()
				if err == nil {
					s.eventsWritten++
				} else {
					s.eventsDiscarded++
				}
				s.mu.Unlock()
			}
			if message.written != nil {
				message.written <- err
			}
			if err != nil {
				s.beginClose(websocket.StatusGoingAway, "write_failed")
				return
			}
		}
	}
}

// Control frames may be written concurrently with data by coder/websocket.
// One heartbeat worker ensures a busy outbound queue cannot starve pings, and
// at most one ping is outstanding for this session.
func (s *session) heartbeatLoop() {
	defer close(s.heartbeatDone)
	defer func() {
		if recover() != nil {
			s.gateway.logger.Error("WebSocket heartbeat panic recovered", "connection_id", s.id, "request_id", s.requestID)
			s.beginClose(websocket.StatusInternalError, "internal_error")
		}
	}()
	heartbeat := time.NewTicker(s.gateway.options.PingInterval)
	defer heartbeat.Stop()
	for {
		select {
		case <-s.closing:
			return
		case <-s.ctx.Done():
			return
		case <-heartbeat.C:
			ctx, cancel := context.WithTimeout(s.ctx, s.gateway.options.PongTimeout)
			err := s.conn.Ping(ctx)
			cancel()
			if err != nil {
				s.beginClose(websocket.StatusGoingAway, "heartbeat_timeout")
				return
			}
		}
	}
}

func (s *session) recordActivity() {
	s.mu.Lock()
	if !s.closed {
		s.lastActivity = time.Now()
	}
	s.mu.Unlock()
	s.signalActivity()
}

func (s *session) signalActivity() {
	select {
	case s.activity <- struct{}{}:
	default:
	}
}

// Idle time counts application text frames after authentication. Pongs and
// outbound events never reset this timer; zero keeps long-lived subscriptions.
func (s *session) idleLoop() {
	defer close(s.idleDone)
	if s.gateway.options.IdleTimeout == 0 {
		return
	}
	defer func() {
		if recover() != nil {
			s.gateway.logger.Error("WebSocket idle timer panic recovered", "connection_id", s.id, "request_id", s.requestID)
			s.beginClose(websocket.StatusInternalError, "internal_error")
		}
	}()
	timer := time.NewTimer(s.gateway.options.IdleTimeout)
	defer timer.Stop()
	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return
		}
		active := s.idleActive
		remaining := time.Until(s.lastActivity.Add(s.gateway.options.IdleTimeout))
		if active && remaining <= 0 {
			s.claimCloseLocked(websocket.StatusGoingAway, "idle_timeout")
			s.mu.Unlock()
			s.finishClose()
			return
		}
		s.mu.Unlock()
		if !active {
			select {
			case <-s.closing:
				return
			case <-s.ctx.Done():
				return
			case <-s.activity:
			}
			continue
		}
		timer.Reset(remaining)
		select {
		case <-s.closing:
			return
		case <-s.ctx.Done():
			return
		case <-s.activity:
		case <-timer.C:
		}
	}
}
