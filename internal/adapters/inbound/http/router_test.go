package httpadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
)

func newTestRouter(t *testing.T, logger *slog.Logger, ready func() bool) *gin.Engine {
	t.Helper()
	router, err := NewRouter(logger, ready, nil)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	return router
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func TestNewRouterRequiresDependencies(t *testing.T) {
	if _, err := NewRouter(nil, func() bool { return true }, nil); err == nil {
		t.Fatal("expected nil logger to be rejected")
	}
	if _, err := NewRouter(discardLogger(), nil, nil); err == nil {
		t.Fatal("expected nil readiness callback to be rejected")
	}
}

func TestHealthAndReadiness(t *testing.T) {
	var ready atomic.Bool
	router := newTestRouter(t, discardLogger(), ready.Load)

	for _, tc := range []struct {
		name   string
		path   string
		ready  bool
		status int
		body   string
	}{
		{"live during initialization", "/healthz", false, http.StatusOK, `{"status":"ok"}`},
		{"not ready", "/readyz", false, http.StatusServiceUnavailable, `{"error":{"code":"not_ready","message":"Service not ready","requestId":"test-request"}}`},
		{"ready", "/readyz", true, http.StatusOK, `{"status":"ready"}`},
		{"draining", "/readyz", false, http.StatusServiceUnavailable, `{"error":{"code":"not_ready","message":"Service not ready","requestId":"test-request"}}`},
		{"live during drain", "/healthz", false, http.StatusOK, `{"status":"ok"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ready.Store(tc.ready)
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.Header.Set(requestIDHeader, "test-request")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, req)
			if response.Code != tc.status || response.Body.String() != tc.body {
				t.Fatalf("response = %d %s; want %d %s", response.Code, response.Body.String(), tc.status, tc.body)
			}
			if response.Header().Get("Content-Type") != "application/json; charset=utf-8" {
				t.Fatalf("unexpected content type: %s", response.Header().Get("Content-Type"))
			}
		})
	}
}

func TestRoutingErrors(t *testing.T) {
	router := newTestRouter(t, discardLogger(), func() bool { return true })
	for _, tc := range []struct {
		method string
		path   string
		status int
		code   string
		allow  string
	}{
		{http.MethodGet, "/missing", http.StatusNotFound, "not_found", ""},
		{http.MethodGet, "/healthz/", http.StatusNotFound, "not_found", ""},
		{http.MethodGet, "/readyz/", http.StatusNotFound, "not_found", ""},
		{http.MethodPost, "/missing", http.StatusNotFound, "not_found", ""},
		{http.MethodPost, "/healthz", http.StatusMethodNotAllowed, "method_not_allowed", http.MethodGet},
		{http.MethodHead, "/readyz", http.StatusMethodNotAllowed, "method_not_allowed", http.MethodGet},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, req)
			assertError(t, response, tc.status, tc.code)
			if got := response.Header().Get("Allow"); got != tc.allow {
				t.Fatalf("Allow = %q; want %q", got, tc.allow)
			}
		})
	}
}

func TestRequestIDAndContext(t *testing.T) {
	router := newTestRouter(t, discardLogger(), func() bool { return true })
	type markerKey struct{}
	router.GET("/context", func(c *gin.Context) {
		if c.Request.Context().Value(markerKey{}) != "preserved" {
			t.Error("parent context values were lost")
		}
		if c.Request.Context().Err() != context.Canceled {
			t.Error("parent context cancellation was lost")
		}
		if RequestID(c.Request.Context()) != c.Writer.Header().Get(requestIDHeader) {
			t.Error("context request ID differs from response header")
		}
		c.Status(http.StatusNoContent)
	})

	for _, id := range []string{"trace-123._ABC", strings.Repeat("a", 128), "", strings.Repeat("a", 129), "has a space", "contains/tab\t", "ไทย"} {
		t.Run(fmt.Sprintf("length_%d_%q", len(id), id), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.WithValue(context.Background(), markerKey{}, "preserved"))
			cancel()
			req := httptest.NewRequest(http.MethodGet, "/context", nil).WithContext(ctx)
			req.Header.Set(requestIDHeader, id)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, req)
			got := response.Header().Get(requestIDHeader)
			if !validRequestID(got) {
				t.Fatalf("response request ID is invalid: %q", got)
			}
			if validRequestID(id) && got != id {
				t.Fatalf("accepted request ID changed: got %q; want %q", got, id)
			}
			if !validRequestID(id) && got == id {
				t.Fatal("invalid request ID was not replaced")
			}
		})
	}
	if RequestID(context.Background()) != "" {
		t.Fatal("context without request ID should return an empty string")
	}
}

func TestForwardedHeadersAreNotTrusted(t *testing.T) {
	router := newTestRouter(t, discardLogger(), func() bool { return true })
	router.GET("/client-ip", func(c *gin.Context) {
		c.String(http.StatusOK, c.ClientIP())
	})
	req := httptest.NewRequest(http.MethodGet, "/client-ip", nil)
	req.RemoteAddr = "192.0.2.10:12345"
	req.Header.Set("X-Forwarded-For", "198.51.100.10")
	req.Header.Set("X-Real-IP", "198.51.100.20")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	if response.Body.String() != "192.0.2.10" {
		t.Fatalf("untrusted forwarded header changed client IP: %s", response.Body.String())
	}
}

func TestRecoveryAndSafeLogs(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	router := newTestRouter(t, logger, func() bool { return true })
	router.GET("/panic/:id", func(c *gin.Context) {
		panic("PANIC_SECRET")
	})
	req := httptest.NewRequest(http.MethodGet, "/panic/PATH_SECRET?token=QUERY_SECRET", strings.NewReader("BODY_SECRET"))
	req.Header.Set("Authorization", "Bearer HEADER_SECRET")
	req.Header.Set(requestIDHeader, "recovery-test")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	assertError(t, response, http.StatusInternalServerError, "internal_error")
	if strings.Contains(response.Body.String(), "SECRET") {
		t.Fatal("panic response exposed internal information")
	}

	// Unknown paths must not be used as log route labels.
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/UNKNOWN_SECRET?key=QUERY_SECRET", nil))
	working := httptest.NewRecorder()
	router.ServeHTTP(working, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if working.Code != http.StatusOK {
		t.Fatal("server did not continue serving after panic")
	}
	if strings.Contains(logs.String(), "SECRET") {
		t.Fatalf("request information leaked in logs: %s", logs.String())
	}

	decoder := json.NewDecoder(&logs)
	var requestLogCount int
	for decoder.More() {
		var entry map[string]any
		if err := decoder.Decode(&entry); err != nil {
			t.Fatalf("decode JSON log: %v", err)
		}
		if entry["msg"] != "HTTP request" {
			continue
		}
		requestLogCount++
		for _, key := range []string{"request_id", "method", "route", "status", "duration_ms"} {
			if _, ok := entry[key]; !ok {
				t.Errorf("request log is missing %s", key)
			}
		}
		if entry["request_id"] == "recovery-test" {
			if entry["route"] != "/panic/:id" || entry["status"] != float64(http.StatusInternalServerError) {
				t.Errorf("panic request log has incorrect route or status: %v", entry)
			}
		}
	}
	if requestLogCount != 3 {
		t.Fatalf("got %d request logs; want 3", requestLogCount)
	}
}

func TestRecoveryPreservesCommittedResponse(t *testing.T) {
	router := newTestRouter(t, discardLogger(), func() bool { return true })
	router.GET("/committed", func(c *gin.Context) {
		c.String(http.StatusAccepted, "partial response")
		panic("failure after response")
	})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/committed", nil))
	if response.Code != http.StatusAccepted || response.Body.String() != "partial response" {
		t.Fatalf("committed response was changed: %d %s", response.Code, response.Body.String())
	}
}

func TestRecoveryHandlesNonComparableValues(t *testing.T) {
	router := newTestRouter(t, discardLogger(), func() bool { return true })
	router.GET("/slice-panic", func(c *gin.Context) { panic([]string{"private"}) })
	router.GET("/map-panic", func(c *gin.Context) { panic(map[string]string{"private": "value"}) })
	for _, path := range []string{"/slice-panic", "/map-panic"} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		assertError(t, response, http.StatusInternalServerError, "internal_error")
	}
}

func TestRecoveryPreservesAbortHandler(t *testing.T) {
	router := newTestRouter(t, discardLogger(), func() bool { return true })
	router.GET("/abort", func(c *gin.Context) {
		panic(http.ErrAbortHandler)
	})
	defer func() {
		if got := recover(); got != http.ErrAbortHandler {
			t.Fatalf("panic = %v; want http.ErrAbortHandler", got)
		}
	}()
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/abort", nil))
	t.Fatal("http.ErrAbortHandler was swallowed")
}

func TestRouterConcurrentRequests(t *testing.T) {
	var ready atomic.Bool
	ready.Store(true)
	router := newTestRouter(t, discardLogger(), ready.Load)
	const requests = 64
	var wg sync.WaitGroup
	for i := range requests {
		wg.Go(func() {
			ready.Store(i%2 == 0)
			id := fmt.Sprintf("concurrent-%d", i)
			req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
			req.Header.Set(requestIDHeader, id)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, req)
			if response.Code != http.StatusOK && response.Code != http.StatusServiceUnavailable {
				t.Errorf("unexpected concurrent response status: %d", response.Code)
			}
			if response.Header().Get(requestIDHeader) != id {
				t.Errorf("request IDs leaked between concurrent requests")
			}
		})
	}
	wg.Wait()
}

func assertError(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status = %d; want %d; body: %s", response.Code, status, response.Body.String())
	}
	var body errorResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if body.Error.Code != code || body.Error.Message == "" {
		t.Fatalf("unexpected error response: %+v", body.Error)
	}
	if body.Error.RequestID == "" || body.Error.RequestID != response.Header().Get(requestIDHeader) {
		t.Fatalf("response request IDs do not match: body=%q header=%q", body.Error.RequestID, response.Header().Get(requestIDHeader))
	}
}
