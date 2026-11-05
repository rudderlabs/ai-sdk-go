package rudderstackai

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	propertyAcronymBoundary = regexp.MustCompile(`([A-Z]+)([A-Z][a-z])`)
	propertyWordBoundary    = regexp.MustCompile(`([a-z0-9])([A-Z])`)
	propertySeparators      = regexp.MustCompile(`[^a-z0-9]+`)
)

// propertyName matches warehouse snake casing without renaming the emitted custom key.
func propertyName(key string) string {
	key = propertyAcronymBoundary.ReplaceAllString(key, "${1}_${2}")
	key = propertyWordBoundary.ReplaceAllString(key, "${1}_${2}")
	return strings.Trim(propertySeparators.ReplaceAllString(strings.ToLower(key), "_"), "_")
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
