package rudderotel

import (
	"encoding/json"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	analytics "github.com/rudderlabs/analytics-go/v4"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
	"github.com/rudderlabs/ai-sdk-go/internal/kit"
	"github.com/rudderlabs/ai-sdk-go/internal/structuredoutput"
)

func spanRole(s sdktrace.ReadOnlySpan) string {
	a := attributes(s.Attributes())
	if op := firstString(a, "gen_ai.operation.name"); op != "" {
		switch op {
		case "chat", "text_completion", "generate_content":
			return "llm"
		case "execute_tool":
			return "tool"
		default:
			return ""
		}
	}
	if kind := firstString(a, "openinference.span.kind"); kind != "" {
		switch strings.ToUpper(kind) {
		case "LLM":
			return "llm"
		case "TOOL":
			return "tool"
		default:
			return ""
		}
	}
	if v := a["traceloop.span.kind"]; v.Type() == attribute.STRING && v.AsString() == "tool" {
		return "tool"
	}
	switch firstString(a, "llm.request.type") {
	case "chat", "completion":
		return "llm"
	}
	return ""
}

var outputPart = regexp.MustCompile(`^llm\.output_messages\.0\.message\.contents\.(\d+)\.message_content\.text$`)

func outputText(a map[string]attribute.Value) string {
	var messages []any
	if v := a["gen_ai.output.messages"]; v.Type() == attribute.STRING && json.Unmarshal([]byte(v.AsString()), &messages) == nil {
		texts := []string{}
		for _, m := range messages {
			message, ok := m.(map[string]any)
			if !ok {
				continue
			}
			parts, _ := message["parts"].([]any)
			for _, p := range parts {
				part, ok := p.(map[string]any)
				if !ok || part["type"] != "text" {
					continue
				}
				if text, ok := part["content"].(string); ok {
					texts = append(texts, text)
				}
			}
		}
		if len(texts) > 0 {
			return strings.Join(texts, "")
		}
	}
	if v := firstString(a, "gen_ai.completion.0.content", "llm.output_messages.0.message.content"); v != "" {
		return v
	}
	type part struct {
		index int
		text  string
	}
	parts := []part{}
	for k, v := range a {
		if match := outputPart.FindStringSubmatch(k); match != nil {
			index, _ := strconv.Atoi(match[1])
			parts = append(parts, part{index, asString(v)})
		}
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].index < parts[j].index })
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.text)
	}
	if b.Len() > 0 {
		return b.String()
	}
	if firstString(a, "output.mime_type") == "text/plain" {
		return firstString(a, "output.value")
	}
	return ""
}

// Mirascope 2.8.2 serialize_mirascope_usage supplies independent fallback counts.
// Bound the UTF-8 JSON before parsing; malformed and non-object values are ignored.
func mirascopeUsage(v attribute.Value) map[string]attribute.Value {
	if v.Type() != attribute.STRING || len(v.AsString()) > 65536 {
		return nil
	}
	var usage map[string]json.RawMessage
	if json.Unmarshal([]byte(v.AsString()), &usage) != nil {
		return nil
	}
	out := map[string]attribute.Value{}
	for _, key := range []string{"input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens"} {
		var value any
		if json.Unmarshal(usage[key], &value) == nil {
			if n, ok := value.(float64); ok {
				out[key] = attribute.Float64Value(n)
			}
		}
	}
	return out
}

func usageCount(a, usage map[string]attribute.Value, fallback string, keys ...string) (int, bool) {
	if n, ok := count(a, keys...); ok {
		return n, true
	}
	return count(usage, fallback)
}

// Only known API provider IDs are aliases; dotted semconv names stay intact.
func productProvider(provider string) string {
	switch provider {
	case "anthropic.messages":
		return "anthropic"
	case "openai.chat", "openai.responses":
		return "openai"
	case "google.generative-ai":
		return "gcp.gemini"
	default:
		return provider
	}
}

