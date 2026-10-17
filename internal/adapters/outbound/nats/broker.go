// Package natsadapter distributes validated events through Core NATS.
package natsadapter

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"socketfabric/internal/application/ports"
	"socketfabric/internal/domain"
)

const (
	maxProjects        = 128
	maxMessageBytes    = 128 << 10
	maxDataBytes       = 64 << 10
	maxPendingMessages = 64
	maxPendingBytes    = 1 << 20
	maxPublishes       = 64
	nodeHeader         = "X-SocketFabric-Node-ID"
	requestHeader      = "X-Request-ID"
	recoveryBackoff    = 2 * time.Second
)

type Options struct {
	URL, Token, SubjectPrefix, NodeID string
	Projects                          []string
	ConnectTimeout, PublishTimeout    time.Duration
	RequestID                         func(context.Context) string
	ProjectAccess                     ports.ProjectAccess
}

type Broker struct {
	opts                                                    Options
	local                                                   ports.EventBroadcaster
	logger                                                  *slog.Logger
	subjects                                                map[string]string
	mu                                                      sync.Mutex
	conn                                                    *nats.Conn
	transport                                               net.Conn
	startCtx                                                context.Context
	subs                                                    []*nats.Subscription
	started, initialized, closing, fatal, recoverConnection bool
	epoch, validatedEpoch, validatedReconnects              uint64
	validated                                               bool
	workerCtx                                               context.Context
	cancelWorker                                            context.CancelFunc
	wake                                                    chan struct{}
	workerDone                                              chan struct{}
	workerStarted                                           bool
	closed                                                  chan struct{}
	closedOnce                                              sync.Once
	slots                                                   chan struct{}
	publishes                                               sync.WaitGroup
}

var _ ports.EventBroadcaster = (*Broker)(nil)

// New validates configuration and constructs an adapter without network I/O.
func New(opts Options, local ports.EventBroadcaster, logger *slog.Logger) (*Broker, error) {
	if local == nil || logger == nil || opts.ConnectTimeout <= 0 || opts.PublishTimeout <= 0 {
		return nil, fmt.Errorf("broker dependencies and positive timeouts are required: %w", domain.ErrInvalid)
	}
	u, err := url.Parse(opts.URL)
	if err != nil || (u.Scheme != "nats" && u.Scheme != "tls") || u.Hostname() == "" || u.User != nil ||
		u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return nil, fmt.Errorf("invalid broker URL: %w", domain.ErrInvalid)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 || len(opts.Token) < 32 {
		return nil, fmt.Errorf("broker port and credential are invalid: %w", domain.ErrInvalid)
	}
	if !validPrefix(opts.SubjectPrefix) || domain.ValidateID(opts.NodeID) != nil || len(opts.Projects) < 1 && opts.ProjectAccess == nil || len(opts.Projects) > maxProjects {
		return nil, fmt.Errorf("broker namespace or projects are invalid: %w", domain.ErrInvalid)
	}
	subjects := make(map[string]string, len(opts.Projects))
	for _, project := range opts.Projects {
		if domain.ValidateID(project) != nil {
			return nil, fmt.Errorf("invalid broker project: %w", domain.ErrInvalid)
		}
		if _, exists := subjects[project]; exists {
			return nil, fmt.Errorf("duplicate broker project: %w", domain.ErrInvalid)
		}
		subjects[project] = opts.SubjectPrefix + ".v1.events." + base64.RawURLEncoding.EncodeToString([]byte(project))
	}
	opts.Projects = append([]string(nil), opts.Projects...)
	workerCtx, cancel := context.WithCancel(context.Background())
	return &Broker{opts: opts, local: local, logger: logger, subjects: subjects,
		workerCtx: workerCtx, cancelWorker: cancel, wake: make(chan struct{}, 1), workerDone: make(chan struct{}),
		closed: make(chan struct{}), slots: make(chan struct{}, maxPublishes)}, nil
}

func validPrefix(prefix string) bool {
	if len(prefix) < 1 || len(prefix) > 128 {
		return false
	}
	for _, token := range strings.Split(prefix, ".") {
		if token == "" {
			return false
		}
		for i := range len(token) {
			ch := token[i]
			if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '_' || ch == '-') {
				return false
			}
		}
	}
	return true
}

