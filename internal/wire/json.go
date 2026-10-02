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

// Decode parses JSON while retaining object property order.
func Decode(data []byte) (any, error) {
	return decodeJSON(data, false)
}

// DecodeExact preserves numeric JSON spelling for tool arguments and retained/inline results.
// Configuration and gateway control fields continue to use Decode's float64 values.
func DecodeExact(data []byte) (any, error) { return decodeJSON(data, true) }

// decodeJSON shares ordered parsing while optionally retaining numbers as json.Number.
func decodeJSON(data []byte, exact bool) (any, error) {
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

// arrayIndex recognizes the integer property keys that JavaScript sorts first.
func arrayIndex(name string) (uint64, bool) {
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
