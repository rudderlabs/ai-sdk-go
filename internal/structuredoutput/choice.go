// Package structuredoutput recognizes a request's forced single named tool,
// independently of the provider or structured-output framework.
package structuredoutput

// ForcedToolName returns the forced name, or empty when the choice is not one named tool.
func ForcedToolName(request any) string {
	r := object(request)
	choice := object(coalesce(r["tool_choice"], r["toolChoice"]))
	switch choice["type"] {
	case "function":
		return name(coalesce(object(choice["function"])["name"], choice["name"]))
	case "tool":
		return name(coalesce(choice["name"], choice["toolName"]))
	}
	config := object(coalesce(r["toolConfig"], object(r["config"])["toolConfig"]))
	if n := name(object(object(config["toolChoice"])["tool"])["name"]); n != "" {
		return n
	}
	gemini := object(config["functionCallingConfig"])
	if gemini["mode"] == "ANY" {
		switch allowed := gemini["allowedFunctionNames"].(type) {
		case []any:
			if len(allowed) == 1 {
				return name(allowed[0])
			}
		case []string:
			if len(allowed) == 1 {
				return name(allowed[0])
			}
		}
	}
	return ""
}
func object(v any) map[string]any { m, _ := v.(map[string]any); return m }
func name(v any) string           { s, _ := v.(string); return s }
func coalesce(a, b any) any {
	if a != nil {
		return a
	}
	return b
}
