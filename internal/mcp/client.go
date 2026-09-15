package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
)

// Logf receives protocol-level diagnostics (never part of stdout output).
type Logf func(format string, args ...any)

// Client is an MCP client session bound to one transport.
type Client struct {
	t   Transport
	log Logf

	mu      sync.Mutex
	nextID  int64
	pending map[string]chan *Response
	closed  bool
	readErr error

	initResult *InitializeResult
	done       chan struct{}
	closeOnce  sync.Once
}

// NewClient wraps a transport. Call Initialize before any other method.
func NewClient(t Transport, log Logf) *Client {
	if log == nil {
		log = func(string, ...any) {}
	}
	c := &Client{
		t:       t,
		log:     log,
		pending: map[string]chan *Response{},
		done:    make(chan struct{}),
	}
	go c.readLoop()
	return c
}

func (c *Client) readLoop() {
	for {
		raw, err := c.t.Recv(context.Background())
		if err != nil {
			c.fail(err)
			return
		}
		var msg Response
		if err := json.Unmarshal(raw, &msg); err != nil {
			c.log("skipping unparseable message: %v", err)
			continue
		}
		switch {
		case msg.IsNotification():
			c.log("notification %s %s", msg.Method, string(msg.Params))
		case msg.IsPeerRequest():
			c.replyToPeer(&msg)
		default:
			c.deliver(&msg)
		}
	}
}

// replyToPeer answers server-initiated requests. This CLI exposes no sampling,
// roots or elicitation capability, so everything but ping is declined — but it
// must be answered, or the server blocks waiting.
func (c *Client) replyToPeer(msg *Response) {
	resp := map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(msg.ID)}
	if msg.Method == "ping" {
		resp["result"] = map[string]any{}
	} else {
		resp["error"] = map[string]any{
			"code":    -32601,
			"message": "mcp-cli does not implement " + msg.Method,
		}
	}
	b, err := json.Marshal(resp)
	if err != nil {
		return
	}
	if err := c.t.Send(context.Background(), b); err != nil {
		c.log("replying to %s: %v", msg.Method, err)
	}
}

func (c *Client) deliver(msg *Response) {
	key := string(msg.ID)
	c.mu.Lock()
	ch, ok := c.pending[key]
	if ok {
		delete(c.pending, key)
	}
	c.mu.Unlock()
	if !ok {
		c.log("response for unknown id %s", key)
		return
	}
	ch <- msg
}

func (c *Client) fail(err error) {
	c.mu.Lock()
	if c.readErr == nil {
		c.readErr = err
	}
	c.closed = true
	pending := c.pending
	c.pending = map[string]chan *Response{}
	c.mu.Unlock()
	for _, ch := range pending {
		close(ch)
	}
	c.closeOnce.Do(func() { close(c.done) })
}

// Call issues a request and decodes the result into out (which may be nil).
func (c *Client) Call(ctx context.Context, method string, params any, out any) error {
	c.mu.Lock()
	if c.closed {
		err := c.readErr
		c.mu.Unlock()
		if err == nil {
			err = io.EOF
		}
		return fmt.Errorf("%s: connection closed: %w", method, err)
	}
	c.nextID++
	id := c.nextID
	key := strconv.FormatInt(id, 10)
	ch := make(chan *Response, 1)
	c.pending[key] = ch
	c.mu.Unlock()

	req := Request{JSONRPC: "2.0", ID: json.RawMessage(key), Method: method, Params: params}
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("%s: encoding params: %w", method, err)
	}
	if err := c.t.Send(ctx, body); err != nil {
		c.mu.Lock()
		delete(c.pending, key)
		c.mu.Unlock()
		return fmt.Errorf("%s: %w", method, err)
	}

	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, key)
		c.mu.Unlock()
		// Best-effort cancellation so the server can stop working.
		_ = c.Notify(context.WithoutCancel(ctx), "notifications/cancelled", map[string]any{
			"requestId": id, "reason": "client timeout",
		})
		return fmt.Errorf("%s: %w", method, ctx.Err())
	case resp, ok := <-ch:
		if !ok {
			c.mu.Lock()
			err := c.readErr
			c.mu.Unlock()
			if err == nil {
				err = io.EOF
			}
			return fmt.Errorf("%s: %w", method, err)
		}
		if resp.Error != nil {
			return resp.Error
		}
		if out == nil {
			return nil
		}
		if len(resp.Result) == 0 {
			return nil
		}
		if err := json.Unmarshal(resp.Result, out); err != nil {
			return fmt.Errorf("%s: decoding result: %w", method, err)
		}
		return nil
	}
}

// Notify sends a notification (no reply expected).
func (c *Client) Notify(ctx context.Context, method string, params any) error {
	req := Request{JSONRPC: "2.0", Method: method, Params: params}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	return c.t.Send(ctx, body)
}

