// Package rudderanthropic tracks Anthropic Messages API calls made with
// github.com/anthropics/anthropic-sdk-go, also through its Bedrock and Vertex AI clients.
//
// Attach it as client middleware. Calls inside an interaction report into it; a call outside one
// emits its own ai_response_received.
//
//	client := anthropic.NewClient(rudderanthropic.WithTracking())
//	err := rudderstackai.RunInteraction(ctx, rudderstackai.InteractionOptions{UserID: "u1"},
//		func(ctx context.Context, i *rudderstackai.Interaction) error {
//			_, err := client.Messages.New(ctx, params)
//			return err
//		})
//
// With the Bedrock or Vertex AI client, pass the tracking option before bedrock.WithConfig /
// vertex.WithGoogleAuth (middleware runs in registration order), so it sees the Messages request
// and the SSE stream those options translate to:
//
//	client := anthropic.NewClient(rudderanthropic.WithTracking(rudderanthropic.WithProvider("aws.bedrock")),
//		bedrock.WithLoadDefaultConfig(ctx))
//
// Token normalisation: Anthropic reports cache reads and writes apart from input_tokens. Events
// report input as the total (uncached + cache read + cache write), with cache reads also counted
// as cached input, so totals mean the same for every provider.
package rudderanthropic

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	analytics "github.com/rudderlabs/analytics-go/v4"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/kit"
)

const integration = "anthropic"

// Option configures [Middleware] and [WithTracking].
type Option func(*config)

type config struct {
	provider  string
	analytics analytics.Client
}

// WithProvider sets the provider label sent as provider. Defaults to anthropic; use aws.bedrock
// or gcp.vertex_ai for the SDK's Bedrock and Vertex AI clients.
func WithProvider(provider string) Option { return func(c *config) { c.provider = provider } }

// WithAnalytics sets the RudderStack client for calls made when no interaction or context names
// one.
func WithAnalytics(client analytics.Client) Option { return func(c *config) { c.analytics = client } }

// WithTracking returns a client option that tracks Messages API calls (streamed or not).
func WithTracking(opts ...Option) option.RequestOption {
	return option.WithMiddleware(Middleware(opts...))
}

// Middleware returns the tracking middleware, for option.WithMiddleware.
func Middleware(opts ...Option) option.Middleware {
	c := config{provider: "anthropic"}
	for _, o := range opts {
		o(&c)
	}
	spec := &kit.Spec{
		Provider:    c.provider,
		Integration: integration,
		Analytics:   c.analytics,
		IsStream:    func(_ *http.Request, body map[string]any) bool { return body["stream"] == true },
		Untracked:   func(req *http.Request, _ map[string]any) bool { return !isMessages(req) },
		OnRequest:   recordToolResults,
		OnResponse:  recordMessage,
		NewStream:   newStream,
	}
	return spec.Intercept
}

// isMessages: only message creation is a model call (not count_tokens or batches). The Bedrock
// and Vertex AI middleware rewrite the path after ours, when registered after it.
func isMessages(req *http.Request) bool {
	return strings.HasSuffix(strings.TrimRight(req.URL.Path, "/"), "/messages")
}

func init() {
	kit.RegisterErrorClassifier(func(err error) (string, bool) {
		var e *anthropic.Error
		if !errors.As(err, &e) {
			return "", false
		}
		if t := rudderstackai.ErrorTypeForStatus(e.StatusCode); t != "" {
			return t, true
		}
		// 529 overloaded, 5xx, and an error event mid-stream (sent with the stream's 200).
		return "api_error", true
	})
}

var toolBlocks = map[string]bool{"tool_use": true, "server_tool_use": true, "mcp_tool_use": true}

// usageFields may arrive in parts (gateways, Bedrock, Vertex): absent ones keep their value.
var usageFields = []string{"input_tokens", "cache_read_input_tokens", "cache_creation_input_tokens", "cache_creation"}

type tokens struct{ total, read, write, write1h int }

func normalise(usage map[string]any) tokens {
	read, write := kit.Int(usage["cache_read_input_tokens"]), kit.Int(usage["cache_creation_input_tokens"])
	return tokens{
		total:   kit.Int(usage["input_tokens"]) + read + write,
		read:    read,
		write:   write,
		write1h: kit.Int(kit.Map(usage["cache_creation"])["ephemeral_1h_input_tokens"]),
	}
}

// thinkingTokens is the extended-thinking part of output_tokens, or nil.
func thinkingTokens(usage map[string]any) *int {
	return kit.IntPtr(kit.Map(usage["output_tokens_details"])["thinking_tokens"])
}

// serviceTier: fast mode is reported in speed, not service_tier.
func serviceTier(usage map[string]any) string {
	if kit.Str(usage["speed"]) == "fast" {
		return "fast"
	}
	return kit.Str(usage["service_tier"])
}

// serverToolCalls keys server_tool_use counts by kind: web_search_requests becomes web_search.
func serverToolCalls(usage map[string]any) map[string]int {
	var out map[string]int
	for k, v := range kit.Map(usage["server_tool_use"]) {
		if n, ok := v.(float64); ok && n > 0 && strings.HasSuffix(k, "_requests") {
			if out == nil {
				out = map[string]int{}
			}
			out[strings.TrimSuffix(k, "_requests")] = int(n)
		}
	}
	return out
}

