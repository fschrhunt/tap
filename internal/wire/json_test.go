package wire

import (
	"math"
	"reflect"
	"testing"
)

// TestDecodeMatchesTokenStream pins the one-pass reading to encoding/json's, with numbers as
// float64 and as their spelling: the same value and property order for JSON it reads itself,
// and the same error for JSON it hands over.
func TestDecodeMatchesTokenStream(t *testing.T) {
	for _, input := range []string{
		`{"b":1,"a":{"z":[true,false,null],"2":"two","1":"one"},"b":2}`,
		` [ -0, 0.5, 1e3, 12E-2, 123456789012345678901234567890 ] `,
		`"plain"`, `"esc \" \\ \/ \n é 😀"`, "\"raw é \U0001F600\"", `""`, `{}`, `[]`, `null`,
		`{"tools":[{"name":"echo","inputSchema":{"type":"object","properties":{}}}]}`,
		``, `{`, `[1,]`, `{"a":1,}`, `{"a" 1}`, `01`, `1.`, `-`, `+1`, `1e`, `tru`, `nul`, `{"a":1} x`, `[1] [2]`,
		"\"tab\tinside\"", "\"bad \xff utf8\"", `"bad \x escape"`, `"unterminated`, `1e999`,
	} {
		for _, exact := range []bool{false, true} {
			want, wantErr := decodeTokens([]byte(input), exact)
			got, gotErr := decodeJSON([]byte(input), exact)
			if !reflect.DeepEqual(got, want) || (gotErr == nil) != (wantErr == nil) || gotErr != nil && gotErr.Error() != wantErr.Error() {
				t.Errorf("decodeJSON(%q, %v) = %#v, %v; the token stream gives %#v, %v", input, exact, got, gotErr, want, wantErr)
			}
		}
		want, wantErr := decodeTokens([]byte(input), true)
		got, gotErr := DecodeExactOwned([]byte(input))
		if !reflect.DeepEqual(got, want) || (gotErr == nil) != (wantErr == nil) {
			t.Errorf("DecodeExactOwned(%q) = %#v, %v; the token stream gives %#v, %v", input, got, gotErr, want, wantErr)
		}
	}
}

// TestJSONMatchesTheEncoder pins the one-pass writer to encoding/json's bytes, compact and
// indented, for every kind of value it writes itself, and its handover of the rest.
func TestJSONMatchesTheEncoder(t *testing.T) {
	for _, input := range []string{
		`{"b":1,"a":{"z":[true,false,null],"2":"two","1":"one","e":{},"l":[]},"n":[[],[{}],{"x":[1]}]}`,
		`[0, -0, 0.5, 1e-7, 1.5e-7, 123456789012345680000, 1e21, 1e300, -2.5e-9, 9007199254740993, 3.0]`,
		`"q\" b\\ s/ \b\f\n\r\t \u0001 \u007f é \ud83d\ude00 \u2028\u2029 <a href=\"x\">&</a>"`,
		`"a literal \\u2028 and a lone \ud800 surrogate"`,
		`{"":"","k\"ey\n":"v"}`, `null`, `true`, `""`, `{}`, `[]`,
	} {
		for _, exact := range []bool{false, true} {
			value, err := decodeTokens([]byte(input), exact)
			if err != nil {
				t.Fatal(input, err)
			}
			for _, pretty := range []bool{false, true} {
				want, wantErr := encodeJSON(value, pretty)
				fast, ok := appendJSON(nil, value, pretty, 0)
				if !ok || wantErr != nil || string(fast) != string(want) {
					t.Errorf("appendJSON(%s, pretty %v) = %q, %v\nthe encoder gives %q, %v", input, pretty, fast, ok, want, wantErr)
				}
			}
		}
	}
	for _, other := range []any{map[string]any{"b": 1, "a": 2}, []string{"x"}, Object{{"n", math.NaN()}}, struct{ A int }{1}, Object(nil), []any(nil)} {
		want, wantErr := encodeJSON(other, true)
		got, gotErr := JSON(other, true)
		if string(got) != string(want) || (gotErr == nil) != (wantErr == nil) {
			t.Errorf("JSON(%#v) = %q, %v; the encoder gives %q, %v", other, got, gotErr, want, wantErr)
		}
	}
}
