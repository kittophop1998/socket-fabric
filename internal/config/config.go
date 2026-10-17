// Package config loads and validates the server's environment configuration.
package config

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"socketfabric/internal/domain"
)

type Config struct {
	Mode              string
	NodeID            string
	Environment       string
	Host              string
	Port              int
	LogLevel          slog.Level
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration
	Auth              Auth
	WebSocket         WebSocket
	DemoEnabled       bool
	NATS              NATS
	Management        Management
}

// NATS is populated only in distributed mode. Token is a server-only credential.
type NATS struct {
	URL            string
	Token          string
	SubjectPrefix  string
	ConnectTimeout time.Duration
	PublishTimeout time.Duration
}

// Auth contains server-only credentials. Never log this value or send it to clients.
type Auth struct {
	ClientTokenSecret string
	PublisherKeys     map[string]string
}

type WebSocket struct {
	QueueCapacity   int
	MaxConnections  int
	MaxMessageBytes int64
	AuthTimeout     time.Duration
	WriteTimeout    time.Duration
	PingInterval    time.Duration
	PongTimeout     time.Duration
	IdleTimeout     time.Duration
	AllowedOrigins  []string
}

func (c Config) Address() string {
	return net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
}

func Load() (Config, error) {
	return load(os.LookupEnv)
}

// LoadAuth reads just the credentials required by the token issuance CLI.
func LoadAuth() (Auth, error) {
	return loadAuth(os.LookupEnv)
}

func environmentValue(lookup func(string) (string, bool), name, fallback string) (string, error) {
	v, exists := lookup(name)
	if !exists {
		return fallback, nil
	}
	if v == "" {
		return "", fmt.Errorf("%s must not be empty", name)
	}
	return v, nil
}

