package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/justynroberts/mcp-cli/internal/mcp"
	"github.com/justynroberts/mcp-cli/internal/mockserver"
)

// The tests re-exec this binary as a stdio MCP server; TestMain turns it into
// one when the child environment variable is set.
const mockEnv = "MCP_CLI_TEST_STDIO_MOCK"

func TestMain(m *testing.M) {
	if os.Getenv(mockEnv) != "" {
		opts := mockserver.Options{}
		if os.Getenv("MCP_CLI_TEST_REJECT_LATEST") != "" {
			opts.RejectVersions = []string{mcp.ProtocolVersion}
		}
		if os.Getenv("MCP_CLI_TEST_PAGINATE") != "" {
			opts.Paginate = true
		}
		_ = mockserver.ServeStdio(opts, os.Stdin, os.Stdout)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func stdioClient(t *testing.T, env ...string) *mcp.Client {
	t.Helper()
	tr, err := mcp.NewStdioTransport(mcp.StdioOptions{
		Command: os.Args[0],
		Env:     append(append(os.Environ(), mockEnv+"=1"), env...),
	})
	if err != nil {
		t.Fatalf("starting stdio mock: %v", err)
	}
	c := mcp.NewClient(tr, nil)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return c
}

func TestStdioHandshakeAndTools(t *testing.T) {
	c := stdioClient(t)
	init, err := c.Initialize(ctx(t), "")
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if init.ServerInfo.Name != "mock-mcp" {
		t.Errorf("serverInfo.name = %q, want mock-mcp", init.ServerInfo.Name)
	}
	if init.ProtocolVersion != mcp.ProtocolVersion {
		t.Errorf("protocolVersion = %q, want %q", init.ProtocolVersion, mcp.ProtocolVersion)
	}
	if !c.Supports("tools") {
		t.Error("expected the tools capability to be advertised")
	}

	tools, err := c.ListTools(ctx(t))
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if len(tools) != 5 {
		t.Fatalf("got %d tools, want 5", len(tools))
	}
	if tools[0].Name != "search_issues" {
		t.Errorf("first tool = %q", tools[0].Name)
	}
}

func TestStdioPagination(t *testing.T) {
	c := stdioClient(t, "MCP_CLI_TEST_PAGINATE=1")
	if _, err := c.Initialize(ctx(t), ""); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	tools, err := c.ListTools(ctx(t))
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if len(tools) != 5 {
		t.Fatalf("pagination lost tools: got %d, want 5", len(tools))
	}
}

func TestCallToolAndErrors(t *testing.T) {
	c := stdioClient(t)
	if _, err := c.Initialize(ctx(t), ""); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	res, err := c.CallTool(ctx(t), "say_hello", map[string]any{"name": "world"})
	if err != nil {
		t.Fatalf("tools/call: %v", err)
	}
	if len(res.Content) != 1 || res.Content[0].Text != "hello world" {
		t.Fatalf("unexpected content: %+v", res.Content)
	}

	// A tool-level failure is data, not a Go error.
	res, err = c.CallTool(ctx(t), "explode", nil)
	if err != nil {
		t.Fatalf("tools/call explode: %v", err)
	}
	if !res.IsError {
		t.Error("expected isError on the explode tool")
	}

	// A protocol-level failure is an *RPCError.
	if _, err := c.CallTool(ctx(t), "nope", nil); err == nil {
		t.Fatal("expected an error for an unknown tool")
	} else {
		var rpcErr *mcp.RPCError
		if !errors.As(err, &rpcErr) {
			t.Fatalf("error %v is not an *RPCError", err)
		}
		if rpcErr.Code != -32602 {
			t.Errorf("code = %d, want -32602", rpcErr.Code)
		}
	}
}

func TestResourcesAndPrompts(t *testing.T) {
	c := stdioClient(t)
	if _, err := c.Initialize(ctx(t), ""); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	res, err := c.ListResources(ctx(t))
	if err != nil || len(res) != 1 {
		t.Fatalf("resources/list: %v %+v", err, res)
	}
	// The mock answers "method not found" for templates; that must be tolerated.
	tmpl, err := c.ListResourceTemplates(ctx(t))
	if err != nil {
		t.Fatalf("resources/templates/list should tolerate -32601: %v", err)
	}
	if len(tmpl) != 0 {
		t.Errorf("got %d templates, want 0", len(tmpl))
	}
	read, err := c.ReadResource(ctx(t), "mock://doc/1")
	if err != nil || len(read.Contents) != 1 {
		t.Fatalf("resources/read: %v %+v", err, read)
	}
	prompts, err := c.ListPrompts(ctx(t))
	if err != nil || len(prompts) != 2 {
		t.Fatalf("prompts/list: %v %+v", err, prompts)
	}
	got, err := c.GetPrompt(ctx(t), "summarise", map[string]any{"page": "x"})
	if err != nil || len(got.Messages) != 1 {
		t.Fatalf("prompts/get: %v %+v", err, got)
	}
}

func TestPing(t *testing.T) {
	c := stdioClient(t)
	if _, err := c.Initialize(ctx(t), ""); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if err := c.Ping(ctx(t)); err != nil {
		t.Fatalf("ping: %v", err)
	}
}

func TestHTTPTransportJSON(t *testing.T) {
	srv := httptest.NewServer(mockserver.Handler(mockserver.Options{
		RequireHeader: [2]string{"Authorization", "Bearer secret"},
		SessionID:     "sess-1",
	}))
	defer srv.Close()

	tr, err := mcp.NewHTTPTransport(context.Background(), mcp.HTTPOptions{
		URL:     srv.URL,
		Headers: map[string]string{"Authorization": "Bearer secret"},
	})
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	c := mcp.NewClient(tr, nil)
	defer c.Close()

	if _, err := c.Initialize(ctx(t), ""); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if got := tr.SessionID(); got != "sess-1" {
		t.Errorf("session id = %q, want sess-1", got)
	}
	tools, err := c.ListTools(ctx(t))
	if err != nil {
		t.Fatalf("tools/list over http: %v", err)
	}
	if len(tools) != 5 {
		t.Errorf("got %d tools, want 5", len(tools))
	}
}

func TestHTTPTransportUnauthorized(t *testing.T) {
	srv := httptest.NewServer(mockserver.Handler(mockserver.Options{
		RequireHeader: [2]string{"Authorization", "Bearer secret"},
	}))
	defer srv.Close()

	tr, err := mcp.NewHTTPTransport(context.Background(), mcp.HTTPOptions{URL: srv.URL})
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	c := mcp.NewClient(tr, nil)
	defer c.Close()

	_, err = c.Initialize(ctx(t), "")
	if err == nil {
		t.Fatal("expected an auth failure")
	}
	if !strings.Contains(err.Error(), "check the auth block") {
		t.Errorf("error should point at the auth config, got: %v", err)
	}
}

func TestHTTPTransportSSEResponses(t *testing.T) {
	srv := httptest.NewServer(mockserver.Handler(mockserver.Options{SSE: true}))
	defer srv.Close()

	tr, err := mcp.NewHTTPTransport(context.Background(), mcp.HTTPOptions{URL: srv.URL})
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	c := mcp.NewClient(tr, nil)
	defer c.Close()

	if _, err := c.Initialize(ctx(t), ""); err != nil {
		t.Fatalf("initialize over SSE responses: %v", err)
	}
	res, err := c.CallTool(ctx(t), "say_hello", map[string]any{"name": "sse"})
	if err != nil {
		t.Fatalf("tools/call over SSE responses: %v", err)
	}
	if res.Content[0].Text != "hello sse" {
		t.Errorf("content = %q", res.Content[0].Text)
	}
}

func TestLegacySSETransport(t *testing.T) {
	srv := httptest.NewServer(mockserver.LegacyHandler(mockserver.Options{}))
	defer srv.Close()

	tr, err := mcp.NewHTTPTransport(context.Background(), mcp.HTTPOptions{
		URL:    srv.URL + "/sse",
		Legacy: true,
	})
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	c := mcp.NewClient(tr, nil)
	defer c.Close()

	if _, err := c.Initialize(ctx(t), ""); err != nil {
		t.Fatalf("initialize over legacy SSE: %v", err)
	}
	tools, err := c.ListTools(ctx(t))
	if err != nil {
		t.Fatalf("tools/list over legacy SSE: %v", err)
	}
	if len(tools) != 5 {
		t.Errorf("got %d tools, want 5", len(tools))
	}
}

func TestCallTimeoutIsReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer srv.Close()

	tr, err := mcp.NewHTTPTransport(context.Background(), mcp.HTTPOptions{URL: srv.URL})
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	c := mcp.NewClient(tr, nil)
	defer c.Close()

	tctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := c.Initialize(tctx, ""); err == nil {
		t.Fatal("expected a timeout")
	}
}

func TestJSONRPCErrorFormatting(t *testing.T) {
	e := &mcp.RPCError{Code: -32602, Message: "bad params", Data: json.RawMessage(`{"field":"query"}`)}
	if !strings.Contains(e.Error(), "-32602") || !strings.Contains(e.Error(), "field") {
		t.Errorf("error text drops detail: %s", e.Error())
	}
	code, data := e.RPCDetails()
	if code != -32602 || string(data) == "" {
		t.Errorf("RPCDetails() = %d, %s", code, data)
	}
}
