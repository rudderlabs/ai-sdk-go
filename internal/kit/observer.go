package kit

import (
	"strings"
	"sync"
	"time"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
)

// End is how a provider stream ended.
type End int

// Stream ends.
const (
	Completed End = iota
	Cancelled
	Errored
)

type streamTool struct {
	id, name, mcpServer string
	args                any
}

// Observer is the common bookkeeping of one streamed call: usage, tools and text seen so far,
// recorded once when the stream ends. Integrations fill its fields as events arrive.
type Observer struct {
	Interaction    *rudderstackai.Interaction
	Ephemeral      bool
	Provider       string
	Integration    string
	Model          string
	RequestedModel string
	StartedAt      time.Time

	ResponseID      string
	InputTokens     *int
	OutputTokens    *int
	ReasoningTokens *int
	// CallCount is the provider requests the stream spans. Zero means 1.
	CallCount int
	// UsageIncomplete: some of those requests reported no usage.
	UsageIncomplete    bool
	CachedInputTokens  int
	CacheWriteTokens   int
	CacheWrite1hTokens int
	FinishReason       string
	CostUSD            *float64
	ServiceTier        string
	ServerToolCalls    map[string]int
	// Incomplete: the stream ended without the provider's terminal event.
	Incomplete bool
	// Done: the provider's terminal event arrived; closing the stream afterwards completes it.
	Done bool
	// Record, when set, records the call in Finish instead (once); false falls back to the default.
	Record func(end End, errorType string) bool

	mu       sync.Mutex
	tools    []*streamTool
	toolKeys map[string]*streamTool
	text     strings.Builder
	keepText bool
	first    bool
	finished bool
}

// NewObserver starts the bookkeeping of a streamed call. If the interaction ends while the
// stream is still open, the call is recorded then (cancelled unless it completed).
func NewObserver(c *Call) *Observer {
	o := &Observer{
		Interaction: c.Interaction, Ephemeral: c.Ephemeral, Provider: c.Provider, Integration: c.Integration,
		Model: c.Model, RequestedModel: c.Model, toolKeys: map[string]*streamTool{},
		keepText: KeepsText(c.Interaction),
	}
	OnBeforeEnd(c.Interaction, func() {
		if o.Done {
			o.Finish(Completed, "")
			return
		}
		o.Finish(Cancelled, "")
	})
	return o
}

// FirstToken marks the interaction's first streamed token, once.
func (o *Observer) FirstToken() {
	if !o.first {
		o.first = true
		FirstToken(o.Interaction, time.Now())
	}
}

// NoteTool records a tool call under a local key; a later sighting fills it in.
func (o *Observer) NoteTool(key, id, name string, args any, mcpServer string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	t := o.toolKeys[key]
	if t == nil {
		t = &streamTool{id: id, name: name, mcpServer: mcpServer, args: args}
		o.toolKeys[key] = t
		o.tools = append(o.tools, t)
		return
	}
	if t.id == "" {
		t.id = id
	}
	if t.name == "" {
		t.name = name
	}
	if t.mcpServer == "" {
		t.mcpServer = mcpServer
	}
	if args != nil {
		t.args = args
	}
}

// NoteToolArgs appends a streamed chunk of a tool call's JSON arguments.
func (o *Observer) NoteToolArgs(key, chunk string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	t := o.toolKeys[key]
	if t == nil || chunk == "" {
		return
	}
	s, _ := t.args.(string)
	t.args = s + chunk
}

// NoteText appends streamed answer text, kept only for the classifier and evaluator.
func (o *Observer) NoteText(s string) {
	if o.keepText && s != "" {
		o.mu.Lock()
		o.text.WriteString(s)
		o.mu.Unlock()
	}
}

// HasTools reports whether any tool call was seen.
func (o *Observer) HasTools() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.tools) > 0
}

func (o *Observer) recordTools() {
	o.mu.Lock()
	calls := make([]rudderstackai.ToolCall, 0, len(o.tools))
	for _, t := range o.tools {
		calls = append(calls, rudderstackai.ToolCall{ID: t.id, Name: t.name, MCPServer: t.mcpServer, Args: ParseToolValue(t.args)})
	}
	o.mu.Unlock()
	if len(calls) > 0 {
		o.Interaction.RecordToolCalls(calls...)
	}
}

func deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// Finish records the call once: a completed stream with its usage, a cancelled one unpriced, a
// failed one with the tokens seen so far (none when it was cut off).
func (o *Observer) Finish(end End, errorType string) {
	o.mu.Lock()
	if o.finished {
		o.mu.Unlock()
		return
	}
	o.finished = true
	o.mu.Unlock()
	if o.Record != nil && o.Record(end, errorType) {
		return
	}
	truncated := end == Completed && o.Incomplete
	if truncated {
		end, errorType = Errored, "api_error"
	}
	if end == Errored && errorType == "cancelled" {
		end = Cancelled
	}
	if end == Errored {
		call := rudderstackai.LLMCall{
			Provider: o.Provider, Model: o.Model, RequestedModel: o.RequestedModel, ResponseID: o.ResponseID,
			ErrorType: errorType, Integration: o.Integration,
		}
		if !truncated {
			call.InputTokens, call.OutputTokens = deref(o.InputTokens), deref(o.OutputTokens)
			call.ReasoningTokens = o.ReasoningTokens
			call.CachedInputTokens, call.CacheWriteTokens, call.CacheWrite1hTokens = o.CachedInputTokens, o.CacheWriteTokens, o.CacheWrite1hTokens
			call.UsageUnknown = true
		}
		o.Interaction.RecordCall(call)
		// Tools the model already invoked still count.
		o.recordTools()
		if o.Ephemeral {
			EndEphemeral(o.Interaction, errorType)
		}
		return
	}
	cancelled := end == Cancelled
	call := rudderstackai.LLMCall{
		Integration: o.Integration, Provider: o.Provider, Model: o.Model, RequestedModel: o.RequestedModel,
		InputTokens: deref(o.InputTokens), OutputTokens: deref(o.OutputTokens), ReasoningTokens: o.ReasoningTokens,
		CallCount: o.CallCount, CachedInputTokens: o.CachedInputTokens, CacheWriteTokens: o.CacheWriteTokens,
		CacheWrite1hTokens: o.CacheWrite1hTokens, ServiceTier: o.ServiceTier, ServerToolCalls: o.ServerToolCalls,
		ResponseID: o.ResponseID, FinishReason: o.FinishReason,
		UsageUnknown: cancelled || o.UsageIncomplete || (o.InputTokens == nil && o.OutputTokens == nil),
	}
	if !cancelled {
		call.CostUSD = o.CostUSD
	} else {
		call.ErrorType = "cancelled"
	}
	o.mu.Lock()
	call.OutputText = o.text.String()
	o.mu.Unlock()
	if o.Interaction.RecordCall(call) {
		o.recordTools()
	}
	if o.Ephemeral {
		EndEphemeral(o.Interaction, "")
	}
}
