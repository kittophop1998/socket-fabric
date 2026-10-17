package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	socketfabric "socketfabric/sdk/go"
)

type exampleUser struct {
	Password  string   `json:"password"`
	ProjectID string   `json:"projectId"`
	Rooms     []string `json:"rooms"`
}

type exampleOptions struct {
	Address, Origin, PublicDir, GatewayURL, SigningSecret string
	WebSocketURLs                                         []string
	PublisherKeys                                         map[string]string
	Users                                                 map[string]exampleUser
	TokenTTL, RequestTimeout                              time.Duration
	HTTPClient                                            *http.Client
}

type principal struct {
	password  [32]byte
	projectID string
	rooms     []string
}

type exampleApplication struct {
	origin     *url.URL
	gateways   []string
	users      map[string]principal
	publishers map[string]*socketfabric.Client
	signer     *socketfabric.TokenSigner
	tokenTTL   time.Duration
	index, js  []byte
	csp        string
}

func serve(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return errors.New("serve: no flags are supported; configure the example through ENV (see docs/QUICKSTART.md)")
	}
	options, err := loadExampleOptions(os.LookupEnv)
	if err != nil {
		return err
	}
	app, err := newExample(options)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", options.Address)
	if err != nil {
		return errors.New("serve: cannot listen on EXAMPLE_ADDR")
	}
	fmt.Fprintln(os.Stdout, "Integration example ready; open the configured EXAMPLE_ORIGIN")
	return app.run(ctx, listener)
}

func loadExampleOptions(lookup func(string) (string, bool)) (exampleOptions, error) {
	value := func(name, fallback string) string {
		if result, ok := lookup(name); ok {
			return result
		}
		return fallback
	}
	o := exampleOptions{
		Address: value("EXAMPLE_ADDR", "127.0.0.1:3000"), Origin: value("EXAMPLE_ORIGIN", "http://127.0.0.1:3000"),
		PublicDir: value("EXAMPLE_PUBLIC_DIR", "public"), GatewayURL: value("SOCKETFABRIC_URL", "http://localhost:8080"),
		SigningSecret: value("CLIENT_TOKEN_SECRET", ""), WebSocketURLs: strings.Split(value("SOCKETFABRIC_WS_URLS", "ws://localhost:8080/ws"), ","),
	}
	var err error
	o.TokenTTL, err = time.ParseDuration(value("EXAMPLE_TOKEN_TTL", "5m"))
	if err != nil {
		return o, errors.New("serve: EXAMPLE_TOKEN_TTL must be a duration between 1s and 15m")
	}
	o.RequestTimeout, err = time.ParseDuration(value("SOCKETFABRIC_TIMEOUT", "5s"))
	if err != nil {
		return o, errors.New("serve: SOCKETFABRIC_TIMEOUT must be a positive duration")
	}
	keys, users := value("PUBLISHER_KEYS_JSON", ""), value("EXAMPLE_USERS_JSON", "")
	if len(keys) > 1<<20 || len(users) > 1<<20 || decodeJSON([]byte(keys), &o.PublisherKeys) != nil || decodeJSON([]byte(users), &o.Users) != nil {
		return o, errors.New("serve: PUBLISHER_KEYS_JSON and EXAMPLE_USERS_JSON must be bounded valid JSON configuration")
	}
	return o, nil
}

