package panel

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/1Panel-dev/mcp-1panel/internal/toolset"
	"github.com/1Panel-dev/mcp-1panel/internal/toolset/shared"
)

type coreTestTransport struct {
	target    *url.URL
	transport http.RoundTripper
}

func (c coreTestTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	copy := r.Clone(r.Context())
	copy.URL.Scheme, copy.URL.Host = c.target.Scheme, c.target.Host
	return c.transport.RoundTrip(copy)
}

func endpointCore(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	target, _ := url.Parse(server.URL)
	return &Client{http: &http.Client{Transport: coreTestTransport{target, http.DefaultTransport}}}
}

func TestManagedEndpointPreservesAdmissionAndIdentity(t *testing.T) {
	spec, ok := toolset.Lookup("api_post_files_check")
	if !ok {
		t.Fatal("missing endpoint")
	}
	hash := PolicyHash(spec.Definition)
	client := endpointCore(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != runtimePrefix+"/invoke" || r.Header.Get("Authorization") != "Bearer mcp_test" || r.Header.Get("1Panel-Token") != "" {
			t.Error("incorrect managed route or identity")
		}
		var invocation Invocation
		if err := json.NewDecoder(r.Body).Decode(&invocation); err != nil {
			t.Error(err)
		}
		if invocation.AdmissionID != "admitted" || invocation.ToolID != spec.Definition.ToolID || invocation.SchemaVersion != 3 || invocation.PolicyHash != hash || invocation.NodeID != "local" || string(invocation.Arguments) != `{"body":{"path":"/tmp/a"}}` {
			t.Errorf("incorrect invocation: %+v", invocation)
		}
		_, _ = io.WriteString(w, `{"invocationId":"audit-1","nodeId":"local","result":true}`)
	})
	bridge := &apiBridge{client: client, tool: spec.Definition, nodeID: "local", token: "mcp_test", admissionID: "admitted", policyHash: hash}
	result, err := spec.Invoke(context.Background(), bridge, json.RawMessage(`{"body":{"path":"/tmp/a"}}`))
	if err != nil || string(result) != "true" || bridge.lastID != "audit-1" {
		t.Fatalf("result=%s error=%v audit=%s", result, err, bridge.lastID)
	}
}

func TestManagedCatalogRequiresEndpointWirePolicy(t *testing.T) {
	spec, _ := toolset.Lookup("api_post_files_upload")
	for _, compatible := range []bool{true, false} {
		definition := spec.Definition
		definition.PolicyHash = PolicyHash(definition)
		if !compatible {
			definition.Operations = append([]shared.Operation(nil), definition.Operations...)
			definition.Operations[0].Transport = nil
			definition.PolicyHash = PolicyHash(definition)
		}
		client := endpointCore(t, func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(Catalog{ContractVersion: ContractVersion, Tools: []ToolDefinition{definition}})
		})
		_, err := client.Tools(context.Background(), "mcp_test")
		if (err == nil) != compatible {
			t.Fatalf("compatible=%v error=%v", compatible, err)
		}
	}
}

func TestManagedEndpointWriteUncertaintyKeepsAudit(t *testing.T) {
	spec, _ := toolset.Lookup("api_post_files")
	client := endpointCore(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"code":"MCP_NODE_UNAVAILABLE","message":"upstream failed","invocationId":"audit-unknown"}`)
	})
	bridge := &apiBridge{client: client, tool: spec.Definition, nodeID: "local", token: "mcp_test", admissionID: "ticket"}
	_, err := spec.Invoke(context.Background(), bridge, json.RawMessage(`{"body":{"path":"/tmp/file"}}`))
	var failure *RuntimeError
	if !errors.As(err, &failure) || !shared.IsUnknownWriteMessage(failure.Message) || failure.InvocationID != "audit-unknown" {
		t.Fatalf("unexpected write failure %v", err)
	}
}
