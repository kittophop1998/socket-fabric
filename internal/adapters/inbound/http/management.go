package httpadapter

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"socketfabric/internal/application/usecase"
	"socketfabric/internal/domain"
)

type ManagementOptions struct {
	AllowInsecureHTTP bool
	TrustedProxyCIDRs []netip.Prefix
	Ready             func() bool
}
type ManagementHandler struct {
	service *usecase.Management
	opts    ManagementOptions
	limiter *loginLimiter
}

func NewManagementHandler(service *usecase.Management, opts ManagementOptions) (*ManagementHandler, error) {
	if service == nil || opts.Ready == nil {
		return nil, domain.ErrInvalid
	}
	opts.TrustedProxyCIDRs = append([]netip.Prefix(nil), opts.TrustedProxyCIDRs...)
	return &ManagementHandler{service: service, opts: opts, limiter: &loginLimiter{entries: map[string]loginWindow{}}}, nil
}

func (h *ManagementHandler) trusted(ip netip.Addr) bool {
	for _, prefix := range h.opts.TrustedProxyCIDRs {
		if prefix.Contains(ip.Unmap()) {
			return true
		}
	}
	return false
}
func (h *ManagementHandler) peer(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}
	}
	ip, _ := netip.ParseAddr(host)
	return ip.Unmap()
}
func (h *ManagementHandler) remoteIP(r *http.Request) string {
	ip := h.peer(r)
	if !ip.IsValid() {
		return "unknown"
	}
	if h.trusted(ip) {
		parts := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
		if len(parts) <= 16 {
			for i := len(parts) - 1; i >= 0; i-- {
				candidate, err := netip.ParseAddr(strings.TrimSpace(parts[i]))
				if err != nil {
					return ip.String()
				}
				if !h.trusted(candidate) || i == 0 {
					return candidate.Unmap().String()
				}
			}
		}
	}
	return ip.String()
}

