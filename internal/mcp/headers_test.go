package mcp

import (
	"encoding/json"
	"testing"
)

func TestParseHeaderBindingsAllowsNestedObjectProperties(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"options":{"type":"object","properties":{"region":{"type":"string","x-mcp-header":"Region"}}}}}`)
	bindings, valid := parseHeaderBindings(schema)
	if !valid || len(bindings) != 1 {
		t.Fatalf("parseHeaderBindings() = %#v, %v", bindings, valid)
	}
	if got := bindings[0].String(); got != "options.region=Region" {
		t.Fatalf("binding = %q", got)
	}
}

func TestParseHeaderBindingsRejectsAnnotationsOutsideStaticPropertyPaths(t *testing.T) {
	tests := []struct {
		name   string
		schema string
	}{
		{"root annotation", `{"type":"object","x-mcp-header":"Region"}`},
		{"array item", `{"type":"object","properties":{"tags":{"type":"array","items":{"type":"string","x-mcp-header":"Tag"}}}}`},
		{"composition", `{"type":"object","properties":{"region":{"oneOf":[{"type":"string","x-mcp-header":"Region"}]}}}`},
		{"number type", `{"type":"object","properties":{"region":{"type":"number","x-mcp-header":"Region"}}}`},
		{"duplicate names", `{"type":"object","properties":{"a":{"type":"string","x-mcp-header":"Tenant"},"b":{"type":"boolean","x-mcp-header":"tenant"}}}`},
		{"invalid field name", `{"type":"object","properties":{"region":{"type":"string","x-mcp-header":"bad name"}}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, valid := parseHeaderBindings(json.RawMessage(test.schema)); valid {
				t.Fatal("expected invalid annotation to reject the schema")
			}
		})
	}
}

func TestEncodeHeaderParameterUsesProtocolEncoding(t *testing.T) {
	tests := []struct {
		value    any
		typeName string
		want     string
	}{
		{json.Number("42"), "integer", "42"},
		{true, "boolean", "true"},
		{"west-1", "string", "west-1"},
		{"上海", "string", "=?base64?5LiK5rW3?="},
		{" padded ", "string", "=?base64?IHBhZGRlZCA=?="},
	}
	for _, test := range tests {
		got, ok := encodeHeaderParameter(test.value, test.typeName)
		if !ok || got != test.want {
			t.Errorf("encodeHeaderParameter(%v, %q) = %q, %v; want %q", test.value, test.typeName, got, ok, test.want)
		}
	}
	if _, ok := encodeHeaderParameter(json.Number("9007199254740992"), "integer"); ok {
		t.Fatal("expected unsafe JavaScript integer to be rejected")
	}
}

func TestValidateElicitationResponseByMode(t *testing.T) {
	if err := validateElicitationResponse(json.RawMessage(`{"action":"accept","content":{"name":"ada"}}`), json.RawMessage(`{"mode":"form"}`)); err != nil {
		t.Fatalf("valid form response rejected: %v", err)
	}
	if err := validateElicitationResponse(json.RawMessage(`{"action":"accept"}`), json.RawMessage(`{"mode":"url","url":"https://example.test"}`)); err != nil {
		t.Fatalf("valid URL response rejected: %v", err)
	}
	if err := validateElicitationResponse(json.RawMessage(`{"action":"accept"}`), json.RawMessage(`{"mode":"form"}`)); err == nil {
		t.Fatal("form accept without content was accepted")
	}
	if err := validateElicitationResponse(json.RawMessage(`{"action":"later"}`), json.RawMessage(`{"mode":"url"}`)); err == nil {
		t.Fatal("unsupported action was accepted")
	}
}
