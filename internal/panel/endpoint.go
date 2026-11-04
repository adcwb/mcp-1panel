package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/1Panel-dev/mcp-1panel/internal/toolset/shared"
)

// RequestOperation preserves Core admission, per-operation grants and auditing.
// Core must implement endpoint-v1 and advertise the exact extended policy hash;
// there is deliberately no fallback to a process-wide public API credential.
func (b *apiBridge) RequestOperation(ctx context.Context, id string, args map[string]any) (json.RawMessage, error) {
	if b.tool.Risk != "privileged" {
		return nil, &RuntimeError{Code: "MCP_FORBIDDEN", Message: "API operation requires an explicit privileged grant", HTTPStatus: http.StatusForbidden}
	}
	for _, op := range b.tool.Operations {
		if op.ID != id || op.Transport == nil {
			continue
		}
		if err := shared.ValidateEndpointSchema(op.BodySchema, args); err != nil {
			return nil, err
		}
		_, params, err := shared.EndpointPath(op, args)
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(op.Path, "/api/v2/core/") && !strings.HasPrefix(op.Path, "/api/v2/core/xpack/") && !op.Transport.NodeQuery && b.nodeID != "local" {
			return nil, invalidArguments()
		}
		arguments := shared.Raw(args)
		if len(arguments) > maxRequestSize {
			return nil, invalidArguments()
		}
		result, err := b.client.Invoke(ctx, b.token, Invocation{AdmissionID: b.admissionID,
			ToolID: b.tool.ToolID, NodeID: b.nodeID, SchemaVersion: b.tool.SchemaVersion,
			OperationID: id, PolicyHash: b.policyHash, Arguments: arguments, PathParams: params, RequestID: b.requestID})
		if err != nil {
			if op.Method != http.MethodGet || op.Transport.Kind == "websocket" {
				return nil, shared.WriteFailure(err)
			}
			return nil, err
		}
		b.lastID = result.InvocationID
		if len(result.Result) > shared.MaxAPIResponseBytes || !json.Valid(result.Result) {
			if op.Method != http.MethodGet || op.Transport.Kind == "websocket" {
				failure := shared.UnknownWriteResult()
				failure.InvocationID = result.InvocationID
				return nil, failure
			}
			return nil, contractError("Invalid API operation result")
		}
		return result.Result, nil
	}
	return nil, &RuntimeError{Code: "MCP_FORBIDDEN", Message: "API operation is not declared by this tool", HTTPStatus: http.StatusForbidden}
}
