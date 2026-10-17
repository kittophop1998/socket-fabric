// Package httpadapter implements the service's inbound HTTP transport.
package httpadapter

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/gin-gonic/gin"
)

// NewRouter wires operational endpoints and the shared HTTP conventions.
// isReady must be safe to call concurrently with server lifecycle changes.
func NewRouter(logger *slog.Logger, isReady func() bool, gateway *Gateway, management ...*ManagementHandler) (*gin.Engine, error) {
	if logger == nil {
		return nil, errors.New("HTTP logger is required")
	}
	if isReady == nil {
		return nil, errors.New("HTTP readiness check is required")
	}

	router := gin.New()
	// Redirects run before middleware in Gin. Use the shared 404 response so
	// trailing-slash requests still receive correlation headers and logs.
	router.RedirectTrailingSlash = false
	if err := router.SetTrustedProxies(nil); err != nil {
		return nil, fmt.Errorf("disable trusted proxies: %w", err)
	}
	router.HandleMethodNotAllowed = true
	router.Use(requestIDMiddleware(), loggingMiddleware(logger), recoveryMiddleware(logger))

	router.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	router.GET("/readyz", func(c *gin.Context) {
		if !isReady() {
			abortWithError(c, http.StatusServiceUnavailable, "not_ready", "Service not ready")
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ready"})
	})
	if gateway != nil {
		gateway.registerRoutes(router, isReady)
	}
	if len(management) > 1 {
		return nil, errors.New("only one management handler is supported")
	}
	if len(management) == 1 && management[0] != nil {
		management[0].registerRoutes(router)
	}
	router.NoRoute(func(c *gin.Context) {
		abortWithError(c, http.StatusNotFound, "not_found", "Route not found")
	})
	router.NoMethod(func(c *gin.Context) {
		// Operational routes have fixed paths. Keep Allow consistent with the
		// registered methods without maintaining another list of endpoints.
		var allowed []string
		for _, route := range router.Routes() {
			if routeMatches(route.Path, c.Request.URL.Path) {
				allowed = append(allowed, route.Method)
			}
		}
		slices.Sort(allowed)
		c.Header("Allow", strings.Join(slices.Compact(allowed), ", "))
		abortWithError(c, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed")
	})
	return router, nil
}

func routeMatches(pattern, path string) bool {
	left, right := strings.Split(pattern, "/"), strings.Split(path, "/")
	if len(left) != len(right) {
		return false
	}
	for i, segment := range left {
		if strings.HasPrefix(segment, ":") && right[i] != "" {
			continue
		}
		if segment != right[i] {
			return false
		}
	}
	return true
}

type errorResponse struct {
	Error apiError `json:"error"`
}

type apiError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"requestId"`
}

func abortWithError(c *gin.Context, status int, code, message string) {
	c.Header("Content-Type", "application/json; charset=utf-8")
	c.AbortWithStatusJSON(status, errorResponse{Error: apiError{
		Code:      code,
		Message:   message,
		RequestID: RequestID(c.Request.Context()),
	}})
}
