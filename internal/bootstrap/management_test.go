//go:build integration && postgres

package bootstrap

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/nats-io/nats.go"

	"socketfabric/internal/adapters/outbound/auth"
	"socketfabric/internal/adapters/outbound/postgres"
	"socketfabric/internal/application/usecase"
	"socketfabric/internal/config"
	"socketfabric/internal/domain"
	contractschema "socketfabric/internal/testsupport/contracts"
	"socketfabric/internal/testsupport/natsserver"
	"socketfabric/internal/testsupport/postgresdb"
	sdk "socketfabric/sdk/go"
)

type managementLogs struct {
	mu   sync.Mutex
	body bytes.Buffer
}

func (l *managementLogs) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.body.Write(b)
}
func (l *managementLogs) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.body.String() }

type managementNode struct {
	server *Server
	url    string
	logs   *managementLogs
}

func managementConfig(t *testing.T, dsn string) config.Config {
	cfg := testServer(t).config
	cfg.Environment = "test"
	cfg.ShutdownTimeout = 2 * time.Second
	cfg.WebSocket.WriteTimeout = 200 * time.Millisecond
	cfg.WebSocket.AuthTimeout = 2 * time.Second
	cfg.Management = config.Management{Enabled: true, DatabaseURL: dsn, MaxConns: 8, ConnectTimeout: 300 * time.Millisecond, QueryTimeout: 500 * time.Millisecond, SessionTTL: time.Minute, MaxProjects: 128, PolicyRefresh: 100 * time.Millisecond, PolicyMaxAge: 1500 * time.Millisecond, AllowInsecureHTTP: true}
	return cfg
}
func startManagementNode(t *testing.T, cfg config.Config) *managementNode {
	t.Helper()
	logs := &managementLogs{}
	server, err := NewServer(cfg, slog.New(slog.NewJSONHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	url, cancel, result := startServer(t, server)
	t.Cleanup(func() {
		cancel()
		if err := awaitResult(t, result); err != nil {
			t.Error(err)
		}
	})
	return &managementNode{server: server, url: url, logs: logs}
}
func provisionManagement(t *testing.T, store *postgres.Store, reserved []string) (*usecase.Management, string) {
	t.Helper()
	if err := store.Migrate(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	service, err := usecase.NewManagement(store, auth.NewPasswords(), nil, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Minute, 128, reserved)
	if err != nil {
		t.Fatal(err)
	}
	password := "test-" + rand.Text()
	if _, err := service.ProvisionOperator(context.Background(), "admin@example.com", password, domain.RoleAdmin, true); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ProvisionOperator(context.Background(), "viewer@example.com", password, domain.RoleViewer, false); err != nil {
		t.Fatal(err)
	}
	return service, password
}
func managementRequest(t *testing.T, url, method, path, token string, input any, want int) []byte {
	t.Helper()
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, url+path, body)
	if err != nil {
		t.Fatal(err)
	}
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := (&http.Client{Timeout: 4 * time.Second}).Do(req)
	if err != nil {
		t.Fatal("management HTTP request failed")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != want {
		t.Fatalf("%s %s status=%d want=%d", method, path, response.StatusCode, want)
	}
	if strings.HasPrefix(path, "/admin/") && (response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("X-Request-ID") == "") {
		t.Fatal("management response missing security/correlation headers")
	}
	return data
}
func managementLogin(t *testing.T, node *managementNode, email, password string) string {
	t.Helper()
	waitHTTPStatus(t, node.url+"/readyz", http.StatusOK)
	waitManagementPolicy(t, node.server, "")
	var result usecase.LoginResult
	body := managementRequest(t, node.url, "POST", "/admin/v1/auth/login", "", map[string]string{"email": email, "password": password}, 200)
	if json.Unmarshal(body, &result) != nil || result.Token == "" {
		t.Fatal("login result invalid")
	}
	return result.Token
}
func waitManagementPolicy(t *testing.T, s *Server, project string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if project == "" && s.policy.Ready() || project != "" && s.policy.CheckProject(context.Background(), project) == nil {
			return
		}
		select {
		case <-deadline:
			t.Fatal("management policy did not converge")
		case <-ticker.C:
		}
	}
}
func managementCreate(t *testing.T, node *managementNode, token, project string) usecase.CreateKeyResult {
	t.Helper()
	managementRequest(t, node.url, "POST", "/admin/v1/projects", token, map[string]string{"id": project, "name": "Managed project"}, 201)
	var result usecase.CreateKeyResult
	body := managementRequest(t, node.url, "POST", "/admin/v1/projects/"+project+"/keys", token, map[string]any{}, 201)
	if json.Unmarshal(body, &result) != nil || result.Key == "" {
		t.Fatal("key result invalid")
	}
	return result
}
func managementConnect(t *testing.T, node *managementNode, project, room, user string) *websocket.Conn {
	t.Helper()
	waitManagementPolicy(t, node.server, project)
	signer, err := sdk.NewTokenSigner([]byte(node.server.config.Auth.ClientTokenSecret))
	if err != nil {
		t.Fatal(err)
	}
	token, err := signer.IssueToken(sdk.ClientTokenInput{ProjectID: project, UserID: user, Rooms: []string{room}, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	conn := dialBootstrapWebSocket(t, node.url)
	writeBootstrapCommand(t, conn, map[string]string{"op": "auth", "token": token})
	readBootstrapOperation(t, conn, "authenticated")
	writeBootstrapCommand(t, conn, map[string]string{"op": "join", "roomId": room})
	readBootstrapOperation(t, conn, "joined")
	return conn
}
func managementPublish(t *testing.T, node *managementNode, key, project, room, eventType string) sdk.PublishResult {
	t.Helper()
	client, err := sdk.NewClient(sdk.ClientOptions{BaseURL: node.url, ProjectID: project, PublisherKey: key, RequestTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Publish(context.Background(), sdk.PublishInput{RoomID: room, Type: eventType, Data: map[string]string{"status": "completed"}})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func managementEvent(t *testing.T, conn *websocket.Conn, id string) {
	t.Helper()
	frame := distributedRead(t, conn)
	if frame.Op != "event" || frame.Event.EventID != id {
		t.Fatal("event ordering/isolation/duplicate mismatch")
	}
}
func managementClose(t *testing.T, conn *websocket.Conn, want websocket.StatusCode) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := conn.Read(ctx)
	if websocket.CloseStatus(err) != want {
		t.Fatalf("close status=%d want=%d", websocket.CloseStatus(err), want)
	}
}

func TestManagementAPIAuthRBACAndDiagnostics(t *testing.T) {
	contracts := managementContractValidator(t)
	dsn, store := postgresdb.New(t)
	_, password := provisionManagement(t, store, []string{"project-a"})
	node := startManagementNode(t, managementConfig(t, dsn))
	waitHTTPStatus(t, node.url+"/readyz", http.StatusOK)
	waitManagementPolicy(t, node.server, "")
	loginBody := managementRequest(t, node.url, "POST", "/admin/v1/auth/login", "", map[string]string{"email": "admin@example.com", "password": password}, 200)
	assertManagementContract(t, contracts, "LoginResult", loginBody)
	var login usecase.LoginResult
	if json.Unmarshal(loginBody, &login) != nil || login.Token == "" {
		t.Fatal("login result invalid")
	}
	admin := login.Token
	viewer := managementLogin(t, node, "viewer@example.com", password)
	assertManagementContract(t, contracts, "ErrorResponse", managementRequest(t, node.url, "GET", "/admin/v1/projects", "", nil, 401))
	for _, test := range []struct {
		body, contentType string
		status            int
	}{{`{"id":"x","name":"x","unexpected":true}`, "application/json", 400}, {`{`, "application/json", 400}, {`{}`, "text/plain", 415}, {`{"id":"x","name":"` + strings.Repeat("x", 17<<10) + `"}`, "application/json", 413}} {
		req, _ := http.NewRequest("POST", node.url+"/admin/v1/projects", strings.NewReader(test.body))
		req.Header.Set("Authorization", "Bearer "+admin)
		req.Header.Set("Content-Type", test.contentType)
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal("management malformed request failed")
		}
		response.Body.Close()
		if response.StatusCode != test.status {
			t.Fatal("management body/content-type limit")
		}
	}
	managementRequest(t, node.url, "GET", "/admin/v1/projects", "", nil, 401)
	managementRequest(t, node.url, "GET", "/admin/v1/projects", node.server.config.Auth.PublisherKeys["project-a"], nil, 401)
	managementRequest(t, node.url, "POST", "/admin/v1/projects", viewer, map[string]string{"id": "denied", "name": "Denied"}, 403)
	managementRequest(t, node.url, "GET", "/admin/v1/audit-logs", viewer, nil, 403)
	managementRequest(t, node.url, "POST", "/admin/v1/projects", admin, map[string]string{"id": "project-a", "name": "ENV collision"}, 409)
	managementRequest(t, node.url, "POST", "/admin/v1/projects", admin, map[string]string{"id": "bad project", "name": "Invalid"}, 400)
	key := managementCreateWithContract(t, node, admin, "managed-a", contracts)
	managementRequest(t, node.url, "POST", "/admin/v1/projects", admin, map[string]string{"id": "managed-a", "name": "Duplicate"}, 409)
	managementRequest(t, node.url, "GET", "/admin/v1/projects?status=enabled&name=Managed&limit=1", viewer, nil, 200)
	managementRequest(t, node.url, "GET", "/admin/v1/projects?limit=1000", viewer, nil, 400)
	managementRequest(t, node.url, "GET", "/admin/v1/projects?unsupported=true", viewer, nil, 400)
	projectDetail := managementRequest(t, node.url, "GET", "/admin/v1/projects/managed-a", viewer, nil, 200)
	assertManagementContract(t, contracts, "Project", projectDetail)
	managementRequest(t, node.url, "PATCH", "/admin/v1/projects/managed-a", viewer, map[string]string{"status": "disabled"}, 403)
	managementRequest(t, node.url, "GET", "/admin/v1/projects/managed-a/keys", viewer, nil, 403)
	keys := managementRequest(t, node.url, "GET", "/admin/v1/projects/managed-a/keys", admin, nil, 200)
	var keyPage struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(keys, &keyPage); err != nil || len(keyPage.Items) != 1 {
		t.Fatal("credential list response invalid")
	}
	assertManagementContract(t, contracts, "ProjectCredential", keyPage.Items[0])
	if bytes.Contains(keys, []byte(key.Key)) || bytes.Contains(keys, []byte("secretHash")) {
		t.Fatal("key/hash leaked from metadata")
	}
	ws := managementConnect(t, node, "managed-a", "room", "user")
	result := managementPublish(t, node, key.Key, "managed-a", "room", "resource.updated")
	managementEvent(t, ws, result.Event.EventID)
	signer, _ := sdk.NewTokenSigner([]byte(node.server.config.Auth.ClientTokenSecret))
	clientToken, err := signer.IssueToken(sdk.ClientTokenInput{ProjectID: "managed-a", UserID: "user", Rooms: []string{"room"}, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	managementRequest(t, node.url, "GET", "/admin/v1/projects", clientToken, nil, 401)
	managementRequest(t, node.url, "POST", "/v1/events", clientToken, map[string]any{"projectId": "managed-a", "roomId": "room", "type": "event", "data": nil}, 401)
	wrongSocket := dialBootstrapWebSocket(t, node.url)
	writeBootstrapCommand(t, wrongSocket, map[string]string{"op": "auth", "token": admin})
	readBootstrapOperation(t, wrongSocket, "error")
	managementClose(t, wrongSocket, websocket.StatusPolicyViolation)
	managementRequest(t, node.url, "POST", "/v1/events", admin, map[string]any{"projectId": "managed-a", "roomId": "room", "type": "event", "data": nil}, 401)
	managementRequest(t, node.url, "POST", "/v1/events", key.Key, map[string]any{"projectId": "other-project", "roomId": "room", "type": "event", "data": nil}, 403)
	for _, path := range []string{"overview", "nodes", "metrics"} {
		body := managementRequest(t, node.url, "GET", "/admin/v1/diagnostics/"+path, viewer, nil, 200)
		if path == "overview" {
			assertManagementContract(t, contracts, "DiagnosticsOverview", body)
		}
		if path == "nodes" {
			assertManagementContract(t, contracts, "DiagnosticsNodes", body)
		}
		if path == "metrics" {
			assertManagementContract(t, contracts, "DiagnosticsMetrics", body)
		}
		if !bytes.Contains(body, []byte(`"scope":"local"`)) || bytes.Contains(body, []byte(key.Key)) {
			t.Fatal("diagnostic scope/secrets")
		}
	}
	managementRequest(t, node.url, "DELETE", "/admin/v1/projects/managed-a/keys/"+key.Credential.ID, admin, nil, 204)
	managementRequest(t, node.url, "POST", "/v1/events", key.Key, map[string]any{"projectId": "managed-a", "roomId": "room", "type": "event", "data": nil}, 401)
	audit := managementRequest(t, node.url, "GET", "/admin/v1/audit-logs", admin, nil, 200)
	var auditPage struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(audit, &auditPage); err != nil || len(auditPage.Items) == 0 {
		t.Fatal("audit response invalid")
	}
	for _, entry := range auditPage.Items {
		assertManagementContract(t, contracts, "AuditLog", entry)
	}
	if !bytes.Contains(audit, []byte("credential.revoked")) || bytes.Contains(audit, []byte(password)) || bytes.Contains(audit, []byte(admin)) {
		t.Fatal("audit missing/revealed secret")
	}
	managementRequest(t, node.url, "POST", "/admin/v1/auth/logout", admin, nil, 204)
	managementRequest(t, node.url, "GET", "/admin/v1/auth/me", admin, nil, 401)
	for range 5 {
		managementRequest(t, node.url, "POST", "/admin/v1/auth/login", "", map[string]string{"email": "missing@example.com", "password": "invalid"}, 401)
	}
	managementRequest(t, node.url, "POST", "/admin/v1/auth/login", "", map[string]string{"email": "missing@example.com", "password": "invalid"}, 429)
	for _, secret := range []string{admin, viewer, key.Key, password, node.server.config.Auth.ClientTokenSecret, node.server.config.Auth.PublisherKeys["project-a"]} {
		if strings.Contains(node.logs.String(), secret) {
			t.Fatal("credential entered structured logs")
		}
	}
}

func TestManagementDistributedDynamicProjectsIsolationAndDisable(t *testing.T) {
	dsn, store := postgresdb.New(t)
	_, password := provisionManagement(t, store, []string{"project-a"})
	broker := natsserver.Start(t, rand.Text()+rand.Text())
	cfg := managementConfig(t, dsn)
	cfg.Mode = "distributed"
	cfg.NATS = config.NATS{URL: broker.URL, Token: broker.Token, SubjectPrefix: "management", ConnectTimeout: time.Second, PublishTimeout: 300 * time.Millisecond}
	a := startManagementNode(t, cfg)
	b := startManagementNode(t, cfg)
	admin := managementLogin(t, a, "admin@example.com", password)
	key := managementCreate(t, a, admin, "managed-distributed")
	otherKey := managementCreate(t, a, admin, "other-project")
	first := managementConnect(t, a, "managed-distributed", "room", "same-user")
	second := managementConnect(t, b, "managed-distributed", "room", "same-user")
	otherRoom := managementConnect(t, b, "managed-distributed", "other-room", "other-user")
	otherProject := managementConnect(t, b, "other-project", "room", "other-user")
	writeBootstrapCommand(t, otherRoom, map[string]string{"op": "join", "roomId": "room"})
	readBootstrapOperation(t, otherRoom, "error")
	writeBootstrapCommand(t, first, map[string]string{"op": "join", "roomId": "room"})
	readBootstrapOperation(t, first, "joined")
	for range 2 {
		result := managementPublish(t, a, key.Key, "managed-distributed", "room", "resource.updated")
		managementEvent(t, first, result.Event.EventID)
		managementEvent(t, second, result.Event.EventID)
	}
	fence := managementPublish(t, b, key.Key, "managed-distributed", "other-room", "room.fence")
	managementEvent(t, otherRoom, fence.Event.EventID)
	fence = managementPublish(t, a, otherKey.Key, "other-project", "room", "project.fence")
	managementEvent(t, otherProject, fence.Event.EventID)
	// A trusted broker publisher still cannot mismatch subject and envelope.
	raw, err := nats.Connect(broker.URL, nats.Token(broker.Token))
	if err != nil {
		t.Fatal("test broker connection")
	}
	defer raw.Close()
	body, _ := json.Marshal(domain.Event{EventID: "malformed", ProjectID: "other-project", RoomID: "room", Type: "forbidden", Data: json.RawMessage(`null`), Timestamp: time.Now().UTC()})
	if err := raw.Publish(cfg.NATS.SubjectPrefix+".v1.events."+base64.RawURLEncoding.EncodeToString([]byte("managed-distributed")), body); err != nil {
		t.Fatal(err)
	}
	if err := raw.FlushTimeout(time.Second); err != nil {
		t.Fatal(err)
	}
	fence = managementPublish(t, a, key.Key, "managed-distributed", "room", "validation.fence")
	managementEvent(t, first, fence.Event.EventID)
	managementEvent(t, second, fence.Event.EventID)
	broker.Stop()
	waitHTTPStatus(t, a.url+"/readyz", 503)
	waitHTTPStatus(t, b.url+"/readyz", 503)
	waitHTTPStatus(t, a.url+"/healthz", 200)
	managementRequest(t, a.url, "POST", "/v1/events", key.Key, map[string]any{"projectId": "managed-distributed", "roomId": "room", "type": "offline", "data": nil}, 503)
	broker.Restart()
	waitHTTPStatus(t, a.url+"/readyz", 200)
	waitHTTPStatus(t, b.url+"/readyz", 200)
	fence = managementPublish(t, b, key.Key, "managed-distributed", "room", "recovered.fence")
	managementEvent(t, first, fence.Event.EventID)
	managementEvent(t, second, fence.Event.EventID)
	managementRequest(t, a.url, "PATCH", "/admin/v1/projects/managed-distributed", admin, map[string]string{"status": "disabled"}, 200)
	managementRequest(t, b.url, "POST", "/v1/events", key.Key, map[string]any{"projectId": "managed-distributed", "roomId": "room", "type": "disabled", "data": nil}, 403)
	managementClose(t, first, websocket.StatusPolicyViolation)
	managementClose(t, second, websocket.StatusPolicyViolation)
	managementClose(t, otherRoom, websocket.StatusPolicyViolation)
	fence = managementPublish(t, a, otherKey.Key, "other-project", "room", "unaffected.fence")
	managementEvent(t, otherProject, fence.Event.EventID)
}

func TestManagementDatabaseOutageKeepsENVAndRecovers(t *testing.T) {
	dsn, store := postgresdb.New(t)
	_, password := provisionManagement(t, store, []string{"project-a"})
	cfg := managementConfig(t, dsn)
	node := startManagementNode(t, cfg)
	admin := managementLogin(t, node, "admin@example.com", password)
	key := managementCreate(t, node, admin, "managed-outage")
	managed := managementConnect(t, node, "managed-outage", "room", "user")
	legacy := managementConnect(t, node, "project-a", "room", "legacy-user")
	postgresdb.Stop(t)
	stopped := true
	defer func() {
		if stopped {
			postgresdb.Restart(t)
		}
	}()
	managementRequest(t, node.url, "POST", "/v1/events", key.Key, map[string]any{"projectId": "managed-outage", "roomId": "room", "type": "offline", "data": nil}, 503)
	managementClose(t, managed, websocket.StatusTryAgainLater)
	waitHTTPStatus(t, node.url+"/healthz", 200)
	waitHTTPStatus(t, node.url+"/readyz", 200)
	result := managementPublish(t, node, cfg.Auth.PublisherKeys["project-a"], "project-a", "room", "legacy.unaffected")
	managementEvent(t, legacy, result.Event.EventID)
	managementRequest(t, node.url, "GET", "/admin/v1/auth/me", admin, nil, 503)
	// Starting additional gateways with a missing DB must preserve ENV startup.
	startup := startManagementNode(t, cfg)
	waitHTTPStatus(t, startup.url+"/readyz", 200)
	managedOnlyConfig := cfg
	managedOnlyConfig.Auth.PublisherKeys = map[string]string{}
	managedOnly := startManagementNode(t, managedOnlyConfig)
	waitHTTPStatus(t, managedOnly.url+"/healthz", 200)
	waitHTTPStatus(t, managedOnly.url+"/readyz", 503)
	postgresdb.Restart(t)
	stopped = false
	waitManagementPolicy(t, node.server, "managed-outage")
	waitManagementPolicy(t, startup.server, "")
	waitManagementPolicy(t, managedOnly.server, "managed-outage")
	waitHTTPStatus(t, managedOnly.url+"/readyz", 200)
	managementRequest(t, node.url, "GET", "/admin/v1/auth/me", admin, nil, 200)
	reconnected := managementConnect(t, managedOnly, "managed-outage", "room", "user")
	result = managementPublish(t, managedOnly, key.Key, "managed-outage", "room", "managed.recovered")
	managementEvent(t, reconnected, result.Event.EventID)
}

func TestManagementHTTPSRequired(t *testing.T) {
	dsn, store := postgresdb.New(t)
	_, password := provisionManagement(t, store, []string{"project-a"})
	cfg := managementConfig(t, dsn)
	cfg.Management.AllowInsecureHTTP = false
	server, err := NewServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer server.closeManagement()
	if err := server.policy.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	server.ready.Store(true)
	tls := httptest.NewTLSServer(server.http.Handler)
	defer tls.Close()
	encoded, _ := json.Marshal(map[string]string{"email": "admin@example.com", "password": password})
	response, err := tls.Client().Post(tls.URL+"/admin/v1/auth/login", "application/json", bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("direct HTTPS login failed")
	}
	plain := httptest.NewServer(server.http.Handler)
	defer plain.Close()
	req, _ := http.NewRequest("GET", plain.URL+"/admin/v1/projects", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	response, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 403 {
		t.Fatal("forged TLS forwarding header accepted")
	}
}

func managementContractValidator(t *testing.T) *contractschema.Validator {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate management contract tests")
	}
	apiDir := filepath.Clean(filepath.Join(filepath.Dir(file), "../../api"))
	validator, err := contractschema.New(apiDir)
	if err != nil {
		t.Fatalf("compile API contracts: %v", err)
	}
	return validator
}

func assertManagementContract(t *testing.T, validator *contractschema.Validator, schema string, body []byte) {
	t.Helper()
	if err := validator.ValidateJSON(schema, body); err != nil {
		t.Fatalf("management response violates %s contract: %v", schema, err)
	}
}

func managementCreateWithContract(t *testing.T, node *managementNode, token, project string, validator *contractschema.Validator) usecase.CreateKeyResult {
	t.Helper()
	managementRequest(t, node.url, "POST", "/admin/v1/projects", token, map[string]string{"id": project, "name": "Managed project"}, 201)
	body := managementRequest(t, node.url, "POST", "/admin/v1/projects/"+project+"/keys", token, map[string]any{}, 201)
	assertManagementContract(t, validator, "CreateCredentialResult", body)
	var result usecase.CreateKeyResult
	if err := json.Unmarshal(body, &result); err != nil || result.Key == "" {
		t.Fatal("credential result invalid")
	}
	return result
}
