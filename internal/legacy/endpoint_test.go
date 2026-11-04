package legacy

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/1Panel-dev/mcp-1panel/internal/toolset"
	"github.com/1Panel-dev/mcp-1panel/internal/toolset/shared"
	"github.com/coder/websocket"
)

func endpointClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		timestamp := r.Header.Get("1Panel-Timestamp")
		sum := md5.Sum([]byte("1paneltest-key" + timestamp))
		if timestamp == "" || r.Header.Get("1Panel-Token") != hex.EncodeToString(sum[:]) || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("request did not use the configured API identity")
		}
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(Config{Host: server.URL, APIKey: "test-key"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return client
}

func TestEndpointRejectsUnprovenSuccess(t *testing.T) {
	for _, reply := range []struct{ name, contentType, body string }{
		{"empty", "application/json", ""},
		{"proxy_text", "text/plain", "upstream unavailable"},
		{"proxy_json", "application/json", `{"error":"upstream unavailable"}`},
		{"raw_null", "application/json", `null`},
		{"unexpected_binary", "application/octet-stream", "binary"},
	} {
		t.Run(reply.name, func(t *testing.T) {
			client := endpointClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", reply.contentType)
				_, _ = io.WriteString(w, reply.body)
			})
			_, err := callEndpoint(t, client, "api_post_files_check", `{"body":{"path":"/tmp/a"}}`)
			var failed *shared.ExecutionError
			if !errors.As(err, &failed) || !shared.IsUnknownWriteMessage(failed.Message) {
				t.Fatalf("unproven POST result was accepted: %v", err)
			}
		})
	}
}

func TestEndpointDownloadPreservesBytes(t *testing.T) {
	for _, content := range []string{"", " \n", `{"code":500,"data":"file contents"}`} {
		client := endpointClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Disposition", `attachment; filename="data.json"`)
			_, _ = io.WriteString(w, content)
		})
		result, err := callEndpoint(t, client, "api_get_files_download", `{"query":{"path":"/tmp/data.json"}}`)
		if err != nil {
			t.Fatal(err)
		}
		var download struct {
			Encoding, Data, Filename string
			Size                     int
		}
		if json.Unmarshal(result, &download) != nil || download.Encoding != "base64" || download.Data != base64.StdEncoding.EncodeToString([]byte(content)) || download.Size != len(content) || download.Filename != "data.json" {
			t.Fatalf("download corrupted: %s", result)
		}
	}
}

func TestEndpointKeepsLargeIntegerArguments(t *testing.T) {
	client := endpointClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			if string(body) != `{"id":9007199254740993}` {
				t.Errorf("rounded body: %s", body)
			}
		} else if !strings.HasSuffix(r.URL.Path, "/9007199254740993") {
			t.Errorf("rounded path: %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"code":200,"data":true}`)
	})
	for _, test := range []struct{ name, input string }{
		{"api_post_backups_refresh_token", `{"body":{"id":9007199254740993}}`},
		{"api_post_backups_refresh_token", `{"body":{"id":9007199254740993.0}}`},
		{"api_post_backups_refresh_token", `{"body":{"id":9.007199254740993e15}}`},
		{"api_get_apps_details_by_id", `{"path":{"id":9007199254740993}}`},
	} {
		if _, err := callEndpoint(t, client, test.name, test.input); err != nil {
			t.Fatal(err)
		}
	}
}

func TestEndpointSSEMultilineAndInheritedID(t *testing.T) {
	client := endpointClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "id: log-1\ndata: first\ndata: second\n\ndata: third\n\n")
	})
	result, err := callEndpoint(t, client, "api_get_containers_search_log", `{"query":{"container":"test","follow":"false"}}`)
	if err != nil {
		t.Fatal(err)
	}
	var capture struct{ Events []struct{ ID, Data string } }
	if json.Unmarshal(result, &capture) != nil || len(capture.Events) != 2 || capture.Events[0].Data != "first\nsecond" || capture.Events[1].ID != "log-1" {
		t.Fatalf("invalid SSE events: %s", result)
	}
}

func callEndpoint(t *testing.T, client *Client, name, input string) (json.RawMessage, error) {
	t.Helper()
	spec, found := toolset.Lookup(name)
	if !found {
		t.Fatalf("missing tool %s", name)
	}
	args := json.RawMessage(input)
	nodeID, err := spec.TargetNode(args)
	if err != nil {
		return nil, err
	}
	return spec.Invoke(context.Background(), &apiBridge{client: client, tool: spec.Definition, nodeID: nodeID}, args)
}

