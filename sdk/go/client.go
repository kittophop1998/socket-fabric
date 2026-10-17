package socketfabric

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
)

const (
	maxPublishBytes       = 64 << 10
	maxResponseBytes      = 1 << 20
	defaultRequestTimeout = 10 * time.Second
)

// ClientOptions configures one immutable, project-scoped HTTP publisher.
type ClientOptions struct {
	// BaseURL may include a reverse-proxy path prefix. Credentials, query
	// parameters and fragments are rejected. Use HTTPS across trust boundaries.
	BaseURL      string
	ProjectID    string
	PublisherKey string
	HTTPClient   *http.Client
	// RequestTimeout defaults to 10 seconds. Negative values are invalid.
	RequestTimeout time.Duration
}

// Client is immutable after construction and safe for concurrent publishing.
// A supplied HTTPClient's Transport must also be safe for concurrent use. The
// SDK clones the client, disables redirects and leaves the original unchanged.
type Client struct {
	endpoint     string
	projectID    string
	publisherKey string
	httpClient   *http.Client
	timeout      time.Duration
}

// NewClient validates configuration and copies the HTTP client. It does not
// connect, retry requests, follow redirects or load environment variables.
func NewClient(options ClientOptions) (*Client, error) {
	base, err := url.Parse(options.BaseURL)
	if err != nil || base == nil || (base.Scheme != "http" && base.Scheme != "https") ||
		base.Host == "" || base.Hostname() == "" || base.User != nil || base.RawQuery != "" ||
		base.ForceQuery || base.Fragment != "" || base.Opaque != "" {
		return nil, fmt.Errorf("base URL must be an HTTP(S) URL without credentials, query or fragment: %w", ErrInvalidInput)
	}
	if !validID(options.ProjectID) {
		return nil, fmt.Errorf("project ID: %w", ErrInvalidInput)
	}
	if len(options.PublisherKey) < 32 || strings.ContainsFunc(options.PublisherKey, func(ch rune) bool { return unicode.IsSpace(ch) || unicode.IsControl(ch) }) {
		return nil, fmt.Errorf("publisher key must contain at least 32 bytes without whitespace or control characters: %w", ErrInvalidInput)
	}
	if options.RequestTimeout < 0 {
		return nil, fmt.Errorf("request timeout must be positive: %w", ErrInvalidInput)
	}
	if options.RequestTimeout == 0 {
		options.RequestTimeout = defaultRequestTimeout
	}
	client := http.Client{}
	if options.HTTPClient != nil {
		client = *options.HTTPClient
	}
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{
		endpoint:  base.JoinPath("v1", "events").String(),
		projectID: options.ProjectID, publisherKey: options.PublisherKey,
		httpClient: &client, timeout: options.RequestTimeout,
	}, nil
}

// Publish sends exactly one HTTP request. It never retries automatically: a
// timeout or 503 may follow remote delivery and retrying can create duplicates.
// Cancellation and deadlines remain inspectable through errors.Is.
func (c *Client) Publish(ctx context.Context, input PublishInput) (PublishResult, error) {
	if c == nil || c.httpClient == nil || ctx == nil {
		return PublishResult{}, fmt.Errorf("client and context are required: %w", ErrInvalidInput)
	}
	if err := ctx.Err(); err != nil {
		return PublishResult{}, fmt.Errorf("publish canceled: %w", err)
	}
	if !validID(input.RoomID) || !validID(input.Type) || (input.RequestID != "" && !validRequestID(input.RequestID)) {
		return PublishResult{}, fmt.Errorf("room ID, event type or request ID: %w", ErrInvalidInput)
	}
	body, err := json.Marshal(struct {
		ProjectID string `json:"projectId"`
		Type      string `json:"type"`
		RoomID    string `json:"roomId"`
		Data      any    `json:"data"`
	}{c.projectID, input.Type, input.RoomID, input.Data})
	if err != nil {
		return PublishResult{}, fmt.Errorf("event data cannot be encoded as JSON: %w", ErrInvalidInput)
	}
	if len(body) > maxPublishBytes {
		return PublishResult{}, fmt.Errorf("publish body exceeds 64 KiB: %w", ErrInvalidInput)
	}
	requestCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return PublishResult{}, ErrRequestFailed
	}
	request.Header.Set("Authorization", "Bearer "+c.publisherKey)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	if input.RequestID != "" {
		request.Header.Set("X-Request-ID", input.RequestID)
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		// net/http errors may include the URL and custom transport errors may
		// include request credentials. Keep those out of returned errors.
		return PublishResult{}, requestFailure(requestCtx, err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return PublishResult{}, requestFailure(requestCtx, err)
	}
	if len(responseBody) > maxResponseBytes {
		return PublishResult{}, ErrInvalidResponse
	}
	if response.StatusCode != http.StatusAccepted {
		apiError := &APIError{StatusCode: response.StatusCode}
		var envelope struct {
			Error struct {
				Code      string `json:"code"`
				Message   string `json:"message"`
				RequestID string `json:"requestId"`
			} `json:"error"`
		}
		if json.Unmarshal(responseBody, &envelope) == nil {
			apiError.Code, apiError.Message = envelope.Error.Code, envelope.Error.Message
			if validRequestID(envelope.Error.RequestID) {
				apiError.RequestID = envelope.Error.RequestID
			}
		}
		if apiError.RequestID == "" && validRequestID(response.Header.Get("X-Request-ID")) {
			apiError.RequestID = response.Header.Get("X-Request-ID")
		}
		return PublishResult{}, apiError
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return PublishResult{}, ErrInvalidResponse
	}
	var result struct {
		Event             Event `json:"event"`
		QueuedConnections *int  `json:"queuedConnections"`
	}
	if json.Unmarshal(responseBody, &result) != nil || result.QueuedConnections == nil || *result.QueuedConnections < 0 ||
		!validID(result.Event.EventID) || result.Event.Timestamp.IsZero() || len(result.Event.Data) == 0 ||
		!json.Valid(result.Event.Data) || result.Event.ProjectID != c.projectID || result.Event.RoomID != input.RoomID || result.Event.Type != input.Type {
		return PublishResult{}, ErrInvalidResponse
	}
	return PublishResult{Event: result.Event, QueuedConnections: *result.QueuedConnections}, nil
}

func requestFailure(ctx context.Context, err error) error {
	if contextErr := ctx.Err(); contextErr != nil {
		return fmt.Errorf("publish canceled: %w", contextErr)
	}
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("publish canceled: %w", context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("publish timed out: %w", context.DeadlineExceeded)
	}
	return ErrRequestFailed
}
