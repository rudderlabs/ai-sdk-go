package rudderopenai

import (
	"net/http"
	"strconv"
	"strings"
	"sync"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/kit"
)

// Every tool the model invoked is an output item named *_call: function and custom tools plus
// built-in ones (web_search_call, file_search_call, code_interpreter_call, mcp_call, ...).
func isToolCall(item map[string]any) bool { return strings.HasSuffix(kit.Str(item["type"]), "_call") }

// itemToolName: built-in tools have no name; web_search_call is the web_search tool.
func itemToolName(item map[string]any) string {
	return firstOf(kit.Str(item["name"]), strings.TrimSuffix(kit.Str(item["type"]), "_call"))
}

func itemMCPServer(item map[string]any) string {
	if item["type"] != "mcp_call" {
		return ""
	}
	return firstOf(kit.Str(item["server_label"]), kit.Str(item["server_name"]), kit.Str(item["server"]))
}

func itemToolArgs(item map[string]any) any {
	if a := kit.Str(item["arguments"]); a != "" {
		return kit.ParseToolValue(a)
	}
	if in := kit.Str(item["input"]); in != "" {
		return kit.ParseToolValue(in)
	}
	return nil
}

func itemToolCall(item map[string]any) rudderstackai.ToolCall {
	return rudderstackai.ToolCall{ID: firstOf(kit.Str(item["call_id"]), kit.Str(item["id"])), Name: itemToolName(item), MCPServer: itemMCPServer(item), Args: itemToolArgs(item)}
}

// Hosted tools OpenAI bills per call; function, custom, MCP and computer calls are not.
var serverTools = map[string]string{
	"web_search_call":       "web_search",
	"file_search_call":      "file_search",
	"code_interpreter_call": "code_interpreter",
	"image_generation_call": "image_generation",
}

// Responses API incomplete reasons in Chat Completions' finish_reason terms.
var incompleteReasons = map[string]string{"max_output_tokens": "length"}

// recordResponsesToolResults: *_call_output items of a later request settle earlier tool calls.
func recordResponsesToolResults(c *kit.Call) {
	for _, it := range kit.List(c.Request["input"]) {
		item := kit.Map(it)
		if strings.HasSuffix(kit.Str(item["type"]), "_call_output") {
			_, hasResult := item["output"]
			kit.RecordToolResult(c.Interaction, kit.Str(item["call_id"]), rudderstackai.ToolResult{Result: kit.ParseToolValue(item["output"])}, hasResult)
		}
	}
}

// responsesText is the output_text parts of the message items.
func responsesText(response map[string]any) string {
	if t := kit.Str(response["output_text"]); t != "" {
		return t
	}
	var b strings.Builder
	for _, it := range kit.List(response["output"]) {
		item := kit.Map(it)
		if item["type"] != "message" {
			continue
		}
		for _, p := range kit.List(item["content"]) {
			if part := kit.Map(p); part["type"] == "output_text" {
				b.WriteString(kit.Str(part["text"]))
			}
		}
	}
	return b.String()
}

// recordResponse records a finished Responses API response; false for a background response that
// is only queued (its outcome comes from polling, which is not tracked).
func recordResponse(c *kit.Call, response map[string]any, g gateway) bool {
	status := kit.Str(response["status"])
	if status == "queued" || status == "in_progress" {
		return false
	}
	usage := kit.Map(response["usage"])
	output := kit.List(response["output"])
	var tools []rudderstackai.ToolCall
	counts := map[string]int{}
	messages, refusal := 0, false
	for _, it := range output {
		item := kit.Map(it)
		if isToolCall(item) {
			tools = append(tools, itemToolCall(item))
		}
		if kind, ok := serverTools[kit.Str(item["type"])]; ok {
			counts[kind]++
		}
		if item["type"] == "message" {
			messages++
			for _, p := range kit.List(item["content"]) {
				refusal = refusal || kit.Map(p)["type"] == "refusal"
			}
		}
	}
	errorType := ""
	switch status {
	case "failed":
		// A failed response arrives as a normal object, not an error: keep its usage, record the error.
		errorType = kit.HTTPErrorType(0, map[string]any{"error": map[string]any{"code": kit.Map(response["error"])["code"]}})
	case "cancelled":
		errorType = "cancelled"
	}
	var finish string
	switch {
	case status == "completed" && refusal:
		finish = "refusal"
	case status == "completed" && len(tools) > 0 && messages == 0:
		// Only tool calls: the model handed over to the app, as Chat Completions' tool_calls says.
		finish = "tool_calls"
	case status == "completed":
		finish = "stop"
	default:
		reason := kit.Str(kit.Map(response["incomplete_details"])["reason"])
		finish = firstOf(incompleteReasons[reason], reason, status)
	}
	var cost *float64
	if errorType != "cancelled" {
		if cost, _ = reportedCost(usage["cost"]); cost == nil {
			cost = g.cost
		}
	}
	if len(counts) == 0 {
		counts = nil
	}
	recorded := c.RecordCall(rudderstackai.LLMCall{
		Integration: c.Integration, Provider: c.Provider,
		Model:          firstOf(g.model, kit.Str(response["model"]), c.Model),
		RequestedModel: c.Model,
		// input_tokens includes cached tokens.
		InputTokens: kit.Int(usage["input_tokens"]), OutputTokens: kit.Int(usage["output_tokens"]),
		ReasoningTokens:   kit.IntPtr(kit.Map(usage["output_tokens_details"])["reasoning_tokens"]),
		CachedInputTokens: kit.Int(kit.Map(usage["input_tokens_details"])["cached_tokens"]),
		CostUSD:           cost, ServiceTier: kit.Str(response["service_tier"]), ServerToolCalls: counts,
		ResponseID: kit.Str(response["id"]), FinishReason: finish, ErrorType: errorType,
		UsageUnknown: usage == nil, OutputText: responsesText(response),
	})
	// A repeated response (same ID) adds no tool calls.
	if recorded && len(tools) > 0 {
		c.Interaction.RecordToolCalls(tools...)
	}
	return true
}

