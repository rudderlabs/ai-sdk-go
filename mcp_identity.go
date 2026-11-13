package rudderstackai

import (
	"context"
	"encoding/json"

	"github.com/rudderlabs/ai-sdk-go/internal/mcpbridge"
)

// MCPToolCall is the current tool's name and original arguments, available to Identify,
// ClientInfo and ClientInfoFallback.
type MCPToolCall struct {
	Name      string
	Arguments json.RawMessage
}

// ToolCallFromContext returns the current tool call, if this request is tools/call.
// It is available in both wrappers' Identify, ClientInfo and ClientInfoFallback hooks,
// including stateless HTTP.
func ToolCallFromContext(ctx context.Context) (MCPToolCall, bool) {
	call, ok := mcpbridge.ToolCallFromContext(ctx)
	return MCPToolCall{Name: call.Name, Arguments: call.Arguments}, ok
}
