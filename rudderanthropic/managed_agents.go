package rudderanthropic

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"time"

	analytics "github.com/rudderlabs/analytics-go/v4"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/kit"
)

// managedAgentsIntegration tags events from Claude Managed Agents sessions.
const managedAgentsIntegration = "claude-managed-agents"

// grader is the agent_name of outcome grader cycles.
const grader = "outcome_grader"

// SessionEvent is a Claude Managed Agents session event as the Anthropic SDK returns it:
// anthropic.BetaManagedAgentsStreamSessionEventsUnion from Beta.Sessions.Events.StreamEvents, or
// anthropic.BetaManagedAgentsSessionEventUnion from Beta.Sessions.Events.ListAutoPaging.
type SessionEvent interface{ RawJSON() string }

// SessionEventStream is the iterator [TrackSessionEvents] reads: the *ssestream.Stream that
// StreamEvents returns, or the pager ListAutoPaging returns.
type SessionEventStream[T SessionEvent] interface {
	Next() bool
	Current() T
	Err() error
}

// SessionOption configures [TrackSessionEvents].
type SessionOption func(*sessionConfig)

type sessionConfig struct {
	sessionID, model, agentName string
	interaction                 rudderstackai.InteractionOptions
	analytics                   analytics.Client
}

// WithSession names the session the events belong to: the anthropic.BetaManagedAgentsSession that
// Beta.Sessions.New or Get returned. Its ID is the default conversation ID and resumes a paused
// turn; its agent's model prices the calls.
func WithSession(session interface{ RawJSON() string }) SessionOption {
	return func(c *sessionConfig) {
		var s struct {
			ID    string `json:"id"`
			Agent struct {
				Model json.RawMessage `json:"model"`
			} `json:"agent"`
		}
		if session == nil || json.Unmarshal([]byte(session.RawJSON()), &s) != nil {
			return
		}
		c.sessionID = s.ID
		var model struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(s.Agent.Model, &c.model) != nil && json.Unmarshal(s.Agent.Model, &model) == nil {
			c.model = model.ID
		}
	}
}

// WithSessionID names the session without its object: the conversation ID and pause key.
func WithSessionID(id string) SessionOption { return func(c *sessionConfig) { c.sessionID = id } }

// WithSessionModel sets the model of the session's agent; it wins over [WithSession]'s.
func WithSessionModel(model string) SessionOption {
	return func(c *sessionConfig) { c.model = model }
}

// WithAgentName sets the agent_name of the session's calls and tools.
func WithAgentName(name string) SessionOption { return func(c *sessionConfig) { c.agentName = name } }

// WithInteractionOptions sets the identity and fields of the interactions the session's turns open.
func WithInteractionOptions(o rudderstackai.InteractionOptions) SessionOption {
	return func(c *sessionConfig) { c.interaction = o }
}

// WithSessionAnalytics sets the RudderStack client used when no interaction or context names one.
func WithSessionAnalytics(client analytics.Client) SessionOption {
	return func(c *sessionConfig) { c.analytics = client }
}

// SessionEvents passes a session's events through unchanged while it tracks them. Use it as
// the stream it wraps; Close also closes that stream when it can be closed.
type SessionEvents[T SessionEvent] struct {
	src     SessionEventStream[T]
	tracker *sessionTracker
	once    sync.Once
}

