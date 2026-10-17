package socketfabric

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testPublisherKey = "sdk-test-publisher-key-32-bytes-minimum"

func makeClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	client, err := NewClient(ClientOptions{BaseURL: baseURL, ProjectID: "project.one", PublisherKey: testPublisherKey})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func eventResponse(data json.RawMessage) PublishResult {
	return PublishResult{Event: Event{EventID: "evt_test", ProjectID: "project.one", RoomID: "room:one", Type: "test.event", Data: data, Timestamp: time.Now().UTC()}, QueuedConnections: 2}
}

func publishInput() PublishInput {
	return PublishInput{RoomID: "room:one", Type: "test.event", Data: nil}
}

func writeResult(w http.ResponseWriter, result PublishResult) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(result)
}

func TestPublishWireContractNullAndPrefix(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		if request.Method != http.MethodPost || request.URL.Path != "/proxy/v1/events" || request.URL.RawQuery != "" {
			t.Errorf("unexpected request route: %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer "+testPublisherKey || request.Header.Get("Content-Type") != "application/json" || request.Header.Get("X-Request-ID") != "request-123" {
			t.Error("missing request headers")
		}
		if request.Header.Get("Idempotency-Key") != "" || request.Header.Get("X-Idempotency-Key") != "" {
			t.Error("publisher must not imply idempotence")
		}
		var input map[string]json.RawMessage
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		if len(input) != 4 || string(input["projectId"]) != `"project.one"` || string(input["roomId"]) != `"room:one"` || string(input["type"]) != `"test.event"` || string(input["data"]) != "null" {
			t.Errorf("unexpected wire input: %s", input)
		}
		writeResult(w, eventResponse(json.RawMessage("null")))
	}))
	defer server.Close()
	client := makeClient(t, server.URL+"/proxy/")
	input := publishInput()
	input.RequestID = "request-123"
	result, err := client.Publish(context.Background(), input)
	if err != nil || result.QueuedConnections != 2 || string(result.Event.Data) != "null" || result.Event.EventID != "evt_test" || calls.Load() != 1 {
		t.Fatalf("publish result = %+v, error = %v, calls = %d", result, err, calls.Load())
	}
}

func TestClientOptionsValidation(t *testing.T) {
	for _, address := range []string{"", "ws://example.com", "/relative", "http://", "https://user:pass@example.com", "https://example.com?key=secret", "https://example.com?", "https://example.com#fragment", "http://example.com:invalid"} {
		t.Run(address, func(t *testing.T) {
			_, err := NewClient(ClientOptions{BaseURL: address, ProjectID: "project", PublisherKey: testPublisherKey})
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("expected invalid URL, got %v", err)
			}
		})
	}
	for _, change := range []func(*ClientOptions){
		func(o *ClientOptions) { o.ProjectID = "project/*" },
		func(o *ClientOptions) { o.PublisherKey = "short" },
		func(o *ClientOptions) { o.PublisherKey = testPublisherKey + "\n" },
		func(o *ClientOptions) { o.RequestTimeout = -time.Second },
	} {
		o := ClientOptions{BaseURL: "https://example.com/base", ProjectID: "project", PublisherKey: testPublisherKey}
		change(&o)
		if _, err := NewClient(o); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("expected invalid options, got %v", err)
		}
	}
	client := makeClient(t, "https://example.com")
	if client.timeout != 10*time.Second {
		t.Fatalf("default timeout %s", client.timeout)
	}
}

