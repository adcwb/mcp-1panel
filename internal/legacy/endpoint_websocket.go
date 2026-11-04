package legacy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"github.com/1Panel-dev/mcp-1panel/internal/toolset/shared"
	"github.com/coder/websocket"
)

// exchange bounds one WebSocket conversation to one authorized tools/call.
// Connections and credentials are never reused across clients or invocations.
// Frame contents follow the panel protocol; binary frames use base64 in JSON.
func (c *Client) exchange(ctx context.Context, endpoint string, args map[string]any, options requestOptions) (json.RawMessage, error) {
	stream, _ := args["stream"].(map[string]any)
	readTimeout := time.Duration(shared.NumberArg(stream, "readTimeoutMs", 1000)) * time.Millisecond
	maxMessages := shared.NumberArg(stream, "maxMessages", 20)
	input, _ := stream["messages"].([]any)
	type frame struct {
		Type string `json:"type"`
		Data string `json:"data"`
	}
	type outgoing struct {
		kind websocket.MessageType
		data []byte
	}
	writes := make([]outgoing, 0, len(input))
	for _, raw := range input {
		value, ok := raw.(map[string]any)
		if !ok {
			return nil, failure("MCP_INVALID_ARGUMENTS")
		}
		data := []byte(shared.StringArg(value, "data", ""))
		kind := websocket.MessageText
		if value["type"] == "binary" {
			kind = websocket.MessageBinary
			decoded, err := base64.StdEncoding.Strict().DecodeString(string(data))
			if err != nil {
				return nil, failure("MCP_INVALID_ARGUMENTS")
			}
			data = decoded
		}
		writes = append(writes, outgoing{kind, data})
	}
	dialContext, cancelDial := context.WithTimeout(ctx, 15*time.Second)
	conn, response, err := websocket.Dial(dialContext, c.host+endpoint, &websocket.DialOptions{
		HTTPClient: c.http, HTTPHeader: c.endpointHeaders(options.NodeName),
	})
	cancelDial()
	if err != nil {
		if response != nil && response.StatusCode != 101 {
			if response.Body != nil {
				_ = response.Body.Close()
			}
			if response.StatusCode >= 400 && response.StatusCode < 500 {
				return nil, responseFailure(response.StatusCode, nil)
			}
		}
		return nil, transportFailure(options, "MCP_NODE_UNAVAILABLE")
	}
	defer conn.CloseNow()
	conn.SetReadLimit(maxResponseSize)
	conversation, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	// Read concurrently with writes so a verbose terminal cannot deadlock both
	// peers while the caller submits multiple input frames.
	written := make(chan error, 1)
	go func() {
		for _, message := range writes {
			if err := conn.Write(conversation, message.kind, message.data); err != nil {
				written <- err
				return
			}
		}
		written <- nil
	}()
	frames := make([]frame, 0)
	total, reason := 0, "message_limit"
	for len(frames) < maxMessages {
		kind, data, err := conn.Read(conversation)
		if err != nil {
			if ctx.Err() != nil {
				cancel()
				<-written
				return nil, transportFailure(options, "MCP_NODE_UNAVAILABLE")
			}
			if errors.Is(err, context.DeadlineExceeded) || conversation.Err() != nil {
				reason = "deadline"
				break
			}
			if websocket.CloseStatus(err) == websocket.StatusNormalClosure || websocket.CloseStatus(err) == websocket.StatusGoingAway {
				reason = "closed"
				break
			}
			cancel()
			<-written
			return nil, transportFailure(options, "MCP_EXECUTION_FAILED")
		}
		total += len(data)
		if total > maxResponseSize {
			cancel()
			<-written
			return nil, transportFailure(options, "MCP_SCHEMA_MISMATCH")
		}
		message := frame{Type: "text", Data: string(data)}
		if kind == websocket.MessageBinary {
			message.Type, message.Data = "binary", base64.StdEncoding.EncodeToString(data)
		}
		frames = append(frames, message)
	}
	cancel()
	if err := <-written; err != nil {
		return nil, transportFailure(options, "MCP_EXECUTION_FAILED")
	}
	return shared.Raw(map[string]any{"messages": frames, "stopReason": reason,
		"connectionClosed": true, "requestStatus": "accepted"}), nil
}