// Dial implements the NATS custom dialer. Retaining the transport allows a
// deadline to interrupt writes that occur before FlushWithContext waits on ctx.
func (b *Broker) Dial(network, address string) (net.Conn, error) {
	b.mu.Lock()
	ctx := b.workerCtx
	if b.startCtx != nil {
		ctx = b.startCtx
	}
	closing := b.closing
	b.mu.Unlock()
	if closing {
		return nil, net.ErrClosed
	}
	conn, err := (&net.Dialer{Timeout: b.opts.ConnectTimeout}).DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	if b.closing {
		b.mu.Unlock()
		_ = conn.Close()
		return nil, net.ErrClosed
	}
	b.transport = conn
	b.mu.Unlock()
	return conn, nil
}

func (b *Broker) Start(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	startupCtx, cancel := context.WithTimeout(ctx, b.opts.ConnectTimeout)
	defer cancel()
	b.mu.Lock()
	if b.started || b.closing {
		b.mu.Unlock()
		return unavailable("broker cannot be started")
	}
	b.started = true
	b.startCtx = startupCtx
	b.mu.Unlock()
	stopDeadline := context.AfterFunc(startupCtx, b.closeTransport)
	defer stopDeadline()
	conn, err := nats.Connect(b.opts.URL, nats.Token(b.opts.Token), nats.NoEcho(), nats.SetCustomDialer(b),
		nats.Timeout(b.opts.ConnectTimeout), nats.FlusherTimeout(b.opts.PublishTimeout), nats.ReconnectOnFlusherError(),
		nats.ReconnectBufSize(-1), nats.MaxReconnects(-1), nats.ReconnectWait(recoveryBackoff),
		nats.PingInterval(5*time.Second), nats.MaxPingsOutstanding(2), nats.Name(b.opts.NodeID),
		nats.DisconnectErrHandler(b.disconnected), nats.ReconnectHandler(b.reconnected),
		nats.ErrorHandler(b.asyncError), nats.ClosedHandler(b.connectionClosed))
	if err != nil {
		b.Close()
		return unavailable("connect to event broker")
	}
	b.mu.Lock()
	b.conn = conn
	closing := b.closing
	b.mu.Unlock()
	if closing || startupCtx.Err() != nil {
		b.Close()
		return unavailable("broker startup interrupted")
	}
	var subs []*nats.Subscription
	projects := b.opts.Projects
	if b.opts.ProjectAccess != nil {
		projects = []string{""}
	}
	for _, project := range projects {
		project := project
		subject := b.subjects[project]
		if b.opts.ProjectAccess != nil {
			subject = b.opts.SubjectPrefix + ".v1.events.*"
		}
		sub, err := conn.Subscribe(subject, func(msg *nats.Msg) { b.consume(project, msg) })
		if err != nil {
			b.Close()
			return unavailable("subscribe to project events")
		}
		if err := sub.SetPendingLimits(maxPendingMessages, maxPendingBytes); err != nil {
			b.Close()
			return unavailable("configure project event limits")
		}
		subs = append(subs, sub)
	}
	b.mu.Lock()
	b.subs = subs
	b.mu.Unlock()
	if !b.validate(startupCtx) {
		b.Close()
		return unavailable("validate event broker subscriptions")
	}
	if startupCtx.Err() != nil {
		b.Close()
		return unavailable("broker startup interrupted")
	}
	stopDeadline()
	b.mu.Lock()
	if b.closing {
		b.mu.Unlock()
		b.Close()
		return unavailable("broker startup interrupted")
	}
	b.startCtx = nil
	b.initialized, b.workerStarted = true, true
	b.mu.Unlock()
	go b.worker()
	b.logger.Info("event broker ready", "node_id", b.opts.NodeID)
	return nil
}

