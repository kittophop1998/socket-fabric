//go:build integration

// Package natsserver runs a real external NATS process for integration tests.
package natsserver

import (
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

type process struct {
	command *exec.Cmd
	done    chan struct{}
	err     error // Written before done closes; read only after done is closed.
}

// Server owns one broker process and restarts on the same authenticated URL.
type Server struct {
	URL   string
	Token string

	t       testing.TB
	binary  string
	args    []string
	mu      sync.Mutex
	running *process
}

func Start(t testing.TB, token string) *Server {
	t.Helper()
	binary := os.Getenv("NATS_SERVER_BIN")
	if binary == "" {
		var err error
		binary, err = exec.LookPath("nats-server")
		if err != nil {
			t.Fatal("integration tests require NATS_SERVER_BIN or nats-server on PATH")
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("allocate broker test port", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	s := &Server{t: t, binary: binary, Token: token, URL: "nats://127.0.0.1:" + strconv.Itoa(port)}
	s.args = []string{"--addr", "127.0.0.1", "--port", strconv.Itoa(port)}
	if token != "" {
		s.args = append(s.args, "--auth", token)
	}
	t.Cleanup(s.Stop)
	s.Restart()
	return s
}

func (s *Server) Restart() {
	s.t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopLocked()
	p := &process{command: exec.Command(s.binary, s.args...), done: make(chan struct{})}
	// Keep credentials out of test output without buffering broker logs.
	p.command.Stdout, p.command.Stderr = io.Discard, io.Discard
	if err := p.command.Start(); err != nil {
		s.t.Fatal("start external broker process", err)
	}
	s.running = p
	go func() {
		p.err = p.command.Wait()
		close(p.done)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		conn, err := nats.Connect(s.URL, nats.Token(s.Token), nats.Timeout(100*time.Millisecond), nats.NoReconnect())
		if err == nil {
			flushCtx, cancelFlush := context.WithTimeout(ctx, 100*time.Millisecond)
			err = conn.FlushWithContext(flushCtx)
			cancelFlush()
			conn.Close()
			if err == nil {
				return
			}
		}
		select {
		case <-p.done:
			s.t.Fatal("broker exited before authenticated readiness", p.err)
		case <-ctx.Done():
			s.stopLocked()
			s.t.Fatal("broker failed authenticated readiness within startup deadline")
		case <-ticker.C:
		}
	}
}

func (s *Server) Stop() {
	s.t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopLocked()
}

func (s *Server) stopLocked() {
	p := s.running
	if p == nil {
		return
	}
	s.running = nil
	_ = p.command.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
		return
	case <-time.After(2 * time.Second):
	}
	_ = p.command.Process.Kill()
	select {
	case <-p.done:
	case <-time.After(time.Second):
		s.t.Error("broker process did not exit after forced cleanup")
	}
}
