package rudderstackai

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	analytics "github.com/rudderlabs/analytics-go/v4"
)

// InteractionOptions configures [StartInteraction] and [RunInteraction].
type InteractionOptions struct {
	// UserID is your user ID. Falls back to the context ([WithValues]).
	UserID string
	// AnonymousID is used when there is no user ID. Falls back to the context.
	AnonymousID string
	// ConversationID is the conversation (thread, chat) ID. Minted when missing: every event
	// needs one.
	ConversationID string
	// InteractionID replaces the generated UUID.
	InteractionID string
	// Feature is the AI feature, for example "support_bot".
	Feature string
	// Surface is where the user met the AI. Falls back to the context.
	Surface Surface
	// Intent is what the user wants, when known up front, for example "refund_request".
	Intent string
	// TraceID links the interaction to a trace (Langfuse, LangSmith, OpenTelemetry). Defaults to
	// what [WithTraceIDFrom] finds in the context, such as the active OpenTelemetry span's trace.
	TraceID string
	// Trigger is TriggerUser (the default) or TriggerSystem for app-initiated runs.
	Trigger Trigger
	// AccountID is the customer, workspace or tenant the work is for. Falls back to the context.
	AccountID string
	// OnBehalfOf is the human who requested or owns the work; not the actor. Falls back to the
	// context.
	OnBehalfOf string
	// TriggerSource is why the run started: "schedule", "webhook", "slack", "queue", "agent"...
	TriggerSource string
	// TaskName is the job or task being run, for example "alert-triage".
	TaskName string
	// TriggerEventID is the ID of the update that started the run, for example the event_id of an
	// MCP Events delivery.
	TriggerEventID string
	// ParentInteractionID is the interaction that started this one: agent hand-offs, sub-tasks.
	ParentInteractionID string
	// Analytics is the RudderStack client for this interaction. Wins over the context and the
	// global client.
	Analytics analytics.Client
	// CaptureContent overrides the global setting for this interaction.
	CaptureContent *bool
	// ToolBreakdown overrides the global setting for this interaction.
	ToolBreakdown *bool
	// Classify classifies this interaction when a classifier is configured. Default: on for
	// user-triggered interactions, off for system-triggered ones.
	Classify *bool
	// Context is extra message context merged into every event of this interaction.
	Context map[string]any
	// Request is the incoming request, whose RudderStack JavaScript SDK cookies supply identity.
	Request *http.Request
	// Cookies is the same cookies as a raw Cookie header. Wins over Request.
	Cookies string
	// CookieValues is the same cookies by name (raw values). Wins over Cookies.
	CookieValues map[string]string
	// IdentityFromCookies overrides the global setting for this interaction.
	IdentityFromCookies *bool

	// InputMethod is how the prompt was entered, for example "text", "voice" or "button".
	InputMethod string
	// PromptText is the user's prompt. Sent as prompt_text only with content capture on.
	PromptText string
	// NoPromptEvent skips ai_user_prompt_created (sent by default for user-triggered
	// interactions).
	NoPromptEvent bool
	// AttachmentCount is the number of files attached to the prompt. Never names or content.
	AttachmentCount *int
	// AttachmentTypes are the kinds attached: image, audio, video, pdf, or file for any other
	// document (unknown kinds become file). Each kind is sent once.
	AttachmentTypes []string

	// startedAt is when an integration that opens the interaction after the fact saw it start.
	startedAt time.Time
	// adapterAnalytics is the client an integration was set up with.
	adapterAnalytics analytics.Client
}

// LLMCall is one model call (or several aggregated ones) reported into an interaction.
type LLMCall struct {
	// Provider as an OpenTelemetry gen_ai.provider.name value (openai, anthropic, aws.bedrock).
	Provider string
	// Model that served the call (or the requested model when the provider reports none).
	Model string
	// RequestedModel is the model the application asked for; sent only when another model
	// served the call.
	RequestedModel string
	// InputTokens includes cached and cache-write tokens.
	InputTokens  int
	OutputTokens int
	// ReasoningTokens (a subset of OutputTokens); nil when the provider reports none.
	ReasoningTokens *int
	// CachedInputTokens are cache-read input tokens (a subset of InputTokens).
	CachedInputTokens int
	// CacheWriteTokens are cache-write input tokens (a subset of InputTokens).
	CacheWriteTokens int
	// CacheWrite1hTokens are Anthropic 1-hour cache writes (a subset of CacheWriteTokens).
	CacheWrite1hTokens int
	// CostUSD is a cost the provider, gateway or framework reported. Wins over registered prices.
	CostUSD *float64
	// ServiceTier as the provider reports it (default, flex, priority, batch...); normalised to
	// batch, flex, standard, fast or other.
	ServiceTier string
	// ServerToolCalls counts provider-hosted tool calls billed per call, by kind: {"web_search": 2}.
	ServerToolCalls map[string]int
	// CallCount is the number of calls this record stands for. Zero means 1.
	CallCount int
	// ErrorCount is the failed calls of an aggregate. Nil: CallCount when ErrorType is set.
	ErrorCount *int
	// AgentName labels the agent or step in usage_breakdown.
	AgentName string
	// ResponseID is the provider response ID; a repeated ID is counted once.
	ResponseID   string
	FinishReason string
	// ErrorType is set for a failed call, which stays unpriced unless CostUSD is given.
	ErrorType string
	// UsageUnknown marks tokens as unknown or partial, so the call stays unpriced.
	UsageUnknown bool
	// Integration reporting the call; the first one tags every event of the interaction.
	// Empty means manual.
	Integration string
	// OutputText is the answer text, given to a configured classifier or evaluator. Never sent.
	OutputText string
}

