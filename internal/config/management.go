package config

import (
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Management contains server-only database settings. Never log this struct.
type Management struct {
	Enabled           bool
	DatabaseURL       string
	MaxConns          int32
	ConnectTimeout    time.Duration
	QueryTimeout      time.Duration
	SessionTTL        time.Duration
	MaxProjects       int
	PolicyRefresh     time.Duration
	PolicyMaxAge      time.Duration
	AllowInsecureHTTP bool
	TrustedProxyCIDRs []netip.Prefix
}

// LoadManagement is also used by explicit migration/operator commands; it does
// not require client JWT secrets, publisher keys or an enabled gateway.
func LoadManagement() (Management, error) {
	environment, err := environmentValue(os.LookupEnv, "APP_ENV", "development")
	if err != nil {
		return Management{}, err
	}
	return loadManagement(os.LookupEnv, environment)
}
func loadManagement(lookup func(string) (string, bool), environment string) (Management, error) {
	var c Management
	c.Enabled = true
	raw, ok := lookup("DATABASE_URL")
	if !ok || raw == "" {
		return Management{}, fmt.Errorf("DATABASE_URL is required when management is enabled")
	}
	u, err := url.Parse(raw)
	if err != nil || u == nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Hostname() == "" || u.Path == "" || u.User == nil || u.Fragment != "" {
		return Management{}, fmt.Errorf("DATABASE_URL must be a PostgreSQL connection URL")
	}
	if environment == "production" && u.Query().Get("sslmode") != "verify-full" {
		return Management{}, fmt.Errorf("production DATABASE_URL requires sslmode=verify-full")
	}
	c.DatabaseURL = raw
	for _, values := range u.Query() {
		if len(values) != 1 {
			return Management{}, fmt.Errorf("DATABASE_URL query options must not repeat")
		}
	}
	for _, field := range []struct {
		name, fallback string
		target         *time.Duration
	}{
		{"DATABASE_CONNECT_TIMEOUT", "2s", &c.ConnectTimeout}, {"DATABASE_QUERY_TIMEOUT", "2s", &c.QueryTimeout},
		{"MANAGEMENT_SESSION_TTL", "15m", &c.SessionTTL}, {"MANAGEMENT_POLICY_REFRESH_INTERVAL", "2s", &c.PolicyRefresh}, {"MANAGEMENT_POLICY_MAX_AGE", "5s", &c.PolicyMaxAge},
	} {
		raw, err := environmentValue(lookup, field.name, field.fallback)
		if err != nil {
			return Management{}, err
		}
		duration, err := time.ParseDuration(raw)
		if err != nil || duration <= 0 {
			return Management{}, fmt.Errorf("%s must be a positive duration", field.name)
		}
		*field.target = duration
	}
	if c.SessionTTL > time.Hour {
		return Management{}, fmt.Errorf("MANAGEMENT_SESSION_TTL must not exceed 1h")
	}
	if c.PolicyMaxAge > 5*time.Second || c.PolicyRefresh >= c.PolicyMaxAge || c.QueryTimeout >= c.PolicyMaxAge {
		return Management{}, fmt.Errorf("management policy requires refresh/query timeouts below max age and max age at most 5s")
	}
	for _, field := range []struct {
		name, fallback string
		max            int
		target         *int
	}{
		{"DATABASE_MAX_CONNS", "8", 64, nil}, {"MANAGEMENT_MAX_PROJECTS", "128", 10000, &c.MaxProjects},
	} {
		raw, err := environmentValue(lookup, field.name, field.fallback)
		if err != nil {
			return Management{}, err
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > field.max {
			return Management{}, fmt.Errorf("%s must be between 1 and %d", field.name, field.max)
		}
		if field.target != nil {
			*field.target = n
		} else {
			c.MaxConns = int32(n)
		}
	}
	raw, err = environmentValue(lookup, "MANAGEMENT_ALLOW_INSECURE_HTTP", "false")
	if err != nil {
		return Management{}, err
	}
	if raw != "false" && raw != "true" {
		return Management{}, fmt.Errorf("MANAGEMENT_ALLOW_INSECURE_HTTP must be true or false")
	}
	c.AllowInsecureHTTP = raw == "true"
	if environment != "development" && environment != "test" && environment != "production" {
		return Management{}, fmt.Errorf("APP_ENV is invalid")
	}
	if environment == "production" && c.AllowInsecureHTTP {
		return Management{}, fmt.Errorf("insecure management HTTP is forbidden in production")
	}
	if raw, ok := lookup("MANAGEMENT_TRUSTED_PROXY_CIDRS"); ok && raw != "" {
		for _, entry := range strings.Split(raw, ",") {
			prefix, err := netip.ParsePrefix(strings.TrimSpace(entry))
			if err != nil {
				return Management{}, fmt.Errorf("MANAGEMENT_TRUSTED_PROXY_CIDRS must contain valid CIDRs")
			}
			if prefix.Bits() == 0 {
				return Management{}, fmt.Errorf("management proxy trust must not include every address")
			}
			c.TrustedProxyCIDRs = append(c.TrustedProxyCIDRs, prefix.Masked())
		}
	}
	return c, nil
}
