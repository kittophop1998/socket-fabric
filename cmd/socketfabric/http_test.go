package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	socketfabric "socketfabric/sdk/go"
)

func writeAccepted(t *testing.T, w http.ResponseWriter, r *http.Request, eventID string) {
	t.Helper()
	var event socketfabric.Event
	if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
		t.Error("invalid publish request body")
	}
	event.EventID, event.Timestamp = eventID, time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(socketfabric.PublishResult{Event: event, QueuedConnections: 0})
}

func TestPublishUsesSDKContractAndOmitsPayload(t *testing.T) {
	for _, payload := range []string{`{"privateValue":"do-not-print"}`, "null", "42", `"a string"`, "[1,2]", "true"} {
		t.Run(payload, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != "POST" || r.URL.Path != "/proxy/v1/events" || r.Header.Get("Authorization") != "Bearer "+testKey || r.Header.Get("Content-Type") != "application/json" {
					t.Error("incorrect publish wire contract")
				}
				writeAccepted(t, w, r, "event-123")
			}))
			defer server.Close()
			app, out, stderr := testApplication(publisherEnv(server.URL+"/proxy"), payload, nil)
			if code := app.run(context.Background(), publishArgs("-")); code != 0 || stderr.Len() != 0 || calls.Load() != 1 {
				t.Fatalf("publish exit=%d attempts=%d error=%s", code, calls.Load(), stderr)
			}
			result := decodeOutput[publishOutput](t, out)
			if !result.Accepted || result.DeliveryConfirmed || result.EventID != "event-123" || result.ProjectID != "project-a" || result.RoomID != "room-123" || result.Type != "resource.updated" || result.Timestamp.IsZero() || result.QueuedConnections != 0 {
				t.Fatal("incorrect acceptance output")
			}
			if strings.Contains(out.String(), "do-not-print") || strings.Contains(out.String(), `"data"`) || strings.Contains(out.String(), testKey) {
				t.Fatal("output exposed payload or credentials")
			}
		})
	}
}

func TestInvalidPublishNeverMakesNetworkRequest(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	for _, args := range [][]string{
		{"publish"}, {"publish", "--room", "room-123"}, {"publish", "--event", "resource.updated", "--data", "null"},
		publishArgs(""), publishArgs("{"), publishArgs("{}{}"), publishArgs(testKey),
		publishArgs(`"` + strings.Repeat("x", maxDataBytes) + `"`),
		// The payload itself fits, but the SDK's complete encoded body does not.
		publishArgs(`"` + strings.Repeat("x", maxDataBytes-2) + `"`),
		append(publishArgs("null"), "--room", "denied/*"),
		append(publishArgs("null"), "--event", strings.Repeat("e", 129)),
	} {
		app, out, stderr := testApplication(publisherEnv(server.URL), "{", nil)
		if code := app.run(context.Background(), args); code != 2 || out.Len() != 0 || strings.Contains(stderr.String(), testKey) {
			t.Fatalf("invalid input exit=%d", code)
		}
	}
	app, _, _ := testApplication(publisherEnv(server.URL), strings.Repeat(" ", maxDataBytes)+"null", nil)
	if code := app.run(context.Background(), publishArgs("-")); code != 2 {
		t.Fatal("oversized stdin accepted")
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid inputs made %d network requests", calls.Load())
	}
}

