//go:build contracttests

package httpadapter

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/goccy/go-yaml"
	"socketfabric/internal/testsupport/contracts"
)

func TestRegisteredRoutesMatchOpenAPI(t *testing.T) {
	root := repositoryRoot(t)
	body, err := os.ReadFile(filepath.Join(root, "api", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := yaml.Unmarshal(body, &document); err != nil {
		t.Fatalf("parse OpenAPI: %v", err)
	}
	paths, ok := document["paths"].(map[string]any)
	if !ok {
		t.Fatal("OpenAPI paths missing")
	}
	assertSecurityContract(t, paths)

	for _, mode := range []struct {
		name       string
		management bool
		demo       bool
	}{
		{name: "gateway only"},
		{name: "gateway and demo", demo: true},
		{name: "gateway and management", management: true},
		{name: "all enabled", management: true, demo: true},
	} {
		previousMode := gin.Mode()
		gin.SetMode(gin.ReleaseMode)
		t.Run(mode.name, func(t *testing.T) {
			t.Cleanup(func() { gin.SetMode(previousMode) })
			gateway := &Gateway{options: GatewayOptions{DemoEnabled: mode.demo}}
			var handlers []*ManagementHandler
			if mode.management {
				handlers = append(handlers, &ManagementHandler{})
			}
			router, err := NewRouter(slog.New(slog.NewJSONHandler(io.Discard, nil)), func() bool { return true }, gateway, handlers...)
			if err != nil {
				t.Fatal(err)
			}

			actual := make([]string, 0, len(router.Routes()))
			for _, route := range router.Routes() {
				actual = append(actual, route.Method+" "+normalizeGinPath(route.Path))
			}
			sort.Strings(actual)
			expected := make([]string, 0)
			operationIDs := map[string]struct{}{}
			for path, rawItem := range paths {
				item, ok := rawItem.(map[string]any)
				if !ok {
					t.Fatalf("invalid path item %s", path)
				}
				for method, rawOperation := range item {
					if !isHTTPMethod(method) {
						continue
					}
					operation, ok := rawOperation.(map[string]any)
					if !ok {
						t.Fatalf("invalid operation %s %s", method, path)
					}
					feature, _ := operation["x-socketfabric-feature"].(string)
					if feature == "management" && !mode.management || feature == "demo" && !mode.demo {
						continue
					}
					id, _ := operation["operationId"].(string)
					if id == "" {
						t.Errorf("missing operationId for %s %s", method, path)
					} else if _, found := operationIDs[id]; found {
						t.Errorf("duplicate operationId %q", id)
					} else {
						operationIDs[id] = struct{}{}
					}
					expected = append(expected, strings.ToUpper(method)+" "+path)
				}
			}
			sort.Strings(expected)
			if !reflect.DeepEqual(actual, expected) {
				t.Fatalf("registered routes differ from OpenAPI\nactual:   %v\nexpected: %v", actual, expected)
			}
		})
	}
}

func assertSecurityContract(t *testing.T, paths map[string]any) {
	t.Helper()
	for _, endpoint := range []struct {
		path, method string
		security     string
		roles        []string
	}{
		{path: "/healthz", method: "get"},
		{path: "/readyz", method: "get"},
		{path: "/ws", method: "get"},
		{path: "/admin/v1/auth/login", method: "post"},
		{path: "/v1/events", method: "post", security: "PublisherKey"},
		{path: "/admin/v1/auth/logout", method: "post", security: "OperatorSession", roles: []string{"admin", "viewer"}},
		{path: "/admin/v1/auth/me", method: "get", security: "OperatorSession", roles: []string{"admin", "viewer"}},
		{path: "/admin/v1/projects", method: "get", security: "OperatorSession", roles: []string{"admin", "viewer"}},
		{path: "/admin/v1/projects", method: "post", security: "OperatorSession", roles: []string{"admin"}},
		{path: "/admin/v1/projects/{id}", method: "get", security: "OperatorSession", roles: []string{"admin", "viewer"}},
		{path: "/admin/v1/projects/{id}", method: "patch", security: "OperatorSession", roles: []string{"admin"}},
		{path: "/admin/v1/projects/{id}/keys", method: "get", security: "OperatorSession", roles: []string{"admin"}},
		{path: "/admin/v1/projects/{id}/keys", method: "post", security: "OperatorSession", roles: []string{"admin"}},
		{path: "/admin/v1/projects/{id}/keys/{keyId}", method: "delete", security: "OperatorSession", roles: []string{"admin"}},
		{path: "/admin/v1/diagnostics/overview", method: "get", security: "OperatorSession", roles: []string{"admin", "viewer"}},
		{path: "/admin/v1/diagnostics/nodes", method: "get", security: "OperatorSession", roles: []string{"admin", "viewer"}},
		{path: "/admin/v1/diagnostics/metrics", method: "get", security: "OperatorSession", roles: []string{"admin", "viewer"}},
		{path: "/admin/v1/audit-logs", method: "get", security: "OperatorSession", roles: []string{"admin"}},
	} {
		item := paths[endpoint.path].(map[string]any)
		operation := item[endpoint.method].(map[string]any)
		security, _ := operation["security"].([]any)
		if endpoint.security == "" {
			if len(security) != 0 {
				t.Errorf("%s %s should be public but declares security", endpoint.method, endpoint.path)
			}
		} else if len(security) != 1 || !reflect.DeepEqual(security[0], map[string]any{endpoint.security: []any{}}) {
			t.Errorf("%s %s has incorrect security requirement", endpoint.method, endpoint.path)
		}
		if len(endpoint.roles) == 0 {
			if _, found := operation["x-socketfabric-roles"]; found {
				t.Errorf("%s %s should not declare operator roles", endpoint.method, endpoint.path)
			}
			continue
		}
		var roles []string
		for _, role := range operation["x-socketfabric-roles"].([]any) {
			roles = append(roles, role.(string))
		}
		if !reflect.DeepEqual(roles, endpoint.roles) {
			t.Errorf("%s %s roles = %v, want %v", endpoint.method, endpoint.path, roles, endpoint.roles)
		}
	}
}

func TestLiveHTTPAndWebSocketPayloadsMatchContracts(t *testing.T) {
	root := repositoryRoot(t)
	validator, err := contracts.New(filepath.Join(root, "api"))
	if err != nil {
		t.Fatalf("compile public contracts: %v", err)
	}
	f := newGatewayFixture(t, nil)
	handshakeResponse, err := (&http.Client{Timeout: 2 * time.Second}).Get(f.server.URL + "/ws")
	if err != nil {
		t.Fatal(err)
	}
	handshakeBody, err := io.ReadAll(handshakeResponse.Body)
	handshakeResponse.Body.Close()
	if err != nil || handshakeResponse.StatusCode != http.StatusUpgradeRequired {
		t.Fatal("non-upgrade handshake status changed")
	}
	if err := validator.ValidateJSON("ErrorResponse", handshakeBody); err != nil {
		t.Fatalf("HTTP handshake error violates OpenAPI: %v", err)
	}
	conn := f.dial(t, nil)
	wsWrite(t, conn, command{Op: "auth", Token: f.token(t, "project-a", "contract-user", []string{"room-contract"}, time.Minute)})
	assertWireFrame(t, conn, validator, "WSAuthenticated")
	wsWrite(t, conn, command{Op: "join", RoomID: "room-contract"})
	assertWireFrame(t, conn, validator, "WSJoined")

	status, response := f.post(t, f.keyA, `{"projectId":"project-a","type":"contract.test","roomId":"room-contract","data":{"ok":true}}`, "application/json")
	if status != http.StatusAccepted {
		t.Fatalf("publish status = %d", status)
	}
	if err := validator.ValidateJSON("PublishResult", response); err != nil {
		t.Fatalf("live publish response violates OpenAPI: %v", err)
	}
	assertWireFrame(t, conn, validator, "WSEvent")
	status, response = f.post(t, "", `{"projectId":"project-a","type":"contract.test","roomId":"room-contract","data":null}`, "application/json")
	if status != http.StatusUnauthorized {
		t.Fatalf("unauthorized publish status = %d", status)
	}
	if err := validator.ValidateJSON("ErrorResponse", response); err != nil {
		t.Fatalf("live publish error violates OpenAPI: %v", err)
	}
}

func assertWireFrame(t *testing.T, conn *websocket.Conn, validator *contracts.Validator, schema string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	kind, body, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read %s frame: %v", schema, err)
	}
	if kind != websocket.MessageText {
		t.Fatalf("%s frame used non-text message", schema)
	}
	if err := validator.ValidateJSON(schema, body); err != nil {
		t.Fatalf("live WebSocket frame violates AsyncAPI: %v", err)
	}
}

func normalizeGinPath(path string) string {
	segments := strings.Split(path, "/")
	for index, segment := range segments {
		if strings.HasPrefix(segment, ":") {
			segments[index] = "{" + strings.TrimPrefix(segment, ":") + "}"
		}
	}
	return strings.Join(segments, "/")
}

func isHTTPMethod(method string) bool {
	switch strings.ToLower(method) {
	case "get", "put", "post", "delete", "options", "head", "patch", "trace":
		return true
	default:
		return false
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate contract test source")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../../../../"))
	if _, err := os.Stat(filepath.Join(root, "api", "openapi.yaml")); err != nil {
		t.Fatalf("cannot locate repository contracts: %v", err)
	}
	return root
}
