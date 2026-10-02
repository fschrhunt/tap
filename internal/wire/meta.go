package wire

import (
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ForwardMeta keeps application metadata but removes protocol identity and progress
// tokens that belong to the current hop; tap does not relay progress notifications.
func ForwardMeta(meta mcp.Meta) mcp.Meta {
	var forwarded mcp.Meta
	for key, value := range meta {
		if strings.HasPrefix(key, "io.modelcontextprotocol/") || key == "progressToken" {
			continue
		}
		if forwarded == nil {
			forwarded = mcp.Meta{}
		}
		forwarded[key] = value
	}
	return forwarded
}