func TestPublishPayloadLimitAndInvalidInput(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(request.Body)
		if len(body) != maxPublishBytes {
			t.Errorf("boundary request size = %d", len(body))
		}
		writeResult(w, eventResponse(json.RawMessage("null")))
	}))
	defer server.Close()
	client := makeClient(t, server.URL)
	input := publishInput()
	input.Data = ""
	base, _ := json.Marshal(map[string]any{"projectId": "project.one", "type": input.Type, "roomId": input.RoomID, "data": ""})
	input.Data = strings.Repeat("a", maxPublishBytes-len(base))
	if _, err := client.Publish(context.Background(), input); err != nil {
		t.Fatalf("exact boundary rejected: %v", err)
	}
	input.Data = strings.Repeat("a", maxPublishBytes-len(base)+1)
	if _, err := client.Publish(context.Background(), input); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("oversized request error %v", err)
	}
	for _, bad := range []PublishInput{
		{RoomID: "bad room", Type: "test.event"},
		{RoomID: "room:one", Type: "*"},
		{RoomID: "room:one", Type: "test.event", RequestID: "request:invalid"},
		{RoomID: "room:one", Type: "test.event", Data: make(chan int)},
		{RoomID: "room:one", Type: "test.event", Data: json.RawMessage("{")},
	} {
		if _, err := client.Publish(context.Background(), bad); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("invalid publish input returned %v", err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("invalid requests reached server: %d", calls.Load())
	}
}