func responsesSpec(cfg config) *kit.Spec {
	return &kit.Spec{Details: cfg.details,
		Provider: cfg.provider, Integration: integration, Analytics: cfg.analytics,
		IsStream:  isStream,
		OnRequest: recordResponsesToolResults,
		// A non-streamed background response is only queued: its outcome comes from polling, which
		// is not tracked, so the call passes through rather than leave an answer that never ends.
		Untracked: func(_ *http.Request, body map[string]any) bool {
			return body["background"] == true && body["stream"] != true
		},
		OnResponse: func(c *kit.Call, body map[string]any, resp *http.Response) bool {
			return recordResponse(c, body, gatewayHeaders(resp))
		},
		NewStream: func(c *kit.Call, resp *http.Response) kit.StreamHandler {
			s := &responsesStream{Observer: kit.NewObserver(c), call: c, gateway: gatewayHeaders(resp)}
			// Until a terminal event arrives, a stream that just stops is a cut-off answer.
			s.Incomplete = true
			s.Record = s.record
			return s
		},
	}
}

// responsesStream reads Responses API stream events.
type responsesStream struct {
	*kit.Observer
	call    *kit.Call
	gateway gateway
	mu      sync.Mutex
	final   map[string]any
	tools   int
}

var terminalEvents = map[string]bool{"response.completed": true, "response.incomplete": true, "response.failed": true, "response.cancelled": true}

func (s *responsesStream) OnEvent(_ string, data []byte) {
	event, ok := decode(data)
	if !ok {
		return
	}
	// openai-go fails the stream on any event with an error key.
	if _, failed := event["error"]; failed {
		s.Finish(kit.Errored, streamErrorType(event))
		return
	}
	typ := kit.Str(event["type"])
	if strings.HasSuffix(typ, ".delta") {
		s.FirstToken()
	}
	if typ == "response.output_text.delta" {
		s.NoteText(kit.Str(event["delta"]))
	}
	// Tool items count as they arrive, so a stream that fails later still reports them.
	if item := kit.Map(event["item"]); item != nil && isToolCall(item) {
		tc := itemToolCall(item)
		s.mu.Lock()
		key := firstOf(kit.Str(item["id"]), tc.ID, strconv.Itoa(s.tools))
		s.tools++
		s.mu.Unlock()
		s.NoteTool(key, tc.ID, tc.Name, tc.Args, tc.MCPServer)
	}
	if response := kit.Map(event["response"]); response != nil {
		s.Model = firstOf(s.gateway.model, kit.Str(response["model"]), s.Model)
		s.ResponseID = firstOf(kit.Str(response["id"]), s.ResponseID)
		s.ServiceTier = firstOf(kit.Str(response["service_tier"]), s.ServiceTier)
		if terminalEvents[typ] {
			s.mu.Lock()
			s.final = response
			s.mu.Unlock()
			s.Incomplete = false
			s.Done = true
		}
	}
}

// record records the terminal event's response, as a non-streamed call would be; without one the
// default (cut off, cancelled or failed) applies.
func (s *responsesStream) record(end kit.End, _ string) bool {
	s.mu.Lock()
	final := s.final
	s.mu.Unlock()
	if final == nil || end == kit.Errored {
		return false
	}
	c := *s.call
	c.FirstTokenAt = s.FirstTokenAt
	recordResponse(&c, final, s.gateway)
	if s.Ephemeral {
		kit.EndEphemeral(s.Interaction, "")
	}
	return true
}
