package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func testSecret(t *testing.T) string {
	t.Helper()
	var data [32]byte
	if _, err := rand.Read(data[:]); err != nil {
		t.Fatal("cannot generate test credentials")
	}
	return hex.EncodeToString(data[:])
}

func testOptions(t *testing.T) exampleOptions {
	t.Helper()
	directory := t.TempDir()
	for name, content := range map[string]string{"index.html": "<!doctype html><title>Example</title>", "app.js": "export {};"} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return exampleOptions{
		Address: "127.0.0.1:3000", Origin: "http://127.0.0.1:3000", PublicDir: directory,
		GatewayURL: "http://127.0.0.1:8080", WebSocketURLs: []string{"ws://127.0.0.1:8080/ws"},
		SigningSecret: testSecret(t), PublisherKeys: map[string]string{"project-a": testSecret(t), "project-b": testSecret(t)},
		Users: map[string]exampleUser{
			"alice": {Password: testSecret(t), ProjectID: "project-a", Rooms: []string{"room-a"}},
			"bob":   {Password: testSecret(t), ProjectID: "project-b", Rooms: []string{"room-b"}},
		},
		TokenTTL: 2 * time.Minute, RequestTimeout: time.Second,
	}
}

func testApplication(t *testing.T, options exampleOptions) *exampleApplication {
	t.Helper()
	app, err := newExample(options)
	if err != nil {
		t.Fatal(err)
	}
	return app
}

