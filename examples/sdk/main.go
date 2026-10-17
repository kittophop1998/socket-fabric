// This example is a trusted backend process. Never serve its environment or
// signing secret to a browser. Real token endpoints must authenticate their user
// and derive project/room permissions from application policy first.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	socketfabric "socketfabric/sdk/go"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil {
		// Do not print raw SDK transport errors, data, URLs, or environment values.
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: go run . serve | token [--ttl 5m] | publish [--room room-a --type resource.updated --data '{}']")
	}
	switch args[0] {
	case "serve":
		return serve(ctx, args[1:])
	case "token":
		return issueToken(args[1:])
	case "publish":
		return publish(ctx, args[1:])
	default:
		return errors.New("unknown command: use serve, token or publish")
	}
}

func issueToken(args []string) error {
	flags := flag.NewFlagSet("token", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	ttl := flags.Duration("ttl", 5*time.Minute, "client token lifetime, up to 15m")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return errors.New("token: invalid arguments")
	}
	secret := os.Getenv("CLIENT_TOKEN_SECRET")
	if secret == "" {
		return errors.New("token: CLIENT_TOKEN_SECRET is required in this trusted process")
	}
	signer, err := socketfabric.NewTokenSigner([]byte(secret))
	if err != nil {
		return errors.New("token: invalid signing secret")
	}
	rooms := strings.Split(env("SOCKETFABRIC_ROOMS", "room-a"), ",")
	for i := range rooms {
		rooms[i] = strings.TrimSpace(rooms[i])
	}
	token, err := signer.IssueToken(socketfabric.ClientTokenInput{
		ProjectID: env("SOCKETFABRIC_PROJECT_ID", "project-a"),
		UserID:    env("SOCKETFABRIC_USER_ID", "sdk-user"),
		Rooms:     rooms,
		TTL:       *ttl,
	})
	if err != nil {
		return errors.New("token: invalid project, user, room permissions, or lifetime")
	}
	_, err = fmt.Fprintln(os.Stdout, token)
	return err
}

func publish(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("publish", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	room := flags.String("room", env("SOCKETFABRIC_ROOM_ID", "room-a"), "target room")
	eventType := flags.String("type", env("SOCKETFABRIC_EVENT_TYPE", "resource.updated"), "event type")
	data := flags.String("data", env("SOCKETFABRIC_DATA_JSON", `{"resourceId":"resource-42","status":"updated"}`), "JSON event data")
	requestID := flags.String("request-id", "", "optional HTTP request correlation ID")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return errors.New("publish: invalid arguments")
	}
	if !json.Valid([]byte(*data)) {
		return errors.New("publish: data must be valid JSON, including null")
	}
	client, err := socketfabric.NewClient(socketfabric.ClientOptions{
		BaseURL:      env("SOCKETFABRIC_URL", "http://localhost:8080"),
		ProjectID:    env("SOCKETFABRIC_PROJECT_ID", "project-a"),
		PublisherKey: os.Getenv("PUBLISHER_KEY"),
	})
	if err != nil {
		return errors.New("publish: invalid URL, project, or publisher credentials")
	}
	result, err := client.Publish(ctx, socketfabric.PublishInput{
		RoomID: *room, Type: *eventType, Data: json.RawMessage(*data), RequestID: *requestID,
	})
	if err != nil {
		var apiError *socketfabric.APIError
		if errors.As(err, &apiError) {
			return fmt.Errorf("publish: server rejected request (HTTP %d)", apiError.StatusCode)
		}
		return errors.New("publish: request failed; outcome may be unknown, so do not retry blindly")
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		return errors.New("publish: cannot write result")
	}
	return nil
}

func env(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}
