package httpadapter

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"

	"socketfabric/internal/application/usecase"
	"socketfabric/internal/domain"
)

const maxPublishBytes = 64 << 10

//go:embed demo.html
var demoHTML string

//go:embed demo.js
var demoJS string

type GatewayOptions struct {
	QueueCapacity   int
	MaxConnections  int
	MaxMessageBytes int64
	AuthTimeout     time.Duration
	WriteTimeout    time.Duration
	PingInterval    time.Duration
	PongTimeout     time.Duration
	IdleTimeout     time.Duration
	AllowedOrigins  []string
	DemoEnabled     bool
}

// Gateway owns upgraded sockets, including unauthenticated sessions. Its
// admission lock also protects WaitGroup additions from racing with shutdown.
type Gateway struct {
	service  *usecase.Service
	options  GatewayOptions
	logger   *slog.Logger
	mu       sync.Mutex
	closing  bool
	pending  int
	sessions map[string]*session
	wg       sync.WaitGroup
}

func NewGateway(service *usecase.Service, options GatewayOptions, logger *slog.Logger) (*Gateway, error) {
	if service == nil || logger == nil {
		return nil, errors.New("gateway service and logger are required")
	}
	if options.QueueCapacity <= 0 || options.MaxConnections <= 0 || options.MaxMessageBytes <= 0 ||
		options.AuthTimeout <= 0 || options.WriteTimeout <= 0 || options.PingInterval <= 0 || options.PongTimeout <= 0 {
		return nil, errors.New("gateway limits and timeouts must be positive")
	}
	if options.IdleTimeout < 0 {
		return nil, errors.New("gateway idle timeout must not be negative")
	}
	options.AllowedOrigins = append([]string(nil), options.AllowedOrigins...)
	for i, origin := range options.AllowedOrigins {
		// OriginPatterns uses path.Match; configured origins are exact values,
		// so IPv6 address brackets must remain literal characters.
		options.AllowedOrigins[i] = strings.NewReplacer("[", `\[`, "]", `\]`).Replace(origin)
	}
	return &Gateway{service: service, options: options, logger: logger, sessions: make(map[string]*session)}, nil
}

func (g *Gateway) registerRoutes(router *gin.Engine, isReady func() bool) {
	available := func(c *gin.Context) bool {
		g.mu.Lock()
		closing := g.closing
		g.mu.Unlock()
		if closing || !isReady() {
			abortWithError(c, http.StatusServiceUnavailable, "not_ready", "Service not ready")
			return false
		}
		return true
	}
	router.GET("/ws", func(c *gin.Context) {
		if available(c) {
			g.upgrade(c)
		}
	})
	router.POST("/v1/events", func(c *gin.Context) {
		if available(c) {
			g.publish(c)
		}
	})
	if g.options.DemoEnabled {
		router.GET("/demo", func(c *gin.Context) {
			c.Header("Cache-Control", "no-store")
			c.Header("X-Content-Type-Options", "nosniff")
			c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(demoHTML))
		})
		router.GET("/demo.js", func(c *gin.Context) {
			c.Header("Cache-Control", "no-store")
			c.Header("X-Content-Type-Options", "nosniff")
			c.Data(http.StatusOK, "text/javascript; charset=utf-8", []byte(demoJS))
		})
	}
}

func (g *Gateway) publish(c *gin.Context) {
	parts := strings.Fields(c.GetHeader("Authorization"))
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		c.Header("WWW-Authenticate", "Bearer")
		abortWithError(c, http.StatusUnauthorized, "unauthorized", "Publisher authentication required")
		return
	}
	publisher, err := g.service.AuthenticatePublisher(c.Request.Context(), parts[1])
	if err != nil {
		if !errors.Is(err, domain.ErrUnauthorized) {
			status, code, message := applicationError(err)
			abortWithError(c, status, code, message)
			return
		}
		c.Header("WWW-Authenticate", "Bearer")
		abortWithError(c, http.StatusUnauthorized, "unauthorized", "Invalid publisher credentials")
		return
	}
	mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || mediaType != "application/json" {
		abortWithError(c, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxPublishBytes)
	var input usecase.PublishInput
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decodeOne(decoder, &input); err != nil {
		var oversized *http.MaxBytesError
		if errors.As(err, &oversized) {
			abortWithError(c, http.StatusRequestEntityTooLarge, "payload_too_large", "Publish body exceeds limit")
		} else {
			abortWithError(c, http.StatusBadRequest, "invalid_request", "Invalid event JSON")
		}
		return
	}
	result, err := g.service.PublishEvent(c.Request.Context(), publisher, input)
	if err != nil {
		status, code, message := applicationError(err)
		abortWithError(c, status, code, message)
		return
	}
	c.JSON(http.StatusAccepted, result)
}