func appRequest(t *testing.T, handler http.Handler, options exampleOptions, method, path, username, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, options.Origin+path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if username != "" {
		request.SetBasicAuth(username, options.Users[username].Password)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func assertNoSecrets(t *testing.T, output string, options exampleOptions) {
	t.Helper()
	secrets := []string{options.SigningSecret}
	for _, key := range options.PublisherKeys {
		secrets = append(secrets, key)
	}
	for _, user := range options.Users {
		secrets = append(secrets, user.Password)
	}
	for _, secret := range secrets {
		if strings.Contains(output, secret) {
			t.Fatal("output exposed a server credential")
		}
	}
}

func TestTokenAuthenticationAndServerOwnedScope(t *testing.T) {
	t.Parallel()
	o := testOptions(t)
	handler := testApplication(t, o).handler()
	for _, username := range []string{"alice", "bob"} {
		response := appRequest(t, handler, o, "POST", "/api/realtime-token", username, "{}")
		if response.Code != 200 || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("token response: HTTP %d", response.Code)
		}
		assertNoSecrets(t, response.Body.String(), o)
		var result struct {
			Token, ProjectID, UserID string
			Rooms                    []string
			ExpiresIn                int64
		}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		claims := jwt.MapClaims{}
		if _, err := jwt.ParseWithClaims(result.Token, claims, func(token *jwt.Token) (any, error) {
			return []byte(o.SigningSecret), nil
		}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithIssuer("socketfabric"), jwt.WithAudience("socketfabric-ws"), jwt.WithExpirationRequired()); err != nil {
			t.Fatal("application did not issue a valid scoped token")
		}
		if result.ProjectID != o.Users[username].ProjectID || result.UserID != username || result.ExpiresIn != 120 || len(result.Rooms) != 1 || result.Rooms[0] != o.Users[username].Rooms[0] || claims["projectId"] != result.ProjectID || claims["sub"] != username {
			t.Fatal("token scope differs from server policy")
		}
		expires, _ := claims.GetExpirationTime()
		issued, _ := claims.GetIssuedAt()
		if expires == nil || issued == nil || expires.Sub(issued.Time) != o.TokenTTL {
			t.Fatal("token TTL differs from configured short lifetime")
		}
	}
	for _, username := range []string{"", "unknown"} {
		response := appRequest(t, handler, o, "POST", "/api/realtime-token", username, "")
		if response.Code != 401 {
			t.Fatalf("unauthorized token request: HTTP %d", response.Code)
		}
		assertNoSecrets(t, response.Body.String(), o)
	}
	request := httptest.NewRequest("POST", o.Origin+"/api/realtime-token", nil)
	request.SetBasicAuth("alice", "incorrect")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != 401 {
		t.Fatal("wrong password accepted")
	}
	for _, body := range []string{`{"projectId":"project-b","rooms":["room-b"]}`, "null", "{} {}", strings.Repeat(" ", 1025)} {
		if response := appRequest(t, handler, o, "POST", "/api/realtime-token", "alice", body); response.Code != 400 {
			t.Fatalf("client-selected scope or invalid body accepted: HTTP %d", response.Code)
		}
	}
}

func TestBusinessAPIPublishesAuthorizedSDKContractOnce(t *testing.T) {
	t.Parallel()
	o := testOptions(t)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "POST" || r.URL.Path != "/v1/events" {
			t.Error("business API did not use existing publish contract")
		}
		var input struct {
			ProjectID, RoomID, Type string
			Data                    struct{ ResourceID, Status string }
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error("invalid SDK request")
		}
		if r.Header.Get("Authorization") != "Bearer "+o.PublisherKeys[input.ProjectID] || input.Type != "resource.updated" || input.Data.ResourceID != "resource-42" || input.Data.Status != "completed" {
			t.Error("business API supplied incorrect authentication or event")
		}
		respond(w, 202, map[string]any{"event": map[string]any{"eventId": "event-42", "projectId": input.ProjectID, "roomId": input.RoomID, "type": input.Type, "data": input.Data, "timestamp": time.Now().UTC()}, "queuedConnections": 1})
	}))
	defer upstream.Close()
	o.GatewayURL = upstream.URL
	handler := testApplication(t, o).handler()
	for username, room := range map[string]string{"alice": "room-a", "bob": "room-b"} {
		response := appRequest(t, handler, o, "POST", "/api/resources/resource-42/update", username, `{"roomId":"`+room+`","status":"completed"}`)
		if response.Code != 202 {
			t.Fatalf("business publish: HTTP %d", response.Code)
		}
		assertNoSecrets(t, response.Body.String(), o)
	}
	if calls.Load() != 2 {
		t.Fatal("publish sent extra requests")
	}
	for _, test := range []struct {
		body, user string
		status     int
	}{
		{`{"roomId":"room-a","status":"completed"}`, "", 401},
		{`{"roomId":"room-b","status":"completed"}`, "alice", 403},
		{`{"roomId":"room-a","status":"completed","projectId":"project-b"}`, "alice", 400},
		{`{"roomId":"room-a","status":"completed","data":{}}`, "alice", 400},
		{`{"roomId":"room-a","status":"<img>"}`, "alice", 400},
		{`{"roomId":"room-a","status":"completed"} {}`, "alice", 400},
		{"not-json", "alice", 400},
		{strings.Repeat("a", (8<<10)+1), "alice", 400},
	} {
		response := appRequest(t, handler, o, "POST", "/api/resources/resource-42/update", test.user, test.body)
		if response.Code != test.status {
			t.Fatalf("invalid business input: got HTTP %d, want %d", response.Code, test.status)
		}
	}
	request := httptest.NewRequest("POST", o.Origin+"/api/resources/resource-42/update", strings.NewReader(`{"roomId":"room-a","status":"completed"}`))
	request.SetBasicAuth("alice", o.Users["alice"].Password)
	request.Header.Set("Content-Type", "text/plain")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != 415 || calls.Load() != 2 {
		t.Fatal("invalid input reached the gateway")
	}
}

