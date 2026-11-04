// Package catalog exposes the complete pinned Swagger contract, grouped by the
// panel's API tags. The original convenience tools retain their public contracts.
package catalog

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/1Panel-dev/mcp-1panel/internal/toolset/shared"
)

//go:generate python ../../../scripts/generate-api-tools.py

//go:embed catalog.json
var document []byte

// Tools builds fixed operation tools from the generated, reviewable manifest.
// Nothing is loaded from the network or the working directory at runtime.
func Tools() []shared.ToolSpec {
	var catalog struct {
		Tools []struct {
			Name          string           `json:"name"`
			Summary       string           `json:"summary"`
			Category      string           `json:"category"`
			CategoryOrder int              `json:"categoryOrder"`
			Operation     shared.Operation `json:"operation"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(document, &catalog); err != nil {
		panic(err)
	}
	tools := make([]shared.ToolSpec, 0, len(catalog.Tools))
	for _, entry := range catalog.Tools {
		category := shared.Category{ID: "api_" + strings.ReplaceAll(strings.ToLower(entry.Category), " ", "_"), Name: entry.Category, Order: entry.CategoryOrder}
		// NewTool adds nodeId to its input; never mutate the operation envelope.
		var input shared.Schema
		_ = json.Unmarshal(shared.Raw(entry.Operation.BodySchema), &input)
		description := fmt.Sprintf("%s. %s %s. Requires full access; returns API data, which may include secrets. Parameters are grouped by path, query, body, form and headers", entry.Summary, entry.Operation.Method, entry.Operation.Path)
		if entry.Operation.Transport.Kind == "websocket" {
			description += ". Opens one bounded WebSocket exchange; stream controls frames and capture limits; the connection closes after this call"
		}
		if entry.Operation.Transport.Kind == "sse" {
			description += ". Captures a bounded server-sent event stream; stream controls capture limits"
		}
		if entry.Operation.Transport.Response == "binary" {
			description += ". Returns base64 file bytes and metadata, including for empty files"
		}
		if strings.HasPrefix(entry.Operation.Path, "/api/v2/hosts/terminal/") {
			description += ". Requires a panel-issued terminal session identity; API-key authentication alone may be insufficient. Captured frames do not confirm command success"
		}
		spec := shared.NewTool(category, invoke, entry.Name, description, false, input, entry.Operation)
		spec.Definition.SchemaVersion = 3
		spec.Definition.Risk = "privileged"
		if _, err := shared.CompileEndpointSchema(input); err != nil {
			panic(fmt.Errorf("invalid API schema %s: %w", entry.Name, err))
		}
		if _, err := shared.CompileEndpointSchema(entry.Operation.BodySchema); err != nil {
			panic(fmt.Errorf("invalid operation schema %s: %w", entry.Name, err))
		}
		tools = append(tools, spec)
	}
	return tools
}

func invoke(ctx context.Context, transport shared.APITransport, args map[string]any) (json.RawMessage, error) {
	endpoint, ok := transport.(shared.EndpointTransport)
	if !ok {
		return nil, shared.ContractError("The transport does not support endpoint-v1 operations")
	}
	return endpoint.RequestOperation(ctx, "api", args)
}