func (h *ManagementHandler) registerRoutes(router *gin.Engine) {
	group := router.Group("/admin/v1")
	group.Use(func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.Header("X-Content-Type-Options", "nosniff")
		forwarded := c.Request.Header.Values("X-Forwarded-Proto")
		secure := c.Request.TLS != nil || h.trusted(h.peer(c.Request)) && len(forwarded) == 1 && forwarded[0] == "https"
		if !secure && !h.opts.AllowInsecureHTTP {
			abortWithError(c, http.StatusForbidden, "https_required", "Management requires HTTPS")
			return
		}
		if !h.opts.Ready() {
			abortWithError(c, http.StatusServiceUnavailable, "not_ready", "Management unavailable")
			return
		}
		c.Next()
	})
	group.POST("/auth/login", h.login)
	authenticated := group.Group("")
	authenticated.Use(h.authenticate)
	authenticated.POST("/auth/logout", func(c *gin.Context) {
		adminRespond(c, http.StatusNoContent, nil, h.service.Logout(c.Request.Context(), managementActor(c), RequestID(c.Request.Context())))
	})
	authenticated.GET("/auth/me", func(c *gin.Context) {
		result, err := h.service.Me(c.Request.Context(), managementActor(c))
		adminRespond(c, http.StatusOK, result, err)
	})
	authenticated.GET("/projects", func(c *gin.Context) {
		o, err := managementList(c, "status", "name")
		if err != nil {
			adminRespond(c, 0, nil, err)
			return
		}
		result, err := h.service.ListProjects(c.Request.Context(), managementActor(c), o)
		adminRespond(c, http.StatusOK, result, err)
	})
	authenticated.GET("/projects/:id", func(c *gin.Context) {
		result, err := h.service.Project(c.Request.Context(), managementActor(c), c.Param("id"))
		adminRespond(c, http.StatusOK, result, err)
	})
	authenticated.GET("/diagnostics/overview", func(c *gin.Context) {
		result, err := h.service.Diagnostics(c.Request.Context(), managementActor(c))
		adminRespond(c, http.StatusOK, result, err)
	})
	authenticated.GET("/diagnostics/nodes", func(c *gin.Context) {
		result, err := h.service.Diagnostics(c.Request.Context(), managementActor(c))
		if err != nil {
			adminRespond(c, 0, nil, err)
			return
		}
		adminRespond(c, http.StatusOK, gin.H{"scope": "local", "clusterVisibility": "unavailable", "items": []any{result["node"]}}, nil)
	})
	authenticated.GET("/diagnostics/metrics", func(c *gin.Context) {
		result, err := h.service.Diagnostics(c.Request.Context(), managementActor(c))
		if err != nil {
			adminRespond(c, 0, nil, err)
			return
		}
		adminRespond(c, http.StatusOK, gin.H{"scope": "local", "collectedAt": result["collectedAt"], "metrics": result["metrics"], "historicalMetrics": "unavailable"}, nil)
	})
	admin := authenticated.Group("")
	admin.Use(h.requireAdmin)
	admin.POST("/projects", func(c *gin.Context) {
		var input struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Status string `json:"status"`
		}
		if !adminJSON(c, &input) {
			return
		}
		result, err := h.service.CreateProject(c.Request.Context(), managementActor(c), domain.Project{ID: input.ID, Name: input.Name, Status: input.Status}, RequestID(c.Request.Context()))
		adminRespond(c, http.StatusCreated, result, err)
	})
	admin.PATCH("/projects/:id", func(c *gin.Context) {
		var input struct {
			Name   *string `json:"name"`
			Status *string `json:"status"`
		}
		if !adminJSON(c, &input) {
			return
		}
		result, err := h.service.UpdateProject(c.Request.Context(), managementActor(c), c.Param("id"), input.Name, input.Status, RequestID(c.Request.Context()))
		adminRespond(c, http.StatusOK, result, err)
	})
	admin.GET("/projects/:id/keys", func(c *gin.Context) {
		o, err := managementList(c)
		if err != nil {
			adminRespond(c, 0, nil, err)
			return
		}
		result, err := h.service.ListKeys(c.Request.Context(), managementActor(c), c.Param("id"), o)
		adminRespond(c, http.StatusOK, result, err)
	})
	admin.POST("/projects/:id/keys", func(c *gin.Context) {
		var input struct {
			ExpiresAt *time.Time `json:"expiresAt"`
		}
		if !adminJSON(c, &input) {
			return
		}
		result, err := h.service.CreateKey(c.Request.Context(), managementActor(c), c.Param("id"), input.ExpiresAt, RequestID(c.Request.Context()))
		adminRespond(c, http.StatusCreated, result, err)
	})
	admin.DELETE("/projects/:id/keys/:keyId", func(c *gin.Context) {
		adminRespond(c, http.StatusNoContent, nil, h.service.RevokeKey(c.Request.Context(), managementActor(c), c.Param("id"), c.Param("keyId"), RequestID(c.Request.Context())))
	})
	admin.GET("/audit-logs", func(c *gin.Context) {
		o, err := managementList(c, "action", "actorId")
		if err != nil {
			adminRespond(c, 0, nil, err)
			return
		}
		result, err := h.service.ListAudit(c.Request.Context(), managementActor(c), o)
		adminRespond(c, http.StatusOK, result, err)
	})
}

const managementActorKey = "socketfabric.management.actor"

func managementActor(c *gin.Context) domain.OperatorActor {
	actor, _ := c.Get(managementActorKey)
	value, _ := actor.(domain.OperatorActor)
	return value
}
func (h *ManagementHandler) authenticate(c *gin.Context) {
	parts := strings.Fields(c.GetHeader("Authorization"))
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		c.Header("WWW-Authenticate", "Bearer")
		abortWithError(c, http.StatusUnauthorized, "unauthorized", "Operator authentication required")
		return
	}
	actor, err := h.service.AuthenticateOperator(c.Request.Context(), parts[1])
	if err != nil {
		adminRespond(c, 0, nil, err)
		return
	}
	c.Set(managementActorKey, actor)
	c.Next()
}
func (h *ManagementHandler) requireAdmin(c *gin.Context) {
	if err := h.service.Authorize(c.Request.Context(), managementActor(c), true, RequestID(c.Request.Context())); err != nil {
		adminRespond(c, 0, nil, err)
		return
	}
	c.Next()
}
func (h *ManagementHandler) login(c *gin.Context) {
	var input struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !adminJSON(c, &input) {
		return
	}
	if !h.limiter.allow(h.remoteIP(c.Request), input.Email, time.Now()) {
		c.Header("Retry-After", "60")
		adminRespond(c, 0, nil, domain.ErrRateLimited)
		return
	}
	result, err := h.service.Login(c.Request.Context(), input.Email, input.Password, RequestID(c.Request.Context()))
	input.Password = ""
	adminRespond(c, http.StatusOK, result, err)
}

