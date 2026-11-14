package rudderstackai

import (
	"regexp"
	"strings"

	"github.com/rudderlabs/ai-sdk-go/internal/mcpbridge"
)

// User-Agent product -> the client's own initialize name (spec/fixtures/mcp-client-user-agent-v0.1.json).
var userAgentClients = map[string]string{
	"claude-code":  "claude-code",
	"claude":       "Anthropic/ClaudeAI",
	"cursor":       "Cursor",
	"openai-mcp":   "openai-mcp",
	"codex_cli_rs": "codex-mcp-client",
	"opencode":     "opencode",
}

var userAgentVersion = regexp.MustCompile(`^[0-9][0-9A-Za-z.+-]{0,31}$`)

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
	if userAgentVersion.MatchString(version) {
		client.Version = version
	}
	return client
}