func TestEndpointJSONPathQueryAndScalarResults(t *testing.T) {
	client := endpointClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v2/files/check":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if r.Method != "POST" || body["path"] != "/tmp/example" || body["body"] != nil {
				t.Errorf("incorrect body: %v", body)
			}
			_, _ = io.WriteString(w, `{"code":200,"data":true}`)
		case "/api/v2/apps/my app":
			_, _ = io.WriteString(w, `{"code":200,"data":{"key":"my app"}}`)
		case "/api/v2/files/share/check":
			if r.URL.Query().Get("code") != "a&b=中文" || len(r.URL.Query()) != 1 {
				t.Errorf("incorrect query: %v", r.URL.Query())
			}
			_, _ = io.WriteString(w, `{"code":200,"data":null}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	})
	for _, test := range []struct{ name, input, want string }{
		{"api_post_files_check", `{"body":{"path":"/tmp/example"}}`, `true`},
		{"api_get_apps_by_key", `{"path":{"key":"my app"}}`, `{"key":"my app"}`},
		{"api_get_files_share_check", `{"query":{"code":"a&b=中文"}}`, `null`},
	} {
		result, err := callEndpoint(t, client, test.name, test.input)
		if err != nil || string(result) != test.want {
			t.Fatalf("%s: %s %v", test.name, result, err)
		}
	}
}

func TestEndpointMultipartAndRangeDownload(t *testing.T) {
	client := endpointClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v2/files/upload" {
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Error(err)
				return
			}
			defer r.MultipartForm.RemoveAll()
			file, header, err := r.FormFile("file")
			if err != nil {
				t.Error(err)
				return
			}
			defer file.Close()
			data, _ := io.ReadAll(file)
			if header.Filename != "hello.txt" || string(data) != "hello" || r.FormValue("path") != "/tmp" || r.FormValue("overwrite") != "false" {
				t.Error("incorrect multipart payload")
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"code":200}`)
			return
		}
		if r.URL.Path != "/api/v2/files/chunkdownload" || r.Header.Get("Range") != "bytes=1-3" {
			t.Error("incorrect download request")
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="data.bin"`)
		w.Header().Set("Content-Range", "bytes 1-3/10")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte{0, 1, 2})
	})
	result, err := callEndpoint(t, client, "api_post_files_upload", `{"form":{"path":"/tmp","overwrite":false,"file":{"filename":"hello.txt","data":"aGVsbG8="}}}`)
	if err != nil || !strings.Contains(string(result), "accepted") {
		t.Fatalf("upload: %s %v", result, err)
	}
	result, err = callEndpoint(t, client, "api_post_files_chunkdownload", `{"body":{"path":"/tmp/data.bin","name":"data.bin"},"headers":{"Range":"bytes=1-3"}}`)
	var download struct {
		Data, ContentRange string
		Size               int
	}
	_ = json.Unmarshal(result, &download)
	if err != nil || download.Data != "AAEC" || download.ContentRange != "bytes 1-3/10" || download.Size != 3 {
		t.Fatalf("download: %s %v", result, err)
	}
}

func TestEndpointInvalidArgumentsNeverReachPanel(t *testing.T) {
	var requests atomic.Int32
	client := endpointClient(t, func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(500) })
	for _, test := range []struct{ name, args string }{
		{"api_get_containers_status", `null`},
		{"api_post_files_check", `{}`},
		{"api_post_files_check", `{"body":{"path":12}}`},
		{"api_get_apps_by_key", `{"path":{"key":"../auth"}}`},
		{"api_get_apps_by_key", `{"path":{"key":"%2f"}}`},
		{"api_get_files_share_check", `{"query":{"code":"ok","operateNode":"other"}}`},
		{"api_get_files_share_check", `{"query":{"code":"ok"},"headers":{"Authorization":"bad"}}`},
		{"api_post_files_upload", `{"form":{"path":"/tmp","file":{"filename":"x","data":"not base64"}}}`},
		{"api_post_files_chunkdownload", `{"body":{"name":"x","path":"/tmp/x"},"headers":{"Range":"bytes=9-1"}}`},
		{"api_post_files_chunkdownload", `{"body":{"name":"x","path":"/tmp/x"},"headers":{"Range":"bytes=0-9000000"}}`},
		{"api_get_core_auth_current", `{"nodeId":"7"}`},
	} {
		if _, err := callEndpoint(t, client, test.name, test.args); err == nil {
			t.Errorf("accepted invalid arguments for %s", test.name)
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("%d invalid calls reached the panel", requests.Load())
	}
}

func TestEndpointWriteUncertaintyAndDeniedResponse(t *testing.T) {
	for _, test := range []struct {
		name       string
		status     int
		body, code string
		unknown    bool
	}{
		{"gateway", 502, "gateway text", "MCP_EXECUTION_FAILED", true},
		{"redirect", 302, "redirect after submit", "MCP_EXECUTION_FAILED", true},
		{"http denied", 403, "secret", "MCP_FORBIDDEN", false},
		{"business", 200, `{"code":500,"message":"secret"}`, "MCP_EXECUTION_FAILED", true},
		{"denied", 200, `{"code":403,"message":"secret"}`, "MCP_FORBIDDEN", false},
		{"invalid success", 200, `broken-json`, "MCP_EXECUTION_FAILED", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var requests atomic.Int32
			client := endpointClient(t, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			})
			_, err := callEndpoint(t, client, "api_post_files_check", `{"body":{"path":"/tmp/a"}}`)
			var execution *shared.ExecutionError
			if !errors.As(err, &execution) || execution.Code != test.code || shared.IsUnknownWriteMessage(execution.Message) != test.unknown || strings.Contains(err.Error(), "secret") || requests.Load() != 1 {
				t.Fatalf("error=%v requests=%d", err, requests.Load())
			}
		})
	}
}

func TestEndpointNodeRoutingAndNoFallback(t *testing.T) {
	var operations atomic.Int32
	client := endpointClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v2/core/nodes/list" {
			_, _ = io.WriteString(w, `{"code":200,"data":[{"id":7,"name":"child east","status":"healthy"}]}`)
			return
		}
		operations.Add(1)
		if r.Header.Get("CurrentNode") != "child+east" {
			t.Errorf("wrong node %q", r.Header.Get("CurrentNode"))
		}
		_, _ = io.WriteString(w, `{"code":200,"data":[]}`)
	})
	if _, err := callEndpoint(t, client, "api_get_containers_image", `{"nodeId":"7"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := callEndpoint(t, client, "api_get_containers_image", `{"nodeId":"8"}`); err == nil {
		t.Fatal("unknown node accepted")
	}
	if operations.Load() != 1 {
		t.Fatal("unknown node fell back to local")
	}
}

func TestEndpointWebSocketAndSSE(t *testing.T) {
	client := endpointClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v2/containers/search/log" {
			if r.Header.Get("Accept") != "text/event-stream" {
				t.Error("SSE accept header missing")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: log\nid: 7\ndata: first\ndata: second\n\n")
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		kind, data, err := conn.Read(ctx)
		if err != nil {
			t.Error(err)
			return
		}
		if string(data) != "input" || kind != websocket.MessageText {
			t.Error("wrong websocket input")
		}
		_ = conn.Write(ctx, websocket.MessageBinary, []byte{0, 1, 2})
		_ = conn.Close(websocket.StatusNormalClosure, "")
	})
	result, err := callEndpoint(t, client, "api_get_hosts_terminal_local", `{"stream":{"messages":[{"type":"text","data":"input"}],"maxMessages":1,"readTimeoutMs":1000}}`)
	if err != nil || !strings.Contains(string(result), base64.StdEncoding.EncodeToString([]byte{0, 1, 2})) || !strings.Contains(string(result), "message_limit") {
		t.Fatalf("websocket %s %v", result, err)
	}
	result, err = callEndpoint(t, client, "api_get_containers_search_log", `{"query":{"container":"test","follow":"false"},"stream":{"maxMessages":1}}`)
	if err != nil || !strings.Contains(string(result), `first\nsecond`) || !strings.Contains(string(result), `"id":"7"`) {
		t.Fatalf("SSE %s %v", result, err)
	}
}

