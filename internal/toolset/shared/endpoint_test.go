package shared

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEndpointJSONSchemaReferencesAndMaps(t *testing.T) {
	var schema Schema
	_ = json.Unmarshal([]byte(`{"type":"object","additionalProperties":false,"required":["body"],"properties":{"body":{"$ref":"#/definitions/node"}},"definitions":{"node":{"type":"object","additionalProperties":false,"required":["kind"],"properties":{"kind":{"type":"integer","enum":[1,2]},"settings":{"type":"object","additionalProperties":{"type":"string"}},"children":{"type":"array","items":{"$ref":"#/definitions/node"}}}}}}`), &schema)
	for _, value := range []struct {
		raw   string
		valid bool
	}{
		{`{"body":{"kind":1,"settings":{"custom":"value"},"children":[{"kind":2}]}}`, true},
		{`{"body":{"kind":"1"}}`, false},
		{`{"body":{"kind":1,"settings":{"custom":3}}}`, false},
		{`{"body":{"kind":1,"children":[{"secret":"do-not-log"}]}}`, false},
		{`{"body":{"kind":1},"headers":{"Authorization":"do-not-log"}}`, false},
	} {
		var args any
		_ = json.Unmarshal([]byte(value.raw), &args)
		err := ValidateEndpointSchema(schema, args)
		if (err == nil) != value.valid {
			t.Fatalf("validation=%v want valid=%v", err, value.valid)
		}
		if err != nil && strings.Contains(err.Error(), "do-not-log") {
			t.Fatal("validation leaked an argument")
		}
	}
}

func TestEndpointPathCannotChangeOperation(t *testing.T) {
	op := Operation{Path: "/api/v2/apps/:key", Transport: &OperationTransport{Kind: "http"}}
	for _, value := range []string{"../settings", "%2fsettings", ".", "..", "a\\b", "a\n"} {
		if _, _, err := EndpointPath(op, map[string]any{"path": map[string]any{"key": value}}); err == nil {
			t.Fatalf("unsafe path accepted: %q", value)
		}
	}
	path, params, err := EndpointPath(op, map[string]any{"path": map[string]any{"key": "my app+中文"}})
	if err != nil || strings.Contains(path, " ") || params["key"] != "my app+中文" {
		t.Fatalf("escaped path=%s params=%v error=%v", path, params, err)
	}
}

func TestEndpointPolicyIncludesWireContract(t *testing.T) {
	op := Operation{ID: "api", Method: "POST", Path: "/api/v2/files/upload", BodySchema: Empty()}
	definition := ToolDefinition{SchemaVersion: 3, InputSchema: Raw(Empty()), Operations: []Operation{op}}
	original := PolicyHash(definition)
	definition.Operations[0].Transport = &OperationTransport{Kind: "http", Encoding: "multipart", FileFields: []string{"file"}}
	endpoint := PolicyHash(definition)
	definition.Operations[0].Transport.Kind = "websocket"
	if original == endpoint || endpoint == PolicyHash(definition) {
		t.Fatal("wire contract change did not invalidate the policy grant")
	}
	encoded := string(Raw(op))
	if strings.Contains(encoded, "transport") {
		t.Fatal("legacy operation JSON and policy hashes must remain unchanged")
	}
}

func TestEndpointExactNumberValidation(t *testing.T) {
	for _, test := range []struct {
		schema, number string
		valid          bool
	}{
		{`{"type":"string"}`, "123", false},
		{`{"type":"integer"}`, "9007199254740993", true},
		{`{"type":"integer"}`, "18446744073709551615", true},
		{`{"type":"integer"}`, "18446744073709551616", false},
		{`{"type":"integer","minimum":1,"maximum":30}`, "31", false},
		{`{"type":"integer","minimum":1,"maximum":30}`, "3e1", true},
		{`{"type":"integer","enum":[1,2]}`, "3", false},
		{`{"type":"integer"}`, "1.0000000000000000001", false},
		{`{"type":"number"}`, "1.25", true},
		{`{"type":"number"}`, "1e999999999", false},
		{`{"type":"number"}`, "1e-999999999", false},
	} {
		var schema Schema
		_ = json.Unmarshal([]byte(test.schema), &schema)
		err := ValidateEndpointSchema(schema, json.Number(test.number))
		if (err == nil) != test.valid {
			t.Fatalf("%s against %s: %v", test.number, test.schema, err)
		}
	}
}