func adminJSON(c *gin.Context, value any) bool {
	mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || mediaType != "application/json" {
		abortWithError(c, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
		return false
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16<<10)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decodeOne(decoder, value); err != nil {
		var oversized *http.MaxBytesError
		if errors.As(err, &oversized) {
			abortWithError(c, http.StatusRequestEntityTooLarge, "payload_too_large", "Management body exceeds limit")
		} else {
			abortWithError(c, http.StatusBadRequest, "invalid_request", "Invalid management JSON")
		}
		return false
	}
	return true
}
func adminRespond(c *gin.Context, status int, result any, err error) {
	if err != nil {
		status, code, message := applicationError(err)
		switch {
		case errors.Is(err, domain.ErrResourceNotFound):
			status, code, message = http.StatusNotFound, "not_found", "Resource not found"
		case errors.Is(err, domain.ErrConflict):
			status, code, message = http.StatusConflict, "conflict", "Resource conflicts with existing state or capacity"
		case errors.Is(err, domain.ErrRateLimited):
			status, code, message = http.StatusTooManyRequests, "rate_limited", "Too many authentication attempts"
			if c.Writer.Header().Get("Retry-After") == "" {
				c.Header("Retry-After", "60")
			}
		}
		if status == http.StatusUnauthorized {
			c.Header("WWW-Authenticate", "Bearer")
		}
		abortWithError(c, status, code, message)
		return
	}
	if status == http.StatusNoContent {
		c.Status(status)
		return
	}
	c.JSON(status, result)
}
func managementList(c *gin.Context, filters ...string) (domain.ListOptions, error) {
	allowed := map[string]bool{"limit": true, "cursor": true}
	for _, key := range filters {
		allowed[key] = true
	}
	for key, values := range c.Request.URL.Query() {
		if !allowed[key] || len(values) != 1 {
			return domain.ListOptions{}, domain.ErrInvalid
		}
	}
	o := domain.ListOptions{Limit: 25, After: c.Query("cursor"), Status: c.Query("status"), Name: c.Query("name"), Action: c.Query("action"), ActorID: c.Query("actorId")}
	if value, ok := c.GetQuery("limit"); ok {
		limit, err := strconv.Atoi(value)
		if err != nil {
			return o, domain.ErrInvalid
		}
		o.Limit = limit
	}
	return o, nil
}

type loginWindow struct {
	started time.Time
	count   int
}
type loginLimiter struct {
	mu      sync.Mutex
	entries map[string]loginWindow
}

func (l *loginLimiter) allow(ip, email string, now time.Time) bool {
	digest := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email))))
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.entries) >= 4094 {
		for key, window := range l.entries {
			if now.Sub(window.started) >= 15*time.Minute {
				delete(l.entries, key)
			}
		}
	}
	checks := []struct {
		key    string
		window time.Duration
		limit  int
	}{{"ip:" + ip, time.Minute, 10}, {"account:" + hex.EncodeToString(digest[:]), 15 * time.Minute, 5}}
	added := 0
	for _, check := range checks {
		w, ok := l.entries[check.key]
		if ok && now.Sub(w.started) < check.window && w.count >= check.limit {
			return false
		}
		if !ok {
			added++
		}
	}
	if len(l.entries)+added > 4096 {
		return false
	}
	for _, check := range checks {
		w := l.entries[check.key]
		if w.started.IsZero() || now.Sub(w.started) >= check.window {
			w = loginWindow{started: now}
		}
		w.count++
		l.entries[check.key] = w
	}
	return true
}
