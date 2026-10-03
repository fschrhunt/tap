package wire

import (
	"fmt"
	"io"
	"strings"
	"unicode"
)

// HumanWriter renders terminal controls as visible escapes, preserving tabs and newlines.
// Use it for human diagnostics, never for protocol or machine-readable JSON output.
type HumanWriter struct{ Writer io.Writer }

// Write neutralizes C0/C1 and Unicode format controls, including ESC/OSC introducers.
func (w HumanWriter) Write(p []byte) (int, error) {
	var b strings.Builder
	for _, r := range string(p) {
		if r != '\n' && r != '\t' && (unicode.IsControl(r) || unicode.Is(unicode.Cf, r)) {
			fmt.Fprintf(&b, "\\u%04x", r)
		} else {
			b.WriteRune(r)
		}
	}
	_, err := io.WriteString(w.Writer, b.String())
	if err != nil {
		return 0, err
	}
	return len(p), nil
}
