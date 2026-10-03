// Package human renders untrusted text for people. Terminal controls become visible
// escapes so a server or connector cannot drive the terminal; tabs and newlines pass
// through. Protocol and machine-readable output must never use it.
package human

import (
	"fmt"
	"io"
	"strings"
	"unicode"
)

// Writer escapes C0/C1 and Unicode format controls, including the ESC/OSC introducers,
// on their way to W. Use it for diagnostics and other text a person reads, never for
// JSON or wire data.
type Writer struct{ W io.Writer }

// Raw returns the writer beneath an escaping Writer, or w itself. JSON output uses it to
// keep exact bytes rather than rendering anything as escapes.
func Raw(w io.Writer) io.Writer {
	if h, ok := w.(Writer); ok {
		return h.W
	}
	return w
}

// Write escapes each control or format rune as \uXXXX and reports all of p as written.
func (w Writer) Write(p []byte) (int, error) {
	var b strings.Builder
	for _, r := range string(p) {
		if r != '\n' && r != '\t' && (unicode.IsControl(r) || unicode.Is(unicode.Cf, r)) {
			fmt.Fprintf(&b, `\u%04x`, r)
			continue
		}
		b.WriteRune(r)
	}
	if _, err := io.WriteString(w.W, b.String()); err != nil {
		return 0, err
	}
	return len(p), nil
}