func TestHTTPFailuresAreActionableRedactedAndNotRetried(t *testing.T) {
	for _, status := range []int{400, 401, 403, 413, 415, 429, 500, 502, 503, 504} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"error":{"code":"`+testKey+`","message":"private-payload `+testKey+`","requestId":"`+testKey+`"}}`)
			}))
			defer server.Close()
			app, out, stderr := testApplication(publisherEnv(server.URL), "", nil)
			if code := app.run(context.Background(), publishArgs("null")); code != 1 || calls.Load() != 1 || out.Len() != 0 {
				t.Fatalf("HTTP %d exit=%d attempts=%d", status, code, calls.Load())
			}
			result := decodeOutput[struct{ Error commandError }](t, stderr)
			if result.Error.HTTPStatus != status || !strings.Contains(result.Error.Message, "HTTP "+strconv.Itoa(status)) || strings.Contains(stderr.String(), testKey) || strings.Contains(stderr.String(), "private-payload") {
				t.Fatal("incorrect or unsafe upstream error")
			}
		})
	}
}

func TestPublishRejectsMalformedSuccessAndRedirect(t *testing.T) {
	var destinationCalls atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { destinationCalls.Add(1) }))
	defer destination.Close()
	for _, test := range []struct {
		status int
		body   string
	}{
		{202, `{"unexpected":"` + testKey + `"}`}, {202, "not JSON " + testKey},
		{302, testKey}, {307, testKey},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Location", destination.URL)
			w.WriteHeader(test.status)
			_, _ = io.WriteString(w, test.body)
		}))
		app, _, stderr := testApplication(publisherEnv(server.URL), "", nil)
		if code := app.run(context.Background(), publishArgs("null")); code != 1 || strings.Contains(stderr.String(), testKey) {
			t.Fatalf("malformed/redirect exit=%d", code)
		}
		server.Close()
	}
	if destinationCalls.Load() != 0 {
		t.Fatal("publisher followed redirect")
	}
}

func TestPublishHumanOutputRedactsEventID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeAccepted(t, w, r, testKey) }))
	defer server.Close()
	for _, args := range [][]string{publishArgs("null"), {"publish", "--room", "room-123", "--event", "resource.updated", "--data", "null"}} {
		app, out, _ := testApplication(publisherEnv(server.URL), "", nil)
		if code := app.run(context.Background(), args); code != 0 || strings.Contains(out.String(), testKey) || !strings.Contains(out.String(), "[redacted]") {
			t.Fatal("event ID leaked credentials")
		}
	}
}

func TestDiagnosticsValidateBothEndpointsWithoutCredentials(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "GET" || r.Header.Get("Authorization") != "" || r.Header.Get("Accept") != "application/json" {
			t.Error("diagnostics sent wrong method or credentials")
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		switch r.URL.Path {
		case "/proxy/healthz":
			_, _ = io.WriteString(w, `{"status":"ok"}`)
		case "/proxy/readyz":
			_, _ = io.WriteString(w, `{"status":"ready"}`)
		default:
			t.Error("incorrect diagnostic path")
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	for _, command := range []string{"status", "ping"} {
		for _, jsonMode := range []bool{false, true} {
			env := publisherEnv("invalid")
			// Bad publisher configuration must not prevent public diagnostics.
			env["SOCKETFABRIC_PROJECT_ID"], env["SOCKETFABRIC_API_KEY"] = "bad project", "bad key"
			app, out, stderr := testApplication(env, "", nil)
			args := []string{command, "--url", server.URL + "/proxy"}
			if jsonMode {
				args = append(args, "--json")
			}
			if code := app.run(context.Background(), args); code != 0 || stderr.Len() != 0 {
				t.Fatalf("%s exit=%d error=%s", command, code, stderr)
			}
			if jsonMode {
				result := decodeOutput[struct {
					Scope, Gateway    string
					Health, Readiness probeResult
					HTTP              probeResult
				}](t, out)
				if command == "status" && (!result.Health.OK || !result.Readiness.OK || result.Scope != "gateway" || !result.Health.Reachable || result.Health.HTTPStatus != 200) {
					t.Fatal("incorrect status result")
				}
				if command == "ping" && (!result.HTTP.OK || result.HTTP.ResponseTimeMS < 0 || result.Scope != "http") {
					t.Fatal("incorrect ping result")
				}
			} else if !strings.Contains(out.String(), "not WebSocket/browser delivery") {
				t.Fatal("missing HTTP diagnostic scope")
			}
		}
	}
	if calls.Load() != 6 {
		t.Fatalf("unexpected diagnostic requests %d", calls.Load())
	}
}

func TestReadinessFailureStillReportsLiveness(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/healthz" {
			_, _ = io.WriteString(w, `{"status":"ok"}`)
		} else {
			w.WriteHeader(503)
			_, _ = io.WriteString(w, testKey)
		}
	}))
	defer server.Close()
	app, out, stderr := testApplication(publisherEnv(server.URL), "", nil)
	if code := app.run(context.Background(), []string{"status", "--json"}); code != 1 {
		t.Fatalf("unready gateway exit=%d", code)
	}
	result := decodeOutput[struct{ Health, Readiness probeResult }](t, out)
	if !result.Health.OK || result.Readiness.OK || !result.Readiness.Reachable || result.Readiness.HTTPStatus != 503 || strings.Contains(out.String()+stderr.String(), testKey) {
		t.Fatal("incorrect broker-outage diagnostic")
	}
	app, _, _ = testApplication(publisherEnv(server.URL), "", nil)
	if code := app.run(context.Background(), []string{"ping"}); code != 0 {
		t.Fatal("HTTP ping must remain successful during readiness failure")
	}
}

func TestDiagnosticMalformedResponsesAndRedirects(t *testing.T) {
	var destinationCalls atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { destinationCalls.Add(1) }))
	defer destination.Close()
	for _, test := range []struct {
		status int
		media  string
		body   string
	}{
		{200, "text/html", `{"status":"ok"}`}, {200, "application/json", `{"status":"ready"}`},
		{200, "application/json", `{"status":"ok"}{}`}, {200, "application/json", strings.Repeat(" ", 8<<10) + `{"status":"ok"}`},
		{200, "application/json", testKey}, {301, "text/html", testKey}, {500, "application/json", testKey},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", test.media)
			w.Header().Set("Location", destination.URL)
			w.WriteHeader(test.status)
			_, _ = io.WriteString(w, test.body)
		}))
		app, out, stderr := testApplication(publisherEnv(server.URL), "", nil)
		if code := app.run(context.Background(), []string{"ping", "--json"}); code != 1 || strings.Contains(out.String()+stderr.String(), testKey) {
			t.Fatalf("invalid diagnostic response exit=%d", code)
		}
		result := decodeOutput[struct{ HTTP probeResult }](t, out)
		if !result.HTTP.Reachable || result.HTTP.OK || result.HTTP.HTTPStatus != test.status {
			t.Fatal("malformed response claimed healthy")
		}
		server.Close()
	}
	if destinationCalls.Load() != 0 {
		t.Fatal("diagnostics followed redirect")
	}
}

func TestNetworkFailureTimeoutAndCancellation(t *testing.T) {
	for _, command := range []string{"publish", "status", "ping"} {
		t.Run(command, func(t *testing.T) {
			args := []string{command, "--json"}
			if command == "publish" {
				args = publishArgs("null")
			}
			client := &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
				return nil, errors.New(testKey)
			})}
			app, out, stderr := testApplication(publisherEnv("http://localhost/"+testKey), "", client)
			if code := app.run(context.Background(), args); code != 1 || strings.Contains(out.String()+stderr.String(), testKey) {
				t.Fatalf("network failure exit=%d", code)
			}
			for _, canceled := range []bool{false, true} {
				started := make(chan struct{}, 2)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					started <- struct{}{}
					<-r.Context().Done()
				}))
				env := publisherEnv(server.URL)
				env["SOCKETFABRIC_TIMEOUT"] = "50ms"
				ctx, cancel := context.WithCancel(context.Background())
				if canceled {
					env["SOCKETFABRIC_TIMEOUT"] = "5s"
					go func() { <-started; cancel() }()
				}
				app, _, stderr = testApplication(env, "", nil)
				code := app.run(ctx, args)
				cancel()
				server.Close()
				want := 1
				if canceled {
					want = 130
				}
				if code != want {
					t.Fatalf("canceled=%t exit=%d want=%d error=%s", canceled, code, want, stderr)
				}
			}
		})
	}
}