// TrackSessionEvents reads a Claude Managed Agents session's events and sends one
// ai_user_prompt_created and one ai_response_received per turn (a user.message until the session
// goes idle for any reason but requires_action). Each span.model_request_end is one model call;
// built-in, MCP and custom tool calls are counted with their outcomes.
//
// Pass the session with [WithSession]: its agent's model prices the calls and its ID is the
// conversation ID. With an interaction in ctx it reports into that one. A stream that ends while
// the session waits on the app (requires_action) pauses the turn; a later stream of the same
// session in this process resumes it. Only the primary thread is recorded: subagent and workflow
// threads report their calls on their own streams.
//
//	stream := client.Beta.Sessions.Events.StreamEvents(ctx, session.ID, anthropic.BetaSessionEventStreamParams{})
//	events := rudderanthropic.TrackSessionEvents(ctx, stream, rudderanthropic.WithSession(session))
//	defer events.Close()
//	for events.Next() {
//		event := events.Current() // the same events as stream
//	}
func TrackSessionEvents[T SessionEvent](ctx context.Context, stream SessionEventStream[T], opts ...SessionOption) *SessionEvents[T] {
	c := sessionConfig{}
	for _, o := range opts {
		if o != nil {
			o(&c)
		}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return &SessionEvents[T]{src: stream, tracker: &sessionTracker{ctx: ctx, cfg: c, seen: newBoundedSet("claude-managed-agents events")}}
}

// Next advances to the next event; false when the stream ended or failed (see Err).
func (s *SessionEvents[T]) Next() bool {
	if s.src.Next() {
		event := s.src.Current()
		contain(func() { s.tracker.observe(event.RawJSON()) })
		return true
	}
	s.finish(s.src.Err())
	return false
}

// Current is the current event, unchanged.
func (s *SessionEvents[T]) Current() T { return s.src.Current() }

// Err is the stream's error, unchanged.
func (s *SessionEvents[T]) Err() error { return s.src.Err() }

// Close ends tracking (a turn still open is paused or cancelled) and closes the stream when it
// has a Close method.
func (s *SessionEvents[T]) Close() error {
	s.finish(nil)
	if closer, ok := s.src.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

func (s *SessionEvents[T]) finish(err error) {
	s.once.Do(func() { contain(func() { s.tracker.close(err) }) })
}

// contain keeps tracking failures away from the app.
func contain(fn func()) {
	defer func() { _ = recover() }()
	fn()
}

// sessionErrors maps session.error types to spec error types; any other is api_error.
var sessionErrors = map[string]string{
	"model_rate_limited_error":          "rate_limit",
	"billing_error":                     "rate_limit",
	"model_overloaded_error":            "api_error",
	"model_request_failed_error":        "api_error",
	"mcp_authentication_failed_error":   "auth_error",
	"repository_authentication_error":   "auth_error",
	"repository_forbidden_error":        "auth_error",
	"mcp_connection_failed_error":       "connection_error",
	"credential_host_unreachable_error": "connection_error",
	"repository_not_found_error":        "invalid_request",
	"repository_checkout_error":         "invalid_request",
	"repository_clone_error":            "invalid_request",
}

// settleEvents settle work already in progress: a turn they open continues an earlier one.
var settleEvents = map[string]bool{
	"user.custom_tool_result": true, "user.tool_confirmation": true, "user.tool_result": true, "user.interrupt": true,
}

// pausedTurns are turns paused at requires_action, by session ID: a later stream resumes them.
var pausedTurns = struct {
	sync.Mutex
	m     map[string]*rudderstackai.Interaction
	order []string
}{m: map[string]*rudderstackai.Interaction{}}

// msTime is a server time in whole epoch milliseconds, as JavaScript's Date.parse reads it.
type msTime struct {
	v  int64
	ok bool
}

func timeOf(e map[string]any) msTime {
	s, _ := e["processed_at"].(string)
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return msTime{}
	}
	return msTime{t.UnixMilli(), true}
}

type turnCall struct {
	usage          map[string]any
	model, agent   string
	serviceTier    string
	errorType      string
	outputText     string
	startAt, endAt msTime
}

type turnTool struct {
	id, name, mcpServer string
	args, result        any
	at                  msTime
	err                 *bool
	errorType           string
	duration            *time.Duration
	settled             bool
}

type turn struct {
	startAt, lastAt msTime
	continuation    bool
	prompted        bool
	promptText      string
	starts          map[string]msTime
	modelError      string
	requestText     string
	calls           []turnCall
	tools           []*turnTool
	toolsByID       map[string]*turnTool
	blocking        map[string]rudderstackai.PauseReason
	answer          string
	interrupted     bool
	errorType       string
	waiting         rudderstackai.PauseReason
	compactions     int
	resumed         *rudderstackai.Interaction
}

type turnEnd struct {
	status      rudderstackai.ResponseStatus
	errorType   string
	pauseReason rudderstackai.PauseReason
}

type sessionTracker struct {
	ctx          context.Context
	cfg          sessionConfig
	seen         *boundedSet
	turn         *turn
	runningAt    msTime
	lastError    string
	conversation string
}

func str(v any) string { s, _ := v.(string); return s }

func obj(v any) map[string]any { m, _ := v.(map[string]any); return m }

func num(v any) int {
	if f, ok := v.(float64); ok {
		return int(f)
	}
	return 0
}

// textOf joins the text blocks of event content.
func textOf(content any) string {
	list, _ := content.([]any)
	var b strings.Builder
	for _, block := range list {
		if m := obj(block); m != nil && m["type"] == "text" {
			if t, ok := m["text"].(string); ok {
				b.WriteString(t)
			}
		}
	}
	return b.String()
}

func (s *sessionTracker) observe(raw string) {
	var e map[string]any
	if json.Unmarshal([]byte(raw), &e) != nil {
		return
	}
	kind := str(e["type"])
	if kind == "" {
		return
	}
	id := str(e["id"])
	if id != "" {
		// The consolidation pattern (stream + history) repeats events.
		if s.seen.has(id) {
			return
		}
		s.seen.add(id)
	}
	at := timeOf(e)
	if str(e["session_thread_id"]) != "" && strings.HasPrefix(kind, "agent.") {
		// Cross-posted from a subagent thread: not this thread's work.
		if s.turn != nil && id != "" {
			if kind == "agent.custom_tool_use" {
				s.turn.blocking[id] = rudderstackai.PauseDeferredTool
			} else if e["evaluated_permission"] == "ask" {
				s.turn.blocking[id] = rudderstackai.PauseApproval
			}
		}
		return
	}
	switch kind {
	case "session.status_running":
		if s.turn == nil {
			s.runningAt = at
		}
	case "user.message", "user.define_outcome":
		t := s.open(kind, at)
		if !t.prompted {
			t.prompted = true
			if kind == "user.message" {
				t.promptText = textOf(e["content"])
			} else {
				t.promptText = str(e["description"])
			}
		}
	case "user.custom_tool_result":
		s.settle(s.open(kind, at), str(e["custom_tool_use_id"]), e, at)
	case "user.tool_result":
		s.settle(s.open(kind, at), str(e["tool_use_id"]), e, at)
	case "user.tool_confirmation":
		t := s.open(kind, at)
		if tool := t.toolsByID[str(e["tool_use_id"])]; tool != nil && e["result"] == "deny" {
			tool.err, tool.errorType = rudderstackai.Ptr(true), "denied"
		}
	case "user.interrupt":
		s.open(kind, at).interrupted = true
	case "span.model_request_start":
		t := s.open(kind, at)
		if id != "" {
			t.starts[id] = at
		}
		t.modelError, t.requestText = "", ""
	case "span.model_request_end":
		s.call(s.open(kind, at), e, at)
	case "span.outcome_evaluation_end":
		t := s.open(kind, at)
		t.calls = append(t.calls, turnCall{usage: obj(e["usage"]), agent: grader, endAt: at})
	case "agent.message":
		t := s.open(kind, at)
		if text := textOf(e["content"]); text != "" {
			t.answer = text
			t.requestText += text
		}
	case "agent.tool_use", "agent.mcp_tool_use", "agent.custom_tool_use":
		s.tool(s.open(kind, at), kind, e, id, at)
	case "agent.tool_result":
		s.settle(s.open(kind, at), str(e["tool_use_id"]), e, at)
	case "agent.mcp_tool_result":
		s.settle(s.open(kind, at), str(e["mcp_tool_use_id"]), e, at)
	case "agent.thread_context_compacted":
		s.open(kind, at).compactions++
	case "session.error":
		s.sessionError(e)
	case "session.status_idle":
		s.idle(e, at)
	case "session.status_terminated":
		if s.turn != nil {
			if s.turn.errorType != "" {
				s.finish(turnEnd{status: rudderstackai.StatusError, errorType: s.turn.errorType}, at)
			} else {
				s.finish(turnEnd{status: rudderstackai.StatusSuccess}, at)
			}
		}
	case "session.deleted":
		if s.turn != nil {
			s.finish(turnEnd{status: rudderstackai.StatusCancelled, errorType: "cancelled"}, at)
		}
	}
}

// close: the stream ended or failed; a turn still open is paused, failed or cancelled.
func (s *sessionTracker) close(err error) {
	t := s.turn
	if t == nil {
		return
	}
	switch {
	case err != nil:
		errorType := rudderstackai.ErrorTypeOf(err)
		status := rudderstackai.StatusError
		if errorType == "cancelled" {
			status = rudderstackai.StatusCancelled
		}
		s.finish(turnEnd{status: status, errorType: errorType}, msTime{})
	case t.waiting != "":
		s.finish(turnEnd{status: rudderstackai.StatusPaused, pauseReason: t.waiting}, msTime{})
	default:
		s.finish(turnEnd{status: rudderstackai.StatusCancelled, errorType: "cancelled"}, msTime{})
	}
}

func (s *sessionTracker) open(kind string, at msTime) *turn {
	t := s.turn
	if t == nil {
		start := at
		if s.runningAt.ok && at.ok && s.runningAt.v < at.v {
			start = s.runningAt
		}
		t = &turn{
			startAt: start, continuation: settleEvents[kind], starts: map[string]msTime{},
			toolsByID: map[string]*turnTool{}, blocking: map[string]rudderstackai.PauseReason{},
		}
		s.turn, s.runningAt = t, msTime{}
		if t.continuation {
			t.resumed = s.resume()
		}
	}
	// Any event after a requires_action idle means the app answered.
	t.waiting = ""
	if at.ok {
		t.lastAt = at
	}
	return t
}

// resume resumes this session's paused answer now, so its paused time and latency are right.
func (s *sessionTracker) resume() *rudderstackai.Interaction {
	if active := rudderstackai.FromContext(s.ctx); (active != nil && !active.Ended()) || s.cfg.sessionID == "" {
		return nil
	}
	pausedTurns.Lock()
	paused := pausedTurns.m[s.cfg.sessionID]
	delete(pausedTurns.m, s.cfg.sessionID)
	pausedTurns.Unlock()
	if paused == nil || paused.Ended() {
		return nil
	}
	paused.Resume()
	return paused
}

func (s *sessionTracker) call(t *turn, e map[string]any, at msTime) {
	usage := obj(e["model_usage"])
	failed := e["is_error"] == true
	startID := str(e["model_request_start_id"])
	if s.cfg.model == "" {
		kit.WarnOnce("claude-managed-agents-model", "claude-managed-agents: pass WithSession or WithSessionModel to TrackSessionEvents; calls are unpriced without a model")
	}
	c := turnCall{usage: usage, model: s.cfg.model, agent: s.cfg.agentName, outputText: t.requestText, endAt: at}
	if speed := str(usage["speed"]); speed == "fast" || speed == "standard" {
		c.serviceTier = speed
	}
	if failed {
		c.errorType = t.modelError
		if c.errorType == "" {
			c.errorType = "api_error"
		}
	}
	if startID != "" {
		c.startAt = t.starts[startID]
		delete(t.starts, startID)
	}
	t.calls = append(t.calls, c)
	t.modelError, t.requestText = "", ""
}

func (s *sessionTracker) tool(t *turn, kind string, e map[string]any, id string, at msTime) {
	tool := &turnTool{id: id, name: str(e["name"]), args: e["input"], at: at}
	if kind == "agent.mcp_tool_use" {
		tool.mcpServer = str(e["mcp_server_name"])
	}
	if e["evaluated_permission"] == "deny" {
		tool.err, tool.errorType = rudderstackai.Ptr(true), "denied"
	}
	t.tools = append(t.tools, tool)
	if id == "" {
		return
	}
	t.toolsByID[id] = tool
	if kind == "agent.custom_tool_use" {
		t.blocking[id] = rudderstackai.PauseDeferredTool
	} else if e["evaluated_permission"] == "ask" {
		t.blocking[id] = rudderstackai.PauseApproval
	}
}

func (s *sessionTracker) settle(t *turn, id string, e map[string]any, at msTime) {
	tool := t.toolsByID[id]
	if tool == nil || tool.settled {
		return
	}
	tool.settled, tool.result = true, e["content"]
	if tool.errorType != "denied" {
		tool.err = rudderstackai.Ptr(e["is_error"] == true)
	}
	if tool.at.ok && at.ok {
		d := time.Duration(max(0, at.v-tool.at.v)) * time.Millisecond
		tool.duration = &d
	}
}

func (s *sessionTracker) sessionError(e map[string]any) {
	errObj := obj(e["error"])
	kind := str(errObj["type"])
	errorType, ok := sessionErrors[kind]
	if !ok {
		errorType = "api_error"
	}
	t := s.turn
	if t != nil && strings.HasPrefix(kind, "model_") && len(t.starts) > 0 {
		t.modelError = errorType
	}
	// A retried error is not the turn's outcome unless it recurs as exhausted or terminal.
	if str(obj(errObj["retry_status"])["type"]) == "retrying" {
		return
	}
	s.lastError = errorType
	if t != nil {
		t.errorType = errorType
	}
}

func (s *sessionTracker) idle(e map[string]any, at msTime) {
	t := s.turn
	if t == nil {
		return
	}
	stop := obj(e["stop_reason"])
	switch str(stop["type"]) {
	case "requires_action":
		ids, _ := stop["event_ids"].([]any)
		t.waiting = rudderstackai.PauseOther
		found := false
		for _, id := range ids {
			reason, ok := t.blocking[str(id)]
			if !ok {
				continue
			}
			if reason == rudderstackai.PauseApproval {
				t.waiting = reason
				break
			}
			if !found {
				t.waiting, found = reason, true
			}
		}
		if at.ok {
			t.lastAt = at
		}
	case "refusal":
		s.finish(turnEnd{status: rudderstackai.StatusError, errorType: "content_filter"}, at)
	case "retries_exhausted":
		errorType := t.errorType
		if errorType == "" {
			errorType = s.lastError
		}
		if errorType == "" {
			errorType = "api_error"
		}
		s.finish(turnEnd{status: rudderstackai.StatusError, errorType: errorType}, at)
	case "budget_reached":
		s.finish(turnEnd{status: rudderstackai.StatusIncomplete, errorType: "max_budget_usd"}, at)
	default:
		if t.interrupted {
			s.finish(turnEnd{status: rudderstackai.StatusCancelled, errorType: "cancelled"}, at)
		} else {
			s.finish(turnEnd{status: rudderstackai.StatusSuccess}, at)
		}
	}
}

func (s *sessionTracker) finish(end turnEnd, at msTime) {
	t := s.turn
	s.turn, s.runningAt = nil, msTime{}
	if t == nil {
		return
	}
	if !at.ok {
		at = t.lastAt
	}
	contain(func() { s.record(t, end, at) })
}

// record reports the turn's calls and tools and ends (or pauses) its answer.
func (s *sessionTracker) record(t *turn, end turnEnd, endAt msTime) {
	// Server times map onto the local clock so that the turn ends now.
	now := time.Now()
	local := func(m msTime) time.Time {
		if !m.ok || !endAt.ok {
			return time.Time{}
		}
		return now.Add(-time.Duration(max(0, endAt.v-m.v)) * time.Millisecond)
	}
	borrowed := rudderstackai.FromContext(s.ctx)
	if borrowed != nil && borrowed.Ended() {
		borrowed = nil
	}
	i := borrowed
	if i == nil && t.resumed != nil && !t.resumed.Ended() {
		i = t.resumed
	}
	if i == nil {
		o := s.cfg.interaction
		o.ConversationID = s.conversationID()
		if o.PromptText == "" {
			o.PromptText = t.promptText
		}
		if t.continuation {
			o.NoPromptEvent = true
		}
		_, i = kit.StartInteraction(s.ctx, o, managedAgentsIntegration, s.cfg.analytics, local(t.startAt))
		s.conversation = i.ConversationID() // minted for the first turn when nothing names one
	}
	kit.Adopt(i, managedAgentsIntegration, s.cfg.analytics)
	for _, c := range t.calls {
		read, write := num(c.usage["cache_read_input_tokens"]), num(c.usage["cache_creation_input_tokens"])
		i.RecordCall(rudderstackai.LLMCall{
			Integration: managedAgentsIntegration, Provider: "anthropic", Model: c.model, AgentName: c.agent,
			InputTokens: num(c.usage["input_tokens"]) + read + write, OutputTokens: num(c.usage["output_tokens"]),
			CachedInputTokens: read, CacheWriteTokens: write, ServiceTier: c.serviceTier,
			ErrorType: c.errorType, UsageUnknown: c.errorType != "", OutputText: c.outputText,
			StartedAt: local(c.startAt), EndedAt: local(c.endAt),
		})
	}
	// The grader's model is unknown: it never becomes primary_model.
	if n := len(t.calls); n > 0 && t.calls[n-1].agent == grader && s.cfg.model != "" {
		kit.PrimaryModel(i, s.cfg.model, "anthropic")
	}
	if len(t.tools) > 0 {
		calls := make([]rudderstackai.ToolCall, len(t.tools))
		for n, tool := range t.tools {
			calls[n] = rudderstackai.ToolCall{
				ID: tool.id, Name: tool.name, MCPServer: tool.mcpServer, AgentName: s.cfg.agentName, Args: tool.args,
			}
		}
		i.RecordToolCalls(calls...)
		for _, tool := range t.tools {
			if tool.id != "" && (tool.settled || tool.err != nil) {
				kit.RecordToolResult(i, tool.id, rudderstackai.ToolResult{
					Error: tool.err, ErrorType: tool.errorType, Duration: tool.duration, Result: tool.result,
				}, tool.settled)
			}
		}
	}
	if t.compactions > 0 {
		i.RecordCompaction(t.compactions)
	}
	if borrowed != nil {
		kit.KeepAnswer(i, t.answer)
		kit.HintStatus(i, end.status, end.errorType, end.pauseReason)
		return
	}
	if end.status == rudderstackai.StatusPaused {
		i.Pause(rudderstackai.PauseOptions{Reason: end.pauseReason, ResponseText: t.answer})
		if s.cfg.sessionID != "" {
			pausedTurns.Lock()
			if _, ok := pausedTurns.m[s.cfg.sessionID]; !ok {
				pausedTurns.order = append(pausedTurns.order, s.cfg.sessionID)
			}
			pausedTurns.m[s.cfg.sessionID] = i
			for len(pausedTurns.m) > kit.MaxMapEntries() && len(pausedTurns.order) > 0 {
				delete(pausedTurns.m, pausedTurns.order[0])
				pausedTurns.order = pausedTurns.order[1:]
				kit.LimitReached("claude-managed-agents paused turns")
			}
			pausedTurns.Unlock()
		}
		return
	}
	i.End(rudderstackai.EndOptions{Status: end.status, ErrorType: end.errorType, ResponseText: t.answer})
}

// conversationID is one conversation for every turn of this stream; empty until one is named
// or the first turn mints it.
func (s *sessionTracker) conversationID() string {
	if s.conversation == "" {
		s.conversation = first(s.cfg.interaction.ConversationID, kit.ContextConversation(s.ctx), s.cfg.sessionID)
	}
	return s.conversation
}

func first(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// boundedSet keeps at most MaxMapEntries IDs, dropping the oldest.
type boundedSet struct {
	where string
	m     map[string]struct{}
	order []string
}

func newBoundedSet(where string) *boundedSet {
	return &boundedSet{where: where, m: map[string]struct{}{}}
}

func (b *boundedSet) has(id string) bool { _, ok := b.m[id]; return ok }

func (b *boundedSet) add(id string) {
	b.m[id] = struct{}{}
	b.order = append(b.order, id)
	if limit := kit.MaxMapEntries(); len(b.m) > limit {
		for len(b.m) > limit && len(b.order) > 0 {
			delete(b.m, b.order[0])
			b.order = b.order[1:]
		}
		kit.LimitReached(b.where)
	}
}