func load(lookup func(string) (string, bool)) (Config, error) {
	var cfg Config
	value := func(name, fallback string) (string, error) {
		return environmentValue(lookup, name, fallback)
	}
	mode, err := value("SOCKETFABRIC_MODE", "single")
	if err != nil {
		return Config{}, err
	}
	if mode != "single" && mode != "distributed" {
		return Config{}, fmt.Errorf("SOCKETFABRIC_MODE must be single or distributed")
	}
	cfg.Mode = mode
	if nodeID, ok := lookup("NODE_ID"); ok {
		if nodeID != "" && (len(nodeID) > 64 || domain.ValidateID(nodeID) != nil) {
			return Config{}, fmt.Errorf("NODE_ID must be an optional identifier containing at most 64 ASCII letters, digits, ., _, :, or -")
		}
		cfg.NodeID = nodeID
	}

	environment, err := value("APP_ENV", "development")
	if err != nil {
		return Config{}, err
	}
	switch environment {
	case "development", "test", "production":
		cfg.Environment = environment
	default:
		return Config{}, fmt.Errorf("APP_ENV must be development, test, or production")
	}

	host, err := value("HTTP_HOST", "0.0.0.0")
	if err != nil {
		return Config{}, err
	}
	if !validHost(host) {
		return Config{}, fmt.Errorf("HTTP_HOST must be an IP address or hostname")
	}
	cfg.Host = host

	port, err := value("HTTP_PORT", "8080")
	if err != nil {
		return Config{}, err
	}
	cfg.Port, err = strconv.Atoi(port)
	if err != nil || cfg.Port < 1 || cfg.Port > 65535 {
		return Config{}, fmt.Errorf("HTTP_PORT must be an integer between 1 and 65535")
	}

	level, err := value("LOG_LEVEL", "info")
	if err != nil {
		return Config{}, err
	}
	switch level {
	case "debug":
		cfg.LogLevel = slog.LevelDebug
	case "info":
		cfg.LogLevel = slog.LevelInfo
	case "warn":
		cfg.LogLevel = slog.LevelWarn
	case "error":
		cfg.LogLevel = slog.LevelError
	default:
		return Config{}, fmt.Errorf("LOG_LEVEL must be debug, info, warn, or error")
	}

	for _, field := range []struct {
		name     string
		fallback string
		target   *time.Duration
	}{
		{"HTTP_READ_HEADER_TIMEOUT", "5s", &cfg.ReadHeaderTimeout},
		{"HTTP_READ_TIMEOUT", "15s", &cfg.ReadTimeout},
		{"HTTP_WRITE_TIMEOUT", "15s", &cfg.WriteTimeout},
		{"HTTP_IDLE_TIMEOUT", "60s", &cfg.IdleTimeout},
		{"SHUTDOWN_TIMEOUT", "10s", &cfg.ShutdownTimeout},
		{"WS_AUTH_TIMEOUT", "5s", &cfg.WebSocket.AuthTimeout},
		{"WS_WRITE_TIMEOUT", "5s", &cfg.WebSocket.WriteTimeout},
		{"WS_PING_INTERVAL", "30s", &cfg.WebSocket.PingInterval},
		{"WS_PONG_TIMEOUT", "10s", &cfg.WebSocket.PongTimeout},
	} {
		raw, err := value(field.name, field.fallback)
		if err != nil {
			return Config{}, err
		}
		duration, err := time.ParseDuration(raw)
		if err != nil || duration <= 0 {
			// Do not include raw environment values in startup diagnostics.
			return Config{}, fmt.Errorf("%s must be a positive Go duration", field.name)
		}
		*field.target = duration
	}
	raw, err := value("WS_IDLE_TIMEOUT", "0s")
	if err != nil {
		return Config{}, err
	}
	cfg.WebSocket.IdleTimeout, err = time.ParseDuration(raw)
	if err != nil || cfg.WebSocket.IdleTimeout < 0 {
		return Config{}, fmt.Errorf("WS_IDLE_TIMEOUT must be a nonnegative Go duration")
	}
	for _, field := range []struct {
		name     string
		fallback string
		max      int64
		target   *int
	}{
		{"WS_QUEUE_CAPACITY", "32", 1024, &cfg.WebSocket.QueueCapacity},
		{"WS_MAX_CONNECTIONS", "256", 10000, &cfg.WebSocket.MaxConnections},
	} {
		raw, err := value(field.name, field.fallback)
		if err != nil {
			return Config{}, err
		}
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 1 || n > field.max {
			return Config{}, fmt.Errorf("%s must be an integer between 1 and %d", field.name, field.max)
		}
		*field.target = int(n)
	}
	raw, err = value("WS_MAX_MESSAGE_BYTES", "16384")
	if err != nil {
		return Config{}, err
	}
	cfg.WebSocket.MaxMessageBytes, err = strconv.ParseInt(raw, 10, 64)
	if err != nil || cfg.WebSocket.MaxMessageBytes < 1 || cfg.WebSocket.MaxMessageBytes > 1048576 {
		return Config{}, fmt.Errorf("WS_MAX_MESSAGE_BYTES must be an integer between 1 and 1048576")
	}
	raw, err = value("DEMO_ENABLED", "false")
	if err != nil {
		return Config{}, err
	}
	if raw != "true" && raw != "false" {
		return Config{}, fmt.Errorf("DEMO_ENABLED must be true or false")
	}
	cfg.DemoEnabled = raw == "true"
	if raw, ok := lookup("WS_ALLOWED_ORIGINS"); ok {
		if raw == "" {
			return Config{}, fmt.Errorf("WS_ALLOWED_ORIGINS must not be empty; omit it for same-origin only")
		}
		cfg.WebSocket.AllowedOrigins, err = parseOrigins(raw)
		if err != nil {
			return Config{}, err
		}
	}
	managementEnabled, err := value("MANAGEMENT_ENABLED", "false")
	if err != nil {
		return Config{}, err
	}
	if managementEnabled != "false" && managementEnabled != "true" {
		return Config{}, fmt.Errorf("MANAGEMENT_ENABLED must be true or false")
	}
	if managementEnabled == "true" {
		cfg.Management, err = loadManagement(lookup, cfg.Environment)
		if err != nil {
			return Config{}, err
		}
	}
	cfg.Auth, err = loadAuthWithEmpty(lookup, cfg.Management.Enabled)
	if err != nil {
		return Config{}, err
	}
	if cfg.Mode == "distributed" {
		if len(cfg.Auth.PublisherKeys) > 128 {
			return Config{}, fmt.Errorf("PUBLISHER_KEYS_JSON must contain at most 128 projects in distributed mode")
		}
		cfg.NATS, err = loadNATS(lookup, cfg.Auth)
		if err != nil {
			return Config{}, err
		}
	}
	return cfg, nil
}

func loadNATS(lookup func(string) (string, bool), auth Auth) (NATS, error) {
	var cfg NATS
	var err error
	cfg.URL, err = environmentValue(lookup, "NATS_URL", "nats://localhost:4222")
	if err != nil {
		return NATS{}, err
	}
	u, err := url.Parse(cfg.URL)
	if err != nil || (u.Scheme != "nats" && u.Scheme != "tls") || u.Host == "" || !validHost(u.Hostname()) ||
		u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || strings.Contains(cfg.URL, "#") || u.Opaque != "" {
		return NATS{}, fmt.Errorf("NATS_URL must be one nats:// or tls:// host:port URL without credentials, paths, query, or fragment")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return NATS{}, fmt.Errorf("NATS_URL must include a port between 1 and 65535")
	}
	cfg.Token, err = environmentValue(lookup, "NATS_TOKEN", "")
	if err != nil || len(cfg.Token) < 32 {
		return NATS{}, fmt.Errorf("NATS_TOKEN is required and must contain at least 32 bytes")
	}
	if cfg.Token == auth.ClientTokenSecret {
		return NATS{}, fmt.Errorf("NATS_TOKEN must be distinct from CLIENT_TOKEN_SECRET and publisher keys")
	}
	for _, key := range auth.PublisherKeys {
		if cfg.Token == key {
			return NATS{}, fmt.Errorf("NATS_TOKEN must be distinct from CLIENT_TOKEN_SECRET and publisher keys")
		}
	}
	cfg.SubjectPrefix, err = environmentValue(lookup, "NATS_SUBJECT_PREFIX", "socketfabric")
	if err != nil {
		return NATS{}, err
	}
	if !validSubjectPrefix(cfg.SubjectPrefix) {
		return NATS{}, fmt.Errorf("NATS_SUBJECT_PREFIX must contain 1 to 128 ASCII letters, digits, _, -, or nonempty dot-separated tokens")
	}
	for _, field := range []struct {
		name     string
		fallback string
		target   *time.Duration
	}{
		{"NATS_CONNECT_TIMEOUT", "5s", &cfg.ConnectTimeout},
		{"NATS_PUBLISH_TIMEOUT", "2s", &cfg.PublishTimeout},
	} {
		raw, err := environmentValue(lookup, field.name, field.fallback)
		if err != nil {
			return NATS{}, err
		}
		duration, err := time.ParseDuration(raw)
		if err != nil || duration <= 0 {
			return NATS{}, fmt.Errorf("%s must be a positive Go duration", field.name)
		}
		*field.target = duration
	}
	return cfg, nil
}

