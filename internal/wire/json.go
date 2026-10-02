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

// MarshalJSON encodes an object without sorting its keys.
func (o Object) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, f := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := JSON(f.Name, false)
		v, err := JSON(f.Value, false)
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

// JSON encodes JSON without HTML escapes, with optional two-space indentation.
func JSON(value any, pretty bool) ([]byte, error) {
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

// Decode parses JSON while retaining object property order. Well-formed JSON is read in one
// pass; anything that pass is not sure of, malformed input included, is read by encoding/json
// instead, so values and errors are the ones encoding/json gives.
func Decode(data []byte) (any, error) {
	p := parser{data: data, shallow: math.MaxInt}
	if v, ok := p.value(0); ok {
		p.space()
		if p.at == len(data) {
			return v, nil
		}
	}
	return decodeTokens(data)
}

// DecodeShallow parses like Decode, but leaves every object and array nested deeper than
// depth as a json.RawMessage of its own text: checked to be well-formed, and not decoded. The
// raw values share data's memory. It reports an error where Decode would ask encoding/json.
func DecodeShallow(data []byte, depth int) (any, error) {
	p := parser{data: data, shallow: depth}
	if v, ok := p.value(0); ok {
		p.space()
		if p.at == len(data) {
			return v, nil
		}
	}
	return nil, fmt.Errorf("JSON this pass does not read")
}

// parser reads JSON from a byte slice without a token stream. A method reports false when it
// meets input it will not vouch for, and the caller abandons the pass. Objects and arrays
// nested deeper than shallow are skipped and returned as their text.
type parser struct {
	data    []byte
	at      int
	shallow int
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
	case depth > p.shallow && (c == '{' || c == '['):
		start := p.at
		if !p.skip(depth) {
			return nil, false
		}
		return json.RawMessage(p.data[start:p.at]), true
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

// skip passes over one value, checking it as value does without building it. A string with
// an escape is checked by encoding/json.
func (p *parser) skip(depth int) bool {
	if depth > 1000 {
		return false
	}
	switch c := p.next(); {
	case c == '{' || c == '[':
		end := c + 2
		p.at++
		if p.next() == end {
			p.at++
			return true
		}
		for {
			if c == '{' {
				if p.next() != '"' {
					return false
				}
				if _, ok := p.span(); !ok || p.next() != ':' {
					return false
				}
				p.at++
			}
			if !p.skip(depth + 1) {
				return false
			}
			switch p.next() {
			case ',':
				p.at++
			case end:
				p.at++
				return true
			default:
				return false
			}
		}
	case c == '"':
		_, ok := p.span()
		return ok
	case c == '-' || c >= '0' && c <= '9':
		_, ok := p.number()
		return ok
	}
	return p.word("true") || p.word("false") || p.word("null")
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
	return string(p.data[start : p.at-1]), true
}

// number reads a number in JSON's grammar as a float64, as encoding/json does.
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
	n, err := strconv.ParseFloat(string(p.data[start:p.at]), 64)
	return n, err == nil
}

// decodeTokens parses JSON through encoding/json's token stream. It is the reference for
// Decode: slower, and the source of every error Decode reports.
func decodeTokens(data []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(data))
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