func decodeOne(decoder *json.Decoder, value any) error {
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func applicationError(err error) (int, string, string) {
	switch {
	case errors.Is(err, domain.ErrUnauthorized):
		return http.StatusUnauthorized, "unauthorized", "Authentication required"
	case errors.Is(err, domain.ErrForbidden):
		return http.StatusForbidden, "forbidden", "Operation not allowed for this scope"
	case errors.Is(err, domain.ErrInvalid):
		return http.StatusBadRequest, "invalid_request", "Invalid request"
	case errors.Is(err, domain.ErrNotFound):
		return http.StatusNotFound, "not_found", "Connection not found"
	case errors.Is(err, domain.ErrUnavailable), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return http.StatusServiceUnavailable, "not_ready", "Service not ready"
	default:
		return http.StatusInternalServerError, "internal_error", "Internal server error"
	}
}

func (g *Gateway) upgrade(c *gin.Context) {
	g.mu.Lock()
	if g.closing || len(g.sessions)+g.pending >= g.options.MaxConnections {
		closing := g.closing
		g.mu.Unlock()
		if closing {
			abortWithError(c, http.StatusServiceUnavailable, "not_ready", "Service not ready")
		} else {
			abortWithError(c, http.StatusServiceUnavailable, "capacity_exceeded", "Connection capacity reached")
		}
		return
	}
	g.pending++
	g.wg.Add(1)
	g.mu.Unlock()

	writer := &handshakeWriter{ResponseWriter: c.Writer}
	var conn *websocket.Conn
	converted := false
	defer func() {
		if !converted {
			if writer.raw != nil {
				_ = writer.raw.Close()
			}
			if conn != nil {
				_ = conn.CloseNow()
			}
			g.mu.Lock()
			g.pending--
			g.mu.Unlock()
			g.wg.Done()
		}
	}()
	var err error
	conn, err = websocket.Accept(writer, c.Request, &websocket.AcceptOptions{OriginPatterns: g.options.AllowedOrigins})
	if err != nil {
		if c.Writer.Written() {
			return
		}
		status := writer.status
		if status < http.StatusBadRequest {
			status = http.StatusInternalServerError
		}
		code, message := "invalid_websocket_handshake", "Invalid WebSocket handshake"
		if status == http.StatusForbidden {
			code, message = "forbidden", "WebSocket origin not allowed"
		} else if status >= http.StatusInternalServerError {
			code, message = "internal_error", "Internal server error"
		}
		abortWithError(c, status, code, message)
		return
	}
	conn.SetReadLimit(g.options.MaxMessageBytes)
	s := newSession(g, conn, writer.raw, context.WithoutCancel(c.Request.Context()))
	g.mu.Lock()
	g.pending--
	g.sessions[s.id] = s
	closing := g.closing
	g.mu.Unlock()
	converted = true
	go s.run()
	if closing {
		s.beginClose(websocket.StatusGoingAway, "server_shutdown")
	}
}

// Buffer library handshake errors so request-derived plaintext never escapes
// the shared JSON convention. Successful upgrades retain Gin's writer/Hijacker.
type handshakeWriter struct {
	gin.ResponseWriter
	status int
	raw    net.Conn
}

func (w *handshakeWriter) WriteHeader(status int) {
	if status >= http.StatusBadRequest {
		w.status = status
		return
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *handshakeWriter) Write(body []byte) (int, error) {
	if w.status >= http.StatusBadRequest {
		return len(body), nil
	}
	return w.ResponseWriter.Write(body)
}

func (w *handshakeWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, buffers, err := w.ResponseWriter.Hijack()
	if err != nil {
		return nil, nil, err
	}
	// HTTP timeouts must not terminate an otherwise healthy upgraded socket.
	if err := conn.SetDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("clear handshake deadlines: %w", err)
	}
	w.raw = conn
	return conn, buffers, nil
}

// BeginDrain closes admission before a wait can begin. Upgrades already in
// progress transfer their tracked lease to a session and close on completion.
func (g *Gateway) BeginDrain() {
	g.mu.Lock()
	g.closing = true
	g.service.Stop()
	sessions := make([]*session, 0, len(g.sessions))
	for _, s := range g.sessions {
		sessions = append(sessions, s)
	}
	g.mu.Unlock()
	for _, s := range sessions {
		s.beginClose(websocket.StatusGoingAway, "server_shutdown")
	}
}

func (g *Gateway) Shutdown(ctx context.Context) error {
	g.BeginDrain()
	done := make(chan struct{})
	go func() {
		g.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		g.ForceClose()
		return ctx.Err()
	}
}

func (g *Gateway) ForceClose() {
	g.mu.Lock()
	sessions := make([]*session, 0, len(g.sessions))
	for _, s := range g.sessions {
		sessions = append(sessions, s)
	}
	g.mu.Unlock()
	for _, s := range sessions {
		s.beginClose(websocket.StatusGoingAway, "server_shutdown")
		_ = s.raw.Close()
		s.cancel()
	}
}

func (g *Gateway) remove(s *session) {
	g.mu.Lock()
	delete(g.sessions, s.id)
	g.mu.Unlock()
	g.wg.Done()
}

// ReconcileProjects never queries PostgreSQL. Closing admission drops queued
// messages through the existing single-owner session close path.
func (g *Gateway) ReconcileProjects() {
	g.mu.Lock()
	sessions := make([]*session, 0, len(g.sessions))
	for _, s := range g.sessions {
		sessions = append(sessions, s)
	}
	g.mu.Unlock()
	for _, s := range sessions {
		s.mu.Lock()
		project := ""
		if s.scope != nil && !s.closed {
			project = s.scope.ProjectID
		}
		s.mu.Unlock()
		if project == "" {
			continue
		}
		if err := g.service.CheckProject(s.ctx, project); err != nil {
			if errors.Is(err, domain.ErrUnavailable) {
				s.beginClose(websocket.StatusTryAgainLater, "management_unavailable")
			} else if errors.Is(err, domain.ErrForbidden) || errors.Is(err, domain.ErrUnauthorized) {
				s.beginClose(websocket.StatusPolicyViolation, "project_disabled")
			}
		}
	}
}

// Snapshot exposes only aggregate live-session gauges/counters. Removed
// sessions remain observable in existing sanitized logs, not fabricated totals.
func (g *Gateway) Snapshot() map[string]any {
	g.mu.Lock()
	pending := g.pending
	sessions := make([]*session, 0, len(g.sessions))
	for _, s := range g.sessions {
		sessions = append(sessions, s)
	}
	g.mu.Unlock()
	metrics := map[string]any{"pendingConnections": pending, "connections": len(sessions), "maxConnections": g.options.MaxConnections}
	var authenticated, depth, capacity, highWater int
	var accepted, written, rejected, discarded uint64
	for _, s := range sessions {
		s.mu.Lock()
		if s.scope != nil && !s.closed {
			authenticated++
		}
		depth += len(s.outbound)
		capacity += cap(s.outbound)
		highWater += s.queueHighWater
		accepted += s.eventsAccepted
		written += s.eventsWritten
		rejected += s.eventsRejected
		discarded += s.eventsDiscarded
		s.mu.Unlock()
	}
	metrics["authenticatedConnections"] = authenticated
	metrics["queueDepth"] = depth
	metrics["queueCapacity"] = capacity
	metrics["activeSessionQueueHighWaterSum"] = highWater
	metrics["activeSessionEventsAccepted"] = accepted
	metrics["activeSessionEventsWritten"] = written
	metrics["activeSessionEventsRejected"] = rejected
	metrics["activeSessionEventsDiscarded"] = discarded
	return metrics
}