func newExample(o exampleOptions) (*exampleApplication, error) {
	invalid := func() (*exampleApplication, error) {
		return nil, errors.New("serve: invalid example configuration; check loopback address, public origin, gateways, distinct credentials and project/room policies")
	}
	host, port, err := net.SplitHostPort(o.Address)
	number, portErr := strconv.Atoi(port)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() || portErr != nil || number < 1 || number > 65535 || o.RequestTimeout <= 0 || o.TokenTTL < time.Second || o.TokenTTL > 15*time.Minute {
		return invalid()
	}
	origin, err := url.Parse(o.Origin)
	if err != nil || origin.Host == "" || origin.Hostname() == "" || (origin.Scheme != "http" && origin.Scheme != "https") || origin.User != nil || origin.RawQuery != "" || origin.ForceQuery || origin.Fragment != "" || origin.Opaque != "" || origin.Path != "" {
		return invalid()
	}
	if len(o.WebSocketURLs) < 1 || len(o.WebSocketURLs) > 8 || len(o.Users) < 1 || len(o.Users) > 128 || len(o.PublisherKeys) < 1 || len(o.PublisherKeys) > 128 {
		return invalid()
	}
	app := &exampleApplication{origin: origin, gateways: append([]string(nil), o.WebSocketURLs...), users: make(map[string]principal), publishers: make(map[string]*socketfabric.Client), tokenTTL: o.TokenTTL.Truncate(time.Second)}
	app.signer, err = socketfabric.NewTokenSigner([]byte(o.SigningSecret))
	if err != nil {
		return invalid()
	}
	app.csp = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'; connect-src 'self'"
	for _, address := range app.gateways {
		ws, err := url.Parse(address)
		if err != nil || (ws.Scheme != "ws" && ws.Scheme != "wss") || ws.Host == "" || ws.Hostname() == "" || ws.User != nil || ws.RawQuery != "" || ws.ForceQuery || ws.Fragment != "" || ws.Opaque != "" || (origin.Scheme == "https" && ws.Scheme != "wss") {
			return invalid()
		}
		app.csp += " " + ws.Scheme + "://" + ws.Host
	}
	seenKeys := make(map[string]bool)
	for project, key := range o.PublisherKeys {
		if key == o.SigningSecret || seenKeys[key] {
			return invalid()
		}
		client, err := socketfabric.NewClient(socketfabric.ClientOptions{BaseURL: o.GatewayURL, ProjectID: project, PublisherKey: key, RequestTimeout: o.RequestTimeout, HTTPClient: o.HTTPClient})
		if err != nil {
			return invalid()
		}
		seenKeys[key], app.publishers[project] = true, client
	}
	for username, user := range o.Users {
		if !identifier(username) || strings.Contains(username, ":") || len(user.Password) < 32 || len(user.Password) > 256 || user.Password == o.SigningSecret || seenKeys[user.Password] || app.publishers[user.ProjectID] == nil || len(user.Rooms) == 0 || len(user.Rooms) > 64 {
			return invalid()
		}
		rooms := make(map[string]bool)
		for _, room := range user.Rooms {
			if !identifier(room) || rooms[room] {
				return invalid()
			}
			rooms[room] = true
		}
		app.users[username] = principal{sha256.Sum256([]byte(user.Password)), user.ProjectID, append([]string(nil), user.Rooms...)}
	}
	for name, target := range map[string]*[]byte{"index.html": &app.index, "app.js": &app.js} {
		file, err := os.Open(filepath.Join(o.PublicDir, name))
		if err != nil {
			return nil, errors.New("serve: browser assets missing; install the packed SDK and run npm run build first")
		}
		data, readErr := io.ReadAll(io.LimitReader(file, (8<<20)+1))
		file.Close()
		if readErr != nil || len(data) > 8<<20 {
			return nil, errors.New("serve: cannot read bounded browser assets")
		}
		*target = data
	}
	return app, nil
}

