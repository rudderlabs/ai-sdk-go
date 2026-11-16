package spanmodel

import (
	"slices"
	"strings"

	"github.com/rudderlabs/ai-sdk-go/internal/capture"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

// Lists match the Node and Python tracing privacy constants, except error text
// protections, which follow in SDK-6802.
var contentKeys = []string{
	"exception.message", "exception.stacktrace",
	"gen_ai.input.messages", "gen_ai.output.messages", "gen_ai.system_instructions",
	"gen_ai.tool.call.arguments", "gen_ai.tool.call.result", "gen_ai.retrieval.documents", "gen_ai.retrieval.query.text",
	"input.value", "output.value", "llm.prompt_template.template", "llm.prompt_template.variables", "reranker.query",
	"ai.response.text", "ai.response.object", "ai.response.reasoning", "ai.response.toolCalls", "ai.response.files",
	"ai.response.providerMetadata", "ai.toolCall.args", "ai.toolCall.result", "ai.request.text", "ai.value", "ai.values",
	"ai.embedding", "ai.embeddings", "ai.documents", "ai.ranking", "ai.decision.state", "ai.decision.questions", "ai.decision.answers",
	"traceloop.entity.input", "traceloop.entity.output",
	// Pydantic AI (instrumentation v1-v5; tool keys before v3, gen_ai.tool.call.* after)
	"pydantic_ai.all_messages", "all_messages_events", "final_result", "tool_arguments", "tool_response",
	"pydantic_ai.tool.deferral.metadata", "pydantic_ai.decision.state", "pydantic_ai.decision.questions",
	"pydantic_ai.decision.route_questions", "pydantic_ai.decision.answers",
	"model_request_parameters", "model_settings", "embedding_settings",
	// Tool descriptions and schemas (GenAI semconv: opt-in, may be sensitive)
	"gen_ai.tool.definitions",
	// Mirascope ops: request and response messages, traced call arguments and outputs, annotations
	"mirascope.response.messages", "mirascope.response.content", "mirascope.trace.arg_values",
	"mirascope.trace.output", "mirascope.annotation.reasoning",
	// Vercel AI SDK snake_case tool calls (Eve)
	"ai.response.tool_calls",
}

// scopedContentKeys are generic keys that are content only on spans from the named instrumentation scope.
var scopedContentKeys = map[string][]string{
	// Pydantic AI image generation prompts and embedding inputs
	"pydantic-ai": {"prompt", "inputs"},
	// Mirascope's native error text (its default tracer names); handled like exception.message
	"mirascope.llm": {"error.message"},
	"mirascope.ops": {"error.message"},
	// Eve agent spans: the session title and a channel's delivered input come from the user's message
	"eve.agent": {"agent.session.title", "agent.channel.delivery.input"},
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

// toolCallKeys are the content keys of the ToolCalls span kind; every other content key and
// content event is the Messages kind.
var toolCallKeys = []string{
	"gen_ai.tool.call.arguments", "gen_ai.tool.call.result", "ai.toolCall.args", "ai.toolCall.result",
	"tool_arguments", "tool_response",
}

// httpKeys are the always-stripped keys kept with SpanCapture.HTTP: url.path as is, the full URLs
// without the query string and fragment.
var httpKeys = []string{"url.path", "url.full", "http.url", "http.target"}

// toolSpan reports a tool-execution span, whose message content is the tool's arguments and result.
func toolSpan(a map[string]any) bool {
	return a["gen_ai.operation.name"] == "execute_tool" || a["openinference.span.kind"] == "TOOL" ||
		a["traceloop.span.kind"] == "tool" || a["ai.operationId"] == "ai.toolCall" || a["langsmith.span.kind"] == "tool"
}

// errorKeys keep the Messages kind even on a tool span.
var errorKeys = []string{"exception.message", "exception.stacktrace", "error.message"}

// cleanKinds is cleanAttributes per span content kind. In a content event (all) every attribute
// is the Messages kind; on a tool-execution span (tool), its attributes' and its other events'
// content is ToolCalls, except error text.
func cleanKinds(a map[string]any, scope string, kinds capture.Spans, all, tool, masked bool) map[string]any {
	if !kinds.HTTP && kinds.Messages == kinds.ToolCalls {
		return cleanAttributes(a, scope, kinds.Messages, all, masked)
	}
	keep := make(map[string]any, len(a))
	for k, v := range a {
		if all || !contentAttribute(k, scope) {
			keep[k] = v
			continue
		}
		if slices.Contains(toolCallKeys, k) || tool && !slices.Contains(errorKeys, k) {
			if kinds.ToolCalls {
				keep[k] = v
			}
		} else if kinds.Messages {
			keep[k] = v
		}
	}
	// Content of a kind turned off is gone, so the rest is cleaned as captured.
	out := cleanAttributes(keep, scope, !all || kinds.Messages, all, masked)
	if kinds.HTTP {
		for _, k := range httpKeys {
			if v, ok := httpValue(k, a[k]); ok {
				out[k] = v
			}
		}
	}
	return out
}

// httpValue is a kept URL attribute: without its query string and fragment (url.path as is),
// masked; arrays element by element, non-strings dropped.
func httpValue(key string, v any) (any, bool) {
	url := func(s string) (string, bool) {
		if key != "url.path" {
			if cut := strings.IndexAny(s, "?#"); cut >= 0 {
				s = s[:cut]
			}
		}
		masked, ok := hook.Tracing.Redact(s)
		return masked, ok && masked != ""
	}
	switch x := v.(type) {
	case string:
		return url(x)
	case []string:
		out := []any{}
		for _, e := range x {
			if m, ok := url(e); ok {
				out = append(out, m)
			}
		}
		return out, len(out) > 0
	case []any:
		out := []any{}
		for _, e := range x {
			if s, ok := e.(string); ok {
				if m, ok := url(s); ok {
					out = append(out, m)
				}
			}
		}
		return out, len(out) > 0
	}
	return nil, false
}
func hasPrefix(key string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

func contentAttribute(key, scope string) bool {
	return slices.Contains(contentKeys, key) || hasPrefix(key, contentPrefixes) ||
		slices.Contains(scopedContentKeys[scope], key)
}

func contentEvent(name string) bool {
	return name == "gen_ai.choice" || name == "gen_ai.client.inference.operation.details" ||
		(strings.HasPrefix(name, "gen_ai.") && strings.HasSuffix(name, ".message") && len(name) > len("gen_ai..message"))
}
