// Package wire preserves JSON field order and JavaScript's printable JSON format
// at tap's CLI and MCP boundaries.
package wire

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
	"unsafe"
)

// Field is one named value in an Object.
type Field struct {
	Name  string
	Value any
}

// Object retains JavaScript property order: integer keys first, then insertion order.
type Object []Field

// Get returns a property, or nil when it is absent.
func (o Object) Get(name string) any {
	for _, f := range o {
		if f.Name == name {
			return f.Value
		}
	}
	return nil
}

// Has reports whether a property is present, including a null value.
func (o Object) Has(name string) bool {
	for _, f := range o {
		if f.Name == name {
			return true
		}
	}
	return false
}

// Set replaces a property in place or inserts it in JavaScript property order.
func (o *Object) Set(name string, value any) {
	for i := range *o {
		if (*o)[i].Name == name {
			(*o)[i].Value = value
			return
		}
	}
	if index, ok := arrayIndex(name); ok {
		for i, f := range *o {
			other, indexed := arrayIndex(f.Name)
			if !indexed || index < other {
				*o = append(*o, Field{})
				copy((*o)[i+1:], (*o)[i:])
				(*o)[i] = Field{name, value}
				return
			}
		}
	}
	*o = append(*o, Field{name, value})
}

// Delete removes a property without reordering the remaining properties.
func (o *Object) Delete(name string) {
	for i := range *o {
		if (*o)[i].Name == name {
			*o = append((*o)[:i], (*o)[i+1:]...)
			return
		}
	}
}

// MarshalJSON encodes an object without sorting its keys, for encoding/json.
func (o Object) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, f := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := encodeJSON(f.Name, false)
		v, err := encodeJSON(f.Value, false)
		if err != nil {
			return nil, err
		}
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// JSON encodes JSON without HTML escapes, with optional two-space indentation. Values made of
// objects, arrays, strings, numbers, booleans and null are written in one pass; any other
// value is encoded by encoding/json, whose output the pass matches byte for byte.
func JSON(value any, pretty bool) ([]byte, error) {
	if b, ok := appendJSON(make([]byte, 0, 512), value, pretty, 0); ok {
		return b, nil
	}
	return encodeJSON(value, pretty)
}

// encodeJSON encodes through encoding/json. It is the reference for JSON: slower, and the
// encoder for every value JSON does not write itself.
func encodeJSON(value any, pretty bool) ([]byte, error) {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if pretty {
		e.SetIndent("", "  ")
	}
	if err := e.Encode(value); err != nil {
		return nil, err
	}
	s := strings.TrimSuffix(b.String(), "\n")
	s = unescapeSeparators(s)
	return []byte(s), nil
}

// appendJSON writes a value as encodeJSON would, at the given nesting depth when pretty. It
// reports false for a value it does not write itself, and the caller starts over.
func appendJSON(b []byte, value any, pretty bool, depth int) ([]byte, bool) {
	line := func(b []byte, depth int) []byte {
		if !pretty {
			return b
		}
		b = append(b, '\n')
		for range depth {
			b = append(b, ' ', ' ')
		}
		return b
	}
	switch v := value.(type) {
	case nil:
		return append(b, "null"...), true
	case bool:
		return strconv.AppendBool(b, v), true
	case string:
		return appendString(b, v), true
	case int:
		return strconv.AppendInt(b, int64(v), 10), true
	case float64:
		return appendFloat(b, v)
	case json.Number:
		p := parser{data: []byte(v)}
		if _, ok := p.number(); !ok || p.at != len(v) || len(v) == 0 {
			return b, false
		}
		return append(b, v...), true
	case Object:
		if v == nil {
			return b, false
		}
		if len(v) == 0 {
			return append(b, '{', '}'), true
		}
		b = append(b, '{')
		for i, f := range v {
			if i > 0 {
				b = append(b, ',')
			}
			b = appendString(line(b, depth+1), f.Name)
			b = append(b, ':')
			if pretty {
				b = append(b, ' ')
			}
			var ok bool
			if b, ok = appendJSON(b, f.Value, pretty, depth+1); !ok {
				return b, false
			}
		}
		return append(line(b, depth), '}'), true
	case []any:
		if v == nil {
			return b, false
		}
		if len(v) == 0 {
			return append(b, '[', ']'), true
		}
		b = append(b, '[')
		for i, item := range v {
			if i > 0 {
				b = append(b, ',')
			}
			var ok bool
			if b, ok = appendJSON(line(b, depth+1), item, pretty, depth+1); !ok {
				return b, false
			}
		}
		return append(line(b, depth), ']'), true
	}
	return b, false
}

