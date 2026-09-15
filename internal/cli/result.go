package cli

import (
	"encoding/json"
	"strings"

	"github.com/justynroberts/mcp-cli/internal/mcp"
)

// toolOutput is the JSON shape emitted for a tool call.
//
// Most MCP servers answer with a text block that itself contains JSON. The
// whole point of this CLI is to hand back a usable object, so that text is
// parsed into JSON when possible and surfaced as "json"; the original blocks
// stay available under "content" with --full.
type toolOutput struct {
	Tool       string        `json:"tool"`
	IsError    bool          `json:"is_error"`
	JSON       any           `json:"json,omitempty"`
	Text       string        `json:"text,omitempty"`
	Structured any           `json:"structured,omitempty"`
	Content    []mcp.Content `json:"content,omitempty"`
}

// payload returns the most useful representation for --raw output.
func (o *toolOutput) payload() any {
	switch {
	case o.JSON != nil:
		return o.JSON
	case o.Structured != nil:
		return o.Structured
	case o.Text != "":
		return o.Text
	case len(o.Content) > 0:
		return o.Content
	default:
		return map[string]any{}
	}
}

// buildToolOutput flattens a tools/call result.
func buildToolOutput(tool string, res *mcp.CallToolResult, full bool) *toolOutput {
	out := &toolOutput{Tool: tool, IsError: res.IsError}
	if full {
		out.Content = res.Content
	}
	if len(res.StructuredContent) > 0 {
		var v any
		if err := json.Unmarshal(res.StructuredContent, &v); err == nil {
			out.Structured = v
		}
	}
	text, parsed := flatten(res.Content)
	out.Text = text
	out.JSON = parsed
	if !full && hasUncapturedContent(res.Content) {
		// Images, audio, resource links and binary resources carry data that
		// flattening to text cannot represent. Keep the raw blocks rather than
		// silently dropping them.
		out.Content = res.Content
	}
	return out
}

// hasUncapturedContent reports whether any block holds data that flatten
// cannot express as text.
func hasUncapturedContent(content []mcp.Content) bool {
	for _, c := range content {
		switch c.Type {
		case "text":
			continue
		case "resource":
			// An embedded resource is captured only if it carries text.
			var r mcp.ResourceContents
			if err := json.Unmarshal(c.Resource, &r); err == nil && r.Text != "" {
				continue
			}
			return true
		default:
			// image, audio, resource_link, and anything added later.
			return true
		}
	}
	return false
}

// flatten concatenates text blocks and, when the result is itself JSON, parses
// it. Embedded resource blocks with text bodies are included too.
func flatten(content []mcp.Content) (string, any) {
	var parts []string
	var parsedEach []any
	allJSON := true

	for _, c := range content {
		text := c.Text
		if text == "" && len(c.Resource) > 0 {
			var r mcp.ResourceContents
			if err := json.Unmarshal(c.Resource, &r); err == nil {
				text = r.Text
			}
		}
		if text == "" {
			continue
		}
		parts = append(parts, text)
		if v, ok := parseJSON(text); ok {
			parsedEach = append(parsedEach, v)
		} else {
			allJSON = false
		}
	}

	joined := strings.Join(parts, "\n")
	if joined == "" {
		return "", nil
	}
	// Prefer parsing the whole thing; fall back to per-block parses.
	if v, ok := parseJSON(joined); ok {
		return joined, v
	}
	if allJSON && len(parsedEach) > 0 {
		if len(parsedEach) == 1 {
			return joined, parsedEach[0]
		}
		return joined, parsedEach
	}
	return joined, nil
}

// parseJSON reports whether s is a JSON object or array, and returns it.
// Bare scalars are deliberately not treated as JSON: a tool answering "42" or
// "ok" is returning text, not a document.
func parseJSON(s string) (any, bool) {
	t := strings.TrimSpace(s)
	if len(t) == 0 || (t[0] != '{' && t[0] != '[') {
		return nil, false
	}
	var v any
	dec := json.NewDecoder(strings.NewReader(t))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	// Reject trailing garbage (e.g. "{...} and then prose").
	if dec.More() {
		return nil, false
	}
	return v, true
}
