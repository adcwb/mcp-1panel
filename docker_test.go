package main

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/1Panel-dev/mcp-1panel/internal/toolset"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Exercise the built image's actual default command through MCP, not --catalog
// (which exports the complete manifest regardless of the active access level).
func TestDockerImageToolAccess(t *testing.T) {
	image := os.Getenv("MCP_DOCKER_TEST_IMAGE")
	if image == "" {
		t.Skip("set MCP_DOCKER_TEST_IMAGE to run the container integration test")
	}
	for _, test := range []struct {
		name  string
		args  []string
		level AccessLevel
		count int
	}{
		{"default_full", nil, AccessFull, 790},
		{"readonly_override", []string{"--mode=legacy", "--transport=stdio", "--access-level=readonly"}, AccessReadOnly, 15},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			// No real panel or credentials are needed to enumerate tools.
			args := []string{"run", "--rm", "--pull=never", "-i", "--network=none",
				"-e", "PANEL_HOST=http://127.0.0.1:1", "-e", "PANEL_ACCESS_TOKEN=ci-placeholder", image}
			command := exec.CommandContext(ctx, "docker", append(args, test.args...)...)
			command.Stderr = os.Stderr
			client := mcp.NewClient(&mcp.Implementation{Name: "docker-ci", Version: "1"}, nil)
			session, err := client.Connect(ctx, &mcp.CommandTransport{Command: command}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			wanted := map[string]bool{}
			for _, spec := range toolset.All() {
				if test.level.allowsTool(spec.Definition.Name) {
					wanted[spec.Definition.Name] = true
				}
			}
			seen, cursors := map[string]bool{}, map[string]bool{}
			params := &mcp.ListToolsParams{}
			for {
				result, err := session.ListTools(ctx, params)
				if err != nil {
					t.Fatal(err)
				}
				for _, tool := range result.Tools {
					if tool == nil || !wanted[tool.Name] || seen[tool.Name] {
						t.Fatal("image returned an unexpected or duplicate tool")
					}
					seen[tool.Name] = true
				}
				if result.NextCursor == "" {
					break
				}
				if cursors[result.NextCursor] {
					t.Fatal("image repeated a pagination cursor")
				}
				cursors[result.NextCursor] = true
				params.Cursor = result.NextCursor
			}
			if len(seen) != test.count || len(seen) != len(wanted) {
				t.Fatalf("image exposes %d tools, want %d", len(seen), test.count)
			}
		})
	}
}