func toolCall(b map[string]any) rudderstackai.ToolCall {
	tc := rudderstackai.ToolCall{ID: kit.Str(b["id"]), Name: kit.Str(b["name"]), Args: b["input"]}
	if b["type"] == "mcp_tool_use" {
		tc.MCPServer = kit.Str(b["server_name"])
	}
	return tc
}

func recordMessage(c *kit.Call, msg map[string]any, _ *http.Response) bool {
	usage := kit.Map(msg["usage"])
	t := normalise(usage)
	model := kit.Str(msg["model"])
	if model == "" {
		model = c.Model
	}
	var text strings.Builder
	var tools []rudderstackai.ToolCall
	for _, v := range kit.List(msg["content"]) {
		b := kit.Map(v)
		if b["type"] == "text" {
			text.WriteString(kit.Str(b["text"]))
		}
		if toolBlocks[kit.Str(b["type"])] {
			tools = append(tools, toolCall(b))
		}
	}
	recorded := c.Interaction.RecordCall(rudderstackai.LLMCall{
		Integration: integration, Provider: c.Provider, Model: model, RequestedModel: c.Model,
		InputTokens: t.total, OutputTokens: kit.Int(usage["output_tokens"]), ReasoningTokens: thinkingTokens(usage),
		CachedInputTokens: t.read, CacheWriteTokens: t.write, CacheWrite1hTokens: t.write1h,
		ServiceTier: serviceTier(usage), ServerToolCalls: serverToolCalls(usage),
		ResponseID: kit.Str(msg["id"]), FinishReason: kit.Str(msg["stop_reason"]),
		UsageUnknown: usage == nil, OutputText: text.String(),
	})
	if recorded && len(tools) > 0 {
		c.Interaction.RecordToolCalls(tools...)
	}
	return true
}

// recordToolResults settles earlier tool calls from the tool_result blocks a request sends back.
func recordToolResults(c *kit.Call) {
	for _, m := range kit.List(c.Request["messages"]) {
		for _, v := range kit.List(kit.Map(m)["content"]) {
			b := kit.Map(v)
			if b["type"] != "tool_result" {
				continue
			}
			failed := b["is_error"] == true
			c.Interaction.RecordToolResult(kit.Str(b["tool_use_id"]), rudderstackai.ToolResult{Error: &failed, Result: kit.ToolResultValue(b["content"])})
		}
	}
}

// stream observes one streamed Messages response.
type stream struct {
	*kit.Observer
	usage map[string]any
}

func newStream(c *kit.Call, _ *http.Response) kit.StreamHandler {
	s := &stream{Observer: kit.NewObserver(c), usage: map[string]any{}}
	// Until message_stop arrives, a stream that just stops is a cut-off answer.
	s.Incomplete = true
	return s
}

func (s *stream) setInput(usage map[string]any) {
	for _, k := range usageFields {
		if v, ok := usage[k]; ok && v != nil {
			s.usage[k] = v
		}
	}
	t := normalise(s.usage)
	s.InputTokens = &t.total
	s.CachedInputTokens, s.CacheWriteTokens, s.CacheWrite1hTokens = t.read, t.write, t.write1h
	if tier := serviceTier(usage); tier != "" {
		s.ServiceTier = tier
	}
	if calls := serverToolCalls(usage); calls != nil {
		s.ServerToolCalls = calls
	}
}

func (s *stream) setOutput(usage map[string]any, fallback int) {
	n := fallback
	if p := kit.IntPtr(usage["output_tokens"]); p != nil {
		n = *p
	}
	s.OutputTokens = &n
	if r := thinkingTokens(usage); r != nil {
		s.ReasoningTokens = r
	}
}

// OnEvent updates the call from one server-sent event.
func (s *stream) OnEvent(_ string, data []byte) {
	var e map[string]any
	if json.Unmarshal(data, &e) != nil {
		return
	}
	switch e["type"] {
	case "message_start":
		msg := kit.Map(e["message"])
		if m := kit.Str(msg["model"]); m != "" {
			s.Model = m
		}
		if id := kit.Str(msg["id"]); id != "" {
			s.ResponseID = id
		}
		if usage := kit.Map(msg["usage"]); usage != nil {
			s.setInput(usage)
			s.setOutput(usage, 0)
		}
	case "content_block_start":
		b := kit.Map(e["content_block"])
		if toolBlocks[kit.Str(b["type"])] {
			tc := toolCall(b)
			s.NoteTool(blockKey(e["index"]), tc.ID, tc.Name, tc.Args, tc.MCPServer)
		}
	case "content_block_delta":
		s.FirstToken()
		d := kit.Map(e["delta"])
		switch d["type"] {
		case "input_json_delta":
			s.NoteToolArgs(blockKey(e["index"]), kit.Str(d["partial_json"]))
		case "text_delta":
			s.NoteText(kit.Str(d["text"]))
		}
	case "message_delta":
		// message_delta usage is cumulative.
		if usage := kit.Map(e["usage"]); usage != nil {
			s.setOutput(usage, deref(s.OutputTokens))
			s.setInput(usage)
		}
		if r := kit.Str(kit.Map(e["delta"])["stop_reason"]); r != "" {
			s.FinishReason = r
		}
	case "message_stop":
		s.Incomplete = false
		s.Done = true
	case "error":
		// The SDK raises it to the app: a failed call that keeps the tokens seen so far.
		s.Finish(kit.Errored, "api_error")
	}
}

func blockKey(index any) string {
	if f, ok := index.(float64); ok {
		return strconv.Itoa(int(f))
	}
	return ""
}

func deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
