package config

import (
	"fmt"
	"strings"
	"unicode/utf16"
)

// JSONError reports invalid JSON using the original CLI's JSON.parse diagnostics.
// Parsing valid values remains the standard library's job; this is only an error formatter.
func JSONError(data []byte, fallback error) string {
	p := jsonDiagnostic{s: string(data)}
	if msg := p.value(); msg != "" {
		return msg
	}
	p.space()
	if p.i < len(p.s) {
		return p.position("Unexpected non-whitespace character after JSON")
	}
	return fallback.Error()
}

type jsonDiagnostic struct {
	s string
	i int
}

// space skips precisely the whitespace permitted by JSON.
func (p *jsonDiagnostic) space() {
	for p.i < len(p.s) && strings.ContainsRune(" \t\r\n", rune(p.s[p.i])) {
		p.i++
	}
}

// position formats a diagnostic with JavaScript UTF-16 positions and columns.
func (p *jsonDiagnostic) position(message string) string {
	prefix := p.s[:p.i]
	offset := len(utf16.Encode([]rune(prefix)))
	line := strings.Count(prefix, "\n") + 1
	tail := prefix[strings.LastIndex(prefix, "\n")+1:]
	column := len(utf16.Encode([]rune(tail))) + 1
	if !strings.HasSuffix(message, "JSON") {
		message += " in JSON"
	}
	return fmt.Sprintf("%s at position %d (line %d column %d)", message, offset, line, column)
}

// unexpected renders the token and nearby input in V8's diagnostic format.
func (p *jsonDiagnostic) unexpected() string {
	if p.i == len(p.s) {
		return "Unexpected end of JSON input"
	}
	if p.s == "undefined" || p.s == "NaN" || p.s == "Infinity" || p.s == "[object Object]" {
		return fmt.Sprintf("\"%s\" is not valid JSON", p.s)
	}
	snippet := "\"" + p.s + "\""
	if len(p.s) >= 20 {
		lo, hi := max(0, p.i-10), min(len(p.s), p.i+10)
		snippet = "\"" + p.s[lo:hi] + "\""
		if lo > 0 {
			snippet = "..." + snippet
		}
		if hi < len(p.s) {
			snippet += "..."
		}
	}
	return fmt.Sprintf("Unexpected token '%c', %s is not valid JSON", []rune(p.s[p.i:])[0], snippet)
}

// value follows JSON grammar until the first syntax failure, without constructing values.
func (p *jsonDiagnostic) value() string {
	p.space()
	if p.i == len(p.s) {
		return p.unexpected()
	}
	switch p.s[p.i] {
	case '{':
		return p.object()
	case '[':
		return p.array()
	case '"':
		return p.quoted()
	case 't', 'f', 'n':
		word := "null"
		if p.s[p.i] == 't' {
			word = "true"
		}
		if p.s[p.i] == 'f' {
			word = "false"
		}
		for _, c := range []byte(word) {
			if p.i == len(p.s) || p.s[p.i] != c {
				return p.unexpected()
			}
			p.i++
		}
		return ""
	default:
		if p.s[p.i] == '-' || p.digit() {
			return p.number()
		}
		return p.unexpected()
	}
}

// object distinguishes missing names, colons and separators as JSON.parse does.
func (p *jsonDiagnostic) object() string {
	p.i++
	p.space()
	if p.i < len(p.s) && p.s[p.i] == '}' {
		p.i++
		return ""
	}
	first := true
	for {
		if p.i == len(p.s) || p.s[p.i] != '"' {
			if first {
				return p.position("Expected property name or '}'")
			}
			return p.position("Expected double-quoted property name")
		}
		if msg := p.quoted(); msg != "" {
			return msg
		}
		p.space()
		if p.i == len(p.s) || p.s[p.i] != ':' {
			return p.position("Expected ':' after property name")
		}
		p.i++
		if msg := p.value(); msg != "" {
			return msg
		}
		p.space()
		if p.i < len(p.s) && p.s[p.i] == '}' {
			p.i++
			return ""
		}
		if p.i == len(p.s) || p.s[p.i] != ',' {
			return p.position("Expected ',' or '}' after property value")
		}
		p.i++
		p.space()
		first = false
	}
}

// array distinguishes missing elements from missing separators.
func (p *jsonDiagnostic) array() string {
	p.i++
	p.space()
	if p.i < len(p.s) && p.s[p.i] == ']' {
		p.i++
		return ""
	}
	for {
		if msg := p.value(); msg != "" {
			return msg
		}
		p.space()
		if p.i < len(p.s) && p.s[p.i] == ']' {
			p.i++
			return ""
		}
		if p.i == len(p.s) || p.s[p.i] != ',' {
			return p.position("Expected ',' or ']' after array element")
		}
		p.i++
	}
}

// quoted recognizes escape failures and unterminated strings.
func (p *jsonDiagnostic) quoted() string {
	p.i++
	for p.i < len(p.s) {
		c := p.s[p.i]
		if c == '"' {
			p.i++
			return ""
		}
		if c < 0x20 {
			return p.position("Bad control character in string literal")
		}
		if c == '\\' {
			p.i++
			if p.i == len(p.s) {
				break
			}
			c = p.s[p.i]
			if c == 'u' {
				for range 4 {
					p.i++
					if p.i == len(p.s) || !strings.ContainsRune("0123456789abcdefABCDEF", rune(p.s[p.i])) {
						return p.position("Bad Unicode escape")
					}
				}
			} else if !strings.ContainsRune(`"\/bfnrt`, rune(c)) {
				return p.position("Bad escaped character")
			}
		}
		p.i++
	}
	return p.position("Unterminated string")
}

// digit reports whether the current byte is a decimal digit.
func (p *jsonDiagnostic) digit() bool { return p.i < len(p.s) && p.s[p.i] >= '0' && p.s[p.i] <= '9' }

// number identifies malformed signs, fractions and exponents.
func (p *jsonDiagnostic) number() string {
	if p.s[p.i] == '-' {
		p.i++
		if !p.digit() {
			return p.position("No number after minus sign")
		}
	}
	if p.s[p.i] == '0' {
		p.i++
		if p.digit() {
			return p.position("Unexpected number")
		}
	} else {
		for p.digit() {
			p.i++
		}
	}
	if p.i < len(p.s) && p.s[p.i] == '.' {
		p.i++
		if !p.digit() {
			return p.position("Unterminated fractional number")
		}
		for p.digit() {
			p.i++
		}
	}
	if p.i < len(p.s) && (p.s[p.i] == 'e' || p.s[p.i] == 'E') {
		p.i++
		if p.i < len(p.s) && (p.s[p.i] == '+' || p.s[p.i] == '-') {
			p.i++
		}
		if !p.digit() {
			return p.position("Exponent part is missing a number")
		}
		for p.digit() {
			p.i++
		}
	}
	return ""
}
