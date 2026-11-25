package rudderstackai

import (
	"regexp"
	"strings"

	"github.com/rudderlabs/ai-sdk-go/internal/mcpbridge"
)

// User-Agent product -> the client's own initialize name (spec/fixtures/mcp-client-user-agent-v0.1.json).
var userAgentClients = map[string]string{
	"claude-code":      "claude-code",
	"claude":           "Anthropic/ClaudeAI",
	"cursor":           "Cursor",
	"openai-mcp":       "openai-mcp",
	"codex_cli_rs":     "codex-mcp-client",
	"opencode":         "opencode",
	"claude-user":      "Anthropic/ClaudeAI",
	"codex-mcp-client": "codex-mcp-client",
	"copilot-cli":      "copilot-cli",
	"devin-mcp-client": "rmcp",
}

// Products whose User-Agent version is their own, not the clientInfo version of the name they map to.
var userAgentVersionless = map[string]bool{"devin-mcp-client": true}

// versionPattern is the shape of every client_version (spec/fixtures/mcp-client-info-v0.1.json).
var versionPattern = regexp.MustCompile(`^[0-9][0-9A-Za-z.+-]{0,31}$`)

// clientFromUserAgent is the last HTTP client fallback: the first product token names a
// known client, else other. Only the mapped name and a version-shaped version leave here.
func clientFromUserAgent(userAgent string) *mcpbridge.Client {
	product := strings.Trim(userAgent, " \t")
	if i := strings.IndexAny(product, " \t"); i >= 0 {
		product = product[:i]
	}
	if product == "" {
		return nil
	}
	key, version, _ := strings.Cut(product, "/")
	key = strings.Map(func(c rune) rune {
		if c >= 'A' && c <= 'Z' {
			return c + ('a' - 'A')
		}
		return c
	}, key)
	name, ok := userAgentClients[key]
	if !ok {
		return &mcpbridge.Client{Name: "other"}
	}
	client := &mcpbridge.Client{Name: name}
	if !userAgentVersionless[key] && versionPattern.MatchString(version) {
		client.Version = version
	}
	return client
}