// Initialize performs the MCP handshake using the given protocol version.
func (c *Client) Initialize(ctx context.Context, version string) (*InitializeResult, error) {
	if version == "" {
		version = ProtocolVersion
	}
	var res InitializeResult
	params := InitializeParams{
		ProtocolVersion: version,
		Capabilities:    map[string]any{},
		ClientInfo:      Implementation{Name: ClientName, Version: ClientVersion, Title: "MCP CLI"},
	}
	if err := c.Call(ctx, "initialize", params, &res); err != nil {
		return nil, err
	}
	if h, ok := c.t.(*HTTPTransport); ok {
		v := res.ProtocolVersion
		if v == "" {
			v = version
		}
		h.SetProtocolVersion(v)
	}
	if err := c.Notify(ctx, "notifications/initialized", map[string]any{}); err != nil {
		return nil, fmt.Errorf("sending initialized notification: %w", err)
	}
	c.initResult = &res
	return &res, nil
}

// ServerInfo returns the initialize result, or nil before the handshake.
func (c *Client) ServerInfo() *InitializeResult { return c.initResult }

// Supports reports whether the server advertised the named capability
// (e.g. "tools", "resources", "prompts").
func (c *Client) Supports(capability string) bool {
	if c.initResult == nil {
		return false
	}
	_, ok := c.initResult.Capabilities[capability]
	return ok
}

// Close ends the session and its transport.
func (c *Client) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return c.t.Close()
}

// Ping round-trips a ping request.
func (c *Client) Ping(ctx context.Context) error {
	return c.Call(ctx, "ping", map[string]any{}, nil)
}

// ListTools returns every tool, following pagination cursors.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	var all []Tool
	cursor := ""
	for {
		var page ListToolsResult
		if err := c.Call(ctx, "tools/list", cursorParams(cursor), &page); err != nil {
			return nil, err
		}
		all = append(all, page.Tools...)
		if page.NextCursor == "" || page.NextCursor == cursor {
			return all, nil
		}
		cursor = page.NextCursor
	}
}

// CallTool invokes a tool. A tool-level failure (isError) is returned in the
// result rather than as a Go error; transport and protocol failures are errors.
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (*CallToolResult, error) {
	if args == nil {
		args = map[string]any{}
	}
	var res CallToolResult
	err := c.Call(ctx, "tools/call", map[string]any{"name": name, "arguments": args}, &res)
	if err != nil {
		return nil, err
	}
	return &res, nil
}

// ListResources returns every resource, following pagination cursors.
func (c *Client) ListResources(ctx context.Context) ([]Resource, error) {
	var all []Resource
	cursor := ""
	for {
		var page ListResourcesResult
		if err := c.Call(ctx, "resources/list", cursorParams(cursor), &page); err != nil {
			return nil, err
		}
		all = append(all, page.Resources...)
		if page.NextCursor == "" || page.NextCursor == cursor {
			return all, nil
		}
		cursor = page.NextCursor
	}
}

// ListResourceTemplates returns every resource template.
func (c *Client) ListResourceTemplates(ctx context.Context) ([]ResourceTemplate, error) {
	var all []ResourceTemplate
	cursor := ""
	for {
		var page ListResourceTemplatesResult
		if err := c.Call(ctx, "resources/templates/list", cursorParams(cursor), &page); err != nil {
			var rpcErr *RPCError
			// Servers that expose resources but no templates may not
			// implement this method at all.
			if errors.As(err, &rpcErr) && rpcErr.Code == -32601 {
				return all, nil
			}
			return nil, err
		}
		all = append(all, page.ResourceTemplates...)
		if page.NextCursor == "" || page.NextCursor == cursor {
			return all, nil
		}
		cursor = page.NextCursor
	}
}

// ReadResource reads one resource URI.
func (c *Client) ReadResource(ctx context.Context, uri string) (*ReadResourceResult, error) {
	var res ReadResourceResult
	if err := c.Call(ctx, "resources/read", map[string]any{"uri": uri}, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// ListPrompts returns every prompt, following pagination cursors.
func (c *Client) ListPrompts(ctx context.Context) ([]Prompt, error) {
	var all []Prompt
	cursor := ""
	for {
		var page ListPromptsResult
		if err := c.Call(ctx, "prompts/list", cursorParams(cursor), &page); err != nil {
			return nil, err
		}
		all = append(all, page.Prompts...)
		if page.NextCursor == "" || page.NextCursor == cursor {
			return all, nil
		}
		cursor = page.NextCursor
	}
}

// GetPrompt renders one prompt with the given arguments.
func (c *Client) GetPrompt(ctx context.Context, name string, args map[string]any) (*GetPromptResult, error) {
	if args == nil {
		args = map[string]any{}
	}
	// Always send an arguments object: servers that validate params with a
	// strict schema reject an absent one even when no argument is required.
	params := map[string]any{"name": name, "arguments": args}
	var res GetPromptResult
	if err := c.Call(ctx, "prompts/get", params, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

func cursorParams(cursor string) map[string]any {
	if cursor == "" {
		return map[string]any{}
	}
	return map[string]any{"cursor": cursor}
}