func validSubjectPrefix(prefix string) bool {
	if len(prefix) == 0 || len(prefix) > 128 {
		return false
	}
	for _, token := range strings.Split(prefix, ".") {
		if token == "" {
			return false
		}
		for i := range len(token) {
			ch := token[i]
			if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '_' || ch == '-') {
				return false
			}
		}
	}
	return true
}

func loadAuth(lookup func(string) (string, bool)) (Auth, error) {
	return loadAuthWithEmpty(lookup, false)
}

func loadAuthWithEmpty(lookup func(string) (string, bool), allowEmpty bool) (Auth, error) {
	secret, ok := lookup("CLIENT_TOKEN_SECRET")
	if !ok || len(secret) < 32 {
		return Auth{}, fmt.Errorf("CLIENT_TOKEN_SECRET is required and must contain at least 32 bytes")
	}
	raw, ok := lookup("PUBLISHER_KEYS_JSON")
	if !ok || raw == "" {
		return Auth{}, fmt.Errorf("PUBLISHER_KEYS_JSON is required and must contain project credentials")
	}
	// Decode entries individually so duplicate project names cannot silently override credentials.
	decoder := json.NewDecoder(strings.NewReader(raw))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return Auth{}, fmt.Errorf("PUBLISHER_KEYS_JSON must be a nonempty JSON object of project IDs to keys")
	}
	keys := make(map[string]string)
	seenKeys := make(map[string]bool)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return Auth{}, fmt.Errorf("PUBLISHER_KEYS_JSON must be a valid JSON object")
		}
		project, ok := token.(string)
		if !ok || domain.ValidateID(project) != nil {
			return Auth{}, fmt.Errorf("PUBLISHER_KEYS_JSON contains an invalid project ID")
		}
		if _, exists := keys[project]; exists {
			return Auth{}, fmt.Errorf("PUBLISHER_KEYS_JSON must not contain duplicate projects")
		}
		var key string
		if err := decoder.Decode(&key); err != nil || len(key) < 32 {
			return Auth{}, fmt.Errorf("PUBLISHER_KEYS_JSON keys must be strings containing at least 32 bytes")
		}
		if seenKeys[key] || key == secret {
			return Auth{}, fmt.Errorf("PUBLISHER_KEYS_JSON keys must be unique and distinct from CLIENT_TOKEN_SECRET")
		}
		keys[project], seenKeys[key] = key, true
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') || len(keys) == 0 && !allowEmpty {
		return Auth{}, fmt.Errorf("PUBLISHER_KEYS_JSON must be a nonempty JSON object of project IDs to keys")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return Auth{}, fmt.Errorf("PUBLISHER_KEYS_JSON must contain one JSON object")
	}
	return Auth{ClientTokenSecret: secret, PublisherKeys: keys}, nil
}

func parseOrigins(raw string) ([]string, error) {
	origins := make([]string, 0)
	seen := make(map[string]bool)
	for _, origin := range strings.Split(raw, ",") {
		u, err := url.Parse(strings.TrimSpace(origin))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
			u.Path != "" || u.RawQuery != "" || u.ForceQuery || strings.Contains(origin, "#") || u.Opaque != "" || !validHost(u.Hostname()) {
			return nil, fmt.Errorf("WS_ALLOWED_ORIGINS must contain comma-separated http/https origins without paths, credentials, or wildcards")
		}
		if port := u.Port(); port != "" {
			n, err := strconv.Atoi(port)
			if err != nil || n < 1 || n > 65535 {
				return nil, fmt.Errorf("WS_ALLOWED_ORIGINS contains an invalid port")
			}
		}
		if strings.HasSuffix(u.Host, ":") {
			return nil, fmt.Errorf("WS_ALLOWED_ORIGINS contains an invalid port")
		}
		normalized := u.Scheme + "://" + strings.ToLower(u.Host)
		if !seen[normalized] {
			origins = append(origins, normalized)
			seen[normalized] = true
		}
	}
	return origins, nil
}

func validHost(host string) bool {
	if net.ParseIP(host) != nil {
		return true
	}
	host = strings.TrimSuffix(host, ".")
	if len(host) == 0 || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range label {
			if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-') {
				return false
			}
		}
	}
	return true
}
