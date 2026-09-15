// Package mockserver implements a minimal MCP server used by mcp-cli's tests.
// It speaks the same protocol over stdio and over HTTP, so both transports can
// be exercised without a real server.
package mockserver

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// Options tune the mock's behaviour for specific test cases.
type Options struct {
	// ProtocolVersion is echoed from initialize; empty means echo the client's.
	ProtocolVersion string
	// RejectVersions causes initialize to fail for these client versions.
	RejectVersions []string
	// RequireHeader, when set, makes every request fail with 401 unless the
	// named HTTP header has the given value.
	RequireHeader [2]string
	// SSE makes the HTTP handler answer with text/event-stream.
	SSE bool
	// SessionID, when set, is returned as Mcp-Session-Id on initialize and
	// required on later requests.
	SessionID string
	// Paginate splits tools/list across two pages.
	Paginate bool
}

// Handle processes one JSON-RPC request and returns the response, or nil for a
// notification.
func Handle(opts Options, raw []byte) []byte {
	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return errorResponse(nil, -32700, "parse error")
	}
	if len(req.ID) == 0 {
		return nil // notification
	}

	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		for _, bad := range opts.RejectVersions {
			if bad == p.ProtocolVersion {
				return errorResponse(req.ID, -32602, "unsupported protocol version: "+p.ProtocolVersion)
			}
		}
		version := opts.ProtocolVersion
		if version == "" {
			version = p.ProtocolVersion
		}
		return okResponse(req.ID, map[string]any{
			"protocolVersion": version,
			"capabilities": map[string]any{
				"tools":     map[string]any{},
				"resources": map[string]any{},
				"prompts":   map[string]any{},
			},
			"serverInfo":   map[string]any{"name": "mock-mcp", "version": "1.2.3"},
			"instructions": "a mock server",
		})

	case "ping":
		return okResponse(req.ID, map[string]any{})

	case "tools/list":
		var p struct {
			Cursor string `json:"cursor"`
		}
		_ = json.Unmarshal(req.Params, &p)
		if opts.Paginate && p.Cursor == "" {
			return okResponse(req.ID, map[string]any{
				"tools":      tools[:1],
				"nextCursor": "page2",
			})
		}
		if opts.Paginate {
			return okResponse(req.ID, map[string]any{"tools": tools[1:]})
		}
		return okResponse(req.ID, map[string]any{"tools": tools})

	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		_ = json.Unmarshal(req.Params, &p)
		switch p.Name {
		case "search_issues":
			payload, _ := json.Marshal(map[string]any{
				"query":   p.Arguments["query"],
				"limit":   p.Arguments["limit"],
				"open":    p.Arguments["open"],
				"results": []map[string]any{{"id": 1, "title": "first"}},
			})
			return okResponse(req.ID, map[string]any{
				"content": []map[string]any{{"type": "text", "text": string(payload)}},
			})
		case "say_hello":
			return okResponse(req.ID, map[string]any{
				"content": []map[string]any{{"type": "text", "text": "hello " + fmt.Sprint(p.Arguments["name"])}},
			})
		case "get_image":
			return okResponse(req.ID, map[string]any{
				"content": []map[string]any{
					{"type": "text", "text": "here it is"},
					{"type": "image", "data": "aGk=", "mimeType": "image/png"},
					{"type": "resource_link", "uri": "mock://doc/7", "name": "doc seven"},
				},
			})
		case "pick_city":
			return okResponse(req.ID, map[string]any{
				"content": []map[string]any{{"type": "text", "text": fmt.Sprint(p.Arguments["city"])}},
			})
		case "explode":
			return okResponse(req.ID, map[string]any{
				"isError": true,
				"content": []map[string]any{{"type": "text", "text": "boom: upstream said no"}},
			})
		default:
			return errorResponse(req.ID, -32602, "unknown tool: "+p.Name)
		}

	case "resources/list":
		return okResponse(req.ID, map[string]any{
			"resources": []map[string]any{
				{"uri": "mock://doc/1", "name": "doc one", "mimeType": "application/json"},
			},
		})

	case "resources/templates/list":
		return errorResponse(req.ID, -32601, "method not found")

	case "resources/read":
		var p struct {
			URI string `json:"uri"`
		}
		_ = json.Unmarshal(req.Params, &p)
		return okResponse(req.ID, map[string]any{
			"contents": []map[string]any{
				{"uri": p.URI, "mimeType": "application/json", "text": `{"body":"hi"}`},
			},
		})

	case "prompts/list":
		return okResponse(req.ID, map[string]any{
			"prompts": []map[string]any{
				{"name": "summarise", "description": "summarise a page",
					"arguments": []map[string]any{{"name": "page", "required": true}}},
				{"name": "translate", "description": "translate a page",
					"arguments": []map[string]any{{"name": "page", "required": true}, {"name": "lang"}}},
			},
		})

	case "prompts/get":
		var p struct {
			Name      string             `json:"name"`
			Arguments *map[string]string `json:"arguments"`
		}
		_ = json.Unmarshal(req.Params, &p)
		if p.Arguments == nil {
			return errorResponse(req.ID, -32602, "expected object, received undefined at arguments")
		}
		return okResponse(req.ID, map[string]any{
			"description": "summarise a page",
			"messages": []map[string]any{
				{"role": "user", "content": map[string]any{"type": "text", "text": "summarise it"}},
			},
		})
	}
	return errorResponse(req.ID, -32601, "method not found: "+req.Method)
}

