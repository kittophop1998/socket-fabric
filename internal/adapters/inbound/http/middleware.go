package httpadapter

import (
	"context"
	"crypto/rand"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

const requestIDHeader = "X-Request-ID"

type requestIDKey struct{}

// RequestID returns the identifier assigned by the HTTP request middleware.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

func requestIDMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(requestIDHeader)
		if !validRequestID(id) {
			id = rand.Text()
		}
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), requestIDKey{}, id))
		c.Header(requestIDHeader, id)
		c.Next()
	}
}

func validRequestID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for i := range len(id) {
		ch := id[i]
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '.' || ch == '_' || ch == '-' {
			continue
		}
		return false
	}
	return true
}

func loggingMiddleware(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		started := time.Now()
		defer func() {
			logger.InfoContext(c.Request.Context(), "HTTP request",
				"request_id", RequestID(c.Request.Context()),
				"method", c.Request.Method,
				"route", safeRoute(c),
				"status", c.Writer.Status(),
				"duration_ms", time.Since(started).Milliseconds(),
			)
		}()
		c.Next()
	}
}

func recoveryMiddleware(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if recovered := recover(); recovered != nil {
				// net/http uses this sentinel to abort a response without logging
				// a panic. Preserve that behavior instead of converting it to JSON.
				if recovered == http.ErrAbortHandler {
					panic(recovered)
				}
				logger.ErrorContext(c.Request.Context(), "HTTP panic recovered",
					"request_id", RequestID(c.Request.Context()),
					"method", c.Request.Method,
					"route", safeRoute(c),
				)
				if c.Writer.Written() {
					c.Abort()
					return
				}
				abortWithError(c, http.StatusInternalServerError, "internal_error", "Internal server error")
			}
		}()
		c.Next()
	}
}

func safeRoute(c *gin.Context) string {
	if route := c.FullPath(); route != "" {
		return route
	}
	return "_unmatched"
}
