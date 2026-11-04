package legacy

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/1Panel-dev/mcp-1panel/internal/toolset/shared"
)

// This opt-in integration test drives the actual handlers and HTTP client against
// the committed Postman simulation. The target is restricted to loopback.
func TestEndpointPostmanSimulation(t *testing.T) {
	host := os.Getenv("MCP_TEST_SIMULATION_URL")
	if host == "" {
		t.Skip("set MCP_TEST_SIMULATION_URL to the local Postman simulation /panel URL")
	}
	u, err := url.Parse(host)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" {
		t.Fatal("simulation must use http://127.0.0.1")
	}
	client, err := NewClient(Config{Host: host, APIKey: "test-key"})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	for _, uncertain := range []bool{false, true} {
		path := "/tmp/mcp-simulation-" + strconv.FormatInt(time.Now().UnixNano(), 10)
		if uncertain {
			path += "-uncertain"
		}
		args, _ := json.Marshal(map[string]any{"body": map[string]any{"path": path}})
		_, err := callEndpoint(t, client, "api_post_files", string(args))
		if uncertain {
			var failure *shared.ExecutionError
			if !errors.As(err, &failure) || !shared.IsUnknownWriteMessage(failure.Message) {
				t.Fatalf("expected uncertain outcome, got %v", err)
			}
		} else if err != nil {
			t.Fatal(err)
		}
		result, err := callEndpoint(t, client, "api_post_files_check", string(args))
		if err != nil || string(result) != "true" {
			t.Fatalf("write was not observed: %s %v", result, err)
		}
	}
}
