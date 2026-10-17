//go:build contracttests

package contracts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/goccy/go-yaml"
)

func TestPublicContractExamplesValidate(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	apiDir := filepath.Clean(filepath.Join(filepath.Dir(file), "../../../api"))
	validator, err := New(apiDir)
	if err != nil {
		t.Fatalf("compile contracts: %v", err)
	}

	valid := map[string]string{
		"Event":                   `{"eventId":"evt_01","projectId":"project-a","type":"resource.updated","roomId":"room-123","data":{"status":"completed"},"timestamp":"2026-10-16T10:00:00Z"}`,
		"PublishRequest":          `{"projectId":"project-a","roomId":"room-123","type":"resource.updated","data":{"status":"completed"}}`,
		"PublishResult":           `{"event":{"eventId":"evt_01","projectId":"project-a","type":"resource.updated","roomId":"room-123","data":null,"timestamp":"2026-10-16T10:00:00Z"},"queuedConnections":0}`,
		"ErrorResponse":           `{"error":{"code":"unauthorized","message":"Authentication required","requestId":"req_01"}}`,
		"LoginRequest":            `{"email":"admin@example.com","password":"example-password"}`,
		"LoginResult":             `{"token":"opaque-session","tokenType":"Bearer","expiresAt":"2026-10-16T10:15:00Z","operator":{"id":"operator-1","email":"admin@example.com","role":"admin","status":"active","createdAt":"2026-10-16T10:00:00Z","updatedAt":"2026-10-16T10:00:00Z"}}`,
		"Project":                 `{"id":"project-a","name":"Project A","status":"enabled","createdAt":"2026-10-16T10:00:00Z","updatedAt":"2026-10-16T10:00:00Z"}`,
		"CreateProjectRequest":    `{"id":"project-a","name":"Project A"}`,
		"UpdateProjectRequest":    `{"status":"disabled"}`,
		"ProjectCredential":       `{"id":"key-1","projectId":"project-a","keyIdentifier":"key-prefix","status":"active","createdAt":"2026-10-16T10:00:00Z","expiresAt":null,"revokedAt":null}`,
		"CreateCredentialRequest": `{}`,
		"CreateCredentialResult":  `{"credential":{"id":"key-1","projectId":"project-a","keyIdentifier":"key-prefix","status":"active","createdAt":"2026-10-16T10:00:00Z","expiresAt":null,"revokedAt":null},"key":"one-time-secret"}`,
		"AuditLog":                `{"id":"audit-1","actorId":null,"action":"credential.created","resourceType":"publisher_credential","resourceId":"key-1","timestamp":"2026-10-16T10:00:00Z","metadata":{}}`,
		"WSAuth":                  `{"op":"auth","token":"short-lived-client-jwt"}`,
		"WSJoin":                  `{"op":"join","roomId":"room-123"}`,
		"WSLeave":                 `{"op":"leave","roomId":"room-123"}`,
		"WSAuthenticated":         `{"op":"authenticated","connectionId":"conn_01","projectId":"project-a","userId":"user-123"}`,
		"WSJoined":                `{"op":"joined","roomId":"room-123"}`,
		"WSLeft":                  `{"op":"left","roomId":"room-123"}`,
		"WSEvent":                 `{"op":"event","event":{"eventId":"evt_01","projectId":"project-a","type":"resource.updated","roomId":"room-123","data":{"status":"completed"},"timestamp":"2026-10-16T10:00:00Z"}}`,
		"WSError":                 `{"op":"error","error":{"code":"forbidden","message":"Operation not allowed for this scope","requestId":"req_01"}}`,
	}
	for schema, value := range valid {
		t.Run(schema, func(t *testing.T) {
			if err := validator.ValidateJSON(schema, []byte(value)); err != nil {
				t.Fatalf("documented frame/example rejected: %v", err)
			}
		})
	}
	validateDocumentedExamples(t, validator, apiDir)

	invalid := map[string]string{
		"PublishRequest": `{"projectId":"project-a","roomId":"room-123","type":"resource.updated","data":{},"unexpected":true}`,
		"WSAuth":         `{"op":"auth","token":"secret","unexpected":true}`,
		"WSJoin":         `{"op":"join","roomId":"room-123","token":"not-empty"}`,
		"WSEvent":        `{"op":"event","event":{"projectId":"project-a"}}`,
	}
	for schema, value := range invalid {
		t.Run("reject_"+schema, func(t *testing.T) {
			if err := validator.ValidateJSON(schema, []byte(value)); err == nil {
				t.Fatal("invalid frame unexpectedly passed validation")
			}
		})
	}
}

