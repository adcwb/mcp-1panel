package legacy

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/1Panel-dev/mcp-1panel/internal/toolset/shared"
)

// captureEvents supports container log streaming without leaving a background
// reader behind. Limits end the capture explicitly; they never imply log EOF.
func (c *Client) captureEvents(ctx context.Context, endpoint string, args map[string]any, options requestOptions) (json.RawMessage, error) {
	stream, _ := args["stream"].(map[string]any)
	duration := time.Duration(shared.NumberArg(stream, "readTimeoutMs", 1000)) * time.Millisecond
	limit := shared.NumberArg(stream, "maxMessages", 20)
	capture, cancel := context.WithTimeout(ctx, duration)
	defer cancel()
	request, err := http.NewRequestWithContext(capture, http.MethodGet, c.host+endpoint, nil)
	if err != nil {
		return nil, failure("MCP_INVALID_ARGUMENTS")
	}
	request.Header = c.endpointHeaders(options.NodeName)
	request.Header.Set("Accept", "text/event-stream")
	response, err := c.http.Do(request)
	if err != nil {
		return nil, transportFailure(options, "MCP_NODE_UNAVAILABLE")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, responseFailure(response.StatusCode, nil)
	}
	if !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		return nil, failure("MCP_SCHEMA_MISMATCH")
	}
	type event struct {
		Event string `json:"event,omitempty"`
		ID    string `json:"id,omitempty"`
		Data  string `json:"data"`
	}
	events := make([]event, 0)
	current := event{}
	var data strings.Builder
	lastID := ""
	hasData := false
	reason := "closed"
	reader := &io.LimitedReader{R: response.Body, N: maxResponseSize + 1}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), maxResponseSize)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if hasData {
				current.Data = strings.TrimSuffix(data.String(), "\n")
				current.ID = lastID
				events = append(events, current)
			}
			current, hasData = event{}, false
			data.Reset()
			if len(events) >= limit {
				reason = "message_limit"
				break
			}
			continue
		}
		name, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch name {
		case "event":
			current.Event = value
		case "id":
			if !strings.ContainsRune(value, 0) {
				lastID = value
			}
		case "data":
			data.WriteString(value)
			data.WriteByte('\n')
			hasData = true
		}
	}
	if reader.N == 0 {
		return nil, failure("MCP_SCHEMA_MISMATCH")
	}
	if ctx.Err() != nil {
		return nil, failure("MCP_NODE_UNAVAILABLE")
	}
	if capture.Err() != nil {
		reason = "deadline"
	} else if scanner.Err() != nil {
		return nil, failure("MCP_NODE_UNAVAILABLE")
	}
	return shared.Raw(map[string]any{"events": events, "stopReason": reason, "connectionClosed": true}), nil
}
