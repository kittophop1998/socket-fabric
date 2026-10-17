//go:build contracttests || (integration && postgres)

// Package contracts provides isolated JSON Schema validation for contract-tagged tests.
package contracts

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

type Validator struct {
	schemas map[string]*jsonschema.Schema
}

// New compiles referenced schemas locally. Remote schema fetches are rejected.
func New(apiDir string) (*Validator, error) {
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	compiler.UseLoader(localLoader{allowedDir: filepath.Clean(apiDir)})

	openAPI, openAPIURL, err := addYAML(compiler, filepath.Join(apiDir, "openapi.yaml"))
	if err != nil {
		return nil, fmt.Errorf("load OpenAPI contract: %w", err)
	}
	asyncAPI, asyncAPIURL, err := addYAML(compiler, filepath.Join(apiDir, "asyncapi.yaml"))
	if err != nil {
		return nil, fmt.Errorf("load AsyncAPI contract: %w", err)
	}
	shared, err := os.ReadFile(filepath.Join(apiDir, "schemas.json"))
	if err != nil {
		return nil, fmt.Errorf("load shared schemas: %w", err)
	}
	sharedDoc, err := jsonschema.UnmarshalJSON(bytes.NewReader(shared))
	if err != nil {
		return nil, fmt.Errorf("parse shared schemas: %w", err)
	}
	sharedURL := fileURL(filepath.Join(apiDir, "schemas.json"))
	if err := compiler.AddResource(sharedURL, sharedDoc); err != nil {
		return nil, fmt.Errorf("register shared schemas: %w", err)
	}
	if openAPI == nil || asyncAPI == nil {
		return nil, errors.New("contract documents are empty")
	}

	locations := map[string]string{
		"Event":                   sharedURL + "#/definitions/Event",
		"APIError":                sharedURL + "#/definitions/APIError",
		"PublishRequest":          openAPIURL + "#/components/schemas/PublishRequest",
		"PublishResult":           openAPIURL + "#/components/schemas/PublishResult",
		"ErrorResponse":           openAPIURL + "#/components/schemas/ErrorResponse",
		"Health":                  openAPIURL + "#/components/schemas/Health",
		"Readiness":               openAPIURL + "#/components/schemas/Readiness",
		"LoginRequest":            openAPIURL + "#/components/schemas/LoginRequest",
		"LoginResult":             openAPIURL + "#/components/schemas/LoginResult",
		"Project":                 openAPIURL + "#/components/schemas/Project",
		"ProjectCredential":       openAPIURL + "#/components/schemas/PublisherCredential",
		"CreateProjectRequest":    openAPIURL + "#/components/schemas/CreateProjectRequest",
		"UpdateProjectRequest":    openAPIURL + "#/components/schemas/UpdateProjectRequest",
		"PublisherCredential":     openAPIURL + "#/components/schemas/PublisherCredential",
		"CreateCredentialRequest": openAPIURL + "#/components/schemas/CreateCredentialRequest",
		"CreateCredentialResult":  openAPIURL + "#/components/schemas/CreateCredentialResult",
		"AuditLog":                openAPIURL + "#/components/schemas/AuditLog",
		"DiagnosticsOverview":     openAPIURL + "#/components/schemas/DiagnosticsOverview",
		"DiagnosticsNodes":        openAPIURL + "#/components/schemas/DiagnosticsNodes",
		"DiagnosticsMetrics":      openAPIURL + "#/components/schemas/DiagnosticsMetrics",
		"WSAuth":                  asyncAPIURL + "#/components/messages/Auth/payload",
		"WSJoin":                  asyncAPIURL + "#/components/messages/Join/payload",
		"WSLeave":                 asyncAPIURL + "#/components/messages/Leave/payload",
		"WSAuthenticated":         asyncAPIURL + "#/components/messages/Authenticated/payload",
		"WSJoined":                asyncAPIURL + "#/components/messages/Joined/payload",
		"WSLeft":                  asyncAPIURL + "#/components/messages/Left/payload",
		"WSEvent":                 asyncAPIURL + "#/components/messages/EventDelivery/payload",
		"WSError":                 asyncAPIURL + "#/components/messages/ProtocolError/payload",
	}
	schemas := make(map[string]*jsonschema.Schema, len(locations))
	for name, location := range locations {
		schema, err := compiler.Compile(location)
		if err != nil {
			return nil, fmt.Errorf("compile contract schema %s: %w", name, err)
		}
		schemas[name] = schema
	}
	return &Validator{schemas: schemas}, nil
}

func addYAML(compiler *jsonschema.Compiler, path string) (any, string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	var document any
	if err := yaml.Unmarshal(body, &document); err != nil {
		return nil, "", err
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return nil, "", err
	}
	normalized, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
	if err != nil {
		return nil, "", err
	}
	location := fileURL(path)
	if err := compiler.AddResource(location, normalized); err != nil {
		return nil, "", err
	}
	return normalized, location, nil
}

func fileURL(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		absolute = filepath.Clean(path)
	}
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(absolute)}).String()
}

type localLoader struct{ allowedDir string }

func (l localLoader) Load(raw string) (any, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "file" || parsed.Host != "" {
		return nil, fmt.Errorf("non-local contract reference rejected")
	}
	path := filepath.Clean(filepath.FromSlash(parsed.Path))
	if filepath.Dir(path) != l.allowedDir || filepath.Ext(path) != ".json" {
		return nil, fmt.Errorf("contract reference is outside the schema directory")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, 1<<20))
	if err != nil {
		return nil, err
	}
	if len(body) >= 1<<20 {
		return nil, errors.New("contract schema exceeds size limit")
	}
	return jsonschema.UnmarshalJSON(strings.NewReader(string(body)))
}

// ValidateJSON parses and validates one request, response or WebSocket frame.
// Errors contain schema locations only, never the supplied JSON value.
func (v *Validator) ValidateJSON(schema string, body []byte) error {
	compiled, ok := v.schemas[schema]
	if !ok {
		return fmt.Errorf("unknown contract schema %q", schema)
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("invalid JSON for contract schema %s", schema)
	}
	if err := compiled.Validate(value); err != nil {
		if validation, ok := err.(*jsonschema.ValidationError); ok {
			return fmt.Errorf("%s: %s", schema, validation.BasicOutput().KeywordLocation)
		}
		return fmt.Errorf("%s: schema validation failed", schema)
	}
	return nil
}
