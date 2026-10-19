package rudderopenai

import (
	"bytes"
	"encoding/json"
	"maps"
	"net/http"
	"strconv"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/kit"
)

func decode(raw []byte) (map[string]any, bool) {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return nil, false
	}
	return m, true
}

func reportedCost(v any) (*float64, bool) {
	c, ok := rudderstackai.ReportedCost(v)
	if !ok {
		return nil, false
	}
	return &c, true
}

func isStream(_ *http.Request, body map[string]any) bool { return body["stream"] == true }

// chatToolName is a function tool's name, or a custom tool's.
func chatToolName(tc map[string]any) string {
	if n := kit.Str(kit.Map(tc["function"])["name"]); n != "" {
		return n
	}
	return kit.Str(kit.Map(tc["custom"])["name"])
}

// chatToolArgs is a function tool's JSON arguments, or a custom tool's input.
func chatToolArgs(tc map[string]any) any {
	if f := kit.Map(tc["function"]); f != nil && f["arguments"] != nil {
		return f["arguments"]
	}
	return kit.Map(tc["custom"])["input"]
}

// recordChatToolResults: outputs of earlier tool calls the next request sends back (no error
// flag, so the outcome stays unknown).
func recordChatToolResults(c *kit.Call) {
	for _, m := range kit.List(c.Request["messages"]) {
		msg := kit.Map(m)
		if msg["role"] == "tool" {
			c.Interaction.RecordToolResult(kit.Str(msg["tool_call_id"]), rudderstackai.ToolResult{Result: kit.ParseToolValue(msg["content"])})
		}
	}
}

func chatSpec(cfg config, rejected *rejectedHosts) *kit.Spec {
	return &kit.Spec{
		Provider: cfg.provider, Integration: integration, Analytics: cfg.analytics,
		IsStream:  isStream,
		OnRequest: recordChatToolResults,
		// Ask for usage in the final chunk; some OpenAI-compatible hosts reject stream_options, so a
		// refused request is sent again as the caller wrote it.
		PrepareStream: func(c *kit.Call) *kit.Prepared {
			opts := kit.Map(c.Request["stream_options"])
			host := c.HTTPRequest.URL.Host
			if opts["include_usage"] == true || rejected.has(host) || (cfg.streamUsage != nil && !*cfg.streamUsage) {
				return nil
			}
			body := maps.Clone(c.Request)
			withUsage := maps.Clone(opts)
			if withUsage == nil {
				withUsage = map[string]any{}
			}
			withUsage["include_usage"] = true
			body["stream_options"] = withUsage
			if cfg.streamUsage != nil {
				return &kit.Prepared{Body: body}
			}
			return &kit.Prepared{Body: body, Names: []string{"stream_options", "include_usage"}, Accepted: func() { rejected.add(host) }}
		},
		OnResponse: onChatResponse,
		NewStream: func(c *kit.Call, resp *http.Response) kit.StreamHandler {
			s := &chatStream{Observer: kit.NewObserver(c)}
			// The request as sent: whether stream_options was ours or the caller's.
			s.injected = injectedUsage(c, resp)
			g := gatewayHeaders(resp)
			s.CostUSD, s.servedModel = g.cost, g.model
			return s
		},
	}
}

// injectedUsage reports whether the request that got this response asked for usage only because
// the integration added it.
func injectedUsage(c *kit.Call, resp *http.Response) bool {
	if kit.Map(c.Request["stream_options"])["include_usage"] == true || resp.Request == nil || resp.Request.GetBody == nil {
		return false
	}
	rc, err := resp.Request.GetBody()
	if err != nil {
		return false
	}
	defer func() { _ = rc.Close() }()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(rc); err != nil {
		return false
	}
	sent, ok := decode(buf.Bytes())
	return ok && kit.Map(sent["stream_options"])["include_usage"] == true
}

