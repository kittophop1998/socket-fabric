// Package bootstrap manages the HTTP server and its process lifecycle.
package bootstrap

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"sync/atomic"
	"time"

	httpadapter "socketfabric/internal/adapters/inbound/http"
	"socketfabric/internal/adapters/outbound/auth"
	"socketfabric/internal/adapters/outbound/memory"
	natsadapter "socketfabric/internal/adapters/outbound/nats"
	"socketfabric/internal/adapters/outbound/postgres"
	"socketfabric/internal/application/ports"
	"socketfabric/internal/application/usecase"
	"socketfabric/internal/config"
	"socketfabric/internal/domain"
)

// Server is a single-use server instance. Construct a new instance to restart it.
type Server struct {
	config           config.Config
	logger           *slog.Logger
	http             *http.Server
	ready            atomic.Bool
	gateway          *httpadapter.Gateway
	broker           *natsadapter.Broker
	brokerStarted    bool
	nodeID           string
	registry         *memory.Registry
	management       *postgres.Store
	policy           *usecase.ProjectPolicy
	managementCancel context.CancelFunc
	managementDone   chan struct{}
}

func NewServer(cfg config.Config, logger *slog.Logger) (*Server, error) {
	if logger == nil {
		return nil, errors.New("server logger must not be nil")
	}
	// Keep zero-valued programmatic configurations compatible with single mode.
	if cfg.Mode == "" {
		cfg.Mode = "single"
	}
	if cfg.Mode != "single" && cfg.Mode != "distributed" {
		return nil, errors.New("server mode must be single or distributed")
	}
	prefix := cfg.NodeID
	if prefix == "" {
		prefix = "node"
	}
	if len(prefix) > 64 || domain.ValidateID(prefix) != nil {
		return nil, errors.New("node identity prefix is invalid")
	}
	nodeID := prefix + "-" + rand.Text()
	logger = logger.With("node_id", nodeID, "mode", cfg.Mode)
	s := &Server{config: cfg, logger: logger, nodeID: nodeID}
	constructed := false
	defer func() {
		if !constructed && s.broker != nil {
			s.broker.Close()
		}
		if !constructed && s.management != nil {
			s.management.Close()
		}
	}()
	projects := make([]string, 0, len(cfg.Auth.PublisherKeys))
	for project := range cfg.Auth.PublisherKeys {
		projects = append(projects, project)
	}
	slices.Sort(projects)
	var access []ports.ProjectAccess
	var managementService *usecase.Management
	var err error
	if cfg.Management.Enabled {
		mc := cfg.Management
		s.management, err = postgres.Open(context.Background(), mc.DatabaseURL, mc.MaxConns, mc.ConnectTimeout, mc.QueryTimeout)
		if err != nil {
			return nil, fmt.Errorf("configure management database: %w", err)
		}
		s.policy, err = usecase.NewProjectPolicy(s.management, projects, mc.MaxProjects, mc.PolicyRefresh, mc.PolicyMaxAge, mc.QueryTimeout, logger)
		if err != nil {
			return nil, fmt.Errorf("configure project policy: %w", err)
		}
		access = append(access, s.policy)
		managementService, err = usecase.NewManagement(s.management, auth.NewPasswords(), s, logger, mc.SessionTTL, mc.MaxProjects, projects)
		if err != nil {
			return nil, fmt.Errorf("configure management service: %w", err)
		}
	}
	credentials, err := auth.New(cfg.Auth.ClientTokenSecret, cfg.Auth.PublisherKeys, access...)
	if err != nil {
		return nil, fmt.Errorf("configure authentication: %w", err)
	}
	registry := memory.NewRegistry()
	s.registry = registry
	var local ports.EventBroadcaster = registry
	if s.policy != nil {
		local = usecase.ProjectBroadcaster{Access: s.policy, Next: registry}
	}
	var broadcaster ports.EventBroadcaster = local
	if cfg.Mode == "distributed" {
		var projectAccess ports.ProjectAccess
		if s.policy != nil {
			projectAccess = s.policy
		}
		s.broker, err = natsadapter.New(natsadapter.Options{
			URL: cfg.NATS.URL, Token: cfg.NATS.Token, SubjectPrefix: cfg.NATS.SubjectPrefix,
			NodeID: nodeID, Projects: projects, ConnectTimeout: cfg.NATS.ConnectTimeout,
			PublishTimeout: cfg.NATS.PublishTimeout, RequestID: httpadapter.RequestID,
			ProjectAccess: projectAccess,
		}, local, logger)
		if err != nil {
			return nil, fmt.Errorf("configure event broker: %w", err)
		}
		broadcaster = s.broker
	}
	var publishers ports.PublisherAuthenticator = credentials
	if managementService != nil {
		publishers = usecase.PublisherCredentials{Legacy: credentials, Managed: managementService}
	}
	service, err := usecase.NewService(registry, credentials, publishers, broadcaster, access...)
	if err != nil {
		return nil, fmt.Errorf("create realtime service: %w", err)
	}
	s.gateway, err = httpadapter.NewGateway(service, httpadapter.GatewayOptions{
		QueueCapacity: cfg.WebSocket.QueueCapacity, MaxConnections: cfg.WebSocket.MaxConnections,
		MaxMessageBytes: cfg.WebSocket.MaxMessageBytes, AuthTimeout: cfg.WebSocket.AuthTimeout,
		WriteTimeout: cfg.WebSocket.WriteTimeout, PingInterval: cfg.WebSocket.PingInterval,
		PongTimeout: cfg.WebSocket.PongTimeout, IdleTimeout: cfg.WebSocket.IdleTimeout,
		AllowedOrigins: cfg.WebSocket.AllowedOrigins, DemoEnabled: cfg.DemoEnabled,
	}, logger)
	if err != nil {
		return nil, fmt.Errorf("create WebSocket gateway: %w", err)
	}
	var handlers []*httpadapter.ManagementHandler
	if managementService != nil {
		handler, err := httpadapter.NewManagementHandler(managementService, httpadapter.ManagementOptions{AllowInsecureHTTP: cfg.Management.AllowInsecureHTTP, TrustedProxyCIDRs: cfg.Management.TrustedProxyCIDRs, Ready: func() bool { return s.ready.Load() && s.policy.Ready() }})
		if err != nil {
			return nil, err
		}
		handlers = append(handlers, handler)
	}
	router, err := httpadapter.NewRouter(logger, s.isReady, s.gateway, handlers...)
	if err != nil {
		return nil, fmt.Errorf("create HTTP router: %w", err)
	}
	s.http = &http.Server{
		Addr:              cfg.Address(),
		Handler:           router,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		MaxHeaderBytes:    1 << 20,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}
	constructed = true
	return s, nil
}

