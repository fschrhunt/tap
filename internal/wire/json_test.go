package wire

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestDecodeMatchesTokenStream pins Decode's one-pass reading to encoding/json's: the same
// value and property order for JSON it reads itself, and the same error for JSON it hands over.
func TestDecodeMatchesTokenStream(t *testing.T) {
	for _, input := range []string{
		`{"b":1,"a":{"z":[true,false,null],"2":"two","1":"one"},"b":2}`,
		` [ -0, 0.5, 1e3, 12E-2, 123456789012345678901234567890 ] `,
		`"plain"`, `"esc \" \\ \/ \n é 😀"`, "\"raw é \U0001F600\"", `""`, `{}`, `[]`, `null`,
		`{"tools":[{"name":"echo","inputSchema":{"type":"object","properties":{}}}]}`,
		``, `{`, `[1,]`, `{"a":1,}`, `{"a" 1}`, `01`, `1.`, `-`, `+1`, `1e`, `tru`, `nul`, `{"a":1} x`, `[1] [2]`,
		"\"tab\tinside\"", "\"bad \xff utf8\"", `"bad \x escape"`, `"unterminated`, `1e999`,
	} {
		want, wantErr := decodeTokens([]byte(input))
		got, gotErr := Decode([]byte(input))
		if !reflect.DeepEqual(got, want) || (gotErr == nil) != (wantErr == nil) || gotErr != nil && gotErr.Error() != wantErr.Error() {
			t.Errorf("Decode(%q) = %#v, %v; the token stream gives %#v, %v", input, got, gotErr, want, wantErr)
		}
	}
}

// TestDecodeShallowKeepsDeepValuesRaw pins the depth below which values stay text, and that
// a malformed deep value is an error rather than raw text.
func TestDecodeShallowKeepsDeepValuesRaw(t *testing.T) {
	v, err := DecodeShallow([]byte(`{"a":{"b":{"c":[1,"x\n"]},"n":1,"s":"t"}}`), 1)
	want := Object{{"a", Object{{"b", json.RawMessage(`{"c":[1,"x\n"]}`)}, {"n", float64(1)}, {"s", "t"}}}}
	if err != nil || !reflect.DeepEqual(v, want) {
		t.Fatalf("DecodeShallow = %#v, %v; want %#v", v, err, want)
	}
	for _, bad := range []string{`{"a":{"b":{"c":[1,}}}}`, `{"a":{"b":{"c":"\x"}}}`, `{"a":{"b":{"c":01}}}`, `{"a":{"b":{"c" 1}}}`} {
		if v, err := DecodeShallow([]byte(bad), 1); err == nil {
			t.Errorf("DecodeShallow(%q) = %#v, want an error", bad, v)
		}
	}
}