// ToolCall is one tool call the model made, for [Interaction.RecordToolCalls].
type ToolCall struct {
	// ID is the provider tool-call ID: a call already recorded under it is merged, not counted
	// again.
	ID string
	// Name is the tool name. Flat mcp__server__tool names are split.
	Name string
	// MCPServer is the MCP server of an MCP tool. Usually inferred from mcp__server__tool.
	MCPServer string
	// ParentTool is the enclosing tool of a nested or sub-agent tool.
	ParentTool string
	// AgentName is the agent that made the call.
	AgentName string
	// Error says whether the call failed, when known.
	Error *bool
	// ErrorType is a fixed error category of a failed call.
	ErrorType string
	// Duration is the measured execution time; never an estimate.
	Duration *time.Duration
	// Args are the call's arguments, given to declared tool rules only; never sent.
	Args any
	// Result is the tool's output, given to declared tool rules only; never sent. Seeing one
	// settles the call.
	Result any
}

// ToolResult is the outcome of a tool call, for [Interaction.RecordToolResult].
type ToolResult struct {
	Error     *bool
	ErrorType string
	Duration  *time.Duration
	Result    any
}

// EndOptions configures [Interaction.End].
type EndOptions struct {
	// Status is the final status. Derived from the recorded calls when empty.
	Status    ResponseStatus
	ErrorType string
	// PauseReason is what a paused run waits for. Defaults to PauseOther.
	PauseReason PauseReason
	// ClientDisconnected: the client went away before the answer finished.
	ClientDisconnected bool
	// ResponseText is the answer. Sent as response_text only with content capture on.
	ResponseText string
	// ExtraProperties are custom properties for ai_response_received. SDK-owned names are
	// ignored.
	ExtraProperties map[string]any
}

type recordedCall struct {
	provider, model, requestedModel, agentName       string
	callCount, errorCount                            int
	inputTokens, outputTokens                        int
	reasoningTokens                                  *int
	cachedInputTokens, cacheWriteTokens, cacheWrite1 int
	serviceTier                                      string
	serverToolCallCount                              int
	serverToolCostUSD, costUSD                       *float64
	finishReason, errorType, responseID              string
}

type recordedTool struct {
	rawName, name, mcpServer, parentTool, agentName, id string
	calls                                               int
	err                                                 *bool
	errorType                                           string
	duration                                            *time.Duration
	args, result                                        any
	hasResult, settled, fired                           bool
}

type pendingEvent struct {
	event string
	props func() map[string]any
	at    time.Time
}

type outcomeHint struct {
	status    ResponseStatus
	errorType string
	calls     int
}

// Interaction is one user prompt and the one answer it gets, however many model calls, tools or
// agents that takes. Integrations report each call into the interaction carried by the request
// context; it sends ai_user_prompt_created and exactly one ai_response_received. Safe for
// concurrent use.
type Interaction struct {
	id, conversationID, userID, anonymousID, feature                       string
	surface                                                                Surface
	trigger                                                                Trigger
	accountID, onBehalfOf, triggerSource, taskName, triggerEventID, parent string
	captureContent, toolBreakdown, classify                                bool
	explicitAnalytics                                                      analytics.Client
	msgContext                                                             map[string]any
	started                                                                time.Time

	mu                 sync.Mutex
	adapterAnalytics   analytics.Client
	intent, outcome    string
	traceID            string
	integration        string
	ended              bool
	firstToken         time.Duration
	calls              []*recordedCall
	seen               map[string]*recordedCall
	tools              []*recordedTool
	toolsByID          map[string]*recordedTool
	hint               *outcomeHint
	clientDisconnected bool
	pending            []pendingEvent
	settled            bool
	beforeEnd          []func()
	prompt, answer     string
	messages           any
	background         chan struct{}
}

// ID is the interaction ID (interaction_id): return it to your client for feedback events.
func (i *Interaction) ID() string { return i.id }

// ConversationID is the conversation_id of the interaction.
func (i *Interaction) ConversationID() string { return i.conversationID }

// Trigger is who started the interaction.
func (i *Interaction) Trigger() Trigger { return i.trigger }

// Ended reports whether ai_response_received was sent.
func (i *Interaction) Ended() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.ended
}

func surfaceOf(s Surface) Surface {
	if s == "" {
		return ""
	}
	if surfaces[s] {
		return s
	}
	warnOnce("invalid-surface", fmt.Sprintf("invalid surface %q; not sent", s), 0)
	return ""
}