func TestBusinessAPIFailureIsSanitizedAndNeverRetried(t *testing.T) {
	t.Parallel()
	for status, expected := range map[int]int{400: 502, 401: 502, 403: 502, 429: 503, 500: 502, 503: 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			o := testOptions(t)
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				respond(w, status, map[string]any{"error": map[string]string{"code": o.SigningSecret, "message": o.PublisherKeys["project-a"]}})
			}))
			defer upstream.Close()
			o.GatewayURL = upstream.URL
			response := appRequest(t, testApplication(t, o).handler(), o, "POST", "/api/resources/resource-42/update", "alice", `{"roomId":"room-a","status":"updated"}`)
			if response.Code != expected || calls.Load() != 1 {
				t.Fatalf("failed publish: HTTP %d, requests %d", response.Code, calls.Load())
			}
			assertNoSecrets(t, response.Body.String(), o)
		})
	}
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestBusinessAPITimeoutAndRequestCancellation(t *testing.T) {
	t.Parallel()
	for _, cancelRequest := range []bool{false, true} {
		o := testOptions(t)
		o.RequestTimeout = 30 * time.Millisecond
		started := make(chan struct{})
		o.HTTPClient = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
			close(started)
			<-r.Context().Done()
			return nil, r.Context().Err()
		})}
		handler := testApplication(t, o).handler()
		ctx, cancel := context.WithCancel(context.Background())
		request := httptest.NewRequest("POST", o.Origin+"/api/resources/resource-42/update", strings.NewReader(`{"roomId":"room-a","status":"updated"}`)).WithContext(ctx)
		request.Header.Set("Content-Type", "application/json")
		request.SetBasicAuth("alice", o.Users["alice"].Password)
		recorder := httptest.NewRecorder()
		done := make(chan struct{})
		go func() { handler.ServeHTTP(recorder, request); close(done) }()
		<-started
		if cancelRequest {
			cancel()
		}
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("publish did not honor request cancellation/timeout")
		}
		cancel()
		if recorder.Code != 503 {
			t.Fatalf("canceled publish: HTTP %d", recorder.Code)
		}
		assertNoSecrets(t, recorder.Body.String(), o)
	}
}