func (a *exampleApplication) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(a.index)
	})
	mux.HandleFunc("GET /app.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Write(a.js)
	})
	mux.HandleFunc("GET /api/config", func(w http.ResponseWriter, r *http.Request) {
		respond(w, 200, struct {
			Gateways []string `json:"gateways"`
		}{a.gateways})
	})
	mux.HandleFunc("POST /api/realtime-token", a.token)
	mux.HandleFunc("POST /api/resources/{id}/update", a.updateResource)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		appError(w, 404, "not_found", "Application route not found")
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", a.csp)
		if r.Host != a.origin.Host || r.Header.Get("Origin") != "" && r.Header.Get("Origin") != a.origin.String() {
			appError(w, 403, "forbidden", "Use the configured application origin")
			return
		}
		if r.URL.RawQuery != "" {
			appError(w, 400, "invalid_request", "Query parameters are not supported")
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (a *exampleApplication) authenticate(w http.ResponseWriter, r *http.Request) (string, principal, bool) {
	username, password, valid := r.BasicAuth()
	user, exists := a.users[username]
	digest := sha256.Sum256([]byte(password))
	matched := subtle.ConstantTimeCompare(digest[:], user.password[:]) == 1
	if !valid || !exists || !matched {
		w.Header().Set("WWW-Authenticate", `Basic realm="SocketFabric example", charset="UTF-8"`)
		appError(w, 401, "unauthorized", "Valid application credentials are required")
		return "", principal{}, false
	}
	return username, user, true
}

func (a *exampleApplication) token(w http.ResponseWriter, r *http.Request) {
	username, user, ok := a.authenticate(w, r)
	if !ok {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1024))
	if err != nil || len(bytes.TrimSpace(body)) != 0 && string(bytes.TrimSpace(body)) != "{}" {
		appError(w, 400, "invalid_request", "Token scope is derived from server policy; no client-selected scope is accepted")
		return
	}
	token, err := a.signer.IssueToken(socketfabric.ClientTokenInput{ProjectID: user.projectID, UserID: username, Rooms: user.rooms, TTL: a.tokenTTL})
	if err != nil {
		appError(w, 500, "token_failed", "Unable to issue a client token")
		return
	}
	respond(w, 200, struct {
		Token     string   `json:"token"`
		ProjectID string   `json:"projectId"`
		UserID    string   `json:"userId"`
		Rooms     []string `json:"rooms"`
		ExpiresIn int64    `json:"expiresIn"`
	}{token, user.projectID, username, user.rooms, int64(a.tokenTTL / time.Second)})
}

func (a *exampleApplication) updateResource(w http.ResponseWriter, r *http.Request) {
	_, user, ok := a.authenticate(w, r)
	if !ok {
		return
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		appError(w, 415, "invalid_request", "Use application/json")
		return
	}
	var input struct {
		RoomID string `json:"roomId"`
		Status string `json:"status"`
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<10))
	if err != nil || decodeJSON(body, &input) != nil || !identifier(r.PathValue("id")) || !identifier(input.RoomID) || (input.Status != "updated" && input.Status != "completed") {
		appError(w, 400, "invalid_request", "Provide a valid resource ID, roomId and status (updated or completed)")
		return
	}
	allowed := false
	for _, room := range user.rooms {
		if room == input.RoomID {
			allowed = true
			break
		}
	}
	if !allowed {
		appError(w, 403, "forbidden", "Room access is not authorized")
		return
	}
	result, err := a.publishers[user.projectID].Publish(r.Context(), socketfabric.PublishInput{RoomID: input.RoomID, Type: "resource.updated", Data: struct {
		ResourceID string `json:"resourceId"`
		Status     string `json:"status"`
	}{r.PathValue("id"), input.Status}})
	if err != nil {
		status := http.StatusServiceUnavailable
		var api *socketfabric.APIError
		if errors.As(err, &api) && api.StatusCode != 503 && api.StatusCode != 429 && api.StatusCode != 504 {
			status = http.StatusBadGateway
		}
		appError(w, status, "publish_failed", "Gateway could not confirm acceptance; no retry was attempted and delivery may be unknown")
		return
	}
	respond(w, http.StatusAccepted, result)
}

func (a *exampleApplication) run(ctx context.Context, listener net.Listener) error {
	server := &http.Server{Handler: a.handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10, ErrorLog: log.New(io.Discard, "", 0)}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			return errors.New("serve: HTTP listener failed")
		}
		return nil
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			server.Close()
			<-done
			return errors.New("serve: graceful shutdown timed out")
		}
		<-done
		return nil
	}
}

func identifier(value string) bool {
	if len(value) < 1 || len(value) > 128 {
		return false
	}
	for i := range len(value) {
		ch := value[i]
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '.' || ch == '_' || ch == ':' || ch == '-') {
			return false
		}
	}
	return true
}

func decodeJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("expected one JSON value")
	}
	return nil
}

func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func appError(w http.ResponseWriter, status int, code, message string) {
	type detail struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	respond(w, status, struct {
		Error detail `json:"error"`
	}{Error: detail{code, message}})
}
