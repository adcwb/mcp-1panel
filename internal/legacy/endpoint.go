package legacy

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/1Panel-dev/mcp-1panel/internal/toolset/shared"
)

const maxEndpointRequest = 1 << 20

// RequestOperation validates the complete envelope against a compiled operation.
// Endpoint tools deliberately require full access: files, settings and database
// responses can contain secrets and are not the convenience tools' projections.
func (b *apiBridge) RequestOperation(ctx context.Context, id string, args map[string]any) (json.RawMessage, error) {
	if b.tool.Risk != "privileged" {
		return nil, failure("MCP_FORBIDDEN")
	}
	for _, op := range b.tool.Operations {
		if op.ID != id || op.Transport == nil {
			continue
		}
		if err := shared.ValidateEndpointSchema(op.BodySchema, args); err != nil {
			return nil, err
		}
		endpoint, _, err := shared.EndpointPath(op, args)
		if err != nil {
			return nil, err
		}
		encoded := shared.Raw(args)
		if len(encoded) > maxEndpointRequest {
			return nil, failure("MCP_INVALID_ARGUMENTS")
		}
		if b.client == nil {
			return nil, failure("MCP_UNAUTHENTICATED")
		}
		// Core-owned operations cannot be redirected to an Agent by nodeId.
		if strings.HasPrefix(op.Path, apiPrefix+"/core/") && !strings.HasPrefix(op.Path, apiPrefix+"/core/xpack/") && !op.Transport.NodeQuery && b.nodeID != "local" {
			return nil, failure("MCP_INVALID_ARGUMENTS")
		}
		selected, err := b.client.resolveNode(ctx, b.nodeID)
		if err != nil {
			return nil, err
		}
		query, err := endpointQuery(args)
		if err != nil {
			return nil, err
		}
		if op.Transport.NodeQuery {
			query.Set("operateNode", selected.Name)
		}
		if len(query) > 0 {
			endpoint += "?" + query.Encode()
		}
		options := requestOptions{NodeName: selected.Name, NodeStatus: selected.Status,
			Mutation: op.Method != http.MethodGet || op.Transport.Kind == "websocket"}
		ctx, cancel := context.WithTimeout(ctx, invokeTimeout)
		defer cancel()
		var result json.RawMessage
		switch op.Transport.Kind {
		case "websocket":
			result, err = b.client.exchange(ctx, endpoint, args, options)
		case "sse":
			result, err = b.client.captureEvents(ctx, endpoint, args, options)
		default:
			result, err = b.client.requestEndpoint(ctx, op, endpoint, args, options)
		}
		if err != nil {
			return nil, err
		}
		if len(result) > shared.MaxAPIResponseBytes || !json.Valid(result) {
			return nil, transportFailure(options, "MCP_SCHEMA_MISMATCH")
		}
		return result, nil
	}
	return nil, failure("MCP_FORBIDDEN")
}

func endpointQuery(args map[string]any) (url.Values, error) {
	query := url.Values{}
	values, _ := args["query"].(map[string]any)
	for name, value := range values {
		// A node selector is always derived from an authorized node ID.
		if strings.EqualFold(name, "operateNode") || strings.EqualFold(name, "CurrentNode") {
			return nil, failure("MCP_INVALID_ARGUMENTS")
		}
		text, err := shared.EndpointScalar(value)
		if err != nil {
			return nil, failure("MCP_INVALID_ARGUMENTS")
		}
		query.Set(name, text)
	}
	return query, nil
}

// endpointBody serializes inline file bytes. It never reads paths on the MCP
// host; panel-side paths remain ordinary declared API parameters.
func endpointBody(op shared.Operation, args map[string]any) ([]byte, string, error) {
	if op.Transport.Encoding != "multipart" {
		value, exists := args["body"]
		if !exists {
			return nil, "", nil
		}
		encoded, err := json.Marshal(value)
		if err != nil || len(encoded) > maxEndpointRequest {
			return nil, "", failure("MCP_INVALID_ARGUMENTS")
		}
		return encoded, "application/json", nil
	}
	form, _ := args["form"].(map[string]any)
	fileFields := map[string]bool{}
	for _, name := range op.Transport.FileFields {
		fileFields[name] = true
	}
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	keys := make([]string, 0, len(form))
	for key := range form {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := form[key]
		if !fileFields[key] {
			text, err := shared.EndpointScalar(value)
			if err != nil || writer.WriteField(key, text) != nil {
				return nil, "", failure("MCP_INVALID_ARGUMENTS")
			}
			continue
		}
		file, ok := value.(map[string]any)
		if !ok {
			return nil, "", failure("MCP_INVALID_ARGUMENTS")
		}
		filename, _ := file["filename"].(string)
		data, _ := file["data"].(string)
		if filename == "" || filename == "." || filename == ".." || strings.ContainsAny(filename, "/\\\r\n\x00") {
			return nil, "", failure("MCP_INVALID_ARGUMENTS")
		}
		decoded, err := base64.StdEncoding.Strict().DecodeString(data)
		if err != nil || len(decoded) > maxEndpointRequest {
			return nil, "", failure("MCP_INVALID_ARGUMENTS")
		}
		part, err := writer.CreateFormFile(key, filename)
		if err != nil {
			return nil, "", failure("MCP_INVALID_ARGUMENTS")
		}
		if _, err := part.Write(decoded); err != nil {
			return nil, "", failure("MCP_INVALID_ARGUMENTS")
		}
	}
	if writer.Close() != nil || buffer.Len() > maxEndpointRequest {
		return nil, "", failure("MCP_INVALID_ARGUMENTS")
	}
	return buffer.Bytes(), writer.FormDataContentType(), nil
}