func (s *Server) isReady() bool {
	return s.ready.Load() && (s.broker == nil || s.broker.Ready()) && (s.policy == nil || len(s.config.Auth.PublisherKeys) > 0 || s.policy.Ready())
}

func (s *Server) startManagement(ctx context.Context) {
	if s.policy == nil || s.managementDone != nil {
		return
	}
	workerCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.managementCancel = cancel
	s.managementDone = make(chan struct{})
	go func() { defer close(s.managementDone); s.policy.Run(workerCtx, s.gateway.ReconcileProjects) }()
}
func (s *Server) closeManagement() {
	if s.managementCancel != nil {
		s.managementCancel()
		<-s.managementDone
	}
	if s.management != nil {
		s.management.Close()
	}
}

// Snapshot implements the read-only diagnostics port. There is no cluster
// membership registry: every gauge belongs only to this process.
func (s *Server) Snapshot(ctx context.Context) (map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	metrics := s.gateway.Snapshot()
	for key, value := range s.registry.Snapshot() {
		metrics[key] = value
	}
	broker := map[string]any{"required": s.broker != nil}
	if s.broker != nil {
		broker["ready"] = s.broker.Ready()
	} else {
		broker["ready"] = nil
	}
	var management any
	if s.policy != nil {
		management = s.policy.Snapshot()
	}
	return map[string]any{"scope": "local", "consistency": "approximate", "collectedAt": time.Now().UTC(), "node": map[string]any{"id": s.nodeID, "mode": s.config.Mode, "ready": s.isReady()}, "clusterVisibility": "unavailable", "broker": broker, "management": management, "metrics": metrics}, nil
}

