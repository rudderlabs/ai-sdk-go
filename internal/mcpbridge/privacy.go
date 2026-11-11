package mcpbridge

import (
	"strings"
	"unicode"
)

// CleanClientText removes Cc before collapsing Unicode whitespace and limiting code points.
func CleanClientText(value string) string {
	value = strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cc, r) {
			return -1
		}
		return r
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	return string(runes[:min(len(runes), 64)])
}

// CleanClient copies implementation details before a session remembers them.
func CleanClient(client *Client) *Client {
	if client == nil {
		return nil
	}
	return &Client{Name: CleanClientText(client.Name), Title: CleanClientText(client.Title), Version: CleanClientText(client.Version)}
}
