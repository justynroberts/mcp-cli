package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParseArgForms(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "body.json")
	if err := os.WriteFile(file, []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		tok  string
		key  string
		want any
	}{
		{"query=mcp", "query", "mcp"},
		{"query=a=b", "query", "a=b"},
		{"limit:=5", "limit", float64(5)},
		{"open:=true", "open", true},
		{"tags:=[\"a\",\"b\"]", "tags", []any{"a", "b"}},
		{"body=@" + file, "body", `{"a":1}`},
		{"body:=@" + file, "body", map[string]any{"a": float64(1)}},
	}
	for _, c := range cases {
		got, err := parseArg(c.tok)
		if err != nil {
			t.Errorf("parseArg(%q): %v", c.tok, err)
			continue
		}
		if got.key != c.key || !reflect.DeepEqual(got.value, c.want) {
			t.Errorf("parseArg(%q) = %q/%#v, want %q/%#v", c.tok, got.key, got.value, c.key, c.want)
		}
	}
}

func TestParseArgRejectsJunk(t *testing.T) {
	for _, tok := range []string{"noequals", "=value", "limit:=notjson"} {
		if _, err := parseArg(tok); err == nil {
			t.Errorf("parseArg(%q) should have failed", tok)
		}
	}
}

const schemaJSON = `{
  "type":"object",
  "properties":{
    "query":{"type":"string"},
    "limit":{"type":"integer"},
    "score":{"type":"number"},
    "open":{"type":"boolean"},
    "tags":{"type":"array","items":{"type":"string"}},
    "ids":{"type":"array","items":{"type":"integer"}},
    "filter":{"type":"object"}
  },
  "required":["query"]
}`

func TestBuildArgumentsCoercesToSchema(t *testing.T) {
	specs := []argSpec{}
	for _, tok := range []string{"query=mcp", "limit=5", "score=1.5", "open=true", "tags=a,b", "ids=1,2", `filter={"x":1}`} {
		s, err := parseArg(tok)
		if err != nil {
			t.Fatal(err)
		}
		specs = append(specs, s)
	}
	args, err := buildArguments(nil, specs, json.RawMessage(schemaJSON))
	if err != nil {
		t.Fatalf("buildArguments: %v", err)
	}
	want := map[string]any{
		"query":  "mcp",
		"limit":  int64(5),
		"score":  1.5,
		"open":   true,
		"tags":   []any{"a", "b"},
		"ids":    []any{int64(1), int64(2)},
		"filter": map[string]any{"x": float64(1)},
	}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("args = %#v\nwant %#v", args, want)
	}
}

func TestBuildArgumentsRejectsWrongTypes(t *testing.T) {
	s, _ := parseArg("limit=lots")
	if _, err := buildArguments(nil, []argSpec{s}, json.RawMessage(schemaJSON)); err == nil {
		t.Fatal("expected a type error for limit=lots")
	}
}

func TestBuildArgumentsExplicitJSONBypassesCoercion(t *testing.T) {
	s, _ := parseArg(`limit:="5"`)
	args, err := buildArguments(nil, []argSpec{s}, json.RawMessage(schemaJSON))
	if err != nil {
		t.Fatal(err)
	}
	if args["limit"] != "5" {
		t.Errorf("key:=json should be passed through verbatim, got %#v", args["limit"])
	}
}

func TestBuildArgumentsWithoutSchema(t *testing.T) {
	s, _ := parseArg("limit=5")
	args, err := buildArguments(map[string]any{"base": true}, []argSpec{s}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if args["limit"] != "5" || args["base"] != true {
		t.Errorf("args = %#v", args)
	}
}

func TestMissingRequired(t *testing.T) {
	if got := missingRequired(map[string]any{}, json.RawMessage(schemaJSON)); len(got) != 1 || got[0] != "query" {
		t.Errorf("missingRequired = %v, want [query]", got)
	}
	if got := missingRequired(map[string]any{"query": "x"}, json.RawMessage(schemaJSON)); got != nil {
		t.Errorf("missingRequired = %v, want nil", got)
	}
}

func TestSchemaSummary(t *testing.T) {
	got := schemaSummary(json.RawMessage(schemaJSON))
	if len(got) != 7 {
		t.Fatalf("got %d entries: %v", len(got), got)
	}
	if got[4] != "query: string (required)" {
		t.Errorf("required arguments should be marked, got %q", got[4])
	}
}