func TestEndpointDirectCallRequiresGrant(t *testing.T) {
	var calls atomic.Int32
	client := endpointClient(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
	spec, _ := toolset.Lookup("api_post_files")
	result, err := toolHandler(spec, func(string) bool { return false }, client)(context.Background(), nil)
	if err != nil || !result.IsError || calls.Load() != 0 {
		t.Fatalf("ungranted direct call reached API: result=%v error=%v calls=%d", result, err, calls.Load())
	}
}

func TestEndpointRedirectAndOversizedResponses(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
	defer target.Close()
	for _, oversized := range []bool{false, true} {
		client := endpointClient(t, func(w http.ResponseWriter, r *http.Request) {
			if !oversized {
				http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"code":200,"data":"`+strings.Repeat("x", maxResponseSize)+`"}`)
		})
		if _, err := callEndpoint(t, client, "api_post_files_check", `{"body":{"path":"/tmp/a"}}`); err == nil {
			t.Fatalf("accepted response oversized=%v", oversized)
		}
	}
	if redirected.Load() != 0 {
		t.Fatal("credentials followed a redirect")
	}
}

func TestEndpointStreamDeadlineIsExplicit(t *testing.T) {
	client := endpointClient(t, func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		_, _, _ = conn.Read(r.Context())
	})
	started := time.Now()
	result, err := callEndpoint(t, client, "api_get_process_ws", `{"stream":{"readTimeoutMs":20}}`)
	if err != nil || !strings.Contains(string(result), `"stopReason":"deadline"`) || time.Since(started) > time.Second {
		t.Fatalf("deadline result=%s error=%v", result, err)
	}
}
