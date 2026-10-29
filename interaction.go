package rudderstackai

import (
	"cmp"
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
	// OneShot uses the interaction ID when no explicit or context conversation ID exists. Since v0.4; default false.
	OneShot bool
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
	// AgentVersion names the app release or configuration; Variant names its experiment arm.
	AgentVersion string
	Variant      string
	// TriggerSource is why the run started: "schedule", "webhook", "slack", "queue", "agent"...
	TriggerSource string
	// TaskName is the job or task being run, for example "alert-triage".
	TaskName string
	// TriggerEventID is the ID of the update that started the run, for example the event_id of an
	// MCP Events delivery.
	TriggerEventID string
	// ParentInteractionID is the interaction that started this one: agent hand-offs, sub-tasks.
	ParentInteractionID string
	// Resume continues a paused run from the snapshot [Interaction.Pause] returned, in this or
	// another process: its IDs, identity and background fields win over these options, no prompt
	// event or ai_task_started is sent, and its pause count and paused time carry on to the final
	// answer.
	Resume *PausedInteraction
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
	// Credits overrides the configured credit pricing hook. Nil means unknown.
	Credits *float64
	// CallPurpose, FallbackUsed and BYOK are app-set call details.
	CallPurpose        string
	FallbackUsed, BYOK *bool
	// StartedAt, EndedAt and FirstTokenAt describe observed call boundaries. EndedAt defaults to now.
	StartedAt, EndedAt, FirstTokenAt time.Time
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

// capLocked queues a notice, once per interaction; unlock reports it after publishing usage.
func (i *Interaction) capLocked() {
	if !i.capped {
		i.capped = true
		i.capPending = true
	}
}

// foldCallLocked folds a call past MaxCallsPerInteraction into its usage group's overflow
// record, so totals and usage_breakdown keep counting without storing the call.
func (i *Interaction) foldCallLocked(r *recordedCall) {
	var dst *recordedCall
	for _, c := range i.overflow {
		if c.agentName == r.agentName && c.callPurpose == r.callPurpose && c.provider == r.provider && c.model == r.model && c.requestedModel == r.requestedModel && c.serviceTier == r.serviceTier {
			dst = c
			break
		}
	}
	if dst == nil {
		first := *r // a copy: lastCall and lastOK may point at r
		i.overflow = append(i.overflow, &first)
		return
	}
	if (dst.credits == nil && dst.errorType == "") || (r.credits == nil && r.errorType == "") {
		dst.uncredited = true
	}
	if r.credits != nil {
		dst.credits = Ptr(round8(derefFloat(dst.credits) + *r.credits))
	}
	dst.callCount += r.callCount
	dst.errorCount += r.errorCount
	dst.inputTokens += r.inputTokens
	dst.outputTokens += r.outputTokens
	if r.reasoningTokens != nil {
		dst.reasoningTokens = Ptr(derefInt(dst.reasoningTokens) + *r.reasoningTokens)
	}
	dst.cachedInputTokens += r.cachedInputTokens
	dst.cacheWriteTokens += r.cacheWriteTokens
	dst.cacheWrite1 += r.cacheWrite1
	if r.serverToolCallCount > 0 {
		dst.serverToolCallCount += r.serverToolCallCount
		if r.serverToolCostUSD == nil {
			dst.feesUnknown = true
		} else {
			dst.serverToolCostUSD = Ptr(round8(derefFloat(dst.serverToolCostUSD) + *r.serverToolCostUSD))
		}
	}
	if dst.costUSD == nil && dst.errorType == "" {
		dst.unpriced = true
	}
	switch {
	case r.costUSD != nil:
		dst.costUSD = Ptr(round8(derefFloat(dst.costUSD) + *r.costUSD))
	case r.errorType == "":
		dst.unpriced = true
	}
}

// foldToolLocked counts a tool call past MaxCallsPerInteraction in its tool_breakdown identity.
func (i *Interaction) foldToolLocked(t *recordedTool) {
	i.capLocked()
	for _, o := range i.toolOverflow {
		if o.name == t.name && o.mcpServer == t.mcpServer && o.parentTool == t.parentTool && o.agentName == t.agentName {
			o.calls += t.calls
			return
		}
	}
	i.toolOverflow = append(i.toolOverflow, t)
}

func derefFloat(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
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
	// Result is the observed output. Nil means absent; json.RawMessage("null") represents explicit JSON null.
	Result any
}

// EndOptions configures [Interaction.End].
type EndOptions struct {
	// Status is the final status. Derived from the recorded calls when empty.
	Status    ResponseStatus
	ErrorType string
	// PauseReason is what a paused run waits for. Defaults to PauseOther.
	PauseReason PauseReason
	// BlockReason explains a StatusBlocked answer; defaults to BlockOther.
	BlockReason BlockReason
	// ClientDisconnected: the client went away before the answer finished.
	ClientDisconnected bool
	// ResponseText is the answer. Sent as response_text only with content capture on.
	ResponseText string
	// ExtraProperties are custom properties for ai_response_received. SDK-owned names are
	// ignored.
	ExtraProperties map[string]any

	// endReason is set when the SDK ends the interaction itself (end_reason).
	endReason string
}

// PauseOptions configures [Interaction.Pause].
type PauseOptions struct {
	// Reason is what the run waits for (pause_reason). Defaults to PauseOther.
	Reason PauseReason
	// ResponseText is what the run said before it paused, such as the question. Sent as
	// response_text only with content capture on.
	ResponseText string
	// ExtraProperties are custom properties for this ai_response_received. SDK-owned names are
	// ignored.
	ExtraProperties map[string]any
}

// PausedInteraction is a paused run, as JSON: store it with the run's own state and pass it as
// [InteractionOptions.Resume] or [TaskOptions.Resume] to continue the same interaction later, in
// any process. Keys match the event properties.
type PausedInteraction struct {
	InteractionID  string  `json:"interaction_id"`
	ConversationID string  `json:"conversation_id"`
	Trigger        Trigger `json:"trigger"`
	// PauseCount is the pauses so far, this one included.
	PauseCount int `json:"pause_count"`
	// TotalPausedMS is the time spent paused before this pause.
	TotalPausedMS int64 `json:"total_paused_ms"`
	// PausedAt is when this pause started (Unix milliseconds).
	PausedAt            int64      `json:"paused_at"`
	UserID              string     `json:"user_id,omitempty"`
	AnonymousID         string     `json:"anonymous_id,omitempty"`
	Feature             string     `json:"feature,omitempty"`
	Surface             Surface    `json:"surface,omitempty"`
	AccountID           string     `json:"account_id,omitempty"`
	OnBehalfOf          string     `json:"on_behalf_of,omitempty"`
	AgentVersion        string     `json:"agent_version,omitempty"`
	Variant             string     `json:"variant,omitempty"`
	Confidence          Confidence `json:"confidence,omitempty"`
	CompactionCount     int        `json:"compaction_count,omitempty"`
	TriggerSource       string     `json:"trigger_source,omitempty"`
	TaskName            string     `json:"task_name,omitempty"`
	TriggerEventID      string     `json:"trigger_event_id,omitempty"`
	ParentInteractionID string     `json:"parent_interaction_id,omitempty"`
	Intent              string     `json:"intent,omitempty"`
	TraceID             string     `json:"trace_id,omitempty"`
	Outcome             string     `json:"outcome,omitempty"`
	Integration         string     `json:"integration,omitempty"`
	// SeenResponseIDs retains provider/response ID pairs already counted. Since v0.4.
	SeenResponseIDs [][2]string `json:"seen_response_ids,omitempty"`
	// PendingCalls and PendingTools carry usage accepted after the paused segment closed. Since v0.4.
	PendingCalls []map[string]any `json:"pending_calls,omitempty"`
	PendingTools []map[string]any `json:"pending_tools,omitempty"`
}

// resumeFrom applies a Resume snapshot's fields to o; false (with a warning) when it is not one.
func resumeFrom(o *InteractionOptions) bool {
	r := o.Resume
	if r == nil {
		return false
	}
	if r.InteractionID == "" || r.ConversationID == "" ||
		(r.Trigger != TriggerUser && r.Trigger != TriggerSystem) ||
		r.PauseCount < 1 || int64(r.PauseCount) > 9007199254740991 ||
		r.TotalPausedMS < 0 || r.TotalPausedMS > 9007199254740991 || r.PausedAt < 0 || r.PausedAt > 9007199254740991 {
		warnOnce("invalid-resume", "Resume: not a paused interaction snapshot; starting a new interaction", 0)
		return false
	}
	o.InteractionID, o.ConversationID, o.Trigger = r.InteractionID, r.ConversationID, r.Trigger
	o.UserID, o.AnonymousID, o.Feature, o.Surface = r.UserID, r.AnonymousID, r.Feature, r.Surface
	o.AccountID, o.OnBehalfOf, o.TriggerSource, o.TaskName = r.AccountID, r.OnBehalfOf, r.TriggerSource, r.TaskName
	o.AgentVersion, o.Variant = r.AgentVersion, r.Variant
	o.TriggerEventID, o.ParentInteractionID, o.Intent, o.TraceID = r.TriggerEventID, r.ParentInteractionID, r.Intent, r.TraceID
	return true
}

type recordedCall struct {
	credits                                          *float64
	callPurpose                                      string
	fallbackUsed, byok                               *bool
	latency, ttft                                    *time.Duration
	uncredited                                       bool
	provider, model, requestedModel, agentName       string
	callCount, errorCount                            int
	inputTokens, outputTokens                        int
	reasoningTokens                                  *int
	cachedInputTokens, cacheWriteTokens, cacheWrite1 int
	serviceTier                                      string
	serverToolCallCount                              int
	serverToolCostUSD, costUSD                       *float64
	finishReason, errorType, responseID              string
	// Past MaxCallsPerInteraction, calls are folded into an overflow record per group: unpriced
	// (a successful call had no cost) and feesUnknown (server-tool fees missing) then stick.
	unpriced, feesUnknown bool
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
	// rates is the snapshot a fitted event was sampled under, when pinned.
	rates  map[string]float64
	pinned bool
}

type outcomeHint struct {
	status    ResponseStatus
	errorType string
	calls     int
}

// Interaction is one user prompt and the one answer it gets, however many model calls, tools or
// agents that takes. Integrations report each call into the interaction carried by the request
// context; it sends ai_user_prompt_created, a response for each pause and one final response. Safe for
// concurrent use.
type Interaction struct {
	agentVersion, variant                                                  string
	confidence                                                             Confidence
	compactions                                                            int
	id, conversationID, userID, anonymousID, feature                       string
	surface                                                                Surface
	trigger                                                                Trigger
	accountID, onBehalfOf, triggerSource, taskName, triggerEventID, parent string
	captureContent, toolBreakdown, classify                                bool
	explicitAnalytics                                                      analytics.Client
	msgContext                                                             map[string]any
	// resumed: continued from a Resume snapshot.
	resumed bool
	// untracked: refused at MaxOpenInteractions; it and its calls are not tracked.
	untracked bool
	// opened and elem place it among the open interactions (guarded by open.mu).
	opened time.Time
	elem   *list.Element

	mu sync.Mutex
	// started is the start of the current segment: the run, or its latest resume.
	started time.Time
	// pausedAt is when the current pause started; zero while running.
	pausedAt           time.Time
	pausing            bool
	pauseGuard         atomic.Bool // checked before mu, including callback reentry
	pauseCount         int
	pausedTotal        time.Duration
	adapterAnalytics   analytics.Client
	intent, outcome    string
	traceID            string
	integration        string
	ended              bool
	firstToken         time.Duration
	calls              []*recordedCall
	callCount          int
	lastCall, lastOK   *recordedCall
	seen               map[string]*recordedCall
	tools              []*recordedTool
	overflow           []*recordedCall // calls past MaxCallsPerInteraction, one per usage group
	toolOverflow       []*recordedTool // tool calls past it, one per tool_breakdown identity
	capped             bool
	capPending         bool
	buildErrors        []error
	toolsByID          map[string]*recordedTool
	hint               *outcomeHint
	clientDisconnected bool
	disconnected       func() bool
	pending            []pendingEvent
	outbox             []outgoing
	settled            bool
	beforeEnd          []func()
	prompt, answer     string
	messages           any
	background         chan struct{}
	// callErrors are errors wrapped calls failed with, newest last, so the app returning one
	// keeps its call's error_type.
	callErrors []callError
}

type callError struct {
	err       error
	errorType string
}

// maxCallErrors bounds callErrors; the newest are kept.
const maxCallErrors = 8

// noteCallError remembers the error a wrapped call failed with, as recorded under errorType.
func (i *Interaction) noteCallError(err error, errorType string) {
	if err == nil || errorType == "" {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if len(i.callErrors) == maxCallErrors {
		i.callErrors = i.callErrors[1:]
	}
	i.callErrors = append(i.callErrors, callError{err, errorType})
}

// answerErrorType is the error_type of an answer the app's code ended with err: a wrapped call's
// own error keeps that call's type, anything else is classified ([appErrorType]).
func (i *Interaction) answerErrorType(err error) string {
	i.mu.Lock()
	noted := slices.Clone(i.callErrors)
	i.mu.Unlock()
	for n := len(noted) - 1; n >= 0; n-- {
		if errors.Is(err, noted[n].err) {
			return noted[n].errorType
		}
	}
	return appErrorType(err)
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
	snapshot := o.Resume
	resumed := resumeFrom(&o)
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
	interactionID := first(o.InteractionID, uuid.NewString())
	conversationID := first(o.ConversationID, cv.conversationID)
	if conversationID == "" {
		if o.OneShot {
			conversationID = interactionID
		} else {
			conversationID = uuid.NewString()
		}
	}
	i := &Interaction{
		id:                interactionID,
		conversationID:    conversationID,
		userID:            id.userID,
		anonymousID:       id.anonymousID,
		feature:           first(o.Feature, cv.feature),
		surface:           first2(surfaceOf(o.Surface), surfaceOf(cv.surface)),
		trigger:           trigger,
		accountID:         first(o.AccountID, cv.accountID),
		onBehalfOf:        first(o.OnBehalfOf, cv.onBehalfOf),
		agentVersion:      first(o.AgentVersion, cv.agentVersion),
		variant:           first(o.Variant, cv.variant),
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
		disconnected:      cv.disconnected,
		resumed:           resumed,
	}
	if resumed {
		i.userID, i.anonymousID = snapshot.UserID, snapshot.AnonymousID
		i.feature, i.surface = snapshot.Feature, surfaceOf(snapshot.Surface)
		i.accountID, i.onBehalfOf = snapshot.AccountID, snapshot.OnBehalfOf
		i.agentVersion, i.variant = snapshot.AgentVersion, snapshot.Variant
		i.confidence, i.compactions = snapshot.Confidence, nonNegative(snapshot.CompactionCount)
		i.outcome, i.integration = snapshot.Outcome, snapshot.Integration
		i.pauseCount = max(snapshot.PauseCount, 0)
		for _, pair := range snapshot.SeenResponseIDs {
			if _, exists := i.seen[pair[0]+"\x00"+pair[1]]; exists {
				continue
			}
			if len(i.seen) >= cfg().limits.MaxCallsPerInteraction {
				break
			}
			i.seen[pair[0]+"\x00"+pair[1]] = nil
		}
		i.restorePending(snapshot)
		i.pausedTotal = time.Duration(max(snapshot.TotalPausedMS, 0))*time.Millisecond +
			max(now.Sub(time.UnixMilli(snapshot.PausedAt)), 0)
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
	i.mu.Lock()
	needed := i.traceID == "" && !i.ended && ctx != nil
	i.mu.Unlock()
	if !needed {
		return
	}
	if from := cfg().traceIDFrom; from != nil {
		var traceID string
		callback("WithTraceIDFrom", func() error { traceID = from(ctx); return nil })
		i.mu.Lock()
		if i.traceID == "" && !i.ended {
			i.traceID = traceID
		}
		i.mu.Unlock()
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
	safely(func() {
		sweepExpired()
		if i = newInteraction(ctx, o); !register(i) {
			i = nil
		}
	})
	if i == nil {
		// Untracked (tracking failed, or MaxOpenInteractions): a detached interaction keeps the
		// caller's code working, and its calls are not tracked either.
		i = &Interaction{id: uuid.NewString(), conversationID: uuid.NewString(), trigger: TriggerUser, ended: true, untracked: true, seen: map[string]*recordedCall{}, toolsByID: map[string]*recordedTool{}}
		return context.WithValue(ctx, keyInteraction, i), i
	}
	i.mu.Lock()
	if i.keepsTextLocked() && o.PromptText != "" {
		i.prompt = o.PromptText
	}
	i.mu.Unlock()
	if !o.NoPromptEvent && i.trigger == TriggerUser && !i.resumed {
		i.emitPrompt(o)
	}
	return context.WithValue(ctx, keyInteraction, i), i
}

// RunInteraction runs fn with a new interaction in its context (and the interaction's identity
// as context values), then ends it. An error returned by fn ends it with an error status and is
// returned unchanged; a panic ends it the same way (app_error for a value that is not an error)
// and continues to panic. An error no wrapped call failed with and that carries no provider
// signal (integration error type, error code, HTTP status) is app_error.
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
		Surface: o.Surface, AccountID: o.AccountID, OnBehalfOf: o.OnBehalfOf, AgentVersion: o.AgentVersion, Variant: o.Variant, Analytics: o.Analytics,
		Context: o.Context, Request: o.Request, Cookies: o.Cookies, CookieValues: o.CookieValues,
	})
	if o.IdentityFromCookies != nil {
		cv := *valuesFrom(ctx)
		cv.identityFromCookies = o.IdentityFromCookies
		ctx = context.WithValue(ctx, keyValues, &cv)
	}
	ctx, i := StartInteraction(ctx, o)
	if i.resumed {
		cv := *valuesFrom(ctx)
		cv.userID, cv.anonymousID, cv.conversationID = i.userID, i.anonymousID, i.conversationID
		cv.feature, cv.surface = i.feature, i.surface
		cv.accountID, cv.onBehalfOf = i.accountID, i.onBehalfOf
		cv.agentVersion, cv.variant = i.agentVersion, i.variant
		cv.cookies, cv.disconnected = nil, nil
		cv.identityFromCookies = Ptr(false)
		ctx = context.WithValue(ctx, keyValues, &cv)
	}
	completed := false
	defer func() {
		if completed {
			return
		}
		r := recover()
		t := "app_error"
		if e, ok := r.(error); ok {
			t = i.answerErrorType(e)
		}
		i.End(EndOptions{Status: statusForErrorType(t), ErrorType: t})
		// nil: runtime.Goexit (t.FailNow), which must keep unwinding rather than become a panic.
		if r != nil {
			panic(r)
		}
	}()
	err = fn(ctx, i)
	completed = true
	if err != nil {
		t := i.answerErrorType(err)
		i.End(EndOptions{Status: statusForErrorType(t), ErrorType: t})
		return err
	}
	// A paused run stays open for its resume.
	if !i.Paused() {
		i.End(EndOptions{})
	}
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
	// Masked now, outside the lock.
	var promptText string
	var promptOK bool
	if o.PromptText != "" && i.captureContent {
		promptText, promptOK = redactText(o.PromptText)
	}
	i.mu.Lock()
	defer i.unlock()
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
		if promptOK {
			p["prompt_text"] = promptText
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
	// Freeze the provider boundary before pricing or waiting for the interaction lock.
	if !call.StartedAt.IsZero() && call.EndedAt.IsZero() {
		call.EndedAt = time.Now()
	}
	call.Provider = routedProvider(call.Provider, call.Model)
	call.RequestedModel = routedModel(call.RequestedModel, call.Model)
	i.mu.Lock()
	defer i.unlock()
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
	credits := creditsOf(call.Credits)
	if call.ErrorType == "cancelled" {
		credits = nil
	}
	if credits == nil && c.credits != nil && call.ErrorType == "" && !call.UsageUnknown {
		// App callbacks may inspect, update or end this interaction.
		i.mu.Unlock()
		credits = priceCredits(c.credits, CreditCall{
			Provider: call.Provider, Model: call.Model, InputTokens: input, OutputTokens: output,
			ReasoningTokens: reasoning, CachedInputTokens: nonNegative(call.CachedInputTokens), CacheWriteTokens: cacheWrite,
			ServiceTier: tier, CostUSD: cost, CallCount: callCount, AgentName: call.AgentName,
			CallPurpose: call.CallPurpose, FallbackUsed: call.FallbackUsed, BYOK: call.BYOK,
		})
		i.mu.Lock()
		if i.ended {
			return false
		}
	}
	if call.ResponseID != "" {
		if earlier, ok := i.seen[call.Provider+"\x00"+call.ResponseID]; ok {
			if earlier == nil {
				return false
			}
			fillString(&earlier.agentName, call.AgentName)
			fillString(&earlier.callPurpose, call.CallPurpose)
			if call.BYOK != nil {
				earlier.byok = call.BYOK
			}
			if call.FallbackUsed != nil {
				earlier.fallbackUsed = call.FallbackUsed
			}
			// Hook-derived credits priced the duplicate's discarded usage, so only explicit ones merge.
			if call.Credits != nil && call.ErrorType != "cancelled" && earlier.errorType != "cancelled" {
				earlier.credits = creditsOf(call.Credits)
			}
			return false
		}
	}
	i.resumeLocked()
	var latency, ttft *time.Duration
	if !call.StartedAt.IsZero() {
		ended := call.EndedAt
		if ended.IsZero() {
			ended = time.Now()
		}
		latency = Ptr(max(ended.Sub(call.StartedAt), 0))
		if !call.FirstTokenAt.IsZero() && !call.FirstTokenAt.Before(call.StartedAt) && !call.FirstTokenAt.After(ended) {
			ttft = Ptr(call.FirstTokenAt.Sub(call.StartedAt))
		}
	}
	r := &recordedCall{
		credits: credits, callPurpose: call.CallPurpose, fallbackUsed: call.FallbackUsed, byok: call.BYOK,
		latency: latency, ttft: ttft,
		provider: call.Provider, model: call.Model, requestedModel: call.RequestedModel, agentName: call.AgentName,
		callCount: callCount, errorCount: min(errorCount, callCount),
		inputTokens: input, outputTokens: output, reasoningTokens: reasoning,
		cachedInputTokens: nonNegative(call.CachedInputTokens), cacheWriteTokens: cacheWrite,
		cacheWrite1: min(nonNegative(call.CacheWrite1hTokens), cacheWrite), serviceTier: tier,
		serverToolCallCount: serverToolCalls, serverToolCostUSD: serverToolCost, costUSD: cost,
		finishReason: call.FinishReason, errorType: call.ErrorType, responseID: call.ResponseID,
	}
	i.callCount++
	i.lastCall = r
	if r.errorType == "" {
		i.lastOK = r
	}
	if len(i.calls) < cfg().limits.MaxCallsPerInteraction {
		i.calls = append(i.calls, r)
		if call.ResponseID != "" && len(i.seen) < cfg().limits.MaxCallsPerInteraction {
			i.seen[call.Provider+"\x00"+call.ResponseID] = r
		}
	} else {
		i.capLocked()
		i.foldCallLocked(r)
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
		i.resumeLocked()
		for _, call := range calls {
			name, server := normalizeTool(call.Name, call.MCPServer)
			var tool *recordedTool
			if call.ID != "" {
				tool = i.toolsByID[call.ID]
			}
			if tool == nil {
				if len(i.tools) >= cfg().limits.MaxCallsPerInteraction {
					// Counted in its tool_breakdown identity only: no declared event.
					i.foldToolLocked(&recordedTool{name: name, mcpServer: server, parentTool: call.ParentTool, agentName: call.AgentName, calls: 1})
					continue
				}
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
			mergeToolResult(tool, ToolResult{Error: call.Error, ErrorType: call.ErrorType, Duration: call.Duration, Result: call.Result}, call.Result != nil)
			fire = append(fire, tool)
		}
		i.unlock()
		i.fireToolEvents(fire, false)
	})
}

// RecordToolCallCount counts n tool calls without IDs or names.
func (i *Interaction) RecordToolCallCount(n int) {
	if n <= 0 {
		return
	}
	i.mu.Lock()
	i.resumeLocked()
	if len(i.tools) < cfg().limits.MaxCallsPerInteraction {
		i.tools = append(i.tools, &recordedTool{calls: n})
	} else {
		i.foldToolLocked(&recordedTool{calls: n})
	}
	i.unlock()
}

// RecordToolResult reports how a tool call recorded under id went. Results for IDs this
// interaction never recorded (earlier turns) are ignored.
func (i *Interaction) RecordToolResult(id string, result ToolResult) {
	i.recordToolResult(id, result, result.Result != nil)
}

func (i *Interaction) recordToolResult(id string, result ToolResult, hasResult bool) {
	safely(func() {
		if id == "" {
			return
		}
		i.mu.Lock()
		tool := i.toolsByID[id]
		if tool != nil {
			mergeToolResult(tool, result, hasResult)
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
func mergeToolResult(tool *recordedTool, r ToolResult, hasResult bool) {
	if r.Error != nil {
		failed := (tool.err != nil && *tool.err) || *r.Error
		tool.err = &failed
		tool.settled = true
		if *r.Error && tool.errorType == "" {
			tool.errorType = normalizeToolErrorType(r.ErrorType)
		}
	}
	if hasResult && !tool.hasResult {
		tool.result, tool.hasResult, tool.settled = r.Result, true, true
	}
	if tool.duration == nil && r.Duration != nil && *r.Duration >= 0 {
		d := *r.Duration
		tool.duration = &d
	}
}

// New optional server fields are merged over app extras only when the SDK sets them.
// Properties owned by the SDK: extra properties with these names are ignored.
var reservedProperties = set(
	"spec_version", "conversation_id", "interaction_id", "feature", "response_status", "trigger",
	"primary_model", "primary_provider", "latency_ms", "time_to_first_token_ms", "llm_call_count",
	"tool_call_count", "total_input_tokens", "total_output_tokens", "total_reasoning_tokens",
	"total_tokens", "total_cached_input_tokens", "total_cache_write_tokens", "total_cost_usd",
	"finish_reason", "error_type", "intent", "outcome", "usage_breakdown", "tool_breakdown",
	"response_text", "prompt_text", "input_method", "trace_id", "account_id", "on_behalf_of",
	"trigger_source", "task_name", "parent_interaction_id", "trigger_event_id", "source", "surface",
	"pause_reason", "client_disconnected", "attachment_count", "attachment_types", "end_reason",
	"pause_count", "total_paused_ms",
)

// customProperties are the extra properties without SDK-owned names.
func customProperties(extra map[string]any) map[string]any {
	props := map[string]any{}
	for k, v := range extra {
		if !reservedProperties[k] {
			props[k] = v
		}
	}
	return props
}

var toolEventReserved = func() map[string]bool {
	m := set("sample_rate", "agent_name", "tool_name", "tool_call_id", "tool_status", "tool_duration_ms")
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
		callback("ToolEventRule.When", func() error { send = rule.When(args, t.result); return nil })
		if !send {
			return
		}
	}
	custom := map[string]any{}
	if rule.Properties != nil {
		var out map[string]any
		if callback("ToolEventRule.Properties", func() error { out = rule.Properties(args, t.result); return nil }) {
			for k, v := range out {
				if !toolEventReserved[k] {
					custom[k] = v
				}
			}
		}
	}
	i.mu.Lock()
	p := i.baseLocked()
	p["trigger"] = string(i.trigger)
	put(p, "agent_name", t.agentName)
	p["tool_name"] = t.name
	put(p, "tool_call_id", t.id)
	if t.err != nil && *t.err && t.errorType == "denied" {
		p["tool_status"] = "denied"
	} else {
		p["tool_status"] = toolStatus(t.err)
	}
	put(p, "mcp_server", t.mcpServer)

	// An unknown outcome never reports a 0 duration.
	if t.duration != nil && (t.err != nil || *t.duration != 0) {
		p["tool_duration_ms"] = roundMS(*t.duration)
	}
	// SDK overrides are no longer custom properties and must survive size trimming.
	keys := make([]string, 0, len(custom))
	for k, v := range custom {
		if _, sdkOwned := p[k]; !sdkOwned {
			p[k] = v
			keys = append(keys, k)
		}
	}
	rates := cfg().sampleRates
	i.mu.Unlock()
	if t.hasResult {
		if text, ok := t.result.(string); ok {
			p["result_size_bytes"] = len(text)
		} else if encoded, ok := encodeNow(t.result); ok {
			p["result_size_bytes"] = len(encoded.(json.RawMessage))
		} else {
			report(KindUserCallback, "MarshalJSON", errUnencodable)
		}
	}

	p = fitToolEventProperties(rule.Event, jsonSafeMap(p), keys, rates)
	i.mu.Lock()
	defer i.unlock()
	i.sendLockedRates(rule.Event, func() map[string]any { return p }, rates, true)
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
	i.sendLockedRates(event, props, nil, false)
}

func (i *Interaction) sendLockedRates(event string, props func() map[string]any, rates map[string]float64, pinned bool) {
	i.pending = append(i.pending, pendingEvent{event, props, time.Now(), rates, pinned})
	if event != "ai_user_prompt_created" {
		i.settled = true
	}
	i.flushLocked()
}

// outgoing is an event built under the lock, sent after it is released.
type outgoing struct {
	client      analytics.Client
	event       string
	props       map[string]any
	integration string
	at          time.Time
	rates       map[string]float64
	pinned      bool
}

// flushLocked builds the pending events; unlock sends them.
func (i *Interaction) flushLocked() {
	if !i.settled || len(i.pending) == 0 {
		return
	}
	client := i.analytics()
	for _, e := range i.pending {
		i.outbox = append(i.outbox, outgoing{client, e.event, e.props(), i.integration, e.at, e.rates, e.pinned})
	}
	i.pending = nil
}

// unlock releases i.mu, then sends what flushLocked built: Enqueue blocks when the RudderStack
// client's queue is full, and that must hold up only the caller, never the interaction.
func (i *Interaction) unlock() {
	out := i.outbox
	i.outbox = nil
	capped := i.capPending
	i.capPending = false
	errors := i.buildErrors
	i.buildErrors = nil
	i.mu.Unlock()
	for _, e := range out {
		if e.pinned {
			emitAIRates(e.client, e.event, i.userID, i.anonymousID, e.props, i.msgContext, e.integration, e.at, e.rates)
			continue
		}
		emitAI(e.client, e.event, i.userID, i.anonymousID, e.props, i.msgContext, e.integration, e.at)
	}
	if capped {
		limitReached("MaxCallsPerInteraction")
	}
	for _, err := range errors {
		report(KindInternal, "interaction.build", err)
	}
}

// buildSafelyLocked contains a builder panic but leaves reporting to unlock. Even an
// internal failure must never call the application's error handler with i.mu held.
func (i *Interaction) buildSafelyLocked(fn func()) {
	defer func() {
		if value := recover(); value != nil {
			i.buildErrors = append(i.buildErrors, recovered(value))
		}
	}()
	fn()
}

func (i *Interaction) baseLocked() map[string]any {
	p := map[string]any{"conversation_id": i.conversationID, "interaction_id": i.id}
	put(p, "feature", i.feature)
	put(p, "surface", string(i.surface))
	put(p, "account_id", i.accountID)
	put(p, "on_behalf_of", i.onBehalfOf)
	put(p, "agent_version", i.agentVersion)
	put(p, "variant", i.variant)
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
	if c.credits != nil {
		p["credits"] = *c.credits
	}
	put(p, "call_purpose", c.callPurpose)
	if c.fallbackUsed != nil {
		p["fallback_used"] = *c.fallbackUsed
	}
	if c.byok != nil {
		p["byok"] = *c.byok
	}
	if c.latency != nil {
		p["latency_ms"] = roundMS(*c.latency)
	}
	if c.ttft != nil {
		p["time_to_first_token_ms"] = roundMS(*c.ttft)
	}
	put(p, "response_id", c.responseID)
	put(p, "finish_reason", c.finishReason)
	put(p, "error_type", c.errorType)
	return p
}

// A last call's finish_reason that ends the answer, as its error_type.
var finishErrors = map[string]string{"length": "length", "max_tokens": "length", "content_filter": "content_filter"}

// Pause pauses the run while it waits for something outside it (an approval, a person's answer):
// it sends ai_response_received with response_status paused and this segment's totals, then starts
// the next segment's totals from zero, so summing the segments counts each call once. It returns
// the snapshot to [Interaction.Resume] from, or to pass as [InteractionOptions.Resume] in another
// process; ok is false once the interaction ended or while pause finalization is in progress.
// Callers, including callbacks, can retry after finalization; Pause never waits on another pause.
// A paused run is never ended by the TTL or
// [Shutdown], and [RunInteraction] and [RunTask] leave it paused when fn returns nil. Pausing again
// before a resume sends nothing.
func (i *Interaction) Pause(o PauseOptions) (paused PausedInteraction, ok bool) {
	safely(func() { paused, ok = i.pause(o) })
	return paused, ok
}

func (i *Interaction) pause(o PauseOptions) (snapshot PausedInteraction, ok bool) {
	// Reentry must return even if the caller already holds mu. Delivery and deferred
	// error reporting remain part of this flight until all of its defers finish.
	if !i.pauseGuard.CompareAndSwap(false, true) {
		return PausedInteraction{}, false
	}
	defer i.pauseGuard.Store(false)
	i.mu.Lock()
	if i.ended || !i.pausedAt.IsZero() {
		defer i.mu.Unlock()
		return i.snapshotLocked()
	}
	i.pausing = true
	i.pauseCount++
	hooks := i.beforeEnd
	i.beforeEnd = nil
	unregister(i)
	i.mu.Unlock()
	defer func() {
		i.mu.Lock()
		// Include writes accepted while unlocked delivery was in progress.
		snapshot, ok = i.snapshotLocked()
		i.pausing = false
		i.mu.Unlock()
	}()
	i.checkDisconnected()
	for _, fn := range hooks {
		safely(fn)
	}
	var responseText string
	var responseOK bool
	if o.ResponseText != "" && i.captureContent {
		responseText, responseOK = redactText(o.ResponseText)
	}
	i.mu.Lock()
	// Drain tools added by redaction or tool callbacks before capturing totals.
	// Calls whose outcome never arrived are sent now, as unknown.
	for drained := 0; drained < len(i.tools); {
		tools := slices.Clone(i.tools[drained:])
		drained = len(i.tools)
		i.mu.Unlock()
		i.fireToolEvents(tools, true)
		i.mu.Lock()
	}
	defer i.unlock()
	if i.ended {
		snapshot, ok = i.snapshotLocked()
		return snapshot, ok
	}
	var props map[string]any
	i.buildSafelyLocked(func() {
		props = customProperties(o.ExtraProperties)
		maps.Copy(props, i.buildLocked(EndOptions{Status: StatusPaused, PauseReason: o.Reason}, responseText, responseOK, false))
	})
	// Finalization belongs to segment latency, not the subsequent paused interval.
	i.pausedAt = time.Now()
	if props != nil {
		i.sendLocked("ai_response_received", func() map[string]any { return props })
	}
	i.calls, i.overflow, i.callCount, i.lastCall, i.lastOK = nil, nil, 0, nil, nil
	for key := range i.seen {
		i.seen[key] = nil
	}
	i.tools, i.toolOverflow, i.toolsByID = nil, nil, map[string]*recordedTool{}
	i.hint, i.clientDisconnected, i.firstToken = nil, false, -1
	i.compactions = 0
	unregister(i)
	snapshot, ok = i.snapshotLocked()
	// The flight stays in progress until unlock finishes delivery and deferred reporting.
	return snapshot, ok
}

// checkDisconnected runs the request callback without holding the interaction lock.
func (i *Interaction) checkDisconnected() {
	i.mu.Lock()
	fn, known := i.disconnected, i.clientDisconnected
	i.mu.Unlock()
	if known || fn == nil {
		return
	}
	var disconnected bool
	safely(func() { disconnected = fn() })
	if disconnected {
		i.mu.Lock()
		i.clientDisconnected = true
		i.mu.Unlock()
	}
}

// Resume resumes a paused run in this process: the next segment starts now. A model or tool call
// recorded while paused resumes it too. It does nothing unless the run is paused.
func (i *Interaction) Resume() {
	if i.pauseGuard.Load() {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	i.resumeLocked()
}

func (i *Interaction) resumeLocked() {
	if i.pausedAt.IsZero() || i.ended || i.pausing {
		return
	}
	i.pausedTotal += max(time.Since(i.pausedAt), 0)
	i.pausedAt = time.Time{}
	i.started = time.Now()
	reopen(i)
}

// Paused reports whether the run is paused and not resumed or ended yet.
func (i *Interaction) Paused() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.pausing || !i.pausedAt.IsZero()
}

// snapshotLocked is the paused run's snapshot; not ok unless it is paused.
func (i *Interaction) snapshotLocked() (PausedInteraction, bool) {
	if i.ended || i.pausedAt.IsZero() {
		return PausedInteraction{}, false
	}
	snapshot := PausedInteraction{
		InteractionID: i.id, ConversationID: i.conversationID, Trigger: i.trigger,
		PauseCount: i.pauseCount, TotalPausedMS: roundMS(i.pausedTotal), PausedAt: i.pausedAt.UnixMilli(),
		UserID: i.userID, AnonymousID: i.anonymousID, Feature: i.feature, Surface: i.surface,
		AccountID: i.accountID, OnBehalfOf: i.onBehalfOf, TriggerSource: i.triggerSource, TaskName: i.taskName,
		AgentVersion: i.agentVersion, Variant: i.variant, Confidence: i.confidence, CompactionCount: i.compactions,
		TriggerEventID: i.triggerEventID, ParentInteractionID: i.parent, Intent: i.intent, TraceID: i.traceID,
		Outcome: i.outcome, Integration: i.integration,
	}
	for key := range i.seen {
		provider, responseID, _ := strings.Cut(key, "\x00")
		snapshot.SeenResponseIDs = append(snapshot.SeenResponseIDs, [2]string{provider, responseID})
	}
	slices.SortFunc(snapshot.SeenResponseIDs, func(a, b [2]string) int {
		if n := compareUTF16(a[0], b[0]); n != 0 {
			return n
		}
		return compareUTF16(a[1], b[1])
	})
	calls := append(slices.Clone(i.calls), i.overflow...)
	for _, c := range calls {
		p := pendingCall(c)
		if c == i.lastCall {
			p["last"] = true
		}
		if c == i.lastOK {
			p["last_ok"] = true
		}
		snapshot.PendingCalls = append(snapshot.PendingCalls, p)
	}
	for _, role := range []struct {
		c   *recordedCall
		key string
	}{{i.lastCall, "last"}, {i.lastOK, "last_ok"}} {
		if role.c != nil && !slices.Contains(calls, role.c) {
			p := pendingCall(role.c)
			p["history_only"], p[role.key] = true, true
			snapshot.PendingCalls = append(snapshot.PendingCalls, p)
		}
	}
	for _, t := range append(slices.Clone(i.tools), i.toolOverflow...) {
		snapshot.PendingTools = append(snapshot.PendingTools, pendingTool(t))
	}
	return snapshot, true
}

// Snapshot records contain only usage and tool metadata; no content or callback objects.
func pendingNumber(p map[string]any, key string) float64 {
	var n float64
	switch v := p[key].(type) {
	case int:
		n = float64(v)
	case int64:
		n = float64(v)
	case float64:
		n = v
	}
	if math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n > 9007199254740991 {
		return 0
	}
	return n
}
func pendingText(p map[string]any, key string) string { s, _ := p[key].(string); return s }
func pendingBool(p map[string]any, key string) bool   { b, _ := p[key].(bool); return b }

func pendingCall(c *recordedCall) map[string]any {
	p := map[string]any{}
	if c.credits != nil {
		p["credits"] = *c.credits
	}
	if c.uncredited {
		p["uncredited"] = true
	}
	put(p, "call_purpose", c.callPurpose)
	if c.fallbackUsed != nil {
		p["fallback_used"] = *c.fallbackUsed
	}
	if c.byok != nil {
		p["byok"] = *c.byok
	}
	if c.latency != nil {
		p["latency_ms"] = float64(*c.latency) / float64(time.Millisecond)
	}
	if c.ttft != nil {
		p["time_to_first_token_ms"] = float64(*c.ttft) / float64(time.Millisecond)
	}
	if c.unpriced {
		p["unpriced"] = true
	}
	if c.feesUnknown {
		p["fees_unknown"] = true
	}
	if c.provider != "" {
		p["provider"] = c.provider
	}
	if c.model != "" {
		p["model"] = c.model
	}
	if c.requestedModel != "" {
		p["requested_model"] = c.requestedModel
	}
	if c.agentName != "" {
		p["agent_name"] = c.agentName
	}
	p["call_count"] = c.callCount
	p["error_count"] = c.errorCount
	p["input_tokens"] = c.inputTokens
	p["output_tokens"] = c.outputTokens
	if c.reasoningTokens != nil {
		p["reasoning_tokens"] = *c.reasoningTokens
	}
	p["cached_input_tokens"] = c.cachedInputTokens
	p["cache_write_tokens"] = c.cacheWriteTokens
	p["cache_write_1h_tokens"] = c.cacheWrite1
	if c.costUSD != nil {
		p["cost_usd"] = *c.costUSD
	}
	if c.finishReason != "" {
		p["finish_reason"] = c.finishReason
	}
	if c.errorType != "" {
		p["error_type"] = c.errorType
	}
	if c.responseID != "" {
		p["response_id"] = c.responseID
	}
	if c.serviceTier != "" {
		p["service_tier"] = c.serviceTier
	}
	p["server_tool_call_count"] = c.serverToolCallCount
	if c.serverToolCostUSD != nil {
		p["server_tool_cost_usd"] = *c.serverToolCostUSD
	}
	return p
}

func pendingTool(c *recordedTool) map[string]any {
	p := map[string]any{}
	if c.name != "" {
		p["tool_name"] = c.name
	}
	if c.rawName != "" {
		p["raw_name"] = c.rawName
	}
	if c.id != "" {
		p["tool_call_id"] = c.id
	}
	if c.agentName != "" {
		p["agent_name"] = c.agentName
	}
	if c.mcpServer != "" {
		p["mcp_server"] = c.mcpServer
	}
	if c.parentTool != "" {
		p["parent_tool"] = c.parentTool
	}
	p["call_count"] = c.calls
	if c.err != nil {
		p["error"] = *c.err
	}
	if c.errorType != "" {
		p["error_type"] = c.errorType
	}
	if c.duration != nil {
		p["duration_ms"] = float64(*c.duration) / float64(time.Millisecond)
	}
	p["settled"] = c.settled
	p["fired"] = c.fired
	return p
}

func (i *Interaction) restorePending(snapshot *PausedInteraction) {
	for _, p := range snapshot.PendingCalls {
		c := &recordedCall{}
		if p["credits"] != nil {
			c.credits = Ptr(pendingNumber(p, "credits"))
		}
		c.uncredited = pendingBool(p, "uncredited")
		c.callPurpose = pendingText(p, "call_purpose")
		if v, ok := p["fallback_used"].(bool); ok {
			c.fallbackUsed = Ptr(v)
		}
		if v, ok := p["byok"].(bool); ok {
			c.byok = Ptr(v)
		}
		if p["latency_ms"] != nil {
			c.latency = Ptr(time.Duration(pendingNumber(p, "latency_ms") * float64(time.Millisecond)))
		}
		if p["time_to_first_token_ms"] != nil {
			c.ttft = Ptr(time.Duration(pendingNumber(p, "time_to_first_token_ms") * float64(time.Millisecond)))
		}
		c.unpriced = pendingBool(p, "unpriced")
		c.feesUnknown = pendingBool(p, "fees_unknown")
		c.provider = pendingText(p, "provider")
		c.model = pendingText(p, "model")
		c.requestedModel = pendingText(p, "requested_model")
		c.agentName = pendingText(p, "agent_name")
		c.callCount = int(pendingNumber(p, "call_count"))
		c.errorCount = int(pendingNumber(p, "error_count"))
		c.inputTokens = int(pendingNumber(p, "input_tokens"))
		c.outputTokens = int(pendingNumber(p, "output_tokens"))
		if p["reasoning_tokens"] != nil {
			c.reasoningTokens = Ptr(int(pendingNumber(p, "reasoning_tokens")))
		}
		c.cachedInputTokens = int(pendingNumber(p, "cached_input_tokens"))
		c.cacheWriteTokens = int(pendingNumber(p, "cache_write_tokens"))
		c.cacheWrite1 = int(pendingNumber(p, "cache_write_1h_tokens"))
		if p["cost_usd"] != nil {
			c.costUSD = Ptr(pendingNumber(p, "cost_usd"))
		}
		c.finishReason = pendingText(p, "finish_reason")
		c.errorType = pendingText(p, "error_type")
		c.responseID = pendingText(p, "response_id")
		c.serviceTier = pendingText(p, "service_tier")
		c.serverToolCallCount = int(pendingNumber(p, "server_tool_call_count"))
		if p["server_tool_cost_usd"] != nil {
			c.serverToolCostUSD = Ptr(pendingNumber(p, "server_tool_cost_usd"))
		}
		c.callCount = max(c.callCount, 1)
		if !pendingBool(p, "history_only") {
			if key := c.provider + "\x00" + c.responseID; c.responseID != "" {
				if _, seen := i.seen[key]; seen {
					i.seen[key] = c
				}
			}
			i.calls = append(i.calls, c)
			i.callCount += c.callCount
		}
		if pendingBool(p, "last") {
			i.lastCall = c
		}
		if pendingBool(p, "last_ok") {
			i.lastOK = c
		}
	}
	for _, p := range snapshot.PendingTools {
		c := &recordedTool{}
		c.name = pendingText(p, "tool_name")
		c.rawName = pendingText(p, "raw_name")
		c.id = pendingText(p, "tool_call_id")
		c.agentName = pendingText(p, "agent_name")
		c.mcpServer = pendingText(p, "mcp_server")
		c.parentTool = pendingText(p, "parent_tool")
		c.calls = int(pendingNumber(p, "call_count"))
		if v, ok := p["error"].(bool); ok {
			c.err = Ptr(v)
		}
		c.errorType = pendingText(p, "error_type")
		if p["duration_ms"] != nil {
			c.duration = Ptr(time.Duration(pendingNumber(p, "duration_ms") * float64(time.Millisecond)))
		}
		c.settled = pendingBool(p, "settled")
		c.fired = pendingBool(p, "fired")
		c.calls = max(c.calls, 1)
		i.tools = append(i.tools, c)
		if c.id != "" {
			i.toolsByID[c.id] = c
		}
	}
}

// taskStarted sends ai_task_started when it is switched on and the run did not resume.
func (i *Interaction) taskStarted() {
	if !cfg().emitTaskStarted || i.resumed {
		return
	}
	i.mu.Lock()
	defer i.unlock()
	if !i.ended {
		i.sendLocked("ai_task_started", i.baseLocked)
	}
}

// End sends the segment response. A paused end can resume; a final end is idempotent.
func (i *Interaction) End(o EndOptions) {
	safely(func() { i.end(o) })
}

func (i *Interaction) end(o EndOptions) {
	if i.pauseGuard.Load() {
		return
	}
	i.mu.Lock()
	if i.ended || i.pausing || (o.endReason != "" && !i.pausedAt.IsZero()) {
		i.mu.Unlock()
		return
	}
	pausingHint := o.Status == "" && i.hint != nil && i.hint.calls == i.callCount && i.hint.status == StatusPaused
	if o.endReason == "" && (o.Status == StatusPaused || pausingHint) {
		if o.ClientDisconnected {
			i.clientDisconnected = true
		}
		i.mu.Unlock()
		i.pause(PauseOptions{Reason: o.PauseReason, ResponseText: o.ResponseText, ExtraProperties: o.ExtraProperties})
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
	if i.ended || i.pausing || (o.endReason != "" && !i.pausedAt.IsZero()) {
		i.mu.Unlock()
		return
	}
	if o.ClientDisconnected {
		i.clientDisconnected = true
	}
	hintedPause := o.Status == "" && i.hint != nil && i.hint.calls == i.callCount && i.hint.status == StatusPaused
	if o.endReason == "" && (o.Status == StatusPaused || hintedPause) {
		i.mu.Unlock()
		i.pause(PauseOptions{Reason: o.PauseReason, ResponseText: o.ResponseText, ExtraProperties: o.ExtraProperties})
		return
	}
	i.ended = true
	i.settled = true
	// Ended while paused: the wait counts as paused time and the final segment starts now.
	if !i.pausedAt.IsZero() {
		i.pausedTotal += max(time.Since(i.pausedAt), 0)
		i.pausedAt = time.Time{}
		i.started = time.Now()
	}
	i.mu.Unlock()
	i.checkDisconnected()
	// Mask once after the paused branch, outside the lock.
	var responseText string
	var responseOK bool
	if o.ResponseText != "" && i.captureContent {
		responseText, responseOK = redactText(o.ResponseText)
	}
	i.mu.Lock()
	var props map[string]any
	i.buildSafelyLocked(func() {
		props = customProperties(o.ExtraProperties)
		maps.Copy(props, i.buildLocked(o, responseText, responseOK, true))
	})
	if props != nil {
		i.sendLocked("ai_response_received", func() map[string]any { return props })
	}
	answer := first(o.ResponseText, i.answer)
	snap := turnSnapshot{prompt: i.prompt, answer: answer, messages: i.messages}
	for _, t := range append(slices.Clone(i.tools), i.toolOverflow...) {
		if t.name != "" {
			snap.toolNames = append(snap.toolNames, t.name)
		}
	}
	i.prompt, i.answer, i.messages = "", "", nil
	i.unlock()
	unregister(i)
	sweepExpired()

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
	defer i.unlock()
	p := i.baseLocked()
	p["trigger"] = string(i.trigger)
	for k, v := range extra {
		p[k] = v
	}
	i.sendLocked(event, func() map[string]any { return p })
}

// waitBackground waits for this interaction's classification and scores until ctx ends; false
// when they did not finish.
func (i *Interaction) waitBackground(ctx context.Context) bool {
	i.mu.Lock()
	ch := i.background
	i.mu.Unlock()
	if ch == nil {
		return true
	}
	select {
	case <-ch:
		return true
	case <-ctx.Done():
		return false
	}
}

func (i *Interaction) buildLocked(o EndOptions, responseText string, responseOK, final bool) map[string]any {
	calls := append(slices.Clone(i.calls), i.overflow...)
	last, lastOK := i.lastCall, i.lastOK
	status, errorType, pauseReason := o.Status, o.ErrorType, o.PauseReason
	if status == "" && i.hint != nil && i.hint.calls == i.callCount {
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
	if status == StatusBlocked {
		reason := o.BlockReason
		if reason != "" && !blockReasons[reason] {
			warnOnce("invalid-block-reason", fmt.Sprintf("invalid block reason %q", reason), 0)
			reason = ""
		}
		p["block_reason"] = string(cmp.Or(reason, BlockOther))
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
	allTools := append(slices.Clone(i.tools), i.toolOverflow...)
	tools := 0
	for _, t := range allTools {
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
	if status != StatusSuccess && status != StatusBlocked {
		put(p, "error_type", errorType)
	}
	put(p, "intent", i.intent)
	put(p, "outcome", i.outcome)
	put(p, "confidence", string(i.confidence))
	if i.compactions > 0 {
		p["compaction_count"] = i.compactions
	}
	if credits, ok := creditedTotal(calls); ok {
		p["total_credits"] = credits
	}
	if b := usageBreakdown(calls); len(b) > 0 {
		p["usage_breakdown"] = b
	}
	if i.toolBreakdown && len(allTools) > 0 {
		p["tool_breakdown"] = toolBreakdown(allTools)
	}
	if responseOK {
		p["response_text"] = responseText
	}
	put(p, "trace_id", i.traceID)
	put(p, "end_reason", o.endReason)
	if final && i.pauseCount > 0 {
		p["pause_count"] = i.pauseCount
		p["total_paused_ms"] = roundMS(i.pausedTotal)
	}
	return p
}

// pricedTotal sums the known costs; not ok when no call was priced or a successful call was not.
func pricedTotal(calls []*recordedCall) (float64, bool) {
	priced := false
	total := 0.0
	for _, c := range calls {
		if c.unpriced {
			return 0, false
		}
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
		key, _ := json.Marshal([]string{c.agentName, c.callPurpose, c.provider, c.model, c.requestedModel, c.serviceTier})
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
		put(e, "call_purpose", g.first.callPurpose)
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
				if c.serverToolCostUSD == nil || c.feesUnknown {
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
		if credits, ok := creditedTotal(g.calls); ok {
			e["credits"] = credits
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
	defer i.unlock()
	if i.adapterAnalytics == nil {
		i.adapterAnalytics = c
		i.flushLocked()
	}
}

// callStarted: a model call of this interaction starts; the held prompt event goes out now.
func (i *Interaction) callStarted(ctx context.Context, integration string) {
	i.mu.Lock()
	if i.ended {
		i.mu.Unlock()
		return
	}
	i.resumeLocked()
	if i.integration == "" {
		i.integration = integration
	}
	i.mu.Unlock()
	i.fillTraceID(ctx)
	i.mu.Lock()
	defer i.unlock()
	if i.ended {
		return
	}
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
	if active != nil && active.untracked {
		return nil, false
	}
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
	safely(func() {
		sweepExpired()
		if i = newInteraction(ctx, o); !register(i) {
			i = nil
		}
	})
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
	i.hint = &outcomeHint{status: status, errorType: errorType, calls: i.callCount}
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

// SetConfidence sets the app's confidence; invalid levels are ignored with a warning.
func (i *Interaction) SetConfidence(level Confidence) {
	if level != "" && !confidences[level] {
		warnOnce("invalid-confidence", fmt.Sprintf("invalid confidence %q", level), 0)
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	i.confidence = level
}

// RecordCompaction counts context compactions; with no argument it records one.
func (i *Interaction) RecordCompaction(count ...int) {
	n := 1
	if len(count) > 0 {
		n = nonNegative(count[0])
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if !i.ended {
		i.compactions += n
	}
}

func creditedTotal(calls []*recordedCall) (float64, bool) {
	known := false
	total := 0.0
	for _, call := range calls {
		if call.uncredited || (call.errorType == "" && call.credits == nil) {
			return 0, false
		}
		if call.credits != nil {
			known = true
			total += *call.credits
		}
	}
	return round8(total), known
}