// appendFloat writes a number the way encoding/json does: plain digits, or an exponent for
// the very small and the very large. It reports false for a value JSON cannot hold.
func appendFloat(b []byte, f float64) ([]byte, bool) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return b, false
	}
	format := byte('f')
	if abs := math.Abs(f); abs != 0 && (abs < 1e-6 || abs >= 1e21) {
		format = 'e'
	}
	b = strconv.AppendFloat(b, f, format, -1, 64)
	if n := len(b); format == 'e' && n >= 4 && b[n-4] == 'e' && (b[n-3] == '-' || b[n-3] == '+') && b[n-2] == '0' {
		b[n-2] = b[n-1]
		b = b[:n-1]
	}
	return b, true
}

// appendString writes a quoted string with the escapes encodeJSON ends up with: quotes,
// backslashes and control characters escaped, invalid UTF-8 replaced, everything else as it is.
func appendString(b []byte, s string) []byte {
	const hex = "0123456789abcdef"
	b = append(b, '"')
	start := 0
	for i := 0; i < len(s); {
		if c := s[i]; c < utf8.RuneSelf {
			if c >= ' ' && c != '"' && c != '\\' {
				i++
				continue
			}
			b = append(b, s[start:i]...)
			switch c {
			case '"', '\\':
				b = append(b, '\\', c)
			case '\b':
				b = append(b, '\\', 'b')
			case '\f':
				b = append(b, '\\', 'f')
			case '\n':
				b = append(b, '\\', 'n')
			case '\r':
				b = append(b, '\\', 'r')
			case '\t':
				b = append(b, '\\', 't')
			default:
				b = append(b, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xf])
			}
			i++
			start = i
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			b = append(b, s[start:i]...)
			b = append(b, `\ufffd`...)
			i += size
			start = i
			continue
		}
		i += size
	}
	return append(append(b, s[start:]...), '"')
}

// Decode parses JSON while retaining object property order. Well-formed JSON is read in one
// pass; anything that pass is not sure of, malformed input included, is read by encoding/json
// instead, so values and errors are the ones encoding/json gives.
func Decode(data []byte) (any, error) { return decodeJSON(data, false) }

// DecodeExact preserves numeric JSON spelling for tool arguments and retained/inline results.
// Configuration and gateway control fields continue to use Decode's float64 values.
func DecodeExact(data []byte) (any, error) { return decodeJSON(data, true) }

// DecodeExactOwned is DecodeExact for data the caller owns and will not change: the strings
// of the value it returns share data's memory instead of being copied out of it.
func DecodeExactOwned(data []byte) (any, error) {
	p := parser{data: data, exact: true, share: true}
	if v, ok := p.value(0); ok {
		p.space()
		if p.at == len(data) {
			return v, nil
		}
	}
	return decodeTokens(data, true)
}

// decodeJSON shares ordered parsing while optionally retaining numbers as json.Number.
func decodeJSON(data []byte, exact bool) (any, error) {
	p := parser{data: data, exact: exact}
	if v, ok := p.value(0); ok {
		p.space()
		if p.at == len(data) {
			return v, nil
		}
	}
	return decodeTokens(data, exact)
}

// parser reads JSON from a byte slice without a token stream. A method reports false when it
// meets input it will not vouch for, and the caller abandons the pass. With exact set, a
// number keeps its spelling as a json.Number. With share set, strings are views of data.
type parser struct {
	data         []byte
	at           int
	exact, share bool
}