func (b *Broker) Ready() bool {
	b.mu.Lock()
	conn, validated := b.conn, b.validated && !b.closing && !b.fatal && b.validatedEpoch == b.epoch
	reconnects := b.validatedReconnects
	b.mu.Unlock()
	return validated && conn != nil && conn.Status() == nats.CONNECTED && conn.Stats().Reconnects == reconnects && conn.LastError() == nil
}

func (b *Broker) Broadcast(ctx context.Context, event domain.Event) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := event.Validate(); err != nil {
		return 0, err
	}
	subject, configured := b.subjects[event.ProjectID]
	if b.opts.ProjectAccess != nil {
		if err := b.opts.ProjectAccess.CheckProject(ctx, event.ProjectID); err != nil {
			return 0, err
		}
		subject = b.opts.SubjectPrefix + ".v1.events." + base64.RawURLEncoding.EncodeToString([]byte(event.ProjectID))
		configured = true
	}
	if !configured || len(event.Data) > maxDataBytes {
		return 0, domain.ErrInvalid
	}
	body, err := json.Marshal(event)
	if err != nil || len(body) > maxMessageBytes {
		return 0, domain.ErrInvalid
	}
	select {
	case b.slots <- struct{}{}:
	default:
		return 0, unavailable("event publish capacity reached")
	}
	defer func() { <-b.slots }()
	b.mu.Lock()
	if b.closing || b.fatal || !b.validated || b.validatedEpoch != b.epoch {
		b.mu.Unlock()
		return 0, unavailable("event broker not ready")
	}
	conn, epoch, reconnects := b.conn, b.epoch, b.validatedReconnects
	b.publishes.Add(1)
	b.mu.Unlock()
	defer b.publishes.Done()
	if !healthyConnection(conn, reconnects) {
		return 0, unavailable("event broker not ready")
	}
	msg := &nats.Msg{Subject: subject, Data: body, Header: nats.Header{nodeHeader: []string{b.opts.NodeID}}}
	if b.opts.RequestID != nil {
		if id := b.opts.RequestID(ctx); validRequestID(id) {
			msg.Header.Set(requestHeader, id)
		}
	}
	publishCtx, cancel := context.WithTimeout(ctx, b.opts.PublishTimeout)
	defer cancel()
	if err := conn.PublishMsg(msg); err != nil {
		b.requestRecovery(conn, err)
		return 0, unavailable("publish broker event")
	}
	if err := conn.FlushWithContext(publishCtx); err != nil {
		b.requestRecovery(conn, err)
		return 0, unavailable("confirm broker event")
	}
	if err := conn.LastError(); err != nil {
		b.requestRecovery(conn, err)
		return 0, unavailable("broker rejected event")
	}
	b.mu.Lock()
	valid := !b.closing && !b.fatal && b.validated && b.epoch == epoch && b.validatedEpoch == epoch
	b.mu.Unlock()
	if !valid || !healthyConnection(conn, reconnects) || publishCtx.Err() != nil {
		return 0, unavailable("event broker changed during publish")
	}
	return b.local.Broadcast(ctx, event)
}

func healthyConnection(conn *nats.Conn, reconnects uint64) bool {
	return conn != nil && conn.Status() == nats.CONNECTED && conn.Stats().Reconnects == reconnects && conn.LastError() == nil
}

func (b *Broker) validate(ctx context.Context) bool {
	b.mu.Lock()
	conn, epoch := b.conn, b.epoch
	subs := append([]*nats.Subscription(nil), b.subs...)
	blocked := b.closing || b.fatal
	b.mu.Unlock()
	if blocked || conn == nil || conn.Status() != nats.CONNECTED {
		return false
	}
	reconnects := conn.Stats().Reconnects
	if err := conn.LastError(); err != nil {
		b.requestRecovery(conn, err)
		return false
	}
	validationCtx, cancel := context.WithTimeout(ctx, b.opts.PublishTimeout)
	defer cancel()
	if err := conn.FlushWithContext(validationCtx); err != nil {
		b.requestRecovery(conn, err)
		return false
	}
	if err := conn.LastError(); err != nil {
		b.requestRecovery(conn, err)
		return false
	}
	for _, sub := range subs {
		if !sub.IsValid() {
			return false
		}
	}
	if !healthyConnection(conn, reconnects) || validationCtx.Err() != nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closing || b.fatal || b.conn != conn || b.epoch != epoch {
		return false
	}
	b.validated, b.validatedEpoch, b.validatedReconnects = true, epoch, reconnects
	b.recoverConnection = false
	return true
}

