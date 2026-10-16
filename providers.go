package rudderstackai

import (
	"regexp"
	"slices"
	"strings"
)

// providerAliases maps provider labels to canonical names; the same table as
// spec/fixtures/providers-v0.1.json. Only aliases are listed: a name already sent as a provider is
// never renamed.
var providerAliases = map[string]string{
	"amazon_bedrock":       "aws.bedrock",
	"amazon-bedrock":       "aws.bedrock",
	"bedrock":              "aws.bedrock",
	"anthropicaws":         "aws.bedrock",
	"google_vertexai":      "gcp.vertex_ai",
	"google.vertex":        "gcp.vertex_ai",
	"vertex":               "gcp.vertex_ai",
	"vertex_ai":            "gcp.vertex_ai",
	"anthropicgooglecloud": "gcp.vertex_ai",
	"google_genai":         "gcp.gemini",
	"google.generative-ai": "gcp.gemini",
	"gemini":               "gcp.gemini",
	"azure":                "azure.ai.openai",
	"azure_openai":         "azure.ai.openai",
	"foundry":              "azure.ai.inference",
	"firstparty":           "anthropic",
	"mistralai":            "mistral",
	"litellm_proxy":        "litellm",
}

// routers are gateways whose served model's prefix names the real vendor (openai/gpt-4o).
var routers = []string{"openrouter", "litellm", "gateway"}

// normalizeProvider returns a provider label's canonical name; an unknown label is unchanged.
func normalizeProvider(provider string) string {
	if provider == "" {
		return ""
	}
	if canonical, ok := providerAliases[strings.ToLower(strings.TrimSpace(provider))]; ok {
		return canonical
	}
	return provider
}

// routedProvider is the vendor of a router's vendor-prefixed model (openrouter + openai/gpt-4o is
// openai).
func routedProvider(provider, model string) string {
	slash := strings.Index(model, "/")
	if provider == "" || slash <= 0 {
		return provider
	}
	if !slices.Contains(routers, strings.ToLower(strings.TrimSpace(normalizeProvider(provider)))) {
		return provider
	}
	return normalizeProvider(model[:slash])
}

func bareModel(model string) string { return strings.ToLower(model[strings.LastIndex(model, "/")+1:]) }

// What may follow a model name without changing its identity: dates, snapshots, versions, tags.
var versionSuffix = regexp.MustCompile(`^(?:-[0-9]{4}-[0-9]{2}-[0-9]{2}|-[0-9]{8}|-[0-9]{3,4}|-v[0-9]+(?:\.[0-9]+)*|-latest)*(?:[:@][0-9a-z._-]+)?$`)

// isVersionOf reports whether name is base, or base followed by a version suffix.
func isVersionOf(name, base string) bool {
	return strings.HasPrefix(name, base) && (len(name) == len(base) || versionSuffix.MatchString(name[len(base):]))
}

// routedModel returns the requested model when a router or fallback served another one; empty
// when the served model is the requested one, with or without a vendor prefix or version suffix.
func routedModel(requested, served string) string {
	if requested == "" || served == "" {
		return ""
	}
	a, b := bareModel(requested), bareModel(served)
	if isVersionOf(a, b) || isVersionOf(b, a) {
		return ""
	}
	return requested
}

// normalizeServiceTier maps a provider-reported tier to a spec service_tier: default and auto are
// standard; priority is fast except at Anthropic (committed capacity: other); unknown is other.
func normalizeServiceTier(provider, tier string) string {
	t := strings.ToLower(strings.TrimSpace(tier))
	switch t {
	case "":
		return ""
	case "batch", "flex", "standard", "fast", "other":
		return t
	case "default", "auto", "standard_only":
		return "standard"
	case "priority":
		if strings.ToLower(normalizeProvider(provider)) == "anthropic" {
			return "other"
		}
		return "fast"
	default:
		return "other"
	}
}
