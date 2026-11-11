package rudderotel

import (
	"slices"
	"strings"

	"go.opentelemetry.io/otel/attribute"

	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

// Lists match the Node and Python tracing privacy constants.
var contentKeys = []string{
	"gen_ai.input.messages", "gen_ai.output.messages", "gen_ai.system_instructions",
	"gen_ai.tool.call.arguments", "gen_ai.tool.call.result", "gen_ai.retrieval.documents", "gen_ai.retrieval.query.text",
	"input.value", "output.value", "llm.prompt_template.template", "llm.prompt_template.variables", "reranker.query",
	"ai.response.text", "ai.response.object", "ai.response.reasoning", "ai.response.toolCalls", "ai.response.files",
	"ai.response.providerMetadata", "ai.toolCall.args", "ai.toolCall.result", "ai.request.text", "ai.value", "ai.values",
	"ai.embedding", "ai.embeddings", "ai.documents", "ai.ranking", "ai.decision.state", "ai.decision.questions", "ai.decision.answers",
	"traceloop.entity.input", "traceloop.entity.output",
}
var contentPrefixes = []string{
	"gen_ai.prompt", "gen_ai.completion", "llm.input_messages", "llm.output_messages", "llm.prompts",
	"retrieval.documents", "embedding.embeddings", "reranker.input_documents", "reranker.output_documents", "ai.prompt",
}
var strippedKeys = []string{
	"client.address", "http.client_ip", "net.peer.ip", "network.peer.address", "user_agent.original", "http.user_agent",
	"url.full", "url.path", "url.query", "http.url", "http.target",
}
var strippedPrefixes = []string{"ai.request.headers.", "http.request.header.", "http.response.header."}

func hasPrefix(key string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

func contentAttribute(key string) bool {
	return slices.Contains(contentKeys, key) || hasPrefix(key, contentPrefixes)
}

func contentEvent(name string) bool {
	return name == "gen_ai.choice" || name == "gen_ai.client.inference.operation.details" ||
		(strings.HasPrefix(name, "gen_ai.") && strings.HasSuffix(name, ".message") && len(name) > len("gen_ai..message"))
}

func cleanAttributes(attrs []attribute.KeyValue, capture, all bool) []attribute.KeyValue {
	out := make([]attribute.KeyValue, 0, len(attrs))
	for _, kv := range attrs {
		key := string(kv.Key)
		if slices.Contains(strippedKeys, key) || hasPrefix(key, strippedPrefixes) {
			continue
		}
		content := all || contentAttribute(key)
		if content && !capture {
			continue
		}
		if content {
			switch kv.Value.Type() {
			case attribute.STRING:
				v, ok := hook.Tracing.Redact(kv.Value.AsString())
				if !ok {
					continue
				}
				kv.Value = attribute.StringValue(v)
			case attribute.STRINGSLICE:
				values := slices.Clone(kv.Value.AsStringSlice())
				keep := true
				for i, v := range values {
					masked, ok := hook.Tracing.Redact(v)
					if !ok {
						keep = false
						break
					}
					values[i] = masked
				}
				if !keep {
					continue
				}
				kv.Value = attribute.StringSliceValue(values)
			}
		}
		out = append(out, kv)
	}
	return out
}