func (c *Client) endpointHeaders(nodeName string) http.Header {
	header := make(http.Header)
	timestamp := strconv.FormatInt(c.now().Unix(), 10)
	sum := md5.Sum([]byte("1panel" + c.key + timestamp))
	header.Set("1Panel-Token", hex.EncodeToString(sum[:]))
	header.Set("1Panel-Timestamp", timestamp)
	if c.keyID != "" {
		header.Set("1Panel-Key-ID", c.keyID)
	}
	header.Set("CurrentNode", url.QueryEscape(nodeName))
	return header
}

func (c *Client) requestEndpoint(ctx context.Context, op shared.Operation, endpoint string, args map[string]any, options requestOptions) (json.RawMessage, error) {
	body, contentType, err := endpointBody(op, args)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, op.Method, c.host+endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, failure("MCP_INVALID_ARGUMENTS")
	}
	request.Header = c.endpointHeaders(options.NodeName)
	request.Header.Set("Accept", "application/json, application/octet-stream, text/plain, */*")
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	headers, _ := args["headers"].(map[string]any)
	for name, value := range headers {
		// Even a bad generated schema cannot override credentials or routing.
		if name != "EntranceCode" && name != "Range" {
			return nil, failure("MCP_INVALID_ARGUMENTS")
		}
		text, err := shared.EndpointScalar(value)
		if err != nil || strings.ContainsAny(text, "\r\n\x00") {
			return nil, failure("MCP_INVALID_ARGUMENTS")
		}
		if name == "Range" && !validEndpointRange(text) {
			return nil, failure("MCP_INVALID_ARGUMENTS")
		}
		request.Header.Set(name, text)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, transportFailure(options, "MCP_NODE_UNAVAILABLE")
	}
	defer response.Body.Close()
	encoded, err := io.ReadAll(io.LimitReader(response.Body, maxResponseSize+1))
	if err != nil || len(encoded) > maxResponseSize {
		return nil, transportFailure(options, "MCP_SCHEMA_MISMATCH")
	}
	return endpointResponse(op, response, encoded, options)
}

func validEndpointRange(value string) bool {
	if !strings.HasPrefix(value, "bytes=") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(value, "bytes="), "-")
	if len(parts) != 2 {
		return false
	}
	start, e1 := strconv.ParseInt(parts[0], 10, 64)
	end, e2 := strconv.ParseInt(parts[1], 10, 64)
	return e1 == nil && e2 == nil && start >= 0 && end >= start && end-start < maxResponseSize
}

// endpointResponse preserves scalar, array and null data. It interprets API
// errors before returning binary/text data and never retries uncertain writes.
func endpointResponse(op shared.Operation, response *http.Response, encoded []byte, options requestOptions) (json.RawMessage, error) {
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		failed := responseFailure(response.StatusCode, nil)
		if options.Mutation {
			return nil, shared.WriteFailure(failed)
		}
		return nil, failed
	}
	mediaType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	disposition, attributes, _ := mime.ParseMediaType(response.Header.Get("Content-Disposition"))
	expectsBinary := op.Transport.Response == "binary"
	binary := expectsBinary && (disposition == "attachment" || mediaType != "application/json")
	if binary && disposition != "attachment" && mediaType == "text/html" {
		return nil, transportFailure(options, "MCP_SCHEMA_MISMATCH")
	}
	if !binary && json.Valid(encoded) {
		var envelope map[string]json.RawMessage
		if json.Unmarshal(encoded, &envelope) == nil {
			if encodedCode, exists := envelope["code"]; exists {
				var code int
				if json.Unmarshal(encodedCode, &code) != nil || code != http.StatusOK {
					failed := responseFailure(http.StatusOK, encodedCode)
					if options.Mutation && (code >= 500 || failed.Code == "MCP_EXECUTION_FAILED") {
						return nil, transportFailure(options, "MCP_EXECUTION_FAILED")
					}
					return nil, failed
				}
				if data, exists := envelope["data"]; exists {
					return data, nil
				}
				return shared.Raw(map[string]any{"requestStatus": "accepted"}), nil
			}
		}
		// A JSON file is still a download. Ordinary API replies must carry the
		// 1Panel status envelope; arbitrary proxy JSON is not proof of success.
		if !expectsBinary {
			return nil, transportFailure(options, "MCP_SCHEMA_MISMATCH")
		}
	}
	if !expectsBinary || (!binary && !json.Valid(encoded)) {
		return nil, transportFailure(options, "MCP_SCHEMA_MISMATCH")
	}
	return shared.Raw(map[string]any{
		"encoding": "base64", "data": base64.StdEncoding.EncodeToString(encoded), "size": len(encoded),
		"contentType": mediaType, "filename": attributes["filename"], "contentRange": response.Header.Get("Content-Range"),
	}), nil
}