var tools = []map[string]any{
	{
		"name":        "search_issues",
		"description": "search issues",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string"},
				"limit": map[string]any{"type": "integer"},
				"open":  map[string]any{"type": "boolean"},
				"tags":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			},
			"required": []string{"query"},
		},
	},
	{
		"name":        "say_hello",
		"description": "greet someone",
		"inputSchema": map[string]any{
			"type":       "object",
			"properties": map[string]any{"name": map[string]any{"type": "string"}},
		},
	},
	{
		"name":        "get_image",
		"description": "returns mixed content",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
	},
	{
		"name":        "pick_city",
		"description": "takes an enum",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"city": map[string]any{"type": "string", "enum": []string{"London", "Paris"}},
			},
			"required": []string{"city"},
		},
	},
	{
		"name":        "explode",
		"description": "always fails",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
	},
}

func okResponse(id json.RawMessage, result any) []byte {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	return b
}

func errorResponse(id json.RawMessage, code int, msg string) []byte {
	if id == nil {
		id = json.RawMessage("null")
	}
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": msg},
	})
	return b
}

// ServeStdio runs the mock over newline-delimited JSON-RPC.
func ServeStdio(opts Options, in io.Reader, out io.Writer) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		resp := Handle(opts, []byte(line))
		if resp == nil {
			continue
		}
		if _, err := out.Write(append(resp, '\n')); err != nil {
			return err
		}
	}
	return sc.Err()
}

// Handler returns an HTTP handler speaking the Streamable HTTP transport (or
// the legacy SSE transport when Options.SSE is set).
func Handler(opts Options) http.Handler {
	var mu sync.Mutex
	issued := false

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if k := opts.RequireHeader[0]; k != "" && r.Header.Get(k) != opts.RequireHeader[1] {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
			return
		case http.MethodPost:
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		isInit := strings.Contains(string(body), `"initialize"`)
		if opts.SessionID != "" {
			mu.Lock()
			if isInit {
				issued = true
				w.Header().Set("Mcp-Session-Id", opts.SessionID)
			} else if issued && r.Header.Get("Mcp-Session-Id") != opts.SessionID {
				mu.Unlock()
				http.Error(w, `{"error":"session expired"}`, http.StatusNotFound)
				return
			}
			mu.Unlock()
		}

		resp := Handle(opts, body)
		if resp == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if opts.SSE {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, ": keep-alive\n\nevent: message\ndata: %s\n\n", resp)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(resp)
	})
	return mux
}

// LegacyHandler returns a handler for the 2024-11-05 HTTP+SSE transport: a GET
// stream that advertises a POST endpoint and carries every response.
func LegacyHandler(opts Options) http.Handler {
	type client struct {
		ch chan []byte
	}
	var mu sync.Mutex
	var current *client

	mux := http.NewServeMux()
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		if k := opts.RequireHeader[0]; k != "" && r.Header.Get(k) != opts.RequireHeader[1] {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no flush", http.StatusInternalServerError)
			return
		}
		c := &client{ch: make(chan []byte, 8)}
		mu.Lock()
		current = c
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: endpoint\ndata: /messages\n\n")
		flusher.Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			case msg := <-c.ch:
				fmt.Fprintf(w, "event: message\ndata: %s\n\n", msg)
				flusher.Flush()
			}
		}
	})
	mux.HandleFunc("/messages", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		resp := Handle(opts, body)
		w.WriteHeader(http.StatusAccepted)
		if resp == nil {
			return
		}
		mu.Lock()
		c := current
		mu.Unlock()
		if c != nil {
			c.ch <- resp
		}
	})
	return mux
}