func TestStaticServingAndOriginBoundary(t *testing.T) {
	t.Parallel()
	o := testOptions(t)
	handler := testApplication(t, o).handler()
	for path, code := range map[string]int{"/": 200, "/app.js": 200, "/api/config": 200, "/.env": 404, "/main.go": 404, "/go.mod": 404, "/node_modules/@socketfabric/client/dist/index.js": 404, "/?token=hidden": 400} {
		response := appRequest(t, handler, o, "GET", path, "", "")
		if response.Code != code || response.Header().Get("Cache-Control") != "no-store" || !strings.Contains(response.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
			t.Fatalf("static route %s: HTTP %d", path, response.Code)
		}
		assertNoSecrets(t, response.Body.String(), o)
	}
	for _, foreignOrigin := range []bool{false, true} {
		request := httptest.NewRequest("POST", o.Origin+"/api/realtime-token", nil)
		request.SetBasicAuth("alice", o.Users["alice"].Password)
		if foreignOrigin {
			request.Header.Set("Origin", "http://untrusted.invalid")
		} else {
			request.Host = "untrusted.invalid"
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != 403 {
			t.Fatal("foreign host/origin accepted")
		}
	}
}

func TestConfigurationRejectsUnsafePoliciesWithoutSecrets(t *testing.T) {
	t.Parallel()
	for name, change := range map[string]func(*exampleOptions){
		"public listen":         func(o *exampleOptions) { o.Address = "0.0.0.0:3000" },
		"negative timeout":      func(o *exampleOptions) { o.RequestTimeout = -time.Second },
		"long token":            func(o *exampleOptions) { o.TokenTTL = 16 * time.Minute },
		"origin credentials":    func(o *exampleOptions) { o.Origin = "http://user:password@localhost:3000" },
		"mixed content":         func(o *exampleOptions) { o.Origin = "https://localhost:3000" },
		"gateway credentials":   func(o *exampleOptions) { o.WebSocketURLs = []string{"ws://user:password@localhost/ws"} },
		"gateway query":         func(o *exampleOptions) { o.WebSocketURLs = []string{"ws://localhost/ws?token=hidden"} },
		"reused keys":           func(o *exampleOptions) { o.PublisherKeys["project-b"] = o.PublisherKeys["project-a"] },
		"reused signing secret": func(o *exampleOptions) { o.PublisherKeys["project-a"] = o.SigningSecret },
		"unknown project":       func(o *exampleOptions) { user := o.Users["alice"]; user.ProjectID = "unknown"; o.Users["alice"] = user },
		"duplicate rooms": func(o *exampleOptions) {
			user := o.Users["alice"]
			user.Rooms = []string{"room-a", "room-a"}
			o.Users["alice"] = user
		},
		"short password": func(o *exampleOptions) { user := o.Users["alice"]; user.Password = "short"; o.Users["alice"] = user },
		"reused password": func(o *exampleOptions) {
			user := o.Users["alice"]
			user.Password = o.SigningSecret
			o.Users["alice"] = user
		},
		"basic username colon": func(o *exampleOptions) { o.Users["invalid:username"] = o.Users["alice"] },
	} {
		t.Run(name, func(t *testing.T) {
			o := testOptions(t)
			change(&o)
			if _, err := newExample(o); err == nil {
				t.Fatal("unsafe configuration accepted")
			} else {
				assertNoSecrets(t, err.Error(), o)
			}
		})
	}
	o := testOptions(t)
	o.PublicDir = t.TempDir()
	if _, err := newExample(o); err == nil {
		t.Fatal("missing built assets accepted")
	}
}

func TestEnvironmentLoading(t *testing.T) {
	t.Parallel()
	o := testOptions(t)
	users, _ := json.Marshal(o.Users)
	keys, _ := json.Marshal(o.PublisherKeys)
	values := map[string]string{"EXAMPLE_USERS_JSON": string(users), "PUBLISHER_KEYS_JSON": string(keys), "CLIENT_TOKEN_SECRET": o.SigningSecret, "EXAMPLE_TOKEN_TTL": "30s", "SOCKETFABRIC_TIMEOUT": "2s"}
	lookup := func(name string) (string, bool) { value, ok := values[name]; return value, ok }
	loaded, err := loadExampleOptions(lookup)
	if err != nil || loaded.TokenTTL != 30*time.Second || loaded.RequestTimeout != 2*time.Second || loaded.Address != "127.0.0.1:3000" {
		t.Fatal("ENV configuration/defaults failed")
	}
	values["EXAMPLE_TOKEN_TTL"] = o.SigningSecret
	if _, err := loadExampleOptions(lookup); err == nil {
		t.Fatal("invalid duration accepted")
	} else {
		assertNoSecrets(t, err.Error(), o)
	}
	values["EXAMPLE_TOKEN_TTL"] = "30s"
	values["EXAMPLE_USERS_JSON"] = `{"alice":{"password":"` + o.SigningSecret + `","projectId":"project-a","rooms":["room-a"],"role":"admin"}}`
	if _, err := loadExampleOptions(lookup); err == nil {
		t.Fatal("unknown configuration field accepted")
	} else {
		assertNoSecrets(t, err.Error(), o)
	}
}

func TestHTTPServerConcurrentRequestsAndGracefulShutdown(t *testing.T) {
	t.Parallel()
	o := testOptions(t)
	app := testApplication(t, o)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	app.origin.Host = listener.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- app.run(ctx, listener) }()
	var requests sync.WaitGroup
	for range 12 {
		requests.Go(func() {
			client := &http.Client{Timeout: time.Second}
			request, _ := http.NewRequest("POST", "http://"+listener.Addr().String()+"/api/realtime-token", nil)
			request.SetBasicAuth("alice", o.Users["alice"].Password)
			response, err := client.Do(request)
			if err != nil {
				t.Error("concurrent token request failed")
				return
			}
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode != 200 {
				t.Errorf("concurrent token request: HTTP %d", response.StatusCode)
			}
		})
	}
	requests.Wait()
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("example did not shut down")
	}
}
