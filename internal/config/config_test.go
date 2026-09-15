package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseInfersTransport(t *testing.T) {
	cfg, err := Parse([]byte(`
version: 1
servers:
  remote:
    url: https://example.test/mcp
  local:
    command: my-server
    args: [--stdio]
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := cfg.Servers["remote"].Transport; got != TransportHTTP {
		t.Errorf("remote transport = %q, want http", got)
	}
	if got := cfg.Servers["local"].Transport; got != TransportStdio {
		t.Errorf("local transport = %q, want stdio", got)
	}
	if cfg.Servers["local"].Name != "local" {
		t.Error("server name should be filled in from the map key")
	}
}

func TestParseRejectsBadDefinitions(t *testing.T) {
	cases := map[string]string{
		"stdio without command": "servers:\n  a:\n    transport: stdio\n",
		"http without url":      "servers:\n  a:\n    transport: http\n",
		"unknown transport":     "servers:\n  a:\n    transport: carrier-pigeon\n    url: x\n",
		"unknown auth type":     "servers:\n  a:\n    url: https://x\n    auth:\n      type: magic\n",
		"neither url nor cmd":   "servers:\n  a:\n    description: nothing\n",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(src)); err == nil {
				t.Fatal("expected a validation error")
			}
		})
	}
}

func TestDurationParsing(t *testing.T) {
	cfg, err := Parse([]byte(`
defaults:
  timeout: 90s
servers:
  a:
    url: https://x
  b:
    url: https://y
    timeout: 5
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := cfg.Servers["a"].Timeout.D(); got != 90*time.Second {
		t.Errorf("a timeout = %v, want 90s (inherited from defaults)", got)
	}
	if got := cfg.Servers["b"].Timeout.D(); got != 5*time.Second {
		t.Errorf("b timeout = %v, want 5s (bare number means seconds)", got)
	}
}

func TestExpandEnv(t *testing.T) {
	t.Setenv("MCP_TEST_TOKEN", "s3cret")
	os.Unsetenv("MCP_TEST_MISSING")
	cases := [][2]string{
		{"${MCP_TEST_TOKEN}", "s3cret"},
		{"Bearer ${MCP_TEST_TOKEN}", "Bearer s3cret"},
		{"${MCP_TEST_MISSING}", ""},
		{"${MCP_TEST_MISSING:-fallback}", "fallback"},
		{"${MCP_TEST_TOKEN:-fallback}", "s3cret"},
		{"no refs here", "no refs here"},
	}
	for _, c := range cases {
		if got := ExpandEnv(c[0]); got != c[1] {
			t.Errorf("ExpandEnv(%q) = %q, want %q", c[0], got, c[1])
		}
	}
}

func TestResolvedHeadersBearer(t *testing.T) {
	t.Setenv("MCP_TEST_TOKEN", "abc123")
	cfg, err := Parse([]byte(`
servers:
  gh:
    url: https://example.test/mcp
    headers:
      X-Trace: ${MCP_TEST_TOKEN}
    auth:
      type: bearer
      token: ${MCP_TEST_TOKEN}
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	h, err := cfg.Servers["gh"].ResolvedHeaders()
	if err != nil {
		t.Fatalf("headers: %v", err)
	}
	if h["Authorization"] != "Bearer abc123" {
		t.Errorf("Authorization = %q", h["Authorization"])
	}
	if h["X-Trace"] != "abc123" {
		t.Errorf("custom headers should expand env too, got %q", h["X-Trace"])
	}
}

func TestResolvedHeadersEmptyTokenIsAnError(t *testing.T) {
	os.Unsetenv("MCP_TEST_ABSENT")
	cfg, _ := Parse([]byte("servers:\n  gh:\n    url: https://x\n    auth:\n      type: bearer\n      token: ${MCP_TEST_ABSENT}\n"))
	_, err := cfg.Servers["gh"].ResolvedHeaders()
	if err == nil {
		t.Fatal("an unresolvable token must fail loudly, not send an empty header")
	}
	if !strings.Contains(err.Error(), "env var") {
		t.Errorf("error should hint at the env var, got: %v", err)
	}
}

func TestResolvedHeadersBasicAndTokenFile(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenPath, []byte("filetoken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Parse([]byte(`
servers:
  basic:
    url: https://x
    auth:
      type: basic
      username: alice
      password: pw
  fromfile:
    url: https://x
    auth:
      type: header
      header: X-Api-Key
      token_file: ` + tokenPath + `
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	h, err := cfg.Servers["basic"].ResolvedHeaders()
	if err != nil {
		t.Fatal(err)
	}
	// base64("alice:pw")
	if h["Authorization"] != "Basic YWxpY2U6cHc=" {
		t.Errorf("basic auth header = %q", h["Authorization"])
	}
	h, err = cfg.Servers["fromfile"].ResolvedHeaders()
	if err != nil {
		t.Fatal(err)
	}
	if h["X-Api-Key"] != "filetoken" {
		t.Errorf("token_file header = %q, want the trimmed file contents", h["X-Api-Key"])
	}
}

func TestResolvedEnvPassesThroughParentValue(t *testing.T) {
	t.Setenv("MCP_TEST_PARENT", "inherited")
	t.Setenv("MCP_TEST_SRC", "expanded")
	s := &Server{Env: map[string]string{
		"MCP_TEST_PARENT": "",
		"MCP_TEST_DEST":   "${MCP_TEST_SRC}",
	}}
	env := s.ResolvedEnv()
	find := func(k string) string {
		want := k + "="
		got := ""
		for _, e := range env {
			if strings.HasPrefix(e, want) {
				got = strings.TrimPrefix(e, want)
			}
		}
		return got
	}
	if find("MCP_TEST_PARENT") != "inherited" {
		t.Errorf("an empty value should pass the parent variable through, got %q", find("MCP_TEST_PARENT"))
	}
	if find("MCP_TEST_DEST") != "expanded" {
		t.Errorf("MCP_TEST_DEST = %q", find("MCP_TEST_DEST"))
	}
}

func TestGetDefaultAndUnknown(t *testing.T) {
	cfg, _ := Parse([]byte("defaults:\n  server: a\nservers:\n  a:\n    url: https://x\n  b:\n    url: https://y\n    disabled: true\n"))
	s, err := cfg.Get("")
	if err != nil || s.Name != "a" {
		t.Fatalf("default server lookup: %v %+v", err, s)
	}
	if _, err := cfg.Get("nope"); err == nil || !strings.Contains(err.Error(), "known:") {
		t.Errorf("unknown server error should list known servers, got: %v", err)
	}
	if _, err := cfg.Get("b"); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Errorf("disabled server should be refused, got: %v", err)
	}
}

func TestGetSingleServerNeedsNoName(t *testing.T) {
	cfg, _ := Parse([]byte("servers:\n  only:\n    url: https://x\n"))
	s, err := cfg.Get("")
	if err != nil || s.Name != "only" {
		t.Fatalf("a single configured server should be the implicit default: %v %+v", err, s)
	}
}

func TestLoadMissingFile(t *testing.T) {
	t.Setenv("MCP_CLI_CONFIG", filepath.Join(t.TempDir(), "absent.yaml"))
	_, err := Load("")
	var missing *ErrNoConfig
	if err == nil || !asErr(err, &missing) {
		t.Fatalf("want ErrNoConfig, got %v", err)
	}
}

func TestLoadJSONConfig(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, []byte(`{"servers":{"a":{"url":"https://x"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("JSON configs should load (YAML is a superset): %v", err)
	}
	if cfg.Servers["a"].Transport != TransportHTTP {
		t.Error("transport not inferred from a JSON config")
	}
}
