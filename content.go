package rudderstackai

import (
	"fmt"
	"strings"
	"unicode"
)

// propertyName matches warehouse snake casing without renaming the emitted custom key.
func propertyName(key string) string {
	runes := []rune(key)
	var out strings.Builder
	for n, r := range runes {
		if r == '-' || unicode.IsSpace(r) {
			out.WriteByte('_')
			continue
		}
		if unicode.IsUpper(r) && n > 0 {
			prev := runes[n-1]
			if unicode.IsLower(prev) || unicode.IsDigit(prev) ||
				(unicode.IsUpper(prev) && n+1 < len(runes) && unicode.IsLower(runes[n+1])) {
				out.WriteByte('_')
			}
		}
		out.WriteRune(unicode.ToLower(r))
	}
	return out.String()
}

// Content names shared by the Node.js, Python and Go SDKs.
var contentProperties = set("prompt_text", "response_text", "feedback_text", "intent_summary", "explanation", "call_reason", "capability_detail")

func dropContentProperty(key string, capture bool) bool {
	if capture || !contentProperties[propertyName(key)] {
		return false
	}
	warnOnce("content-property:"+key, fmt.Sprintf("custom property %s was dropped because content capture is off; use a non-content property name", key), 0)
	return true
}
