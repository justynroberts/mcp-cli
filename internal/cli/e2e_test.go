package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/justynroberts/mcp-cli/internal/mockserver"
)

// The end-to-end tests re-exec this binary as a stdio MCP server.
const mockEnv = "MCP_CLI_TEST_STDIO_MOCK"

func TestMain(m *testing.M) {
	if os.Getenv(mockEnv) != "" {
		_ = mockserver.ServeStdio(mockserver.Options{}, os.Stdin, os.Stdout)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runCLI executes the CLI with argv and returns its exit code and stdout.
func runCLI(t *testing.T, argv ...string) (int, string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = w

	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()

	code := Run(argv)
	w.Close()
	os.Stdout = saved
	return code, <-done
}

// writeConfig writes a config whose "mock" server is this test binary.
func writeConfig(t *testing.T, extra string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := fmt.Sprintf(`
version: 1
defaults:
  timeout: 20s
servers:
  mock:
    description: the test mock
    transport: stdio
    command: %q
    env:
      %s: "1"
%s
`, os.Args[0], mockEnv, extra)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func decode(t *testing.T, s string) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("stdout is not a JSON object: %v\n%s", err, s)
	}
	return v
}

func TestServersCommand(t *testing.T) {
	cfg := writeConfig(t, "")
	code, out := runCLI(t, "-c", cfg, "servers")
	if code != 0 {
		t.Fatalf("exit = %d, out = %s", code, out)
	}
	env := decode(t, out)
	data := env["data"].(map[string]any)
	servers := data["servers"].([]any)
	if len(servers) != 1 {
		t.Fatalf("got %d servers", len(servers))
	}
	if servers[0].(map[string]any)["name"] != "mock" {
		t.Errorf("servers = %v", servers)
	}
}

func TestToolsAndCallEndToEnd(t *testing.T) {
	cfg := writeConfig(t, "")

	code, out := runCLI(t, "-c", cfg, "tools", "mock")
	if code != 0 {
		t.Fatalf("tools exit = %d: %s", code, out)
	}
	data := decode(t, out)["data"].(map[string]any)
	if data["count"].(float64) != 5 {
		t.Errorf("count = %v, want 5", data["count"])
	}

	// Schema-aware coercion: limit=5 becomes a number, open=true a boolean.
	code, out = runCLI(t, "-c", cfg, "call", "mock", "search_issues", "query=bug", "limit=5", "open=true")
	if code != 0 {
		t.Fatalf("call exit = %d: %s", code, out)
	}
	env := decode(t, out)
	if env["command"] != "call" || env["server"] != "mock" {
		t.Errorf("envelope = %v", env)
	}
	result := env["data"].(map[string]any)["json"].(map[string]any)
	if result["query"] != "bug" {
		t.Errorf("query = %v", result["query"])
	}
	if result["limit"].(float64) != 5 {
		t.Errorf("limit was not coerced to a number: %#v", result["limit"])
	}
	if result["open"] != true {
		t.Errorf("open was not coerced to a boolean: %#v", result["open"])
	}
}

func TestRawOutputIsJustThePayload(t *testing.T) {
	cfg := writeConfig(t, "")
	code, out := runCLI(t, "-c", cfg, "-raw", "call", "mock", "search_issues", "query=bug")
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, out)
	}
	v := decode(t, out)
	if _, isEnvelope := v["ok"]; isEnvelope {
		t.Errorf("-raw should drop the envelope, got %s", out)
	}
	if v["query"] != "bug" {
		t.Errorf("payload = %s", out)
	}
}

func TestToolErrorExitCode(t *testing.T) {
	cfg := writeConfig(t, "")
	code, out := runCLI(t, "-c", cfg, "call", "mock", "explode")
	if code != 6 {
		t.Errorf("a tool-reported failure should exit 6, got %d (%s)", code, out)
	}
	env := decode(t, out)
	if env["ok"] != false {
		t.Errorf("ok = %v", env["ok"])
	}
	errObj := env["error"].(map[string]any)
	if errObj["kind"] != "tool" || !strings.Contains(errObj["message"].(string), "boom") {
		t.Errorf("error = %v", errObj)
	}
}

func TestUnknownToolIsAUsageError(t *testing.T) {
	cfg := writeConfig(t, "")
	code, out := runCLI(t, "-c", cfg, "call", "mock", "does_not_exist")
	if code != 2 {
		t.Errorf("exit = %d, want 2 (usage): %s", code, out)
	}
	if !strings.Contains(out, "mcp-cli tools mock") {
		t.Errorf("the error should suggest listing the tools, got %s", out)
	}
}

func TestMissingRequiredArgumentIsCaughtLocally(t *testing.T) {
	cfg := writeConfig(t, "")
	code, out := runCLI(t, "-c", cfg, "call", "mock", "search_issues")
	if code != 2 {
		t.Errorf("exit = %d, want 2: %s", code, out)
	}
	if !strings.Contains(out, "requires: query") {
		t.Errorf("out = %s", out)
	}
}

