package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/1Panel-dev/mcp-1panel/internal/toolset/shared"
)

// Compare against the supplied contract, not just the generated manifest count.
func TestCatalogCoversEverySwaggerOperation(t *testing.T) {
	raw, err := os.ReadFile("../../../doc.json")
	if err != nil {
		t.Fatal(err)
	}
	var source struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(raw, &source); err != nil {
		t.Fatal(err)
	}
	var generated struct {
		SourceSHA256 string `json:"sourceSHA256"`
	}
	_ = json.Unmarshal(document, &generated)
	hash := sha256.Sum256(raw)
	if generated.SourceSHA256 != hex.EncodeToString(hash[:]) {
		t.Fatal("Swagger changed; regenerate the catalog")
	}
	wanted := map[string]bool{}
	parameter := regexp.MustCompile(`\{([^}]+)\}`)
	for path, methods := range source.Paths {
		for method := range methods {
			switch method {
			case "get", "post", "put", "patch", "delete", "head", "options":
				wanted[strings.ToUpper(method)+" /api/v2"+parameter.ReplaceAllString(path, ":$1")] = true
			}
		}
	}
	names, categories := map[string]bool{}, map[string]bool{}
	for _, tool := range Tools() {
		d := tool.Definition
		if names[d.Name] || !regexp.MustCompile(`^[a-z0-9_]{1,64}$`).MatchString(d.Name) {
			t.Fatalf("invalid/duplicate name %q", d.Name)
		}
		names[d.Name], categories[d.CategoryID] = true, true
		if !shared.IsEndpointTool(d) || d.Risk != "privileged" || d.ReadOnly || d.SchemaVersion != 3 {
			t.Fatalf("endpoint lacks its explicit full-access boundary: %s", d.Name)
		}
		op := d.Operations[0]
		key := op.Method + " " + op.Path
		if !wanted[key] {
			t.Fatalf("unexpected or duplicate operation %s", key)
		}
		delete(wanted, key)
	}
	if len(wanted) != 0 || len(categories) != 58 {
		t.Fatalf("uncovered operations=%v categories=%d", wanted, len(categories))
	}
}

// These examples use the DTOs actually bound by the pinned panel handlers.
// The supplied Swagger points at unrelated DTOs for several of these routes.
func TestCorrectedRequestContracts(t *testing.T) {
	examples := []struct {
		name, valid, invalid string
	}{
		{"api_post_backups_refresh_token", `{"body":{"id":7}}`, `{"body":{"type":"s3","vars":"{}"}}`},
		{"api_post_containers_template_del", `{"body":{"ids":[7]}}`, `{"body":{"names":["template"]}}`},
		{"api_post_databases_pg_privileges", `{"body":{"name":"db","database":"pg","username":"user","superUser":false}}`, `{"body":{"name":"db","database":"pg"}}`},
		{"api_post_containers_ipv6option_update", `{"body":{"ip6Tables":false,"fixedCidrV6":"fd00::/64"}}`, `{"body":{"logMaxSize":"10m"}}`},
		{"api_post_files_convert", `{"body":{"files":[{"path":"/tmp","type":"image","inputFile":"a.png","extension":"png","outputFormat":"jpg"}],"outputPath":"/tmp"}}`, `{"body":{"path":"/tmp","type":"image","inputFile":"a.png","extension":"png","outputFormat":"jpg"}}`},
		{"api_post_containers_repo", `{"body":{"name":"registry","downloadUrl":"registry.example","auth":false}}`, `{"body":{"ids":[7]}}`},
		{"api_post_runtimes_node_modules_operate", `{"body":{"ID":7,"operate":"install","pkgManager":"npm","module":"express"}}`, `{"body":{"ID":7,"operate":"invalid"}}`},
		{"api_post_websites_redirect", `{"body":{"websiteId":7}}`, `{"body":{"id":7}}`},
		{"api_post_websites_ca_renew", `{"body":{"SSLID":7}}`, `{"body":{"sslID":7}}`},
		{"api_post_core_auth_mfa", `{"body":{"title":"panel","interval":30}}`, `{"body":{"secret":"test","code":"123456","interval":30}}`},
		{"api_post_core_commands_search", `{"body":{"page":1,"pageSize":20,"orderBy":"name","order":"ascending","type":"command"}}`, `{"body":{"page":1,"pageSize":20}}`},
		{"api_post_groups_search", `{"body":{}}`, `{"body":{"type":true}}`},
		{"api_post_core_groups_search", `{"body":{}}`, `{"body":{"type":true}}`},
		{"api_get_containers_list_stats", `{"query":{"ids":"a,b"}}`, `{"query":{"ids":["a","b"]}}`},
		{"api_get_containers_status", `{"query":{"containersOnly":true}}`, `{"query":{"containersOnly":"true"}}`},
	}
	byName := map[string]shared.ToolSpec{}
	for _, tool := range Tools() {
		byName[tool.Definition.Name] = tool
	}
	for _, example := range examples {
		t.Run(example.name, func(t *testing.T) {
			tool, ok := byName[example.name]
			if !ok {
				t.Fatal("missing tool")
			}
			for _, input := range []struct {
				raw   string
				valid bool
			}{{example.valid, true}, {example.invalid, false}} {
				var value any
				if err := json.Unmarshal([]byte(input.raw), &value); err != nil {
					t.Fatal(err)
				}
				err := shared.ValidateEndpointSchema(tool.Definition.Operations[0].BodySchema, value)
				if (err == nil) != input.valid {
					t.Fatalf("valid=%t: %v", input.valid, err)
				}
			}
		})
	}
}