// string returns data[from:to] as a string, a view of data when the parser shares it.
func (p *parser) string(from, to int) string {
	if p.share && to > from {
		return unsafe.String(&p.data[from], to-from)
	}
	return string(p.data[from:to])
}

// space skips JSON whitespace.
func (p *parser) space() {
	for p.at < len(p.data) {
		switch p.data[p.at] {
		case ' ', '\n', '\t', '\r':
			p.at++
		default:
			return
		}
	}
}

// next skips whitespace and reports the byte it stops at, or zero at the end.
func (p *parser) next() byte {
	p.space()
	if p.at < len(p.data) {
		return p.data[p.at]
	}
	return 0
}

// value reads one value. Nesting deeper than encoding/json's limit is left to it to refuse.
func (p *parser) value(depth int) (any, bool) {
	if depth > 1000 {
		return nil, false
	}
	switch c := p.next(); {
	case c == '{':
		p.at++
		o := Object{}
		if p.next() == '}' {
			p.at++
			return o, true
		}
		for {
			if p.next() != '"' {
				return nil, false
			}
			k, ok := p.text()
			if !ok || p.next() != ':' {
				return nil, false
			}
			p.at++
			v, ok := p.value(depth + 1)
			if !ok {
				return nil, false
			}
			o.Set(k, v)
			switch p.next() {
			case ',':
				p.at++
			case '}':
				p.at++
				return o, true
			default:
				return nil, false
			}
		}
	case c == '[':
		p.at++
		a := []any{}
		if p.next() == ']' {
			p.at++
			return a, true
		}
		for {
			v, ok := p.value(depth + 1)
			if !ok {
				return nil, false
			}
			a = append(a, v)
			switch p.next() {
			case ',':
				p.at++
			case ']':
				p.at++
				return a, true
			default:
				return nil, false
			}
		}
	case c == '"':
		return p.text()
	case c == '-' || c >= '0' && c <= '9':
		return p.number()
	case p.word("true"):
		return true, true
	case p.word("false"):
		return false, true
	case p.word("null"):
		return nil, true
	}
	return nil, false
}

// span passes over a string that starts at the opening quote and reports whether it holds
// an escape. One with a control character, invalid UTF-8 or a bad escape is not vouched for.
func (p *parser) span() (escaped, ok bool) {
	start := p.at + 1
	for i := start; i < len(p.data); i++ {
		switch c := p.data[i]; {
		case c == '\\':
			escaped = true
			i++
		case c < ' ':
			return escaped, false
		case c == '"':
			p.at = i + 1
			if escaped {
				return true, json.Valid(p.data[start-1 : i+1])
			}
			return false, utf8.Valid(p.data[start:i])
		}
	}
	return escaped, false
}

// word consumes a literal when the input continues with it.
func (p *parser) word(literal string) bool {
	if !bytes.HasPrefix(p.data[p.at:], []byte(literal)) {
		return false
	}
	p.at += len(literal)
	return true
}

// text reads a string that starts at the opening quote. One with escapes is unquoted by
// encoding/json.
func (p *parser) text() (string, bool) {
	start := p.at + 1
	escaped, ok := p.span()
	if !ok {
		return "", false
	}
	if escaped {
		var s string
		err := json.Unmarshal(p.data[start-1:p.at], &s)
		return s, err == nil
	}
	return p.string(start, p.at-1), true
}

// number reads a number in JSON's grammar: a float64 as encoding/json gives, or its spelling.
func (p *parser) number() (any, bool) {
	start := p.at
	digits := func() bool {
		from := p.at
		for p.at < len(p.data) && p.data[p.at] >= '0' && p.data[p.at] <= '9' {
			p.at++
		}
		return p.at > from
	}
	if p.data[p.at] == '-' {
		p.at++
	}
	if p.at < len(p.data) && p.data[p.at] == '0' {
		p.at++
	} else if !digits() {
		return nil, false
	}
	if p.at < len(p.data) && p.data[p.at] == '.' {
		p.at++
		if !digits() {
			return nil, false
		}
	}
	if p.at < len(p.data) && (p.data[p.at] == 'e' || p.data[p.at] == 'E') {
		p.at++
		if p.at < len(p.data) && (p.data[p.at] == '+' || p.data[p.at] == '-') {
			p.at++
		}
		if !digits() {
			return nil, false
		}
	}
	if p.exact {
		return json.Number(p.string(start, p.at)), true
	}
	n, err := strconv.ParseFloat(string(p.data[start:p.at]), 64)
	return n, err == nil
}