func spanCall(s sdktrace.ReadOnlySpan) rudderstackai.LLMCall {
	a := attributes(s.Attributes())
	usage := mirascopeUsage(a["mirascope.response.usage"])
	input, inputKnown := usageCount(a, usage, "input_tokens", "gen_ai.usage.input_tokens", "gen_ai.usage.prompt_tokens", "llm.token_count.prompt")
	output, outputKnown := usageCount(a, usage, "output_tokens", "gen_ai.usage.output_tokens", "gen_ai.usage.completion_tokens", "llm.token_count.completion")
	cacheRead, _ := usageCount(a, usage, "cache_read_tokens", "gen_ai.usage.cache_read.input_tokens", "gen_ai.usage.cache_read_input_tokens", "llm.token_count.prompt_details.cache_read")
	cacheWrite, _ := usageCount(a, usage, "cache_write_tokens", "gen_ai.usage.cache_write.input_tokens", "gen_ai.usage.cache_creation.input_tokens", "gen_ai.usage.cache_creation_input_tokens", "llm.token_count.prompt_details.cache_write")
	reason := firstString(a, "llm.finish_reason", "gen_ai.completion.0.finish_reason")
	if reasons := stringArray(a["gen_ai.response.finish_reasons"]); len(reasons) > 0 {
		reason = reasons[0]
	} else if _, exists := a["gen_ai.response.finish_reasons"]; !exists {
		if singular := firstString(a, "gen_ai.response.finish_reason"); singular != "" {
			reason = strings.ToLower(singular[strings.LastIndex(singular, ".")+1:])
		}
	}
	call := rudderstackai.LLMCall{
		Provider:       productProvider(strings.ToLower(firstString(a, "gen_ai.provider.name", "gen_ai.system", "llm.provider", "llm.system"))),
		Model:          firstString(a, "gen_ai.response.model", "llm.response.model_name", "llm.model_name", "gen_ai.request.model", "llm.request.model_name"),
		RequestedModel: firstString(a, "gen_ai.request.model", "llm.request.model_name"),
		InputTokens:    input, OutputTokens: output, CachedInputTokens: cacheRead, CacheWriteTokens: cacheWrite,
		UsageUnknown: !inputKnown && !outputKnown, FinishReason: reason, ResponseID: firstString(a, "gen_ai.response.id"),
		AgentName: firstString(a, "gen_ai.agent.name", "agent.name"), Integration: "tracing", StartedAt: s.StartTime(), EndedAt: s.EndTime(), OutputText: outputText(a),
	}
	for _, k := range []string{"gen_ai.response.time_to_first_chunk", "gen_ai.client.operation.time_to_first_chunk"} {
		v := a[k]
		var seconds float64
		switch v.Type() {
		case attribute.FLOAT64:
			seconds = v.AsFloat64()
		case attribute.INT64:
			seconds = float64(v.AsInt64())
		default:
			continue
		}
		if seconds >= 0 && !math.IsNaN(seconds) && !math.IsInf(seconds, 0) && seconds < float64(math.MaxInt64)/float64(time.Second) {
			call.FirstTokenAt = s.StartTime().Add(time.Duration(seconds * float64(time.Second)))
			break
		}
	}
	if s.Status().Code == codes.Error {
		call.ErrorType = spanErrorType(s)
	}
	return call
}

func spanErrorType(s sdktrace.ReadOnlySpan) string {
	a := attributes(s.Attributes())
	typeName := firstString(a, "error.type")
	if code, err := strconv.Atoi(firstString(a, "http.response.status_code", "error.type")); err == nil {
		if t := rudderstackai.ErrorTypeForStatus(code); t != "" {
			return t
		}
	}
	events := s.Events()
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Name == "exception" {
			typeName += " " + firstString(attributes(events[i].Attributes), "exception.type")
			break
		}
	}
	n := strings.ToLower(typeName)
	switch {
	case strings.Contains(n, "ratelimit"), strings.Contains(n, "throttl"), strings.Contains(n, "rate_limit"), strings.Contains(n, "insufficient_quota"):
		return "rate_limit"
	case strings.Contains(n, "timeout"), strings.Contains(n, "deadline"):
		return "timeout"
	case strings.Contains(n, "abort"), strings.Contains(n, "cancel"):
		return "cancelled"
	case strings.Contains(n, "authentication"), strings.Contains(n, "permissiondenied"), strings.Contains(n, "accessdenied"):
		return "auth_error"
	case strings.Contains(n, "validation"), strings.Contains(n, "badrequest"):
		return "invalid_request"
	case strings.Contains(n, "connection"):
		return "connection_error"
	default:
		return "api_error"
	}
}

func toolValue(a map[string]attribute.Value, keys ...string) any {
	for _, k := range keys {
		if v, exists := a[k]; exists {
			if v.Type() != attribute.STRING {
				return v.AsInterface()
			}
			var decoded any
			if json.Unmarshal([]byte(v.AsString()), &decoded) != nil {
				return v.AsString()
			}
			if decoded == nil {
				return json.RawMessage("null")
			}
			return decoded
		}
	}
	return nil
}