func (b *Broker) disconnected(conn *nats.Conn, _ error) { b.invalidate(conn, false, false) }
func (b *Broker) reconnected(conn *nats.Conn)           { b.invalidate(conn, false, false) }
func (b *Broker) asyncError(conn *nats.Conn, _ *nats.Subscription, err error) {
	b.requestRecovery(conn, err)
}

func (b *Broker) requestRecovery(conn *nats.Conn, err error) {
	fatal := errors.Is(err, nats.ErrPermissionViolation) || errors.Is(err, nats.ErrMaxSubscriptionsExceeded) ||
		errors.Is(err, nats.ErrAuthorization) || errors.Is(err, nats.ErrAuthExpired) || errors.Is(err, nats.ErrAuthRevoked)
	b.invalidate(conn, fatal, !fatal)
	b.logger.Warn("event broker unavailable", "node_id", b.opts.NodeID, "reason", brokerErrorCategory(err))
}

func brokerErrorCategory(err error) string {
	if errors.Is(err, nats.ErrPermissionViolation) {
		return "permission_denied"
	}
	if errors.Is(err, nats.ErrSlowConsumer) {
		return "slow_consumer"
	}
	if errors.Is(err, nats.ErrAuthorization) || errors.Is(err, nats.ErrAuthExpired) || errors.Is(err, nats.ErrAuthRevoked) {
		return "authentication_failed"
	}
	if errors.Is(err, nats.ErrMaxSubscriptionsExceeded) {
		return "subscription_limit"
	}
	return "transport_failed"
}

func (b *Broker) invalidate(conn *nats.Conn, fatal, recoverConnection bool) {
	b.mu.Lock()
	if b.conn != conn || b.closing {
		b.mu.Unlock()
		return
	}
	b.validated = false
	b.epoch++
	b.fatal = b.fatal || fatal
	b.recoverConnection = b.recoverConnection || recoverConnection
	b.mu.Unlock()
	b.signalWorker()
}

func (b *Broker) connectionClosed(conn *nats.Conn) {
	b.invalidate(conn, true, false)
	b.closedOnce.Do(func() { close(b.closed) })
}

func (b *Broker) signalWorker() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