func first(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func boolOr(v *bool, fallback bool) bool {
	if v != nil {
		return *v
	}
	return fallback
}

func newInteraction(ctx context.Context, o InteractionOptions) *Interaction {
	c := cfg()
	cv := valuesFrom(ctx)
	if cv == nil {
		cv = &contextValues{}
	}
	header := o.Cookies
	if header == "" {
		header = requestCookies(o.Request)
	}
	id := resolveIdentity(identityInputs{
		userID: o.UserID, anonymousID: o.AnonymousID,
		cookies: rudderstackCookies(header, o.CookieValues), fromCookies: o.IdentityFromCookies,
	}, cv, c)
	trigger := o.Trigger
	if trigger != TriggerSystem {
		trigger = TriggerUser
	}
	now := time.Now()
	started := now
	if !o.startedAt.IsZero() && o.startedAt.Before(now) {
		started = now.Add(-now.Sub(o.startedAt))
	}
	msgContext := o.Context
	if msgContext == nil {
		msgContext = cv.context
	}
	analyticsClient := o.Analytics
	if analyticsClient == nil {
		analyticsClient = cv.analytics
	}
	i := &Interaction{
		id:                first(o.InteractionID, uuid.NewString()),
		conversationID:    first(o.ConversationID, cv.conversationID, uuid.NewString()),
		userID:            id.userID,
		anonymousID:       id.anonymousID,
		feature:           first(o.Feature, cv.feature),
		surface:           first2(surfaceOf(o.Surface), surfaceOf(cv.surface)),
		trigger:           trigger,
		accountID:         first(o.AccountID, cv.accountID),
		onBehalfOf:        first(o.OnBehalfOf, cv.onBehalfOf),
		triggerSource:     o.TriggerSource,
		taskName:          o.TaskName,
		triggerEventID:    o.TriggerEventID,
		parent:            o.ParentInteractionID,
		captureContent:    boolOr(o.CaptureContent, c.captureContent),
		toolBreakdown:     boolOr(o.ToolBreakdown, c.toolBreakdown),
		classify:          boolOr(o.Classify, trigger == TriggerUser),
		explicitAnalytics: analyticsClient,
		adapterAnalytics:  o.adapterAnalytics,
		msgContext:        withSessionID(msgContext, id.sessionID),
		started:           started,
		intent:            o.Intent,
		traceID:           o.TraceID,
		seen:              map[string]*recordedCall{},
		toolsByID:         map[string]*recordedTool{},
		firstToken:        -1,
	}
	i.fillTraceID(ctx)
	return i
}

func first2(a, b Surface) Surface {
	if a != "" {
		return a
	}
	return b
}

// fillTraceID takes the trace ID [WithTraceIDFrom] finds in the context, unless one is set.
func (i *Interaction) fillTraceID(ctx context.Context) {
	if i.traceID != "" || ctx == nil {
		return
	}
	if from := cfg().traceIDFrom; from != nil {
		safely(func() { i.traceID = from(ctx) })
	}
}

func (i *Interaction) analytics() analytics.Client {
	if i.explicitAnalytics != nil {
		return i.explicitAnalytics
	}
	if i.adapterAnalytics != nil {
		return i.adapterAnalytics
	}
	return cfg().analytics
}

// StartInteraction starts an interaction and, for a user-triggered one, holds its
// ai_user_prompt_created until the first model call. Pass the returned context to the model
// calls, then call [Interaction.End].
//
//	ctx, in := rudderstackai.StartInteraction(ctx, rudderstackai.InteractionOptions{UserID: id})
//	defer in.End(rudderstackai.EndOptions{})
func StartInteraction(ctx context.Context, o InteractionOptions) (context.Context, *Interaction) {
	if ctx == nil {
		ctx = context.Background()
	}
	var i *Interaction
	safely(func() { i = newInteraction(ctx, o) })
	if i == nil {
		// Tracking failed: a detached interaction keeps the caller's code working.
		i = &Interaction{id: uuid.NewString(), conversationID: uuid.NewString(), trigger: TriggerUser, ended: true, seen: map[string]*recordedCall{}, toolsByID: map[string]*recordedTool{}}
	}
	i.mu.Lock()
	if i.keepsTextLocked() && o.PromptText != "" {
		i.prompt = o.PromptText
	}
	i.mu.Unlock()
	if !o.NoPromptEvent && i.trigger == TriggerUser {
		i.emitPrompt(o)
	}
	return context.WithValue(ctx, keyInteraction, i), i
}

// RunInteraction runs fn with a new interaction in its context (and the interaction's identity
// as context values), then ends it. An error returned by fn ends it with an error status and is
// returned unchanged; a panic ends it as an error and continues to panic.
//
//	err := rudderstackai.RunInteraction(ctx, rudderstackai.InteractionOptions{UserID: id},
//		func(ctx context.Context, in *rudderstackai.Interaction) error {
//			_, err := client.Chat.Completions.New(ctx, params)
//			return err
//		})
func RunInteraction(ctx context.Context, o InteractionOptions, fn func(context.Context, *Interaction) error) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = WithValues(ctx, ContextValues{
		UserID: o.UserID, AnonymousID: o.AnonymousID, ConversationID: o.ConversationID, Feature: o.Feature,
		Surface: o.Surface, AccountID: o.AccountID, OnBehalfOf: o.OnBehalfOf, Analytics: o.Analytics,
		Context: o.Context, Request: o.Request, Cookies: o.Cookies, CookieValues: o.CookieValues,
	})
	if o.IdentityFromCookies != nil {
		cv := *valuesFrom(ctx)
		cv.identityFromCookies = o.IdentityFromCookies
		ctx = context.WithValue(ctx, keyValues, &cv)
	}
	ctx, i := StartInteraction(ctx, o)
	completed := false
	defer func() {
		if completed {
			return
		}
		r := recover()
		i.End(EndOptions{Status: StatusError, ErrorType: "api_error"})
		// nil: runtime.Goexit (t.FailNow), which must keep unwinding rather than become a panic.
		if r != nil {
			panic(r)
		}
	}()
	err = fn(ctx, i)
	completed = true
	if err != nil {
		t := ErrorTypeOf(err)
		i.End(EndOptions{Status: statusForErrorType(t), ErrorType: t})
		return err
	}
	i.End(EndOptions{})
	return nil
}

// SetIntent sets the intent. After the prompt event was sent it lands on ai_response_received
// only.
func (i *Interaction) SetIntent(intent string) {
	i.mu.Lock()
	i.intent = intent
	i.mu.Unlock()
}

// SetOutcome sets the outcome (for example "resolved", "escalated") before the interaction ends.
func (i *Interaction) SetOutcome(outcome string) {
	i.mu.Lock()
	i.outcome = outcome
	i.mu.Unlock()
}

// SetTraceID links the interaction to a trace in Langfuse, LangSmith or OpenTelemetry.
func (i *Interaction) SetTraceID(traceID string) {
	i.mu.Lock()
	i.traceID = traceID
	i.mu.Unlock()
}

