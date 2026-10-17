package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

type probeResult struct {
	Endpoint       string  `json:"endpoint"`
	Reachable      bool    `json:"reachable"`
	HTTPStatus     int     `json:"httpStatus"`
	ResponseTimeMS float64 `json:"responseTimeMs"`
	OK             bool    `json:"ok"`
	Error          string  `json:"error,omitempty"`
}

func (a application) diagnose(parent context.Context, command string, o options, cfg configuration) int {
	ctx, cancel := context.WithTimeout(parent, cfg.timeout)
	defer cancel()
	client := http.Client{}
	if a.httpClient != nil {
		client = *a.httpClient
	}
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	var health, readiness probeResult
	var wg sync.WaitGroup
	wg.Go(func() { health = probe(ctx, &client, cfg.base, "healthz", "ok") })
	if command == "status" {
		wg.Go(func() { readiness = probe(ctx, &client, cfg.base, "readyz", "ready") })
	}
	wg.Wait()
	var result any
	human := fmt.Sprintf("Gateway: %s\nScope: this gateway only; HTTP connectivity, not WebSocket/browser delivery.\nLiveness: %s\n", o.safe(cfg.base.String()), probeText(health))
	passed := health.OK
	if command == "status" {
		result = struct {
			Gateway   string      `json:"gateway"`
			Scope     string      `json:"scope"`
			Health    probeResult `json:"health"`
			Readiness probeResult `json:"readiness"`
		}{o.safe(cfg.base.String()), "gateway", health, readiness}
		human += "Readiness: " + probeText(readiness) + "\nThis result does not describe full cluster health.\n"
		passed = passed && readiness.OK
	} else {
		result = struct {
			Gateway string      `json:"gateway"`
			Scope   string      `json:"scope"`
			HTTP    probeResult `json:"http"`
		}{o.safe(cfg.base.String()), "http", health}
	}
	if code := a.finish(o.json, result, human); code != 0 {
		return code
	}
	if passed {
		return 0
	}
	if parent.Err() != nil {
		return a.fail(o.json, contextFailure(parent.Err()))
	}
	if ctx.Err() != nil {
		return a.fail(o.json, contextFailure(ctx.Err()))
	}
	return a.fail(o.json, &commandError{Code: "diagnostic_failed", Message: "Gateway checks failed. Inspect the endpoint results; verify --url, gateway readiness and required broker availability.", exit: 1})
}

func probe(ctx context.Context, client *http.Client, base *url.URL, path, expected string) (result probeResult) {
	result.Endpoint = "/" + path
	started := time.Now()
	defer func() { result.ResponseTimeMS = float64(time.Since(started)) / float64(time.Millisecond) }()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base.JoinPath(path).String(), nil)
	if err != nil {
		result.Error = "invalid_request"
		return
	}
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		result.Error = "network_unavailable"
		var network net.Error
		switch {
		case errors.Is(ctx.Err(), context.Canceled):
			result.Error = "canceled"
		case errors.Is(ctx.Err(), context.DeadlineExceeded), errors.As(err, &network) && network.Timeout():
			result.Error = "timeout"
		}
		return
	}
	defer response.Body.Close()
	result.Reachable, result.HTTPStatus = true, response.StatusCode
	if response.StatusCode != http.StatusOK {
		result.Error = "unexpected_http_status"
		return
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (8<<10)+1))
	if err != nil {
		result.Error = "response_read_failed"
		if ctx.Err() != nil {
			result.Error = "canceled_or_timeout"
		}
		return
	}
	mediaType, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	var payload struct {
		Status string `json:"status"`
	}
	if len(body) > 8<<10 || mediaErr != nil || mediaType != "application/json" || json.Unmarshal(body, &payload) != nil || payload.Status != expected {
		result.Error = "invalid_response"
		return
	}
	result.OK = true
	return
}

func probeText(result probeResult) string {
	text := fmt.Sprintf("ok=%t reachable=%t HTTP=%d %.2f ms", result.OK, result.Reachable, result.HTTPStatus, result.ResponseTimeMS)
	if result.Error != "" {
		text += " (" + result.Error + ")"
	}
	return text
}
