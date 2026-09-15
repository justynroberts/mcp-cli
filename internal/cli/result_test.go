package cli

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/justynroberts/mcp-cli/internal/mcp"
)

func TestFlattenParsesJSONText(t *testing.T) {
	res := &mcp.CallToolResult{Content: []mcp.Content{
		{Type: "text", Text: `{"total":2,"items":["a","b"]}`},
	}}
	out := buildToolOutput("search", res, false)
	m, ok := out.JSON.(map[string]any)
	if !ok {
		t.Fatalf("json payload = %#v, want an object", out.JSON)
	}
	if fmtNum(m["total"]) != "2" {
		t.Errorf("total = %#v", m["total"])
	}
	if out.Text == "" {
		t.Error("the original text should still be available")
	}
}

func TestFlattenLeavesProseAlone(t *testing.T) {
	res := &mcp.CallToolResult{Content: []mcp.Content{{Type: "text", Text: "just words"}}}
	out := buildToolOutput("t", res, false)
	if out.JSON != nil {
		t.Errorf("prose should not become json, got %#v", out.JSON)
	}
	if out.payload() != "just words" {
		t.Errorf("payload = %#v", out.payload())
	}
}

func TestFlattenRejectsTrailingGarbage(t *testing.T) {
	res := &mcp.CallToolResult{Content: []mcp.Content{{Type: "text", Text: `{"a":1} and some prose`}}}
	if out := buildToolOutput("t", res, false); out.JSON != nil {
		t.Errorf("partial JSON should not be treated as a document, got %#v", out.JSON)
	}
}

func TestFlattenMultipleJSONBlocks(t *testing.T) {
	res := &mcp.CallToolResult{Content: []mcp.Content{
		{Type: "text", Text: `{"a":1}`},
		{Type: "text", Text: `{"b":2}`},
	}}
	out := buildToolOutput("t", res, false)
	arr, ok := out.JSON.([]any)
	if !ok || len(arr) != 2 {
		t.Fatalf("json = %#v, want a 2-element array", out.JSON)
	}
}

func TestStructuredContentPreferred(t *testing.T) {
	res := &mcp.CallToolResult{
		Content:           []mcp.Content{{Type: "text", Text: "human summary"}},
		StructuredContent: json.RawMessage(`{"count":3}`),
	}
	out := buildToolOutput("t", res, false)
	if out.Structured == nil {
		t.Fatal("structuredContent should be surfaced")
	}
	if !reflect.DeepEqual(out.payload(), map[string]any{"count": float64(3)}) {
		t.Errorf("payload = %#v, want the structured content", out.payload())
	}
}

func TestEmbeddedResourceText(t *testing.T) {
	res := &mcp.CallToolResult{Content: []mcp.Content{
		{Type: "resource", Resource: json.RawMessage(`{"uri":"x://1","text":"{\"k\":1}"}`)},
	}}
	out := buildToolOutput("t", res, false)
	if out.JSON == nil {
		t.Fatalf("embedded resource text should be extracted, got %#v", out)
	}
}

func TestNonTextContentIsKept(t *testing.T) {
	res := &mcp.CallToolResult{Content: []mcp.Content{{Type: "image", Data: "base64", MimeType: "image/png"}}}
	out := buildToolOutput("t", res, false)
	if len(out.Content) != 1 {
		t.Error("image blocks must survive into the output")
	}
}

func fmtNum(v any) string {
	if n, ok := v.(json.Number); ok {
		return n.String()
	}
	return ""
}
