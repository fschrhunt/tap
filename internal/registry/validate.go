package registry

import (
	"encoding/json"
	"fmt"
	"math/big"
	"reflect"
	"strings"

	"github.com/fschrhunt/tap/internal/wire"
	"github.com/google/jsonschema-go/jsonschema"
)

// validateArguments checks the complete advertised schema without remote loading or coercion.
// Common violations get value-free field diagnostics; arbitrary argument values are never echoed.
func validateArguments(schema any, args any) error {
	b, err := wire.JSON(schema, false)
	if err != nil {
		return &Failure{"schema_unavailable", "input schema cannot be decoded", "Refresh the server catalog."}
	}
	var s jsonschema.Schema
	if json.Unmarshal(b, &s) != nil {
		return &Failure{"schema_unavailable", "input schema is malformed", "The server must publish a valid inputSchema."}
	}
	if s.Schema != "" && s.Schema != "https://json-schema.org/draft/2020-12/schema" && s.Schema != "http://json-schema.org/draft-07/schema#" && s.Schema != "https://json-schema.org/draft-07/schema#" {
		return &Failure{"schema_unavailable", "input schema draft is unsupported", "Use draft-07 or draft 2020-12; no tool call was sent."}
	}
	r, err := s.Resolve(nil)
	if err != nil {
		return &Failure{"schema_unavailable", "input schema cannot be resolved locally", "Remote schema fetching is disabled; the server must inline its schema references."}
	}
	ab, err := wire.JSON(args, false)
	if err != nil {
		return &Failure{"invalid_arguments", "arguments must be JSON", "Inspect the tool's full schema."}
	}
	plain, err := decodeResult(ab)
	if err != nil {
		return &Failure{"invalid_arguments", "arguments must be JSON", "Inspect the tool's full schema."}
	}
	if err = r.Validate(plain); err != nil {
		message := diagnose(&s, plain, "")
		if message == "" {
			message = "arguments violate the complete inputSchema"
		}
		return &Failure{"invalid_arguments", message, "Inspect this tool with plugin_search ids and detail full; correct arguments without changing their intended meaning."}
	}
	return nil
}

// diagnose explains common schema constraints using JSON Pointer paths, never instance values.
func diagnose(s *jsonschema.Schema, v any, p string) string {
	if s == nil {
		return ""
	}
	label := p
	if label == "" {
		label = "/"
	}
	kind := "null"
	switch x := v.(type) {
	case map[string]any:
		kind = "object"
	case []any:
		kind = "array"
	case string:
		kind = "string"
	case bool:
		kind = "boolean"
	case float64:
		kind = "number"
		if x == float64(int64(x)) {
			kind = "integer"
		}
	case json.Number:
		kind = "number"
		if n, ok := new(big.Rat).SetString(string(x)); ok && n.IsInt() {
			kind = "integer"
		}
	}
	if s.Type != "" && kind != s.Type && !(s.Type == "number" && kind == "integer") {
		return fmt.Sprintf("%s: expected %s, received %s", label, s.Type, kind)
	}
	if s.Enum != nil {
		found := false
		for _, e := range s.Enum {
			found = found || equalDiagnosticValue(e, v)
		}
		if !found {
			b, _ := json.Marshal(s.Enum)
			return fmt.Sprintf("%s: expected one of %s", label, b)
		}
	}
	if o, ok := v.(map[string]any); ok {
		for _, key := range s.Required {
			if _, ok := o[key]; !ok {
				return pointerChild(p, key) + ": required field is missing"
			}
		}
		for _, key := range sortedKeys(o) {
			if child, ok := s.Properties[key]; ok {
				if message := diagnose(child, o[key], pointerChild(p, key)); message != "" {
					return message
				}
			}
		}
	}
	if a, ok := v.([]any); ok {
		for i, x := range a {
			if message := diagnose(s.Items, x, fmt.Sprintf("%s/%d", p, i)); message != "" {
				return message
			}
		}
	}
	return ""
}

// equalDiagnosticValue compares schema enum numbers by value rather than Go representation.
func equalDiagnosticValue(a, b any) bool {
	if reflect.DeepEqual(a, b) {
		return true
	}
	toRat := func(v any) *big.Rat {
		var text string
		switch x := v.(type) {
		case json.Number:
			text = string(x)
		case float64:
			text = fmt.Sprint(x)
		default:
			return nil
		}
		r, _ := new(big.Rat).SetString(text)
		return r
	}
	x, y := toRat(a), toRat(b)
	return x != nil && y != nil && x.Cmp(y) == 0
}

// pointerChild escapes property names according to RFC 6901.
func pointerChild(p, key string) string {
	return p + "/" + strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
}
