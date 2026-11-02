package registry

import "testing"

// Numeric arguments validate as numbers, not as the strings json.Number would look like.
func TestValidateArgumentsAcceptsNumbers(t *testing.T) {
	schema := map[string]any{
		"type":     "object",
		"required": []any{"limit"},
		"properties": map[string]any{
			"limit": map[string]any{"type": []any{"integer", "null"}, "minimum": 0, "format": "uint"},
		},
	}
	args, err := decodeResult([]byte(`{"limit":3}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := validateArguments(schema, args); err != nil {
		t.Fatalf("valid integer refused: %v", err)
	}
	args, _ = decodeResult([]byte(`{"limit":-1}`))
	if err := validateArguments(schema, args); err == nil {
		t.Fatal("negative integer accepted below minimum")
	}
}