func (i *Interaction) emitPrompt(o InteractionOptions) {
	attachments := -1
	if o.AttachmentCount != nil && *o.AttachmentCount >= 0 {
		attachments = *o.AttachmentCount
	}
	var kinds []string
	for _, t := range o.AttachmentTypes {
		if t == "" {
			continue
		}
		k := strings.ToLower(t)
		if !attachmentKinds[k] {
			k = "file"
		}
		if !slices.Contains(kinds, k) {
			kinds = append(kinds, k)
		}
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	i.sendLocked("ai_user_prompt_created", func() map[string]any {
		p := i.baseLocked()
		put(p, "input_method", o.InputMethod)
		if attachments >= 0 {
			p["attachment_count"] = attachments
		}
		if len(kinds) > 0 {
			p["attachment_types"] = kinds
		}
		put(p, "intent", i.intent)
		if o.PromptText != "" && i.captureContent {
			if text, ok := redactText(o.PromptText); ok {
				p["prompt_text"] = text
			}
		}
		return p
	})
}

func nonNegative(n int) int { return max(n, 0) }

// RecordCall reports one model call (or CallCount aggregated calls). It returns false when the
// call was ignored: a repeated provider response ID, or the interaction already ended.
func (i *Interaction) RecordCall(call LLMCall) (recorded bool) {
	safely(func() { recorded = i.recordCall(call) })
	return recorded
}

func (i *Interaction) recordCall(call LLMCall) bool {
	c := cfg()
	call.Provider = routedProvider(call.Provider, call.Model)
	call.RequestedModel = routedModel(call.RequestedModel, call.Model)
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.ended {
		debugf("call reported after interaction ended", "interaction_id", i.id)
		return false
	}
	i.settled = true
	if i.integration == "" {
		i.integration = call.Integration
	}
	if call.ErrorType == "" {
		i.keepAnswerLocked(call.OutputText)
	}
	if call.ResponseID != "" {
		key := call.Provider + "\x00" + call.ResponseID
		if earlier, ok := i.seen[key]; ok {
			if call.AgentName != "" && earlier.agentName == "" {
				earlier.agentName = call.AgentName
			}
			return false
		}
	}
	input, output := nonNegative(call.InputTokens), nonNegative(call.OutputTokens)
	var reasoning *int
	if call.ReasoningTokens != nil {
		r := min(nonNegative(*call.ReasoningTokens), output)
		reasoning = &r
	}
	cacheWrite := nonNegative(call.CacheWriteTokens)
	tier := normalizeServiceTier(call.Provider, call.ServiceTier)
	serverTools := map[string]int{}
	serverToolCalls := 0
	for kind, n := range call.ServerToolCalls {
		if n > 0 {
			serverTools[kind] = n
			serverToolCalls += n
		}
	}
	var cost, serverToolCost *float64
	if call.CostUSD != nil && validRate(*call.CostUSD) {
		cost = Ptr(*call.CostUSD)
	}
	if call.ErrorType == "cancelled" {
		cost = nil
	} else if call.ErrorType == "" && cost == nil && !call.UsageUnknown {
		if priced, fees, ok := priceCall(call.Provider, call.Model, input, output, nonNegative(call.CachedInputTokens), cacheWrite, min(nonNegative(call.CacheWrite1hTokens), cacheWrite), tier, serverTools); ok {
			cost, serverToolCost = &priced, fees
		}
	}
	callCount := max(call.CallCount, 1)
	errorCount := 0
	if call.ErrorCount != nil {
		errorCount = nonNegative(*call.ErrorCount)
	} else if call.ErrorType != "" {
		errorCount = callCount
	}
	r := &recordedCall{
		provider: call.Provider, model: call.Model, requestedModel: call.RequestedModel, agentName: call.AgentName,
		callCount: callCount, errorCount: min(errorCount, callCount),
		inputTokens: input, outputTokens: output, reasoningTokens: reasoning,
		cachedInputTokens: nonNegative(call.CachedInputTokens), cacheWriteTokens: cacheWrite,
		cacheWrite1: min(nonNegative(call.CacheWrite1hTokens), cacheWrite), serviceTier: tier,
		serverToolCallCount: serverToolCalls, serverToolCostUSD: serverToolCost, costUSD: cost,
		finishReason: call.FinishReason, errorType: call.ErrorType, responseID: call.ResponseID,
	}
	i.calls = append(i.calls, r)
	if call.ResponseID != "" {
		i.seen[call.Provider+"\x00"+call.ResponseID] = r
	}
	if c.emitLLMCalls {
		props := i.callEventLocked(r)
		i.sendLocked("ai_llm_response_received", func() map[string]any { return props })
	} else {
		i.flushLocked()
	}
	return true
}

// RecordToolCalls reports tool calls the model made. A call already recorded under the same
// provider ID is not counted again, but its name and outcome fill in.
func (i *Interaction) RecordToolCalls(calls ...ToolCall) {
	safely(func() {
		var fire []*recordedTool
		i.mu.Lock()
		for _, call := range calls {
			name, server := normalizeTool(call.Name, call.MCPServer)
			var tool *recordedTool
			if call.ID != "" {
				tool = i.toolsByID[call.ID]
			}
			if tool == nil {
				tool = &recordedTool{calls: 1}
				i.tools = append(i.tools, tool)
				if call.ID != "" {
					i.toolsByID[call.ID] = tool
				}
			}
			fillString(&tool.id, call.ID)
			fillString(&tool.rawName, call.Name)
			fillString(&tool.name, name)
			fillString(&tool.mcpServer, server)
			fillString(&tool.parentTool, call.ParentTool)
			fillString(&tool.agentName, call.AgentName)
			if tool.args == nil {
				tool.args = call.Args
			}
			mergeToolResult(tool, ToolResult{Error: call.Error, ErrorType: call.ErrorType, Duration: call.Duration, Result: call.Result})
			fire = append(fire, tool)
		}
		i.mu.Unlock()
		i.fireToolEvents(fire, false)
	})
}

// RecordToolCallCount counts n tool calls without IDs or names.
func (i *Interaction) RecordToolCallCount(n int) {
	if n <= 0 {
		return
	}
	i.mu.Lock()
	i.tools = append(i.tools, &recordedTool{calls: n})
	i.mu.Unlock()
}

// RecordToolResult reports how a tool call recorded under id went. Results for IDs this
// interaction never recorded (earlier turns) are ignored.
func (i *Interaction) RecordToolResult(id string, result ToolResult) {
	safely(func() {
		if id == "" {
			return
		}
		i.mu.Lock()
		tool := i.toolsByID[id]
		if tool != nil {
			mergeToolResult(tool, result)
		}
		i.mu.Unlock()
		if tool != nil {
			i.fireToolEvents([]*recordedTool{tool}, false)
		}
	})
}

func fillString(dst *string, v string) {
	if *dst == "" {
		*dst = v
	}
}

// normalizeTool splits Claude-style mcp__server__tool names; an explicit server wins.
func normalizeTool(name, server string) (string, string) {
	if !strings.HasPrefix(name, "mcp__") {
		return name, server
	}
	rest := name[len("mcp__"):]
	sep := strings.Index(rest, "__")
	if sep <= 0 || sep+2 >= len(rest) {
		return name, server
	}
	return rest[sep+2:], first(server, rest[:sep])
}

// mergeToolResult: a failure from any source sticks; the first measured duration and output win.
func mergeToolResult(tool *recordedTool, r ToolResult) {
	if r.Error != nil {
		failed := (tool.err != nil && *tool.err) || *r.Error
		tool.err = &failed
		tool.settled = true
		if *r.Error && tool.errorType == "" {
			tool.errorType = normalizeToolErrorType(r.ErrorType)
		}
	}
	if r.Result != nil && !tool.hasResult {
		tool.result, tool.hasResult, tool.settled = r.Result, true, true
	}
	if tool.duration == nil && r.Duration != nil && *r.Duration >= 0 {
		d := *r.Duration
		tool.duration = &d
	}
}

// Properties owned by the SDK: extra properties with these names are ignored.
var reservedProperties = set(
	"spec_version", "conversation_id", "interaction_id", "feature", "response_status", "trigger",
	"primary_model", "primary_provider", "latency_ms", "time_to_first_token_ms", "llm_call_count",
	"tool_call_count", "total_input_tokens", "total_output_tokens", "total_reasoning_tokens",
	"total_tokens", "total_cached_input_tokens", "total_cache_write_tokens", "total_cost_usd",
	"finish_reason", "error_type", "intent", "outcome", "usage_breakdown", "tool_breakdown",
	"response_text", "prompt_text", "input_method", "trace_id", "account_id", "on_behalf_of",
	"trigger_source", "task_name", "parent_interaction_id", "trigger_event_id", "source", "surface",
	"pause_reason", "client_disconnected", "attachment_count", "attachment_types",
)

var toolEventReserved = func() map[string]bool {
	m := set("agent_name", "tool_name", "tool_call_id", "tool_status", "tool_duration_ms")
	for k := range reservedProperties {
		m[k] = true
	}
	return m
}()

// fireToolEvents sends the declared tool event of each settled call (any call when final), once.
// Rules run without the lock held.
func (i *Interaction) fireToolEvents(tools []*recordedTool, final bool) {
	rules := cfg().toolRules
	if len(rules) == 0 {
		return
	}
	type job struct {
		rule ToolEventRule
		tool recordedTool
	}
	var jobs []job
	i.mu.Lock()
	for _, t := range tools {
		if i.ended || t.fired || t.name == "" || (!t.settled && !final) {
			continue
		}
		rule, ok := rules[first(t.rawName, t.name)]
		if !ok {
			continue
		}
		t.fired = true
		jobs = append(jobs, job{rule, *t})
	}
	i.mu.Unlock()
	for _, j := range jobs {
		i.toolEvent(j.rule, j.tool)
	}
}

func (i *Interaction) toolEvent(rule ToolEventRule, t recordedTool) {
	args, _ := t.args.(map[string]any)
	if rule.When != nil {
		send := false
		safely(func() { send = rule.When(args, t.result) })
		if !send {
			return
		}
	}
	custom := map[string]any{}
	if rule.Properties != nil {
		safely(func() {
			for k, v := range rule.Properties(args, t.result) {
				if !toolEventReserved[k] {
					custom[k] = v
				}
			}
		})
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	p := custom
	for k, v := range i.baseLocked() {
		p[k] = v
	}
	p["trigger"] = string(i.trigger)
	put(p, "agent_name", t.agentName)
	p["tool_name"] = t.name
	put(p, "tool_call_id", t.id)
	p["tool_status"] = toolStatus(t.err)
	// An unknown outcome never reports a 0 duration.
	if t.duration != nil && (t.err != nil || *t.duration != 0) {
		p["tool_duration_ms"] = roundMS(*t.duration)
	}
	i.sendLocked(rule.Event, func() map[string]any { return p })
}

func toolStatus(err *bool) string {
	switch {
	case err == nil:
		return "unknown"
	case *err:
		return "error"
	}
	return "success"
}

func roundMS(d time.Duration) int64 {
	return int64(math.Floor(float64(d)/float64(time.Millisecond) + 0.5))
}

// sendLocked queues an event; the prompt event is held until the first call or the end, so it
// carries that call's integration tag and an integration's client.
func (i *Interaction) sendLocked(event string, props func() map[string]any) {
	i.pending = append(i.pending, pendingEvent{event, props, time.Now()})
	if event != "ai_user_prompt_created" {
		i.settled = true
	}
	i.flushLocked()
}

func (i *Interaction) flushLocked() {
	if !i.settled || len(i.pending) == 0 {
		return
	}
	client := i.analytics()
	batch := i.pending
	i.pending = nil
	for _, e := range batch {
		emitAI(client, e.event, i.userID, i.anonymousID, e.props(), i.msgContext, i.integration, e.at)
	}
}

func (i *Interaction) baseLocked() map[string]any {
	p := map[string]any{"conversation_id": i.conversationID, "interaction_id": i.id}
	put(p, "feature", i.feature)
	put(p, "surface", string(i.surface))
	put(p, "account_id", i.accountID)
	put(p, "on_behalf_of", i.onBehalfOf)
	put(p, "trigger_source", i.triggerSource)
	put(p, "task_name", i.taskName)
	put(p, "trigger_event_id", i.triggerEventID)
	put(p, "parent_interaction_id", i.parent)
	return p
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (i *Interaction) callEventLocked(c *recordedCall) map[string]any {
	p := i.baseLocked()
	put(p, "agent_name", c.agentName)
	p["provider"] = nullable(c.provider)
	p["model"] = nullable(c.model)
	put(p, "requested_model", c.requestedModel)
	p["input_tokens"] = c.inputTokens
	p["output_tokens"] = c.outputTokens
	if c.reasoningTokens != nil {
		p["reasoning_tokens"] = *c.reasoningTokens
	}
	p["total_tokens"] = c.inputTokens + c.outputTokens
	p["cached_input_tokens"] = c.cachedInputTokens
	p["cache_write_tokens"] = c.cacheWriteTokens
	if c.cacheWrite1 > 0 {
		p["cache_write_1h_tokens"] = c.cacheWrite1
	}
	put(p, "service_tier", c.serviceTier)
	if c.serverToolCallCount > 0 {
		p["server_tool_call_count"] = c.serverToolCallCount
	}
	if c.serverToolCostUSD != nil {
		p["server_tool_cost_usd"] = *c.serverToolCostUSD
	}
	if c.costUSD != nil {
		p["cost_usd"] = *c.costUSD
	}
	put(p, "response_id", c.responseID)
	put(p, "finish_reason", c.finishReason)
	put(p, "error_type", c.errorType)
	return p
}

// A last call's finish_reason that ends the answer, as its error_type.
var finishErrors = map[string]string{"length": "length", "max_tokens": "length", "content_filter": "content_filter"}

// End sends ai_response_received once. Later calls do nothing.
func (i *Interaction) End(o EndOptions) {
	safely(func() { i.end(o) })
}

func (i *Interaction) end(o EndOptions) {
	i.mu.Lock()
	if i.ended {
		i.mu.Unlock()
		return
	}
	hooks := i.beforeEnd
	i.beforeEnd = nil
	i.mu.Unlock()
	for _, fn := range hooks {
		safely(fn)
	}
	i.mu.Lock()
	tools := slices.Clone(i.tools)
	i.mu.Unlock()
	// Calls whose outcome never arrived are sent now, as unknown.
	i.fireToolEvents(tools, true)

	if o.Status != "" && !responseStatuses[o.Status] {
		warnOnce("invalid-status", fmt.Sprintf("End: invalid status %q; using the status derived from the calls", o.Status), 0)
		o.Status = ""
	}
	i.mu.Lock()
	if i.ended {
		i.mu.Unlock()
		return
	}
	if o.ClientDisconnected {
		i.clientDisconnected = true
	}
	i.ended = true
	i.settled = true
	var props map[string]any
	safely(func() {
		props = map[string]any{}
		for k, v := range o.ExtraProperties {
			if !reservedProperties[k] {
				props[k] = v
			}
		}
		for k, v := range i.buildLocked(o) {
			props[k] = v
		}
	})
	if props != nil {
		i.sendLocked("ai_response_received", func() map[string]any { return props })
	}
	answer := first(o.ResponseText, i.answer)
	snap := turnSnapshot{prompt: i.prompt, answer: answer, messages: i.messages}
	for _, t := range i.tools {
		if t.name != "" {
			snap.toolNames = append(snap.toolNames, t.name)
		}
	}
	i.prompt, i.answer, i.messages = "", "", nil
	i.mu.Unlock()

	if props == nil || props["response_status"] != string(StatusSuccess) {
		return
	}
	c := cfg()
	var done []chan struct{}
	if i.classify && c.classifier != nil {
		if ch := scheduleClassification(i, snap, c.classifier); ch != nil {
			done = append(done, ch)
		}
	}
	if c.evaluator != nil {
		if ch := scheduleEvaluation(i, snap, c.evaluator); ch != nil {
			done = append(done, ch)
		}
	}
	if len(done) > 0 {
		all := make(chan struct{})
		go func() {
			for _, ch := range done {
				<-ch
			}
			close(all)
		}()
		i.mu.Lock()
		i.background = all
		i.mu.Unlock()
	}
}

// sendBackground sends a classification or score of an ended interaction.
func (i *Interaction) sendBackground(event string, extra map[string]any) {
	i.mu.Lock()
	defer i.mu.Unlock()
	p := i.baseLocked()
	p["trigger"] = string(i.trigger)
	for k, v := range extra {
		p[k] = v
	}
	i.sendLocked(event, func() map[string]any { return p })
}

// waitBackground waits for this interaction's classification and scores, at most d.
func (i *Interaction) waitBackground(d time.Duration) {
	i.mu.Lock()
	ch := i.background
	i.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case <-ch:
	case <-time.After(d):
	}
}

func (i *Interaction) buildLocked(o EndOptions) map[string]any {
	calls := i.calls
	var last, lastOK *recordedCall
	for _, c := range calls {
		last = c
		if c.errorType == "" {
			lastOK = c
		}
	}
	status, errorType, pauseReason := o.Status, o.ErrorType, o.PauseReason
	if status == "" && i.hint != nil && i.hint.calls == len(calls) {
		status = i.hint.status
		errorType = first(errorType, i.hint.errorType)
	}
	if status == "" {
		finished := ""
		if last != nil {
			finished = finishErrors[last.finishReason]
		}
		switch {
		case last != nil && last.errorType != "":
			status = statusForErrorType(last.errorType)
			errorType = first(errorType, last.errorType)
		case finished != "":
			errorType = first(errorType, finished)
			status = statusForErrorType(finished)
		default:
			status = StatusSuccess
		}
	}
	if status == StatusCancelled {
		errorType = first(errorType, "cancelled")
	}
	p := i.baseLocked()
	p["response_status"] = string(status)
	if status == StatusPaused {
		if pauseReason != "" && !pauseReasons[pauseReason] {
			warnOnce("invalid-pause-reason", fmt.Sprintf("invalid pause reason %q", pauseReason), 0)
			pauseReason = ""
		}
		p["pause_reason"] = string(cmp.Or(pauseReason, PauseOther))
	}
	p["trigger"] = string(i.trigger)
	if i.clientDisconnected {
		p["client_disconnected"] = true
	}
	named := lastOK
	if named == nil {
		named = last
	}
	if named != nil {
		put(p, "primary_model", named.model)
		put(p, "primary_provider", named.provider)
	}
	p["latency_ms"] = roundMS(time.Since(i.started))
	if i.firstToken >= 0 {
		p["time_to_first_token_ms"] = roundMS(i.firstToken)
	}
	sum := func(f func(*recordedCall) int) int {
		n := 0
		for _, c := range calls {
			n += f(c)
		}
		return n
	}
	p["llm_call_count"] = sum(func(c *recordedCall) int { return c.callCount })
	tools := 0
	for _, t := range i.tools {
		tools += t.calls
	}
	p["tool_call_count"] = tools
	// A cancelled turn with no recorded call has unknown usage: omitted, not a free turn.
	if status != StatusCancelled || len(calls) > 0 {
		in := sum(func(c *recordedCall) int { return c.inputTokens })
		out := sum(func(c *recordedCall) int { return c.outputTokens })
		p["total_input_tokens"] = in
		p["total_output_tokens"] = out
		if slices.ContainsFunc(calls, func(c *recordedCall) bool { return c.reasoningTokens != nil }) {
			p["total_reasoning_tokens"] = sum(func(c *recordedCall) int {
				if c.reasoningTokens == nil {
					return 0
				}
				return *c.reasoningTokens
			})
		}
		p["total_tokens"] = in + out
		p["total_cached_input_tokens"] = sum(func(c *recordedCall) int { return c.cachedInputTokens })
		p["total_cache_write_tokens"] = sum(func(c *recordedCall) int { return c.cacheWriteTokens })
	}
	if total, ok := pricedTotal(calls); ok {
		p["total_cost_usd"] = total
	}
	if lastOK != nil {
		put(p, "finish_reason", lastOK.finishReason)
	}
	if status != StatusSuccess {
		put(p, "error_type", errorType)
	}
	put(p, "intent", i.intent)
	put(p, "outcome", i.outcome)
	if b := usageBreakdown(calls); len(b) > 0 {
		p["usage_breakdown"] = b
	}
	if i.toolBreakdown && len(i.tools) > 0 {
		p["tool_breakdown"] = toolBreakdown(i.tools)
	}
	if o.ResponseText != "" && i.captureContent {
		if text, ok := redactText(o.ResponseText); ok {
			p["response_text"] = text
		}
	}
	put(p, "trace_id", i.traceID)
	return p
}

// pricedTotal sums the known costs; not ok when no call was priced or a successful call was not.
func pricedTotal(calls []*recordedCall) (float64, bool) {
	priced := false
	total := 0.0
	for _, c := range calls {
		if c.costUSD == nil {
			if c.errorType == "" {
				return 0, false
			}
			continue
		}
		priced = true
		total += *c.costUSD
	}
	return round8(total), priced
}

func usageBreakdown(calls []*recordedCall) []map[string]any {
	type group struct {
		first                                                    *recordedCall
		calls                                                    []*recordedCall
		callCount, input, output, cached, write, write1h, server int
		errors                                                   int
		reasoning                                                *int
	}
	var order []string
	groups := map[string]*group{}
	for _, c := range calls {
		key, _ := json.Marshal([]string{c.agentName, c.provider, c.model, c.requestedModel, c.serviceTier})
		g := groups[string(key)]
		if g == nil {
			g = &group{first: c}
			groups[string(key)] = g
			order = append(order, string(key))
		}
		g.calls = append(g.calls, c)
		g.callCount += c.callCount
		g.input += c.inputTokens
		g.output += c.outputTokens
		if c.reasoningTokens != nil {
			g.reasoning = Ptr(*c.reasoningTokens + derefInt(g.reasoning))
		}
		g.cached += c.cachedInputTokens
		g.write += c.cacheWriteTokens
		g.write1h += c.cacheWrite1
		g.server += c.serverToolCallCount
		g.errors += c.errorCount
	}
	out := make([]map[string]any, 0, len(order))
	for _, k := range order {
		g := groups[k]
		e := map[string]any{}
		put(e, "agent_name", g.first.agentName)
		e["provider"] = nullable(g.first.provider)
		e["model"] = nullable(g.first.model)
		put(e, "requested_model", g.first.requestedModel)
		e["call_count"] = g.callCount
		e["input_tokens"] = g.input
		e["output_tokens"] = g.output
		if g.reasoning != nil {
			e["reasoning_tokens"] = *g.reasoning
		}
		e["total_tokens"] = g.input + g.output
		e["cached_input_tokens"] = g.cached
		e["cache_write_tokens"] = g.write
		if g.write1h > 0 {
			e["cache_write_1h_tokens"] = g.write1h
		}
		put(e, "service_tier", g.first.serviceTier)
		if g.server > 0 {
			e["server_tool_call_count"] = g.server
			fees, all := 0.0, true
			for _, c := range g.calls {
				if c.serverToolCallCount == 0 {
					continue
				}
				if c.serverToolCostUSD == nil {
					all = false
					break
				}
				fees += *c.serverToolCostUSD
			}
			if all {
				e["server_tool_cost_usd"] = round8(fees)
			}
		}
		e["error_count"] = g.errors
		if cost, ok := pricedTotal(g.calls); ok {
			e["cost_usd"] = cost
		}
		out = append(out, e)
	}
	return out
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func toolBreakdown(tools []*recordedTool) []map[string]any {
	type totals struct {
		name, agent, server, parent string
		calls, errors, measured     int
		known                       bool
		ms, maxMS                   float64
		errorTypes                  map[string]int
	}
	groups := map[[4]string]*totals{}
	for _, t := range tools {
		name := cmp.Or(t.name, "unknown")
		key := [4]string{t.agentName, t.mcpServer, name, t.parentTool}
		g := groups[key]
		if g == nil {
			g = &totals{name: name, agent: t.agentName, server: t.mcpServer, parent: t.parentTool, errorTypes: map[string]int{}}
			groups[key] = g
		}
		g.calls += t.calls
		if t.err != nil {
			g.known = true
			if *t.err {
				g.errors += t.calls
				g.errorTypes[cmp.Or(t.errorType, "api_error")] += t.calls
			}
		}
		if t.duration != nil {
			ms := float64(*t.duration) / float64(time.Millisecond)
			g.ms += ms
			g.measured += t.calls
			g.maxMS = max(g.maxMS, ms)
		}
	}
	keys := make([][4]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	// UTF-16 code-unit order, as the Node.js and Python SDKs sort.
	slices.SortFunc(keys, func(a, b [4]string) int {
		for n := range a {
			if c := compareUTF16(a[n], b[n]); c != 0 {
				return c
			}
		}
		return 0
	})
	out := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		g := groups[k]
		e := map[string]any{"tool_name": g.name, "call_count": g.calls}
		put(e, "agent_name", g.agent)
		put(e, "mcp_server", g.server)
		put(e, "parent_tool", g.parent)
		if g.known {
			e["error_count"] = g.errors
		}
		if g.errors > 0 {
			types := map[string]any{}
			for t, n := range g.errorTypes {
				types[t] = n
			}
			e["error_types"] = types
		}
		if g.measured > 0 {
			e["duration_ms"] = int64(math.Floor(g.ms + 0.5))
			e["measured_count"] = g.measured
			e["max_duration_ms"] = int64(math.Floor(g.maxMS + 0.5))
		}
		out = append(out, e)
	}
	return out
}

func compareUTF16(a, b string) int {
	ua, ub := []rune(a), []rune(b)
	ea, eb := utf16Units(ua), utf16Units(ub)
	return slices.Compare(ea, eb)
}

func utf16Units(rs []rune) []uint16 {
	out := make([]uint16, 0, len(rs))
	for _, r := range rs {
		if r >= 0x10000 {
			r -= 0x10000
			out = append(out, uint16(0xD800+(r>>10)), uint16(0xDC00+(r&0x3FF)))
			continue
		}
		out = append(out, uint16(r))
	}
	return out
}

// ----------------------------------------------------------------- integration hooks

func (i *Interaction) keepsTextLocked() bool {
	if i.ended {
		return false
	}
	c := cfg()
	return (i.classify && c.classifier != nil) || c.evaluator != nil
}

func (i *Interaction) keepAnswerLocked(text string) {
	if text != "" && i.keepsTextLocked() {
		i.answer = text
	}
}

// adoptAnalytics uses the client an integration was set up with when nothing more specific is.
func (i *Interaction) adoptAnalytics(c analytics.Client) {
	if c == nil {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.adapterAnalytics == nil {
		i.adapterAnalytics = c
		i.flushLocked()
	}
}

// callStarted: a model call of this interaction starts; the held prompt event goes out now.
func (i *Interaction) callStarted(ctx context.Context, integration string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.ended {
		return
	}
	if i.integration == "" {
		i.integration = integration
	}
	i.fillTraceID(ctx)
	i.settled = true
	i.flushLocked()
}

// forCall is the interaction a model call starting now reports into: the context's active one,
// or a new ephemeral one the integration must end. Nil for the SDK's own (untracked) calls.
func forCall(ctx context.Context, integration string, client analytics.Client) (*Interaction, bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	if isUntracked(ctx) {
		return nil, false
	}
	active := FromContext(ctx)
	if active != nil && !active.Ended() {
		active.adoptAnalytics(client)
		active.callStarted(ctx, integration)
		return active, false
	}
	if active != nil {
		warnOnce("call-after-end", "a model call ran after its interaction ended, so it is reported as a separate answer", 0)
	}
	id := resolveIdentity(identityInputs{}, valuesFrom(ctx), cfg())
	trigger := TriggerSystem
	if id.userID != "" || id.anonymousID != "" {
		trigger = TriggerUser
	}
	o := InteractionOptions{Trigger: trigger, Classify: Ptr(false), adapterAnalytics: client}
	if active != nil {
		o.ParentInteractionID = active.id
	}
	var i *Interaction
	safely(func() { i = newInteraction(ctx, o) })
	if i == nil {
		return nil, false
	}
	i.mu.Lock()
	i.integration = integration
	i.mu.Unlock()
	return i, true
}

func (i *Interaction) keepMessages(messages any) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if messages != nil && i.keepsTextLocked() {
		i.messages = messages
	}
}

func (i *Interaction) keepsText() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.keepsTextLocked()
}

// firstTokenAt marks the first streamed token; the earliest mark wins.
func (i *Interaction) firstTokenAt(at time.Time) {
	i.mu.Lock()
	defer i.mu.Unlock()
	d := at.Sub(i.started)
	if d < 0 {
		return
	}
	if i.firstToken < 0 || d < i.firstToken {
		i.firstToken = d
	}
}

// hintOutcome sets the status End reports unless a call is recorded afterwards.
func (i *Interaction) hintOutcome(status ResponseStatus, errorType string) {
	i.mu.Lock()
	i.hint = &outcomeHint{status: status, errorType: errorType, calls: len(i.calls)}
	i.mu.Unlock()
}

// onBeforeEnd runs fn just before ai_response_received is built.
func (i *Interaction) onBeforeEnd(fn func()) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if !i.ended {
		i.beforeEnd = append(i.beforeEnd, fn)
	}
}