// decodeTokens parses JSON through encoding/json's token stream. It is the reference for
// Decode: slower, and the source of every error Decode reports.
func decodeTokens(data []byte, exact bool) (any, error) {
	d := json.NewDecoder(bytes.NewReader(data))
	if exact {
		d.UseNumber()
	}
	v, err := decode(d)
	if err != nil {
		return nil, err
	}
	if _, err = d.Token(); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("unexpected trailing JSON")
		}
		return nil, err
	}
	return v, nil
}

// decode reads one recursive JSON value from a token stream.
func decode(d *json.Decoder) (any, error) {
	t, err := d.Token()
	if err != nil {
		return nil, err
	}
	switch t {
	case json.Delim('{'):
		o := Object{}
		for d.More() {
			k, err := d.Token()
			if err != nil {
				return nil, err
			}
			v, err := decode(d)
			if err != nil {
				return nil, err
			}
			o.Set(k.(string), v)
		}
		_, err = d.Token()
		return o, err
	case json.Delim('['):
		a := []any{}
		for d.More() {
			v, err := decode(d)
			if err != nil {
				return nil, err
			}
			a = append(a, v)
		}
		_, err = d.Token()
		return a, err
	}
	return t, nil
}

// String returns JavaScript's String(value) for config interpolation.
func String(v any) string {
	switch v := v.(type) {
	case nil:
		return "null"
	case string:
		return v
	case float64:
		if math.IsNaN(v) {
			return "NaN"
		}
		if math.IsInf(v, 1) {
			return "Infinity"
		}
		if math.IsInf(v, -1) {
			return "-Infinity"
		}
		if v == 0 {
			return "0"
		}
		b, _ := JSON(v, false)
		return string(b)
	case Object:
		return "[object Object]"
	case []any:
		s := make([]string, len(v))
		for i, x := range v {
			if x != nil {
				s[i] = String(x)
			}
		}
		return strings.Join(s, ",")
	default:
		return fmt.Sprint(v)
	}
}

// arrayIndex recognizes the integer property keys that JavaScript sorts first. A key that
// does not start with a digit is refused before it is parsed, since nearly every key is one.
func arrayIndex(name string) (uint64, bool) {
	if name == "" || name[0] < '0' || name[0] > '9' {
		return 0, false
	}
	n, err := strconv.ParseUint(name, 10, 32)
	return n, err == nil && n < 4294967295 && strconv.FormatUint(n, 10) == name
}

// unescapeSeparators follows JSON.stringify while preserving literal backslash escapes.
func unescapeSeparators(s string) string {
	if !strings.Contains(s, `\u202`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			if strings.HasPrefix(s[i:], `\u2028`) || strings.HasPrefix(s[i:], `\u2029`) {
				if s[i+5] == '8' {
					b.WriteRune('\u2028')
				} else {
					b.WriteRune('\u2029')
				}
				i += 5
				continue
			}
			b.WriteByte(s[i])
			i++
			b.WriteByte(s[i])
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// Number parses CLI numbers with JavaScript's whitespace, radix and NaN behavior.
func Number(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	if len(s) > 2 && s[0] == '0' && strings.ContainsAny(s[1:2], "xXbBoO") {
		if n, err := strconv.ParseUint(s, 0, 64); err == nil {
			return float64(n)
		}
		return math.NaN()
	}
	if strings.Contains(strings.ToLower(s), "inf") && s != "Infinity" && s != "+Infinity" && s != "-Infinity" {
		return math.NaN()
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil && !math.IsInf(n, 0) {
		return math.NaN()
	}
	return n
}