func TestPrefixToolNameResolves(t *testing.T) {
	cfg := writeConfig(t, "")
	code, out := runCLI(t, "-c", cfg, "call", "mock", "hello", "name=bob")
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, out)
	}
	data := decode(t, out)["data"].(map[string]any)
	if data["text"] != "hello bob" {
		t.Errorf("data = %v", data)
	}
}

func TestUnknownServer(t *testing.T) {
	cfg := writeConfig(t, "")
	code, out := runCLI(t, "-c", cfg, "tools", "ghost")
	if code != 3 {
		t.Errorf("exit = %d, want 3 (config): %s", code, out)
	}
}

func TestInfoPingResourcesPrompts(t *testing.T) {
	cfg := writeConfig(t, "")

	code, out := runCLI(t, "-c", cfg, "info", "mock")
	if code != 0 {
		t.Fatalf("info: %d %s", code, out)
	}
	info := decode(t, out)["data"].(map[string]any)
	if info["server"].(map[string]any)["name"] != "mock-mcp" {
		t.Errorf("info = %v", info)
	}

	if code, out = runCLI(t, "-c", cfg, "ping", "mock"); code != 0 {
		t.Fatalf("ping: %d %s", code, out)
	}
	if code, out = runCLI(t, "-c", cfg, "resources", "mock"); code != 0 {
		t.Fatalf("resources: %d %s", code, out)
	}
	if code, out = runCLI(t, "-c", cfg, "read", "mock", "mock://doc/1"); code != 0 {
		t.Fatalf("read: %d %s", code, out)
	}
	if !strings.Contains(out, `"json"`) {
		t.Errorf("read should surface parsed JSON: %s", out)
	}
	if code, out = runCLI(t, "-c", cfg, "prompts", "mock"); code != 0 {
		t.Fatalf("prompts: %d %s", code, out)
	}
	if code, out = runCLI(t, "-c", cfg, "prompt", "mock", "summarise", "page=x"); code != 0 {
		t.Fatalf("prompt: %d %s", code, out)
	}
}

func TestSchemaCommand(t *testing.T) {
	cfg := writeConfig(t, "")
	code, out := runCLI(t, "-c", cfg, "schema", "mock", "search_issues")
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, out)
	}
	data := decode(t, out)["data"].(map[string]any)
	if _, ok := data["input_schema"]; !ok {
		t.Errorf("no input_schema in %v", data)
	}
}

func TestDiscoverAcrossServers(t *testing.T) {
	cfg := writeConfig(t, "  broken:\n    transport: stdio\n    command: /nonexistent/mcp-server\n")
	code, out := runCLI(t, "-c", cfg, "discover", "search")
	if code != 0 {
		t.Fatalf("discover should succeed even when a server is down: %d %s", code, out)
	}
	data := decode(t, out)["data"].(map[string]any)
	if data["count"].(float64) != 1 {
		t.Errorf("count = %v, want 1 matching tool", data["count"])
	}
	var sawError bool
	for _, s := range data["servers"].([]any) {
		if msg, ok := s.(map[string]any)["error"].(string); ok && msg != "" {
			sawError = true
		}
	}
	if !sawError {
		t.Errorf("the unreachable server should be reported per-server: %s", out)
	}
}

func TestInlineURLServerNeedsNoConfig(t *testing.T) {
	srv := httptest.NewServer(mockserver.Handler(mockserver.Options{
		RequireHeader: [2]string{"Authorization", "Bearer tok"},
	}))
	defer srv.Close()

	t.Setenv("MCP_CLI_CONFIG", filepath.Join(t.TempDir(), "absent.yaml"))
	code, out := runCLI(t, "-url", srv.URL, "-token", "tok", "tools")
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, out)
	}
	data := decode(t, out)["data"].(map[string]any)
	if data["count"].(float64) != 5 {
		t.Errorf("data = %v", data)
	}
}

func TestInlineTokenExpandsEnv(t *testing.T) {
	srv := httptest.NewServer(mockserver.Handler(mockserver.Options{
		RequireHeader: [2]string{"Authorization", "Bearer fromenv"},
	}))
	defer srv.Close()

	t.Setenv("MCP_CLI_TEST_TOKEN", "fromenv")
	t.Setenv("MCP_CLI_CONFIG", filepath.Join(t.TempDir(), "absent.yaml"))
	code, out := runCLI(t, "-url", srv.URL, "-token", "${MCP_CLI_TEST_TOKEN}", "ping")
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, out)
	}
}

func TestMissingConfigIsExplained(t *testing.T) {
	t.Setenv("MCP_CLI_CONFIG", filepath.Join(t.TempDir(), "absent.yaml"))
	code, out := runCLI(t, "servers")
	if code != 3 {
		t.Errorf("exit = %d, want 3: %s", code, out)
	}
	if !strings.Contains(out, "mcp-cli init") {
		t.Errorf("the error should point at `mcp-cli init`: %s", out)
	}
}

func TestInitWritesConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.yaml")
	code, out := runCLI(t, "init", path)
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, out)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config not written: %v", err)
	}
	// Writing over an existing file must fail rather than clobber it.
	if code, _ = runCLI(t, "init", path); code == 0 {
		t.Error("init should refuse to overwrite an existing config")
	}
}

func TestVersionAndUsage(t *testing.T) {
	if code, out := runCLI(t, "version"); code != 0 || !strings.Contains(out, "protocol") {
		t.Errorf("version: %d %s", code, out)
	}
	if code, out := runCLI(t); code != 0 || !strings.Contains(out, "Usage:") {
		t.Errorf("bare invocation should print usage: %d %s", code, out)
	}
	if code, _ := runCLI(t, "nonsense"); code != 2 {
		t.Errorf("unknown command should exit 2, got %d", code)
	}
}

// --- regressions found by testing against real MCP servers ---

func TestNonTextContentSurvivesFlattening(t *testing.T) {
	cfg := writeConfig(t, "")
	code, out := runCLI(t, "-c", cfg, "call", "mock", "get_image")
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, out)
	}
	data := decode(t, out)["data"].(map[string]any)
	blocks, ok := data["content"].([]any)
	if !ok {
		t.Fatalf("image and resource_link blocks were dropped: %s", out)
	}
	var types []string
	for _, b := range blocks {
		types = append(types, b.(map[string]any)["type"].(string))
	}
	want := []string{"text", "image", "resource_link"}
	if !reflect.DeepEqual(types, want) {
		t.Errorf("content types = %v, want %v", types, want)
	}
	if data["text"] != "here it is" {
		t.Errorf("the text block should still be flattened, got %v", data["text"])
	}
}

func TestEnumIsCheckedLocally(t *testing.T) {
	cfg := writeConfig(t, "")
	code, out := runCLI(t, "-c", cfg, "call", "mock", "pick_city", "city=Madrid")
	if code != 2 {
		t.Fatalf("exit = %d, want 2: %s", code, out)
	}
	if !strings.Contains(out, "London") || !strings.Contains(out, "Paris") {
		t.Errorf("the error should list the allowed values: %s", out)
	}
	if code, out = runCLI(t, "-c", cfg, "call", "mock", "pick_city", "city=Paris"); code != 0 {
		t.Errorf("a valid enum value should pass: %d %s", code, out)
	}
}

func TestPromptAlwaysSendsArgumentsObject(t *testing.T) {
	// The mock rejects a prompts/get without an "arguments" object, the way
	// schema-validating servers do.
	cfg := writeConfig(t, "")
	code, out := runCLI(t, "-c", cfg, "prompt", "mock", "summarise", "page=x")
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, out)
	}
}

func TestPromptRequiredArgumentAndPrefix(t *testing.T) {
	cfg := writeConfig(t, "")
	code, out := runCLI(t, "-c", cfg, "prompt", "mock", "summarise")
	if code != 2 {
		t.Fatalf("exit = %d, want 2: %s", code, out)
	}
	if !strings.Contains(out, "requires: page") {
		t.Errorf("out = %s", out)
	}
	// A unique prefix resolves, like it does for tools.
	if code, out = runCLI(t, "-c", cfg, "prompt", "mock", "trans", "page=x"); code != 0 {
		t.Errorf("prefix match failed: %d %s", code, out)
	}
	// An ambiguous one does not.
	if code, out = runCLI(t, "-c", cfg, "prompt", "mock", "a"); code != 2 {
		t.Errorf("ambiguous prompt name should be a usage error: %d %s", code, out)
	}
}

func TestInlineErrorsDoNotSuggestTheWordInline(t *testing.T) {
	srv := httptest.NewServer(mockserver.Handler(mockserver.Options{}))
	defer srv.Close()
	t.Setenv("MCP_CLI_CONFIG", filepath.Join(t.TempDir(), "absent.yaml"))

	code, out := runCLI(t, "-url", srv.URL, "call", "search_issues")
	if code != 2 {
		t.Fatalf("exit = %d: %s", code, out)
	}
	if strings.Contains(out, "schema inline") || strings.Contains(out, "tools inline") {
		t.Errorf("hint tells the user to type a server name that does not exist: %s", out)
	}
	if !strings.Contains(out, "-url "+srv.URL) {
		t.Errorf("hint should repeat the -url invocation: %s", out)
	}
}

func TestTimeoutExitsAsAConnectionFailure(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * time.Second)
	}))
	defer slow.Close()
	t.Setenv("MCP_CLI_CONFIG", filepath.Join(t.TempDir(), "absent.yaml"))

	code, out := runCLI(t, "-url", slow.URL, "-timeout", "200ms", "ping")
	if code != 4 {
		t.Errorf("a timeout should exit 4, got %d: %s", code, out)
	}
	if !strings.Contains(out, "timeout") {
		t.Errorf("the error kind should say timeout: %s", out)
	}
}
