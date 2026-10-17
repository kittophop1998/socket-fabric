package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func lookup(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := values[name]
		if !ok {
			switch name {
			case "CLIENT_TOKEN_SECRET":
				return strings.Repeat("s", 32), true
			case "PUBLISHER_KEYS_JSON":
				return `{"project-a":"` + strings.Repeat("k", 32) + `"}`, true
			}
		}
		return v, ok
	}
}

func expectedAuth() Auth {
	return Auth{ClientTokenSecret: strings.Repeat("s", 32), PublisherKeys: map[string]string{"project-a": strings.Repeat("k", 32)}}
}

func expectedWebSocket() WebSocket {
	return WebSocket{QueueCapacity: 32, MaxConnections: 256, MaxMessageBytes: 16384,
		AuthTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, PingInterval: 30 * time.Second, PongTimeout: 10 * time.Second}
}

func TestDefaults(t *testing.T) {
	cfg, err := load(lookup(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Mode:        "single",
		Environment: "development", Host: "0.0.0.0", Port: 8080, LogLevel: slog.LevelInfo,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: 15 * time.Second, IdleTimeout: time.Minute, ShutdownTimeout: 10 * time.Second,
		Auth: expectedAuth(), WebSocket: expectedWebSocket(),
	}
	if !reflect.DeepEqual(cfg, want) || cfg.Address() != "0.0.0.0:8080" {
		t.Fatalf("config = %+v, want %+v", cfg, want)
	}
}

func TestOverrides(t *testing.T) {
	cfg, err := load(lookup(map[string]string{
		"SOCKETFABRIC_MODE": "single", "NODE_ID": "gateway-a",
		"APP_ENV": "production", "HTTP_HOST": "::1", "HTTP_PORT": "9090", "LOG_LEVEL": "debug",
		"HTTP_READ_HEADER_TIMEOUT": "2s", "HTTP_READ_TIMEOUT": "3s", "HTTP_WRITE_TIMEOUT": "4s",
		"HTTP_IDLE_TIMEOUT": "5s", "SHUTDOWN_TIMEOUT": "6s",
		"WS_QUEUE_CAPACITY": "64", "WS_MAX_CONNECTIONS": "1000", "WS_MAX_MESSAGE_BYTES": "32000",
		"WS_AUTH_TIMEOUT": "3s", "WS_WRITE_TIMEOUT": "4s", "WS_PING_INTERVAL": "20s", "WS_PONG_TIMEOUT": "7s", "WS_IDLE_TIMEOUT": "2m",
		"WS_ALLOWED_ORIGINS": "https://APP.example.com,http://localhost:3000", "DEMO_ENABLED": "true",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Mode: "single", NodeID: "gateway-a",
		Environment: "production", Host: "::1", Port: 9090, LogLevel: slog.LevelDebug,
		ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 3 * time.Second,
		WriteTimeout: 4 * time.Second, IdleTimeout: 5 * time.Second, ShutdownTimeout: 6 * time.Second,
		Auth: expectedAuth(), DemoEnabled: true,
		WebSocket: WebSocket{QueueCapacity: 64, MaxConnections: 1000, MaxMessageBytes: 32000,
			AuthTimeout: 3 * time.Second, WriteTimeout: 4 * time.Second, PingInterval: 20 * time.Second, PongTimeout: 7 * time.Second, IdleTimeout: 2 * time.Minute,
			AllowedOrigins: []string{"https://app.example.com", "http://localhost:3000"}},
	}
	if !reflect.DeepEqual(cfg, want) || cfg.Address() != "[::1]:9090" {
		t.Fatalf("config = %+v, want %+v", cfg, want)
	}
}

func TestInvalidConfiguration(t *testing.T) {
	fields := []string{"APP_ENV", "HTTP_HOST", "HTTP_PORT", "LOG_LEVEL", "HTTP_READ_HEADER_TIMEOUT", "HTTP_READ_TIMEOUT", "HTTP_WRITE_TIMEOUT", "HTTP_IDLE_TIMEOUT", "SHUTDOWN_TIMEOUT"}
	for _, field := range fields {
		t.Run(field+"/empty", func(t *testing.T) {
			_, err := load(lookup(map[string]string{field: ""}))
			if err == nil || !strings.Contains(err.Error(), field) {
				t.Fatalf("expected an error identifying %s, got %v", field, err)
			}
		})
	}
	for _, tc := range []struct{ field, value string }{
		{"APP_ENV", "staging"}, {"LOG_LEVEL", "trace"}, {"HTTP_HOST", "127.0.0.1:8080"},
		{"HTTP_HOST", "https://localhost"}, {"HTTP_HOST", "bad host"}, {"HTTP_HOST", "-bad.example"},
		{"HTTP_PORT", "0"}, {"HTTP_PORT", "65536"}, {"HTTP_PORT", "-1"}, {"HTTP_PORT", "abc"},
	} {
		t.Run(tc.field+"/"+tc.value, func(t *testing.T) {
			_, err := load(lookup(map[string]string{tc.field: tc.value}))
			if err == nil || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("expected an error identifying %s, got %v", tc.field, err)
			}
		})
	}
	for _, field := range fields[4:] {
		for _, value := range []string{"0s", "-1s", "10", "invalid-sensitive-value"} {
			t.Run(field+"/"+value, func(t *testing.T) {
				_, err := load(lookup(map[string]string{field: value}))
				if err == nil || !strings.Contains(err.Error(), field) || strings.Contains(err.Error(), value) {
					t.Fatalf("expected sanitized error identifying %s, got %v", field, err)
				}
			})
		}
	}
}

func TestAcceptedEnvironmentsHostsAndLevels(t *testing.T) {
	for _, environment := range []string{"development", "test", "production"} {
		if _, err := load(lookup(map[string]string{"APP_ENV": environment})); err != nil {
			t.Fatal(err)
		}
	}
	for _, host := range []string{"localhost", "server.internal", "server.internal.", "127.0.0.1", "::", "::1"} {
		if _, err := load(lookup(map[string]string{"HTTP_HOST": host})); err != nil {
			t.Fatal(err)
		}
	}
	for _, level := range []string{"debug", "info", "warn", "error"} {
		if _, err := load(lookup(map[string]string{"LOG_LEVEL": level})); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLoadReadsEnvironment(t *testing.T) {
	t.Setenv("HTTP_PORT", "7777")
	// Test the public environment loader without inheriting unrelated values.
	for name, value := range map[string]string{
		"SOCKETFABRIC_MODE": "single", "NODE_ID": "",
		"APP_ENV": "test", "HTTP_HOST": "localhost", "LOG_LEVEL": "info",
		"HTTP_READ_HEADER_TIMEOUT": "5s", "HTTP_READ_TIMEOUT": "15s", "HTTP_WRITE_TIMEOUT": "15s",
		"HTTP_IDLE_TIMEOUT": "60s", "SHUTDOWN_TIMEOUT": "10s",
		"CLIENT_TOKEN_SECRET": strings.Repeat("s", 32), "PUBLISHER_KEYS_JSON": `{"project-a":"` + strings.Repeat("k", 32) + `"}`,
		"WS_QUEUE_CAPACITY": "32", "WS_MAX_CONNECTIONS": "256", "WS_MAX_MESSAGE_BYTES": "16384",
		"WS_AUTH_TIMEOUT": "5s", "WS_WRITE_TIMEOUT": "5s", "WS_PING_INTERVAL": "30s", "WS_PONG_TIMEOUT": "10s", "WS_IDLE_TIMEOUT": "0s",
		"DEMO_ENABLED": "false",
	} {
		t.Setenv(name, value)
	}
	oldOrigins, hadOrigins := os.LookupEnv("WS_ALLOWED_ORIGINS")
	if err := os.Unsetenv("WS_ALLOWED_ORIGINS"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if hadOrigins {
			_ = os.Setenv("WS_ALLOWED_ORIGINS", oldOrigins)
		}
	})
	cfg, err := Load()
	if err != nil || cfg.Port != 7777 {
		t.Fatalf("Load() = %+v, %v", cfg, err)
	}
}

func TestInvalidWebSocketConfiguration(t *testing.T) {
	for _, field := range []string{"WS_QUEUE_CAPACITY", "WS_MAX_CONNECTIONS", "WS_MAX_MESSAGE_BYTES", "DEMO_ENABLED", "WS_AUTH_TIMEOUT", "WS_WRITE_TIMEOUT", "WS_PING_INTERVAL", "WS_PONG_TIMEOUT", "WS_IDLE_TIMEOUT", "WS_ALLOWED_ORIGINS"} {
		t.Run(field+"/empty", func(t *testing.T) {
			if _, err := load(lookup(map[string]string{field: ""})); err == nil || !strings.Contains(err.Error(), field) {
				t.Fatalf("expected field error, got %v", err)
			}
		})
	}
	for _, tc := range []struct{ field, value string }{
		{"WS_QUEUE_CAPACITY", "0"}, {"WS_QUEUE_CAPACITY", "1025"}, {"WS_QUEUE_CAPACITY", "-1"},
		{"WS_MAX_CONNECTIONS", "0"}, {"WS_MAX_CONNECTIONS", "10001"}, {"WS_MAX_CONNECTIONS", "9999999999999999999999"},
		{"WS_MAX_MESSAGE_BYTES", "0"}, {"WS_MAX_MESSAGE_BYTES", "1048577"}, {"DEMO_ENABLED", "1"}, {"DEMO_ENABLED", "TRUE"},
		{"WS_AUTH_TIMEOUT", "0s"}, {"WS_WRITE_TIMEOUT", "-1s"}, {"WS_PING_INTERVAL", "abc"}, {"WS_PONG_TIMEOUT", "10"},
	} {
		t.Run(tc.field+"/"+tc.value, func(t *testing.T) {
			if _, err := load(lookup(map[string]string{tc.field: tc.value})); err == nil || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("expected field error, got %v", err)
			}
		})
	}
	for _, tc := range []struct{ field, value string }{
		{"WS_QUEUE_CAPACITY", "1024"}, {"WS_MAX_CONNECTIONS", "10000"}, {"WS_MAX_MESSAGE_BYTES", "1048576"},
	} {
		if _, err := load(lookup(map[string]string{tc.field: tc.value})); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWebSocketIdleTimeout(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values map[string]string
		want   time.Duration
	}{
		{name: "unset"},
		{name: "disabled", values: map[string]string{"WS_IDLE_TIMEOUT": "0s"}},
		{name: "positive", values: map[string]string{"WS_IDLE_TIMEOUT": "2m30s"}, want: 150 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := load(lookup(tc.values))
			if err != nil || cfg.WebSocket.IdleTimeout != tc.want {
				t.Fatalf("idle timeout = %v, error = %v; want %v", cfg.WebSocket.IdleTimeout, err, tc.want)
			}
		})
	}
	for _, value := range []string{"-1s", "10", "invalid-sensitive-duration"} {
		t.Run(value, func(t *testing.T) {
			_, err := load(lookup(map[string]string{"WS_IDLE_TIMEOUT": value}))
			if err == nil || !strings.Contains(err.Error(), "WS_IDLE_TIMEOUT") || strings.Contains(err.Error(), value) {
				t.Fatalf("expected sanitized idle-timeout error, got %v", err)
			}
		})
	}
}

func TestOrigins(t *testing.T) {
	for _, value := range []string{"http://localhost:3000", "https://example.com", "http://[::1]:8080", "https://APP.example.com, https://app.example.com"} {
		_, err := load(lookup(map[string]string{"WS_ALLOWED_ORIGINS": value}))
		if err != nil {
			t.Fatalf("valid origin rejected: %v", err)
		}
	}
	for _, value := range []string{"*", "https://*.example.com", "example.com", "ftp://example.com", "https://example.com/", "https://example.com/path", "https://example.com?q=1", "https://example.com?", "https://example.com#x", "https://example.com#", "http://user:password@example.com", "http://example.com:0", "http://example.com:65536", "http://example.com:", "https://example.com,", "https://example.com,,http://localhost", " "} {
		t.Run(value, func(t *testing.T) {
			_, err := load(lookup(map[string]string{"WS_ALLOWED_ORIGINS": value}))
			if err == nil || !strings.Contains(err.Error(), "WS_ALLOWED_ORIGINS") || strings.Contains(err.Error(), "password") {
				t.Fatalf("expected sanitized origin error, got %v", err)
			}
		})
	}
}

func TestAuthConfiguration(t *testing.T) {
	secret, key := strings.Repeat("s", 32), strings.Repeat("k", 32)
	for _, values := range []map[string]string{
		nil, {"CLIENT_TOKEN_SECRET": secret}, {"PUBLISHER_KEYS_JSON": `{"project-a":"` + key + `"}`},
		{"CLIENT_TOKEN_SECRET": "too-short", "PUBLISHER_KEYS_JSON": `{"project-a":"` + key + `"}`},
	} {
		_, err := loadAuth(func(name string) (string, bool) { value, ok := values[name]; return value, ok })
		if err == nil {
			t.Fatal("missing or short credentials must be rejected")
		}
	}
	for _, raw := range []string{"", "{}", "null", "[]", "malformed-sensitive-value", `{"project-a":null}`, `{"project-a":123}`, `{"project-a":"short"}`, `{"bad project":"` + key + `"}`, `{"project-a":"` + key + `","project-b":"` + key + `"}`, `{"project-a":"` + secret + `"}`, `{"project-a":"` + key + `","project-a":"` + strings.Repeat("z", 32) + `"}`, `{"project-a":"` + key + `"} {}`} {
		t.Run(raw, func(t *testing.T) {
			_, err := loadAuth(lookup(map[string]string{"PUBLISHER_KEYS_JSON": raw}))
			if err == nil || !strings.Contains(err.Error(), "PUBLISHER_KEYS_JSON") || strings.Contains(err.Error(), key) || strings.Contains(err.Error(), secret) {
				t.Fatalf("expected sanitized auth error, got %v", err)
			}
		})
	}
	auth, err := loadAuth(lookup(map[string]string{"PUBLISHER_KEYS_JSON": `{"project-a":"` + key + `","project-b":"` + strings.Repeat("z", 32) + `"}`}))
	if err != nil || len(auth.PublisherKeys) != 2 {
		t.Fatalf("valid auth config rejected: %v", err)
	}
}

func TestLoadAuthDoesNotReadHTTPConfiguration(t *testing.T) {
	t.Setenv("CLIENT_TOKEN_SECRET", strings.Repeat("s", 32))
	t.Setenv("PUBLISHER_KEYS_JSON", `{"project-a":"`+strings.Repeat("k", 32)+`"}`)
	t.Setenv("HTTP_PORT", "invalid")
	t.Setenv("SOCKETFABRIC_MODE", "invalid")
	t.Setenv("NATS_TOKEN", "")
	auth, err := LoadAuth()
	if err != nil || !reflect.DeepEqual(auth, expectedAuth()) {
		t.Fatalf("standalone auth loader: %v", err)
	}
}

func TestDistributedDefaultsAndOverrides(t *testing.T) {
	cfg, err := load(lookup(map[string]string{"SOCKETFABRIC_MODE": "distributed", "NATS_TOKEN": strings.Repeat("n", 32)}))
	if err != nil {
		t.Fatal(err)
	}
	want := NATS{URL: "nats://localhost:4222", Token: strings.Repeat("n", 32), SubjectPrefix: "socketfabric", ConnectTimeout: 5 * time.Second, PublishTimeout: 2 * time.Second}
	if cfg.Mode != "distributed" || cfg.NATS != want || cfg.NodeID != "" {
		t.Fatal("distributed defaults differ from their contract")
	}
	cfg, err = load(lookup(map[string]string{
		"SOCKETFABRIC_MODE": "distributed", "NODE_ID": "gateway-a", "NATS_TOKEN": strings.Repeat("n", 32),
		"NATS_URL": "tls://nats.internal:4443", "NATS_SUBJECT_PREFIX": "sf.events_v2-prod", "NATS_CONNECT_TIMEOUT": "3s", "NATS_PUBLISH_TIMEOUT": "1s",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want = NATS{URL: "tls://nats.internal:4443", Token: strings.Repeat("n", 32), SubjectPrefix: "sf.events_v2-prod", ConnectTimeout: 3 * time.Second, PublishTimeout: time.Second}
	if cfg.NodeID != "gateway-a" || cfg.NATS != want {
		t.Fatal("distributed overrides differ from their contract")
	}
}

func TestSingleModeIgnoresNATSEnvironment(t *testing.T) {
	cfg, err := load(lookup(map[string]string{
		"NATS_URL": "sensitive-invalid-value", "NATS_TOKEN": "", "NATS_SUBJECT_PREFIX": "*",
		"NATS_CONNECT_TIMEOUT": "", "NATS_PUBLISH_TIMEOUT": "-1s",
	}))
	if err != nil || cfg.Mode != "single" || cfg.NATS != (NATS{}) {
		t.Fatalf("single mode must ignore all NATS configuration: %v", err)
	}
}

func TestModeAndNodeIDValidation(t *testing.T) {
	for _, mode := range []string{"", "cluster", "SINGLE"} {
		if _, err := load(lookup(map[string]string{"SOCKETFABRIC_MODE": mode})); err == nil || !strings.Contains(err.Error(), "SOCKETFABRIC_MODE") {
			t.Fatalf("invalid mode accepted: %v", err)
		}
	}
	for _, nodeID := range []string{"", "gateway-a", "a.b_c:1-2", strings.Repeat("a", 64)} {
		if _, err := load(lookup(map[string]string{"NODE_ID": nodeID})); err != nil {
			t.Fatalf("valid node prefix rejected: %v", err)
		}
	}
	for _, nodeID := range []string{"node id", "node*", ">", "ไทย", strings.Repeat("a", 65)} {
		if _, err := load(lookup(map[string]string{"NODE_ID": nodeID})); err == nil || !strings.Contains(err.Error(), "NODE_ID") {
			t.Fatalf("invalid node prefix accepted: %v", err)
		}
	}
}

func TestDistributedNATSValidation(t *testing.T) {
	for _, field := range []string{"NATS_URL", "NATS_TOKEN", "NATS_SUBJECT_PREFIX", "NATS_CONNECT_TIMEOUT", "NATS_PUBLISH_TIMEOUT"} {
		t.Run(field+"/empty", func(t *testing.T) {
			values := map[string]string{"SOCKETFABRIC_MODE": "distributed", "NATS_TOKEN": strings.Repeat("n", 32), field: ""}
			if _, err := load(lookup(values)); err == nil || !strings.Contains(err.Error(), field) {
				t.Fatalf("expected field error, got %v", err)
			}
		})
	}
	for _, tc := range []struct{ field, value string }{
		{"NATS_TOKEN", "short-sensitive-key"}, {"NATS_TOKEN", strings.Repeat("s", 32)}, {"NATS_TOKEN", strings.Repeat("k", 32)},
		{"NATS_URL", "http://localhost:4222"}, {"NATS_URL", "nats://localhost"}, {"NATS_URL", "nats://localhost:0"}, {"NATS_URL", "nats://localhost:65536"},
		{"NATS_URL", "nats://localhost:4222/"}, {"NATS_URL", "nats://localhost:4222/path"}, {"NATS_URL", "nats://localhost:4222?"}, {"NATS_URL", "nats://localhost:4222?q=1"},
		{"NATS_URL", "nats://user:sensitive-password@localhost:4222"}, {"NATS_URL", "nats://sensitive-token@localhost:4222"}, {"NATS_URL", "nats://localhost:4222#"}, {"NATS_URL", "nats://localhost:4222#fragment"},
		{"NATS_URL", "nats://localhost:4222,nats://other:4222"}, {"NATS_URL", "nats://-badhost:4222"},
		{"NATS_SUBJECT_PREFIX", "*"}, {"NATS_SUBJECT_PREFIX", ">"}, {"NATS_SUBJECT_PREFIX", ".socketfabric"}, {"NATS_SUBJECT_PREFIX", "sf."}, {"NATS_SUBJECT_PREFIX", "sf..events"},
		{"NATS_SUBJECT_PREFIX", "sf:events"}, {"NATS_SUBJECT_PREFIX", "sf events"}, {"NATS_SUBJECT_PREFIX", strings.Repeat("a", 129)},
		{"NATS_CONNECT_TIMEOUT", "0s"}, {"NATS_CONNECT_TIMEOUT", "invalid-sensitive-duration"}, {"NATS_PUBLISH_TIMEOUT", "-1s"},
	} {
		t.Run(tc.field+"/"+tc.value, func(t *testing.T) {
			_, err := load(lookup(map[string]string{"SOCKETFABRIC_MODE": "distributed", "NATS_TOKEN": strings.Repeat("n", 32), tc.field: tc.value}))
			if err == nil || !strings.Contains(err.Error(), tc.field) || strings.Contains(err.Error(), "sensitive") || strings.Contains(err.Error(), strings.Repeat("s", 32)) || strings.Contains(err.Error(), strings.Repeat("k", 32)) {
				t.Fatalf("expected sanitized field error, got %v", err)
			}
		})
	}
	for _, brokerURL := range []string{"nats://localhost:4222", "tls://broker.internal:4443", "nats://[::1]:4222"} {
		if _, err := load(lookup(map[string]string{"SOCKETFABRIC_MODE": "distributed", "NATS_TOKEN": strings.Repeat("n", 32), "NATS_URL": brokerURL})); err != nil {
			t.Fatalf("valid NATS URL rejected: %v", err)
		}
	}
}

func TestDistributedProjectLimitOnly(t *testing.T) {
	for _, count := range []int{128, 129} {
		keys := make(map[string]string, count)
		for i := range count {
			keys[fmt.Sprintf("project-%d", i)] = fmt.Sprintf("%s-%d", strings.Repeat("k", 32), i)
		}
		raw, err := json.Marshal(keys)
		if err != nil {
			t.Fatal(err)
		}
		for _, mode := range []string{"single", "distributed"} {
			_, err := load(lookup(map[string]string{"SOCKETFABRIC_MODE": mode, "NATS_TOKEN": strings.Repeat("n", 32), "PUBLISHER_KEYS_JSON": string(raw)}))
			if mode == "distributed" && count > 128 {
				if err == nil || !strings.Contains(err.Error(), "PUBLISHER_KEYS_JSON") {
					t.Fatalf("distributed project limit not enforced: %v", err)
				}
			} else if err != nil {
				t.Fatalf("allowed project configuration rejected: %v", err)
			}
		}
		if _, err := loadAuth(lookup(map[string]string{"PUBLISHER_KEYS_JSON": string(raw)})); err != nil {
			t.Fatalf("standalone token CLI auth loading must remain mode-independent: %v", err)
		}
	}
}