func validateDocumentedExamples(t *testing.T, validator *Validator, apiDir string) {
	t.Helper()
	var openAPI map[string]any
	openAPIBody, err := os.ReadFile(filepath.Join(apiDir, "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(openAPIBody, &openAPI); err != nil {
		t.Fatalf("parse OpenAPI examples: %v", err)
	}
	paths := openAPI["paths"].(map[string]any)
	for path, rawPath := range paths {
		for method, rawOperation := range rawPath.(map[string]any) {
			operation, ok := rawOperation.(map[string]any)
			if !ok {
				continue
			}
			requestBody, ok := operation["requestBody"].(map[string]any)
			if ok {
				validateOpenAPIContentExamples(t, validator, requestBody["content"], method+" "+path+" request")
			}
			if responses, ok := operation["responses"].(map[string]any); ok {
				for status, rawResponse := range responses {
					response, ok := rawResponse.(map[string]any)
					if ok {
						validateOpenAPIContentExamples(t, validator, response["content"], method+" "+path+" response "+status)
					}
				}
			}
		}
	}
	components := openAPI["components"].(map[string]any)
	if responses, ok := components["responses"].(map[string]any); ok {
		for name, rawResponse := range responses {
			response, ok := rawResponse.(map[string]any)
			if ok {
				validateOpenAPIContentExamples(t, validator, response["content"], "response component "+name)
			}
		}
	}

	var asyncAPI map[string]any
	asyncAPIBody, err := os.ReadFile(filepath.Join(apiDir, "asyncapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(asyncAPIBody, &asyncAPI); err != nil {
		t.Fatalf("parse AsyncAPI examples: %v", err)
	}
	messages := asyncAPI["components"].(map[string]any)["messages"].(map[string]any)
	for name, rawMessage := range messages {
		message := rawMessage.(map[string]any)
		schema := map[string]string{
			"Auth": "WSAuth", "Join": "WSJoin", "Leave": "WSLeave", "Authenticated": "WSAuthenticated",
			"Joined": "WSJoined", "Left": "WSLeft", "EventDelivery": "WSEvent", "ProtocolError": "WSError",
		}[name]
		if schema == "" {
			continue
		}
		for _, rawExample := range message["examples"].([]any) {
			example := rawExample.(map[string]any)
			assertContractExample(t, validator, schema, example["payload"], "AsyncAPI message "+name)
		}
	}
}

func validateOpenAPIContentExamples(t *testing.T, validator *Validator, rawContent any, source string) {
	t.Helper()
	content, ok := rawContent.(map[string]any)
	if !ok {
		return
	}
	for _, rawMedia := range content {
		media, ok := rawMedia.(map[string]any)
		if !ok {
			continue
		}
		schema := mediaSchemaName(media)
		if schema == "" {
			continue
		}
		if sample, found := media["example"]; found {
			assertContractExample(t, validator, schema, sample, source)
		}
		if examples, ok := media["examples"].(map[string]any); ok {
			for name, rawExample := range examples {
				example, ok := rawExample.(map[string]any)
				if ok {
					if sample, found := example["value"]; found {
						assertContractExample(t, validator, schema, sample, source+" example "+name)
					}
				}
			}
		}
	}
}

func mediaSchemaName(media map[string]any) string {
	schema, ok := media["schema"].(map[string]any)
	if !ok {
		return ""
	}
	ref, _ := schema["$ref"].(string)
	const prefix = "#/components/schemas/"
	if len(ref) > len(prefix) && ref[:len(prefix)] == prefix {
		return ref[len(prefix):]
	}
	return ""
}

func assertContractExample(t *testing.T, validator *Validator, schema string, sample any, source string) {
	t.Helper()
	body, err := json.Marshal(sample)
	if err != nil {
		t.Fatalf("encode example from %s: %v", source, err)
	}
	if err := validator.ValidateJSON(schema, body); err != nil {
		t.Fatalf("documented example in %s violates %s: %v", source, schema, err)
	}
}
