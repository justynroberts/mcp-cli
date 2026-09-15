package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// argSpec describes one command-line supplied tool argument.
//
// Accepted forms:
//
//	key=value     value is a string (coerced later if the schema asks for it)
//	key:=json     value is parsed as JSON (numbers, booleans, arrays, objects)
//	key=@path     value is the contents of path ("-" reads stdin)
//	key:=@path    the contents of path are parsed as JSON
type argSpec struct {
	key    string
	value  any
	isJSON bool
}

// parseArg turns one "key=value" style token into an argSpec.
func parseArg(tok string) (argSpec, error) {
	jsonEq := strings.Index(tok, ":=")
	eq := strings.Index(tok, "=")
	switch {
	case jsonEq >= 0 && (eq < 0 || jsonEq <= eq):
		key := tok[:jsonEq]
		raw := tok[jsonEq+2:]
		if key == "" {
			return argSpec{}, fmt.Errorf("argument %q has an empty name", tok)
		}
		if strings.HasPrefix(raw, "@") {
			data, err := readSource(raw[1:])
			if err != nil {
				return argSpec{}, fmt.Errorf("argument %q: %w", key, err)
			}
			raw = string(data)
		}
		var v any
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			return argSpec{}, fmt.Errorf("argument %q: %q is not valid JSON: %w", key, raw, err)
		}
		return argSpec{key: key, value: v, isJSON: true}, nil
	case eq > 0:
		key := tok[:eq]
		raw := tok[eq+1:]
		if strings.HasPrefix(raw, "@") {
			data, err := readSource(raw[1:])
			if err != nil {
				return argSpec{}, fmt.Errorf("argument %q: %w", key, err)
			}
			raw = string(data)
		}
		return argSpec{key: key, value: raw}, nil
	default:
		return argSpec{}, fmt.Errorf("argument %q must be key=value, key:=json or key=@file", tok)
	}
}

func readSource(path string) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(path)
}

// jsonSchema is the subset of JSON Schema needed to coerce string arguments
// into the types a tool expects.
type jsonSchema struct {
	Type       any                    `json:"type"`
	Properties map[string]*jsonSchema `json:"properties"`
	Items      *jsonSchema            `json:"items"`
	Required   []string               `json:"required"`
	Enum       []any                  `json:"enum"`
}

func (s *jsonSchema) typeNames() []string {
	switch t := s.Type.(type) {
	case string:
		return []string{t}
	case []any:
		var out []string
		for _, v := range t {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func (s *jsonSchema) hasType(name string) bool {
	for _, t := range s.typeNames() {
		if t == name {
			return true
		}
	}
	return false
}

// buildArguments merges a base JSON object with key=value arguments, coercing
// plain strings to the types the tool's input schema declares.
func buildArguments(base map[string]any, specs []argSpec, rawSchema []byte) (map[string]any, error) {
	args := map[string]any{}
	for k, v := range base {
		args[k] = v
	}

	var schema *jsonSchema
	if len(rawSchema) > 0 {
		var s jsonSchema
		if err := json.Unmarshal(rawSchema, &s); err == nil {
			schema = &s
		}
	}

	for _, spec := range specs {
		v := spec.value
		if schema != nil {
			prop, ok := schema.Properties[spec.key]
			if ok && prop != nil {
				if !spec.isJSON {
					coerced, err := coerce(v.(string), prop)
					if err != nil {
						return nil, fmt.Errorf("argument %q: %w", spec.key, err)
					}
					v = coerced
				}
				if err := checkEnum(v, prop); err != nil {
					return nil, fmt.Errorf("argument %q: %w", spec.key, err)
				}
			}
		}
		args[spec.key] = v
	}
	return args, nil
}

// coerce converts a command-line string to the type the schema declares.
// Unknown or string-typed schemas pass the value through untouched.
func coerce(s string, schema *jsonSchema) (any, error) {
	types := schema.typeNames()
	if len(types) == 0 || schema.hasType("string") {
		return s, nil
	}
	switch {
	case schema.hasType("boolean"):
		b, err := strconv.ParseBool(s)
		if err != nil {
			return nil, fmt.Errorf("expected a boolean, got %q", s)
		}
		return b, nil
	case schema.hasType("integer"):
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("expected an integer, got %q", s)
		}
		return n, nil
	case schema.hasType("number"):
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil, fmt.Errorf("expected a number, got %q", s)
		}
		return f, nil
	case schema.hasType("array"):
		var v []any
		if err := json.Unmarshal([]byte(s), &v); err == nil {
			return v, nil
		}
		// Fall back to a comma-separated list, coercing each element.
		parts := strings.Split(s, ",")
		out := make([]any, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if schema.Items != nil {
				cv, err := coerce(p, schema.Items)
				if err != nil {
					return nil, err
				}
				out = append(out, cv)
				continue
			}
			out = append(out, p)
		}
		return out, nil
	case schema.hasType("object"):
		var v map[string]any
		if err := json.Unmarshal([]byte(s), &v); err != nil {
			return nil, fmt.Errorf("expected a JSON object, got %q", s)
		}
		return v, nil
	case schema.hasType("null"):
		return nil, nil
	}
	return s, nil
}

// checkEnum rejects a value the schema does not list, so the mistake is caught
// here with the allowed values rather than in a server-side validation error.
func checkEnum(v any, schema *jsonSchema) error {
	if len(schema.Enum) == 0 {
		return nil
	}
	want, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	allowed := make([]string, 0, len(schema.Enum))
	for _, e := range schema.Enum {
		got, err := json.Marshal(e)
		if err != nil {
			continue
		}
		if string(got) == string(want) {
			return nil
		}
		allowed = append(allowed, string(got))
	}
	return fmt.Errorf("%s is not one of %s", want, strings.Join(allowed, ", "))
}

// missingRequired returns required properties absent from args.
func missingRequired(args map[string]any, rawSchema []byte) []string {
	if len(rawSchema) == 0 {
		return nil
	}
	var s jsonSchema
	if err := json.Unmarshal(rawSchema, &s); err != nil {
		return nil
	}
	var missing []string
	for _, r := range s.Required {
		if _, ok := args[r]; !ok {
			missing = append(missing, r)
		}
	}
	return missing
}