func (b *Broker) worker() {
	defer close(b.workerDone)
	var lastRecovery time.Time
	for {
		select {
		case <-b.workerCtx.Done():
			return
		case <-b.wake:
		}
		b.mu.Lock()
		conn, recoverConnection := b.conn, b.recoverConnection
		blocked := !b.initialized || b.closing || b.fatal
		b.mu.Unlock()
		if blocked || conn == nil || conn.Status() != nats.CONNECTED {
			continue
		}
		if recoverConnection || conn.LastError() != nil {
			if remaining := recoveryBackoff - time.Since(lastRecovery); remaining > 0 {
				timer := time.NewTimer(remaining)
				select {
				case <-b.workerCtx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
			b.mu.Lock()
			blocked = b.closing || b.fatal
			b.recoverConnection = false
			b.mu.Unlock()
			if blocked {
				continue
			}
			lastRecovery = time.Now()
			_ = conn.ForceReconnect()
			continue
		}
		if b.validate(b.workerCtx) {
			b.logger.Info("event broker ready", "node_id", b.opts.NodeID)
		}
	}
}

func (b *Broker) consume(project string, msg *nats.Msg) {
	b.mu.Lock()
	closing := b.closing
	b.mu.Unlock()
	if closing {
		return
	}
	event, err := b.decode(project, msg)
	if err != nil {
		b.logger.Warn("invalid broker event dropped", "node_id", b.opts.NodeID)
		return
	}
	attrs := []any{"node_id", b.opts.NodeID, "event_id", event.EventID}
	if id := msg.Header.Get(requestHeader); validRequestID(id) {
		attrs = append(attrs, "request_id", id)
	}
	if source := msg.Header.Get(nodeHeader); domain.ValidateID(source) == nil {
		attrs = append(attrs, "source_node_id", source)
	}
	queued, err := b.local.Broadcast(b.workerCtx, event)
	if err != nil {
		b.logger.Warn("broker event local delivery unavailable", attrs...)
		return
	}
	attrs = append(attrs, "queued_connections", queued)
	b.logger.Debug("broker event routed", attrs...)
}

func (b *Broker) decode(project string, msg *nats.Msg) (domain.Event, error) {
	if b.opts.ProjectAccess != nil {
		if msg == nil || len(msg.Subject) > 512 {
			return domain.Event{}, domain.ErrInvalid
		}
		prefix := b.opts.SubjectPrefix + ".v1.events."
		if !strings.HasPrefix(msg.Subject, prefix) {
			return domain.Event{}, domain.ErrInvalid
		}
		encoded := strings.TrimPrefix(msg.Subject, prefix)
		decoded, err := base64.RawURLEncoding.DecodeString(encoded)
		if err != nil || domain.ValidateID(string(decoded)) != nil || base64.RawURLEncoding.EncodeToString(decoded) != encoded {
			return domain.Event{}, domain.ErrInvalid
		}
		project = string(decoded)
		if err := b.opts.ProjectAccess.CheckProject(b.workerCtx, project); err != nil {
			return domain.Event{}, domain.ErrInvalid
		}
	}
	subject, known := b.subjects[project]
	if b.opts.ProjectAccess != nil {
		subject = b.opts.SubjectPrefix + ".v1.events." + base64.RawURLEncoding.EncodeToString([]byte(project))
		known = true
	}
	if !known || msg == nil || msg.Subject != subject || len(msg.Data) > maxMessageBytes {
		return domain.Event{}, domain.ErrInvalid
	}
	var event domain.Event
	decoder := json.NewDecoder(bytes.NewReader(msg.Data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&event); err != nil {
		return domain.Event{}, domain.ErrInvalid
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return domain.Event{}, domain.ErrInvalid
	}
	if event.ProjectID != project || len(event.Data) > maxDataBytes || event.Validate() != nil {
		return domain.Event{}, domain.ErrInvalid
	}
	return event, nil
}

func validRequestID(id string) bool {
	if len(id) < 1 || len(id) > 128 {
		return false
	}
	for i := range len(id) {
		ch := id[i]
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '.' || ch == '_' || ch == '-') {
			return false
		}
	}
	return true
}

func (b *Broker) BeginDrain() {
	b.mu.Lock()
	b.closing, b.validated = true, false
	b.epoch++
	b.mu.Unlock()
	b.cancelWorker()
}

func (b *Broker) Shutdown(ctx context.Context) error {
	b.BeginDrain()
	b.mu.Lock()
	conn, workerStarted := b.conn, b.workerStarted
	b.mu.Unlock()
	if conn == nil {
		b.Close()
		return nil
	}
	// Additions are prohibited by BeginDrain before this wait starts.
	done := make(chan struct{})
	go func() { b.publishes.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		b.Close()
		return ctx.Err()
	}
	if err := conn.Drain(); err != nil && !conn.IsClosed() {
		b.Close()
		return unavailable("drain event broker")
	}
	select {
	case <-b.closed:
	case <-ctx.Done():
		b.Close()
		return ctx.Err()
	}
	if workerStarted {
		select {
		case <-b.workerDone:
		case <-ctx.Done():
			b.Close()
			return ctx.Err()
		}
	}
	return nil
}

func (b *Broker) closeTransport() {
	b.mu.Lock()
	conn := b.transport
	b.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

// Close forcefully closes the underlying transport before the library lock.
func (b *Broker) Close() {
	b.BeginDrain()
	b.closeTransport()
	b.mu.Lock()
	conn := b.conn
	b.mu.Unlock()
	if conn != nil {
		conn.Close()
	}
	b.closedOnce.Do(func() { close(b.closed) })
}

func unavailable(operation string) error {
	return fmt.Errorf("%s: %w", operation, domain.ErrUnavailable)
}