// startBroker is owned by the single Run/serve caller. Constructors do not
// connect, and Run confirms broker subscriptions before binding HTTP.
func (s *Server) startBroker(ctx context.Context) error {
	if s.broker == nil || s.brokerStarted {
		return nil
	}
	if err := s.broker.Start(ctx); err != nil {
		s.broker.Close()
		return fmt.Errorf("start event broker: %w", err)
	}
	s.brokerStarted = true
	return nil
}

func (s *Server) beginDrain() {
	s.ready.Store(false)
	if s.broker != nil {
		s.broker.BeginDrain()
	}
	s.gateway.BeginDrain()
}

func (s *Server) shutdownBroker(ctx context.Context) error {
	if s.broker == nil {
		return nil
	}
	if err := s.broker.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown event broker: %w", err)
	}
	return nil
}

func (s *Server) closeBroker() {
	if s.broker != nil {
		s.broker.Close()
	}
}

// Run binds the configured address, serves requests, and drains on cancellation.
func (s *Server) Run(ctx context.Context) error {
	defer s.closeManagement()
	if ctx.Err() != nil {
		s.closeBroker()
		return nil
	}
	if err := s.startBroker(ctx); err != nil {
		return err
	}
	s.startManagement(ctx)
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", s.http.Addr)
	if err != nil {
		s.closeBroker()
		return fmt.Errorf("listen for HTTP requests: %w", err)
	}
	return s.serve(ctx, listener)
}

func (s *Server) serve(ctx context.Context, listener net.Listener) error {
	defer s.closeManagement()
	if err := s.startBroker(ctx); err != nil {
		_ = listener.Close()
		return err
	}
	s.startManagement(ctx)
	defer s.closeBroker()
	// Signal cancellation initiates draining; active requests keep their context
	// until they finish or the shutdown deadline forces connections closed.
	requestsCtx, cancelRequests := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRequests()
	s.http.BaseContext = func(net.Listener) context.Context { return requestsCtx }
	serveResult := make(chan error, 1)
	s.ready.Store(true)
	go func() {
		serveResult <- s.http.Serve(listener)
	}()
	s.logger.Info("HTTP server started", "address", listener.Addr().String(), "environment", s.config.Environment)

	select {
	case err := <-serveResult:
		s.beginDrain()
		closeErr := s.http.Close()
		cleanupCtx, cancelCleanup := context.WithTimeout(context.WithoutCancel(ctx), s.config.ShutdownTimeout)
		defer cancelCleanup()
		gatewayErr := s.gateway.Shutdown(cleanupCtx)
		brokerErr := s.shutdownBroker(cleanupCtx)
		if errors.Is(err, http.ErrServerClosed) {
			return errors.Join(closeErr, gatewayErr, brokerErr)
		}
		return errors.Join(fmt.Errorf("serve HTTP requests: %w", err), closeErr, gatewayErr, brokerErr)
	case <-ctx.Done():
		s.ready.Store(false)
	}

	s.logger.Info("HTTP server shutting down")
	shutdownCtx, cancelShutdown := context.WithTimeout(context.WithoutCancel(ctx), s.config.ShutdownTimeout)
	defer cancelShutdown()
	s.beginDrain()
	httpResult := make(chan error, 1)
	gatewayResult := make(chan error, 1)
	brokerResult := make(chan error, 1)
	go func() { httpResult <- s.http.Shutdown(shutdownCtx) }()
	go func() { gatewayResult <- s.gateway.Shutdown(shutdownCtx) }()
	go func() { brokerResult <- s.shutdownBroker(shutdownCtx) }()
	httpErr, gatewayErr, brokerErr := <-httpResult, <-gatewayResult, <-brokerResult
	if httpErr != nil || gatewayErr != nil || brokerErr != nil {
		closeErr := s.http.Close()
		s.gateway.ForceClose()
		s.closeBroker()
		cancelRequests()
		<-serveResult
		return errors.Join(httpErr, gatewayErr, brokerErr, closeErr)
	}
	if err := <-serveResult; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve HTTP requests during shutdown: %w", err)
	}
	s.logger.Info("HTTP server stopped")
	return nil
}
