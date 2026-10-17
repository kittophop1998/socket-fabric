package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	socketfabric "socketfabric/sdk/go"
)

const maxDataBytes = 64 << 10

type publishOutput struct {
	Accepted          bool      `json:"accepted"`
	EventID           string    `json:"eventId"`
	ProjectID         string    `json:"projectId"`
	RoomID            string    `json:"roomId"`
	Type              string    `json:"type"`
	Timestamp         time.Time `json:"timestamp"`
	QueuedConnections int       `json:"queuedConnections"`
	DeliveryConfirmed bool      `json:"deliveryConfirmed"`
}

func (a application) publish(ctx context.Context, o options, cfg configuration) int {
	if !validIdentifier(o.room) || !validIdentifier(o.event) {
		return a.fail(o.json, &commandError{Code: "invalid_input", Message: "--room and --event are required: use 1-128 ASCII letters/digits or . _ : -.", exit: 2})
	}
	data, err := readData(ctx, o.data, a.stdin)
	if err != nil {
		return a.fail(o.json, err)
	}
	client, sdkErr := socketfabric.NewClient(socketfabric.ClientOptions{
		BaseURL: cfg.base.String(), ProjectID: o.project, PublisherKey: o.key,
		HTTPClient: a.httpClient, RequestTimeout: cfg.timeout,
	})
	if sdkErr != nil {
		return a.fail(o.json, &commandError{Code: "invalid_config", Message: "Invalid gateway, project, publisher key or timeout configuration.", exit: 2})
	}
	result, sdkErr := client.Publish(ctx, socketfabric.PublishInput{RoomID: o.room, Type: o.event, Data: data})
	if sdkErr != nil {
		return a.fail(o.json, publishFailure(sdkErr))
	}
	output := publishOutput{
		Accepted: true, EventID: o.safe(result.Event.EventID), ProjectID: o.safe(result.Event.ProjectID),
		RoomID: o.safe(result.Event.RoomID), Type: o.safe(result.Event.Type),
		Timestamp: result.Event.Timestamp, QueuedConnections: result.QueuedConnections,
	}
	return a.finish(o.json, output, fmt.Sprintf("Event accepted: %s\nProject: %s\nRoom: %s\nType: %s\nTimestamp: %s\nQueued connections on accepting gateway: %d\nBest-effort acceptance; browser delivery is not confirmed.\n", output.EventID, output.ProjectID, output.RoomID, output.Type, output.Timestamp.Format(time.RFC3339Nano), output.QueuedConnections))
}

func readData(ctx context.Context, value string, stdin io.Reader) (json.RawMessage, *commandError) {
	if err := ctx.Err(); err != nil {
		return nil, contextFailure(err)
	}
	data := []byte(value)
	if value == "-" {
		// Some inherited OS stdin handles cannot be interrupted by Close.
		// One bounded worker lets the command return on cancellation anyway;
		// the CLI process then exits and releases such a blocked OS handle.
		// Buffer completion so the worker never blocks on an abandoned result.
		if closer, ok := stdin.(io.ReadCloser); ok {
			stop := context.AfterFunc(ctx, func() { _ = closer.Close() })
			defer stop()
		}
		type inputResult struct {
			data []byte
			err  error
		}
		completed := make(chan inputResult, 1)
		go func() {
			input, err := io.ReadAll(io.LimitReader(stdin, maxDataBytes+1))
			completed <- inputResult{input, err}
		}()
		var result inputResult
		select {
		case <-ctx.Done():
			return nil, contextFailure(ctx.Err())
		case result = <-completed:
			data = result.data
		}
		if ctx.Err() != nil {
			return nil, contextFailure(ctx.Err())
		}
		if result.err != nil {
			return nil, &commandError{Code: "input_failed", Message: "Unable to read event JSON from stdin.", exit: 1}
		}
	}
	if len(data) == 0 || len(data) > maxDataBytes || !json.Valid(data) {
		return nil, &commandError{Code: "invalid_payload", Message: "--data must contain one valid JSON value (null is allowed). Use --data - for stdin. The complete publish body must fit within 64 KiB.", exit: 2}
	}
	return json.RawMessage(data), nil
}

func publishFailure(err error) *commandError {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return contextFailure(err)
	}
	var api *socketfabric.APIError
	if errors.As(err, &api) {
		message := "Gateway rejected the publish. Check gateway availability and configuration. No retry was attempted."
		code := "publish_rejected"
		switch api.StatusCode {
		case 400, 413, 415:
			message = "Gateway rejected the event format or size. Check project, room, event and JSON data."
		case 401:
			code, message = "unauthorized", "Publisher authentication failed. Check SOCKETFABRIC_API_KEY."
		case 403:
			code, message = "forbidden", "Publisher key is not authorized for this project. Check --project and SOCKETFABRIC_API_KEY."
		case 429:
			code, message = "rate_limited", "Gateway rate limit exceeded. No retry was attempted."
		case 500, 502, 503, 504:
			code, message = "gateway_unavailable", "Gateway could not confirm acceptance. Check readiness and broker availability. The event may already have reached another gateway; do not retry blindly."
		}
		return &commandError{Code: code, Message: fmt.Sprintf("HTTP %d: %s", api.StatusCode, message), HTTPStatus: api.StatusCode, exit: 1}
	}
	if errors.Is(err, socketfabric.ErrInvalidInput) {
		return &commandError{Code: "invalid_payload", Message: "Invalid publish input, or encoded request body exceeds 64 KiB.", exit: 2}
	}
	if errors.Is(err, socketfabric.ErrInvalidResponse) {
		return &commandError{Code: "invalid_response", Message: "Gateway returned an invalid publish response. Verify --url and any reverse proxy. Acceptance and delivery are unknown; do not retry blindly.", exit: 1}
	}
	return &commandError{Code: "network_unavailable", Message: "Unable to complete the gateway request. Check --url, the gateway listener, network and TLS. Acceptance is unknown; no retry was attempted.", exit: 1}
}