func TestPublishAPIErrorNoRetry(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("X-Request-ID", "header-request")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"code":"not_ready","message":"Service not ready","requestId":"body-request"}}`))
	}))
	defer server.Close()
	_, err := makeClient(t, server.URL).Publish(context.Background(), publishInput())
	var apiError *APIError
	if !errors.As(err, &apiError) || apiError.StatusCode != 503 || apiError.Code != "not_ready" || apiError.Message != "Service not ready" || apiError.RequestID != "body-request" || calls.Load() != 1 {
		t.Fatalf("API error = %#v, calls = %d", apiError, calls.Load())
	}
}

func TestPublishDisablesRedirectsWithoutMutatingHTTPClient(t *testing.T) {
	var leaked atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { leaked.Add(1) }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		http.Redirect(w, request, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	var originalRedirects atomic.Int32
	httpClient := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { originalRedirects.Add(1); return nil }}
	client, err := NewClient(ClientOptions{BaseURL: server.URL, ProjectID: "project.one", PublisherKey: testPublisherKey, HTTPClient: httpClient})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Publish(context.Background(), publishInput())
	var apiError *APIError
	if !errors.As(err, &apiError) || apiError.StatusCode != 307 || leaked.Load() != 0 || originalRedirects.Load() != 0 {
		t.Fatalf("redirect result = %v, leaked = %d", err, leaked.Load())
	}
	response, err := httpClient.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if originalRedirects.Load() != 1 || leaked.Load() != 1 {
		t.Fatal("caller HTTP client changed")
	}
}

func TestPublishResponseValidation(t *testing.T) {
	valid, _ := json.Marshal(eventResponse(json.RawMessage("null")))
	cases := map[string]string{
		"invalid JSON":     "not JSON",
		"multiple values":  string(valid) + "{}",
		"wrong project":    strings.Replace(string(valid), "project.one", "project.two", 1),
		"wrong room":       strings.Replace(string(valid), "room:one", "room:two", 1),
		"wrong type":       strings.Replace(string(valid), "test.event", "other.event", 1),
		"invalid event ID": strings.Replace(string(valid), "evt_test", "bad/id", 1),
		"missing count":    strings.Replace(string(valid), `,"queuedConnections":2`, "", 1),
		"null count":       strings.Replace(string(valid), `"queuedConnections":2`, `"queuedConnections":null`, 1),
		"negative count":   strings.Replace(string(valid), `"queuedConnections":2`, `"queuedConnections":-1`, 1),
		"missing data":     strings.Replace(string(valid), `"data":null,`, "", 1),
		"zero timestamp":   strings.Replace(string(valid), time.Now().Format("2006"), "0001", 1),
		"oversize":         strings.Repeat(" ", maxResponseBytes+1),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if name == "zero timestamp" {
				body = `{"event":{"eventId":"evt_test","projectId":"project.one","roomId":"room:one","type":"test.event","data":null,"timestamp":"0001-01-01T00:00:00Z"},"queuedConnections":0}`
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusAccepted)
				_, _ = io.WriteString(w, body)
			}))
			defer server.Close()
			_, err := makeClient(t, server.URL).Publish(context.Background(), publishInput())
			if !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("response error = %v", err)
			}
		})
	}
}

func TestPublishAcceptsBoundedHTMLEscapedResponse(t *testing.T) {
	// HTML escaping by JSON encoders can expand data far beyond 128 KiB.
	data, _ := json.Marshal(strings.Repeat("<>&", 18000))
	if len(data) < 300000 {
		t.Fatal("fixture must exercise expanded response")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { writeResult(w, eventResponse(data)) }))
	defer server.Close()
	result, err := makeClient(t, server.URL).Publish(context.Background(), publishInput())
	if err != nil || len(result.Event.Data) < 300000 {
		t.Fatalf("large valid response returned %v", err)
	}
}

func TestPublishCancellationAndTimeouts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { _, _ = io.Copy(io.Discard, r.Body); <-r.Context().Done() }))
	defer server.Close()
	tests := []struct {
		name                                   string
		callerTimeout, sdkTimeout, httpTimeout time.Duration
		canceled                               bool
	}{
		{"canceled", 0, time.Second, 0, true},
		{"caller deadline", 20 * time.Millisecond, time.Second, 0, false},
		{"SDK timeout", time.Second, 20 * time.Millisecond, 0, false},
		{"HTTP client timeout", time.Second, time.Second, 20 * time.Millisecond, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if test.canceled {
				cancel()
			} else {
				var done context.CancelFunc
				ctx, done = context.WithTimeout(ctx, test.callerTimeout)
				defer done()
			}
			client, err := NewClient(ClientOptions{BaseURL: server.URL, ProjectID: "project.one", PublisherKey: testPublisherKey, RequestTimeout: test.sdkTimeout, HTTPClient: &http.Client{Timeout: test.httpTimeout}})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Publish(ctx, publishInput())
			want := context.DeadlineExceeded
			if test.canceled {
				want = context.Canceled
			}
			if !errors.Is(err, want) {
				t.Fatalf("cancellation error = %v", err)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type secretMarshaler struct{}

func (secretMarshaler) MarshalJSON() ([]byte, error) {
	return nil, errors.New(testPublisherKey + " secret-data")
}

func TestPublishErrorsDoNotLeakCredentialsOrRawBodies(t *testing.T) {
	var attempts atomic.Int32
	httpClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		attempts.Add(1)
		return nil, errors.New(testPublisherKey + " secret-data")
	})}
	client, err := NewClient(ClientOptions{BaseURL: "https://example.com", ProjectID: "project.one", PublisherKey: testPublisherKey, HTTPClient: httpClient})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Publish(context.Background(), publishInput())
	if !errors.Is(err, ErrRequestFailed) || strings.Contains(err.Error(), testPublisherKey) || attempts.Load() != 1 {
		t.Fatalf("unsafe transport error: %v", err)
	}
	input := publishInput()
	input.Data = secretMarshaler{}
	_, err = client.Publish(context.Background(), input)
	if !errors.Is(err, ErrInvalidInput) || strings.Contains(err.Error(), testPublisherKey) {
		t.Fatalf("unsafe data error: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(401)
		_, _ = fmt.Fprintf(w, `{"error":{"code":"%s","message":"secret-data","requestId":"safe-id"}}`, testPublisherKey)
	}))
	defer server.Close()
	_, err = makeClient(t, server.URL).Publish(context.Background(), publishInput())
	if strings.Contains(err.Error(), testPublisherKey) || strings.Contains(err.Error(), "secret-data") {
		t.Fatalf("unsafe API error: %v", err)
	}
}

func TestClientConcurrentPublish(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeResult(w, eventResponse(json.RawMessage("null")))
	}))
	defer server.Close()
	client := makeClient(t, server.URL)
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			if _, err := client.Publish(context.Background(), publishInput()); err != nil {
				t.Errorf("concurrent publish: %v", err)
			}
		})
	}
	wg.Wait()
	if calls.Load() != 32 {
		t.Fatalf("concurrent calls = %d", calls.Load())
	}
}

func TestUninitializedClientFailsSafely(t *testing.T) {
	for _, client := range []*Client{nil, {}} {
		if _, err := client.Publish(context.Background(), publishInput()); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("uninitialized client error = %v", err)
		}
	}
	if _, err := makeClient(t, "http://example.com").Publish(nil, publishInput()); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("nil context error = %v", err)
	}
}
