package config

import (
	"strings"
	"testing"
	"time"
)

func TestManagementConfiguration(t *testing.T) {
	base := map[string]string{"MANAGEMENT_ENABLED": "true", "DATABASE_URL": "postgres://operator:secret@localhost:5432/socketfabric?sslmode=disable", "PUBLISHER_KEYS_JSON": "{}"}
	cfg, err := load(lookup(base))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Management.Enabled || len(cfg.Auth.PublisherKeys) != 0 || cfg.Management.SessionTTL != 15*time.Minute || cfg.Management.MaxProjects != 128 {
		t.Fatal("unexpected management defaults")
	}
	for name, value := range map[string]string{"DATABASE_URL": "http://credential-must-not-leak", "DATABASE_MAX_CONNS": "0", "DATABASE_QUERY_TIMEOUT": "5s", "MANAGEMENT_POLICY_MAX_AGE": "6s", "MANAGEMENT_POLICY_REFRESH_INTERVAL": "5s", "MANAGEMENT_SESSION_TTL": "2h", "MANAGEMENT_TRUSTED_PROXY_CIDRS": "0.0.0.0/0", "MANAGEMENT_ALLOW_INSECURE_HTTP": "yes"} {
		t.Run(name, func(t *testing.T) {
			values := map[string]string{}
			for k, v := range base {
				values[k] = v
			}
			values[name] = value
			_, err := load(lookup(values))
			if err == nil || strings.Contains(err.Error(), "credential-must-not-leak") {
				t.Fatal("expected sanitized validation error")
			}
		})
	}
	base["APP_ENV"] = "production"
	if _, err := load(lookup(base)); err == nil {
		t.Fatal("production accepted plaintext DB")
	}
	base["DATABASE_URL"] = "postgres://operator:secret@db.example:5432/socketfabric?sslmode=verify-full"
	if _, err := load(lookup(base)); err != nil {
		t.Fatal(err)
	}
	base["MANAGEMENT_ALLOW_INSECURE_HTTP"] = "true"
	if _, err := load(lookup(base)); err == nil {
		t.Fatal("production accepted insecure management")
	}
}
func TestDisabledManagementIgnoresDatabase(t *testing.T) {
	cfg, err := load(lookup(map[string]string{"DATABASE_URL": "invalid", "MANAGEMENT_ALLOW_INSECURE_HTTP": "invalid"}))
	if err != nil || cfg.Management.Enabled || cfg.Management.DatabaseURL != "" {
		t.Fatal("disabled mode changed")
	}
	if _, err := load(lookup(map[string]string{"MANAGEMENT_ENABLED": "yes"})); err == nil {
		t.Fatal("invalid toggle accepted")
	}
}