// resolveProductOwner prefers a live matching ambient interaction; otherwise,
// it requires exactly one live owner of the ID.
func resolveProductOwner(id string, ambient *rudderstackai.Interaction) *rudderstackai.Interaction {
	if ambient != nil {
		if id != "" && ambient.ID() != id {
			ambient = nil
		} else if ambient.Ended() {
			// A matching ended context is unresolved, never a detached lookup.
			return nil
		}
	}
	if id == "" {
		return ambient
	}
	owner, _ := hook.Tracing.ProductOwner(id, ambient).(*rudderstackai.Interaction)
	return owner
}

func recordSpan(s sdktrace.ReadOnlySpan, client analytics.Client, owner *rudderstackai.Interaction) {
	role := spanRole(s)
	if role == "" {
		return
	}
	a := attributes(s.Attributes())
	id := firstString(a, InteractionIDAttribute)
	if id == "" {
		return
	}
	i := owner
	// A late or changed ID cannot reroute an already-owned span.
	if i == nil || i.ID() != id || i.Ended() {
		return
	}
	hook.Tracing.AdoptAnalytics(i, client)
	if role == "llm" {
		call := spanCall(s)
		// Built only when the event will carry content; core masks and caps it.
		if hook.CapturesCallContent(i) {
			call.InputMessages, call.OutputToolCalls = spanCallContent(a)
		}
		hook.Tracing.RecordSpan(i, call, spanSource(s, call))
		// Read original attributes for accounting, independently of content capture.
		forced := structuredoutput.ForcedToolName(parsedJSON(rawAttr(a, "llm.invocation_parameters")))
		if forced == "" {
			choice := coalesce(rawAttr(a, "gen_ai.request.tool_choice"), coalesce(rawAttr(a, "gen_ai.request.toolChoice"), rawAttr(a, "ai.prompt.toolChoice")))
			forced = structuredoutput.ForcedToolName(map[string]any{"tool_choice": parsedJSON(choice)})
		}
		if forced != "" || hook.HasStructuredTools(i) {
			for _, message := range inputOf(a) {
				for _, part := range message.Parts {
					if part["type"] == "tool_call_response" {
						id, _ := part["id"].(string)
						kit.RecordToolOutput(i, id, rudderstackai.ToolResult{Error: rudderstackai.Ptr(false), Result: part["response"]}, true, kit.RawOutput)
					}
				}
			}
			var tools []rudderstackai.ToolCall
			for _, message := range outputOf(a) {
				for _, part := range message.Parts {
					if forced != "" && part["type"] == "tool_call" && part["name"] == forced {
						id, _ := part["id"].(string)
						tools = append(tools, rudderstackai.ToolCall{ID: id, Name: forced, Args: part["arguments"], StructuredCandidate: true})
					}
				}
			}
			i.RecordToolCalls(tools...)
		}
		if !call.FirstTokenAt.IsZero() {
			hook.FirstToken(i, call.FirstTokenAt)
		}
		return
	}
	failed, duration := s.Status().Code == codes.Error, max(s.EndTime().Sub(s.StartTime()), 0)
	call := rudderstackai.ToolCall{
		ID: firstString(a, "gen_ai.tool.call.id", "tool_call.id", "tool.id"), Name: firstString(a, "gen_ai.tool.name", "tool.name", "traceloop.entity.name"),
		AgentName: firstString(a, "gen_ai.agent.name", "agent.name"), MCPServer: firstString(a, "gen_ai.tool.mcp.server", "mcp.server.name"),
		ParentTool: firstString(a, "gen_ai.tool.parent.name", "tool.parent.name"), Error: &failed, Duration: &duration,
		StartedAt: s.StartTime(),
		Args:      toolValue(a, "gen_ai.tool.call.arguments", "input.value", "traceloop.entity.input"), Result: toolValue(a, "gen_ai.tool.call.result", "output.value", "traceloop.entity.output"),
	}
	outputFailed := !failed && kit.ToolOutputFailed(call.Result, kit.DecodedOutput)
	switch {
	case failed:
		call.ErrorType = spanErrorType(s)
	case outputFailed:
		call.Error, call.ErrorType = rudderstackai.Ptr(true), "api_error"
	}
	hook.Tracing.RecordTool(i, call, outputFailed)
}