func onChatResponse(c *kit.Call, body map[string]any, resp *http.Response) bool {
	g := gatewayHeaders(resp)
	usage := kit.Map(body["usage"])
	choices := kit.List(body["choices"])
	choice := kit.Map(nil)
	if len(choices) > 0 {
		choice = kit.Map(choices[0])
	}
	message := kit.Map(choice["message"])
	finish := kit.Str(choice["finish_reason"])
	// A refusal (structured outputs) finishes with stop on the wire; name it as Anthropic does.
	if kit.Str(message["refusal"]) != "" {
		finish = "refusal"
	}
	cost, _ := reportedCost(usage["cost"])
	if cost == nil {
		cost = g.cost
	}
	recorded := c.Interaction.RecordCall(rudderstackai.LLMCall{
		Integration: c.Integration, Provider: c.Provider,
		Model:          firstOf(g.model, kit.Str(body["model"]), c.Model),
		RequestedModel: c.Model,
		// prompt_tokens includes cached tokens.
		InputTokens: kit.Int(usage["prompt_tokens"]), OutputTokens: kit.Int(usage["completion_tokens"]),
		ReasoningTokens:   kit.IntPtr(kit.Map(usage["completion_tokens_details"])["reasoning_tokens"]),
		CachedInputTokens: kit.Int(kit.Map(usage["prompt_tokens_details"])["cached_tokens"]),
		CostUSD:           cost, ServiceTier: kit.Str(body["service_tier"]), ResponseID: kit.Str(body["id"]),
		FinishReason: finish, UsageUnknown: usage == nil,
		OutputText: kit.Str(message["content"]),
	})
	// Every returned choice was generated: count the tool calls of all of them. A repeated
	// response (same ID) adds none.
	var tools []rudderstackai.ToolCall
	for _, ch := range choices {
		for _, t := range kit.List(kit.Map(kit.Map(ch)["message"])["tool_calls"]) {
			tc := kit.Map(t)
			tools = append(tools, rudderstackai.ToolCall{ID: kit.Str(tc["id"]), Name: chatToolName(tc), Args: kit.ParseToolValue(chatToolArgs(tc))})
		}
	}
	if recorded && len(tools) > 0 {
		c.Interaction.RecordToolCalls(tools...)
	}
	return true
}

func firstOf(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// chatStream reads chat.completion.chunk events.
type chatStream struct {
	*kit.Observer
	injected bool
	// servedModel is the deployment a gateway named in its headers; it wins over the chunks' alias.
	servedModel string
	usageOnly   bool
}

func (s *chatStream) OnEvent(_ string, data []byte) {
	s.usageOnly = false
	if bytes.HasPrefix(data, []byte("[DONE]")) {
		s.Done = true
		return
	}
	chunk, ok := decode(data)
	if !ok {
		return
	}
	// openai-go fails the stream on any chunk with an error key.
	if _, failed := chunk["error"]; failed {
		s.Finish(kit.Errored, streamErrorType(chunk))
		return
	}
	// Azure OpenAI sends chunks with an empty model and id (prompt_filter_results).
	s.Model = firstOf(s.servedModel, kit.Str(chunk["model"]), s.Model)
	s.ResponseID = firstOf(kit.Str(chunk["id"]), s.ResponseID)
	s.ServiceTier = firstOf(kit.Str(chunk["service_tier"]), s.ServiceTier)
	usage := kit.Map(chunk["usage"])
	if usage != nil {
		in, out := kit.Int(usage["prompt_tokens"]), kit.Int(usage["completion_tokens"])
		s.InputTokens, s.OutputTokens = &in, &out
		s.ReasoningTokens = kit.IntPtr(kit.Map(usage["completion_tokens_details"])["reasoning_tokens"])
		s.CachedInputTokens = kit.Int(kit.Map(usage["prompt_tokens_details"])["cached_tokens"])
		if c, ok := reportedCost(usage["cost"]); ok {
			s.CostUSD = c
		}
	}
	choices := kit.List(chunk["choices"])
	for _, ch := range choices {
		choice := kit.Map(ch)
		delta := kit.Map(choice["delta"])
		calls := kit.List(delta["tool_calls"])
		if kit.Str(delta["content"]) != "" || kit.Str(delta["refusal"]) != "" || len(calls) > 0 {
			s.FirstToken()
		}
		index := kit.Int(choice["index"])
		if index == 0 {
			s.NoteText(kit.Str(delta["content"]))
		}
		// Tool indices are per choice: key by both so n > 1 is not undercounted.
		for _, t := range calls {
			tc := kit.Map(t)
			key := strconv.Itoa(index) + ":" + strconv.Itoa(kit.Int(tc["index"]))
			s.NoteTool(key, kit.Str(tc["id"]), chatToolName(tc), nil, "")
			s.NoteToolArgs(key, kit.Str(chatToolArgs(tc)))
		}
		// The answer's finish reason is the first choice's.
		if f := kit.Str(choice["finish_reason"]); f != "" && index == 0 {
			s.FinishReason = f
		}
	}
	s.usageOnly = len(choices) == 0 && usage != nil
}

// Hide drops the usage-only chunk the integration asked for, so the app sees what it requested.
func (s *chatStream) Hide(string, []byte) bool { return s.injected && s.usageOnly }
