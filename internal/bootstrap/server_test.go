package bootstrap

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"socketfabric/internal/config"
)

func testServer(t testing.TB) *Server {
	t.Helper()
	cfg := config.Config{
		Environment: "test", Host: "127.0.0.1", Port: 8080,
		ReadHeaderTimeout: time.Second, ReadTimeout: time.Second,
		WriteTimeout: time.Second, IdleTimeout: time.Second, ShutdownTimeout: time.Second,
		Auth: config.Auth{ClientTokenSecret: strings.Repeat("s", 32), PublisherKeys: map[string]string{"project-a": strings.Repeat("a", 32)}},
		WebSocket: config.WebSocket{QueueCapacity: 32, MaxConnections: 256, MaxMessageBytes: 16384,
			AuthTimeout: time.Second, WriteTimeout: time.Second, PingInterval: 30 * time.Second, PongTimeout: time.Second},
	}
	s, err := NewServer(cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func startServer(t testing.TB, s *Server) (string, context.CancelFunc, <-chan error) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- s.serve(ctx, listener) }()
	t.Cleanup(func() {
		cancel()
		_ = s.http.Close()
	})
	return "http://" + listener.Addr().String(), cancel, result
}

func awaitResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("server did not stop within test deadline")
		return nil
	}
}

func request(t *testing.T, url string) *http.Response {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { response.Body.Close() })
	return response
}

func TestHealthStartupAndShutdown(t *testing.T) {
	s := testServer(t)
	if s.ready.Load() {
		t.Fatal("ready before startup")
	}
	url, cancel, result := startServer(t, s)
	for _, path := range []string{"/healthz", "/readyz"} {
		response := request(t, url+path)
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || response.StatusCode != http.StatusOK || len(body) == 0 || response.Header.Get("X-Request-ID") == "" {
			t.Fatalf("%s: status=%d body=%s err=%v", path, response.StatusCode, body, err)
		}
	}
	cancel()
	if err := awaitResult(t, result); err != nil {
		t.Fatal(err)
	}
	if s.ready.Load() {
		t.Fatal("still ready after shutdown")
	}
}

func TestGracefulShutdownPreservesActiveRequestContext(t *testing.T) {
	s := testServer(t)
	started := make(chan context.Context, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	finishRequest := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(finishRequest)
	shutdownStarted := make(chan struct{})
	s.http.RegisterOnShutdown(func() { close(shutdownStarted) })
	s.http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- r.Context()
		<-release
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("completed"))
	})
	url, cancel, result := startServer(t, s)
	responseResult := make(chan error, 1)
	go func() {
		response, err := (&http.Client{Timeout: 2 * time.Second}).Get(url)
		if err == nil {
			defer response.Body.Close()
			body, readErr := io.ReadAll(response.Body)
			err = readErr
			if string(body) != "completed" || response.StatusCode != http.StatusOK {
				err = errors.New("active response did not complete")
			}
		}
		responseResult <- err
	}()
	var requestCtx context.Context
	select {
	case requestCtx = <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("request never started")
	}
	cancel()
	select {
	case <-shutdownStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown never started")
	}
	if s.ready.Load() {
		t.Error("server remained ready while draining")
	}
	if requestCtx.Err() != nil {
		t.Error("signal canceled active request before drain completed")
	}
	finishRequest()
	if err := awaitResult(t, responseResult); err != nil {
		t.Fatal(err)
	}
	if err := awaitResult(t, result); err != nil {
		t.Fatal(err)
	}
}

func TestShutdownTimeoutClosesActiveConnection(t *testing.T) {
	s := testServer(t)
	s.config.ShutdownTimeout = 30 * time.Millisecond
	started := make(chan struct{})
	finished := make(chan struct{})
	s.http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(finished)
	})
	url, cancel, result := startServer(t, s)
	requestResult := make(chan error, 1)
	go func() {
		response, err := (&http.Client{Timeout: 2 * time.Second}).Get(url)
		if response != nil {
			response.Body.Close()
		}
		requestResult <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("request never started")
	}
	cancel()
	if err := awaitResult(t, result); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown error = %v, want deadline exceeded", err)
	}
	if err := awaitResult(t, requestResult); err == nil {
		t.Fatal("active connection was not closed")
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("active request context was not canceled")
	}
}

func TestBindFailure(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	s := testServer(t)
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	s.config.Port, _ = strconv.Atoi(port)
	s.http.Addr = s.config.Address()
	if err := s.Run(context.Background()); err == nil || s.ready.Load() {
		t.Fatalf("bind failure = %v, ready=%v", err, s.ready.Load())
	}
}

func TestCanceledStartup(t *testing.T) {
	s := testServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Run(ctx); err != nil || s.ready.Load() {
		t.Fatalf("canceled startup = %v, ready=%v", err, s.ready.Load())
	}
}

func TestUnexpectedListenerFailure(t *testing.T) {
	s := testServer(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener.Close()
	if err := s.serve(context.Background(), listener); err == nil || s.ready.Load() {
		t.Fatalf("listener failure = %v, ready=%v", err, s.ready.Load())
	}
}

func TestRejectNilLogger(t *testing.T) {
	if _, err := NewServer(config.Config{}, nil); err == nil {
		t.Fatal("nil logger accepted")
	}
}
