package rudderanthropic

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	analytics "github.com/rudderlabs/analytics-go/v4"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/capture"
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
	sessionModel                string // the session agent's; model wins
	interaction                 rudderstackai.InteractionOptions
	analytics                   analytics.Client
}

// WithSession names the session the events belong to: the anthropic.BetaManagedAgentsSession that
// Beta.Sessions.New or Get returned. Its ID is the default conversation ID and resumes a paused
// turn; its agent's model prices the calls.
func WithSession(session interface{ RawJSON() string }) SessionOption {
	return func(c *sessionConfig) {
		// A typed nil (*anthropic.BetaManagedAgentsSession)(nil) panics in RawJSON.
		defer func() { _ = recover() }()
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
		if json.Unmarshal(s.Agent.Model, &c.sessionModel) != nil && json.Unmarshal(s.Agent.Model, &model) == nil {
			c.sessionModel = model.ID
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
	src       SessionEventStream[T]
	tracker   *sessionTracker   // nil: untracked
	mu        sync.Mutex        // guards tracker and finished; never held while reading src or by Close
	closed    atomic.Bool       // Close was called: whoever releases mu last finishes the tracker
	srcClosed atomic.Bool       // the source was closed: it is closed at most once
	current   atomic.Pointer[T] // the event Next read from src, kept so the app sees the one tracked
	finished  bool
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
	if ctx == nil {
		ctx = context.Background()
	}
	events := &SessionEvents[T]{src: stream}
	// The SDK's own calls (a classifier or evaluator) are never tracked; options that panic leave
	// the events untracked.
	if !kit.Untracked(ctx) {
		contain(func() {
			c := sessionConfig{}
			for _, o := range opts {
				if o != nil {
					o(&c)
				}
			}
			if c.model == "" {
				c.model = c.sessionModel
			}
			c.interaction = snapshotOptions(c.interaction) // a later change by the caller has no effect
			scope := c.sessionID
			if scope == "" {
				scope = "stream:" + strconv.FormatUint(streams.Add(1), 10)
			}
			events.tracker = &sessionTracker{ctx: ctx, cfg: c, scope: scope + "\x00"}
		})
	}
	return events
}

// Next advances to the next event; false when the stream ended, failed (see Err) or was closed.
func (s *SessionEvents[T]) Next() bool {
	if s.closed.Load() {
		return false
	}
	more := s.src.Next()
	// Read before taking mu: a panic in the source's Current or Err reaches the caller with mu free.
	var err error
	var event T
	if !more {
		err = s.src.Err()
	} else {
		// Once per event, tracked or not: a source's Current may have side effects.
		event = s.src.Current()
		s.current.Store(&event)
	}
	s.mu.Lock()
	if s.finished {
		s.mu.Unlock()
		return false
	}
	if !more {
		s.finishLocked(err)
		s.mu.Unlock()
		return false
	}
	if s.tracker != nil {
		contain(func() { s.tracker.observe(event.RawJSON()) })
	}
	s.mu.Unlock()
	// A Close during the event (from a hook) could not finish: the last to release mu does.
	s.finishIfClosed()
	return true
}

// finishIfClosed finishes after a Close that found mu held; a failed TryLock means the holder
// finishes when it releases.
func (s *SessionEvents[T]) finishIfClosed() {
	if s.closed.Load() && s.mu.TryLock() {
		s.finishLocked(nil)
		s.mu.Unlock()
	}
}

// Current is the event the last Next read, unchanged (the zero value before the first Next).
func (s *SessionEvents[T]) Current() T {
	if event := s.current.Load(); event != nil {
		return *event
	}
	var zero T
	return zero
}

// Err is the stream's error, unchanged.
func (s *SessionEvents[T]) Err() error { return s.src.Err() }

// Close ends tracking (a turn still open is paused or cancelled) and closes the stream when it
// has a Close method. Next returns false after it. It never waits for a Next in progress: a
// close during an event is applied when that event has been read.
func (s *SessionEvents[T]) Close() error {
	s.closed.Store(true)
	s.finishIfClosed()
	// Closed at most once, also after the stream ended or failed.
	if closer, ok := s.src.(io.Closer); ok && s.srcClosed.CompareAndSwap(false, true) {
		return closer.Close()
	}
	return nil
}

func (s *SessionEvents[T]) finishLocked(err error) {
	if s.finished {
		return
	}
	s.finished = true
	s.closed.Store(true) // an ended stream is not read again
	if s.tracker != nil {
		contain(func() { s.tracker.close(err) })
	}
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

// promptEvents start a new user turn.
var promptEvents = map[string]bool{"user.message": true, "user.define_outcome": true}

// settleEvents settle work already in progress: a turn they open continues an earlier one.
var settleEvents = map[string]bool{
	"user.custom_tool_result": true, "user.tool_confirmation": true, "user.tool_result": true, "user.interrupt": true,
}

// seenEvents holds the event IDs already read, by session, so a reconnect that reads history again
// counts each event once. One set for every session: the process keeps at most MaxMapEntries.
var seenEvents = newBoundedSet("claude-managed-agents events")

// childThreads holds the subagent threads each session started, keyed like seenEvents.
var childThreads = newBoundedSet("claude-managed-agents threads")

// childTools holds the tool uses of subagent threads, keyed like seenEvents: the app's answers to them.
var childTools = newBoundedSet("claude-managed-agents child tools")

// streams numbers the streams without a session, for their keys in seenEvents and childThreads.
var streams atomic.Uint64

// pausedTurn and borrowedTurn carry all adapter-held work and decisions across streams.
type pausedTurn struct {
	i     *rudderstackai.Interaction
	state *turnState
}

type borrowedTurn struct {
	i     *rudderstackai.Interaction
	state *turnState
}

// borrowedTurns are unfinished turns in caller-owned interactions, by session ID.
var borrowedTurns = struct {
	sync.Mutex
	m     map[string]*borrowedTurn
	order []string
}{m: map[string]*borrowedTurn{}}

// pausedTurns are turns paused at requires_action, by session ID: a later stream resumes them.
var pausedTurns = struct {
	sync.Mutex
	m     map[string]*pausedTurn
	order []string
}{m: map[string]*pausedTurn{}}

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
	input, output, cached, cacheWrite int // input includes cache reads and writes
	model, agent                      string
	serviceTier                       string
	errorType                         string
	outputText                        string
	startAt, endAt                    msTime
}

// withUsage sets the tokens of a model_usage (or grader usage): input includes cache reads and writes.
func (c turnCall) withUsage(usage map[string]any) turnCall {
	c.cached, c.cacheWrite = num(usage["cache_read_input_tokens"]), num(usage["cache_creation_input_tokens"])
	c.input = num(usage["input_tokens"]) + c.cached + c.cacheWrite
	c.output = num(usage["output_tokens"])
	return c
}

type turnTool struct {
	id, name, mcpServer, agent string // id is empty when the event had none
	args, result               any
	at                         msTime
	err                        *bool
	errorType                  string
	duration                   *time.Duration
	settled, hasResult         bool
}

// requestStart remembers the clock and options in force when a request started.
type requestStart struct {
	at           msTime
	model, agent string
}

type provisionalMessage struct {
	text    string
	request bool
}

// turnState is all work and decisions that survive a stream boundary. New fields belong here by default.
type turnState struct {
	continuation  bool
	prompted      bool
	promptText    string
	starts        *boundedMap[requestStart]
	modelError    string
	requestText   string
	provisional   *boundedMap[*provisionalMessage] // preview IDs not yet replaced by buffered messages
	answerEventID string                           // latest output event, when its answer is still provisional
	calls         []turnCall
	tools         *boundedMap[*turnTool]
	localTools    int // sequence for tools without vendor IDs; survives with their draft keys
	recorded      *boundedMap[msTime]
	blocking      *boundedMap[rudderstackai.PauseReason]
	answer        string
	interrupted   bool
	errorType     string
	compactions   int
	promptSent    bool
	lastModel     string
	hasGrader     bool
	requestSeen   bool
}

// turn holds only the stream segment's clock, waiting status and reporting handles outside state.
type turn struct {
	state           *turnState
	startAt, lastAt msTime
	waiting         rudderstackai.PauseReason
	borrowed        *rudderstackai.Interaction
	resumed         *rudderstackai.Interaction
	interaction     *rudderstackai.Interaction
	backdated       bool
	abandoned       bool
}

type turnEnd struct {
	status      rudderstackai.ResponseStatus
	errorType   string
	pauseReason rudderstackai.PauseReason
}

type sessionTracker struct {
	ctx             context.Context
	cfg             sessionConfig
	scope           string // key prefix in seenEvents and childThreads: the session, else this stream
	turn            *turn
	runningAt       msTime
	runningSet      bool                       // runningBorrowed was fixed at that running event
	runningBorrowed *rudderstackai.Interaction // the caller's interaction when that running event was read
	runningAnswer   *rudderstackai.Interaction // the answer that running event started
	lastError       string
	conversation    string
}

// snapshotOptions copies what the caller could change through a pointer, slice or map after the
// call: the turn's settings are the ones given.
func snapshotOptions(o rudderstackai.InteractionOptions) rudderstackai.InteractionOptions {
	o.Resume = nil // Each turn is its own answer; a snapshot would merge every turn into one.
	dup := func(p *bool) *bool {
		if p == nil {
			return nil
		}
		return rudderstackai.Ptr(*p)
	}
	o.CaptureContent, o.ContentCapture = dup(o.CaptureContent), capture.Clone(o.ContentCapture)
	o.ToolBreakdown, o.Classify, o.IdentityFromCookies = dup(o.ToolBreakdown), dup(o.Classify), dup(o.IdentityFromCookies)
	if o.AttachmentCount != nil {
		o.AttachmentCount = rudderstackai.Ptr(*o.AttachmentCount)
	}
	if o.Request != nil {
		o.Request = o.Request.Clone(o.Request.Context()) // deep-copies the headers that carry cookies
	}
	o.AttachmentTypes, o.CookieValues = slices.Clone(o.AttachmentTypes), maps.Clone(o.CookieValues)
	if o.Context != nil {
		o.Context = plainCopy(o.Context).(map[string]any)
	}
	return o
}

// plainCopy copies maps, slices and arrays all the way down; other values are kept.
func plainCopy(v any) any {
	type visit struct {
		typ reflect.Type
		ptr uintptr
		len int // Slice views sharing a pointer can have different lengths.
	}
	seen := map[visit]reflect.Value{}
	var copyValue func(reflect.Value) reflect.Value
	copyValue = func(v reflect.Value) reflect.Value {
		switch v.Kind() {
		case reflect.Interface:
			if v.IsNil() {
				return v
			}
			out := reflect.New(v.Type()).Elem()
			out.Set(copyValue(v.Elem()))
			return out
		case reflect.Map, reflect.Slice:
			if v.IsNil() {
				return v
			}
			key := visit{typ: v.Type(), ptr: v.Pointer()}
			if v.Kind() == reflect.Slice {
				key.len = v.Len()
			}
			if out, ok := seen[key]; ok {
				return out
			}
			if v.Kind() == reflect.Map {
				out := reflect.MakeMapWithSize(v.Type(), v.Len())
				seen[key] = out
				iter := v.MapRange()
				for iter.Next() {
					out.SetMapIndex(iter.Key(), copyValue(iter.Value()))
				}
				return out
			}
			out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
			seen[key] = out
			for i := 0; i < v.Len(); i++ {
				out.Index(i).Set(copyValue(v.Index(i)))
			}
			return out
		case reflect.Array:
			out := reflect.New(v.Type()).Elem()
			for i := 0; i < v.Len(); i++ {
				out.Index(i).Set(copyValue(v.Index(i)))
			}
			return out
		default:
			return v
		}
	}
	return copyValue(reflect.ValueOf(v)).Interface()
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
	// Previews share the buffered event's ID; they must not mark it as read.
	if kind == "event_start" || kind == "event_delta" {
		s.preview(e, kind)
		return
	}
	id := str(e["id"])
	if id != "" {
		// The consolidation pattern (stream + history) repeats events.
		if !seenEvents.add(s.scope + id) {
			return
		}
	}
	at := timeOf(e)
	thread := str(e["session_thread_id"])
	// Agent and thread status events naming a thread are cross-posted from a subagent thread, so
	// it is a child even when its session.thread_created was not read. A thread no event named so
	// is unknown, not a child: per the Managed Agents docs (multiagent sessions, "Interrupting with
	// runs open"), "a user.interrupt with no session_thread_id, or naming the primary thread, stops
	// the agent's turn", and the primary thread's own events name no thread.
	if thread != "" && (strings.HasPrefix(kind, "agent.") || strings.HasPrefix(kind, "session.thread_")) {
		childThreads.add(s.scope + thread)
	}
	// The app's answer to a subagent's tool, by the tool's ID: the docs say to "answer with the
	// tool_use_id or custom_tool_use_id alone", so answers need not name the thread.
	answered := first(str(e["custom_tool_use_id"]), str(e["tool_use_id"]))
	child := thread != "" && childThreads.has(s.scope+thread)
	if (thread != "" && (strings.HasPrefix(kind, "agent.") || (child && (strings.HasPrefix(kind, "user.") || strings.HasPrefix(kind, "span."))))) ||
		(strings.HasPrefix(kind, "user.") && answered != "" && childTools.has(s.scope+answered)) {
		// Cross-posted from a subagent thread (agent events, a known child's requests), or aimed at
		// one: not this thread's work. An unknown thread's request counts here, once.
		if id != "" && strings.HasPrefix(kind, "agent.") && strings.HasSuffix(kind, "tool_use") {
			childTools.add(s.scope + id)
		}
		if id != "" && (kind == "agent.custom_tool_use" || e["evaluated_permission"] == "ask") {
			if s.turn == nil && (s.hasCarriedState() || s.isRunning()) {
				s.open(kind, at)
			}
			if s.turn == nil {
				return
			}
			if kind == "agent.custom_tool_use" {
				s.turn.state.blocking.set(id, rudderstackai.PauseDeferredTool)
			} else if e["evaluated_permission"] == "ask" {
				s.turn.state.blocking.set(id, rudderstackai.PauseApproval)
			}
		}
		return
	}
	switch kind {
	case "session.thread_created":
		if thread != "" {
			childThreads.add(s.scope + thread)
		}
	case "session.status_running", "session.status_rescheduled":
		if s.turn != nil {
			s.turn.waiting = ""
			// The turn runs at least until this event, also when it is the last one read.
			if at.ok {
				s.turn.lastAt = at
			}
		} else if !s.isRunning() {
			s.running(at)
		}
	case "agent.thinking", "agent.thread_message_received", "agent.thread_message_sent", "span.outcome_evaluation_ongoing", "system.message":
		s.open(kind, at)
	case "span.outcome_evaluation_start":
		t := s.open(kind, at)
		if id != "" {
			t.state.starts.set(id, requestStart{at: at, model: s.cfg.model, agent: s.cfg.agentName})
		}
	case "user.message", "user.define_outcome":
		t := s.open(kind, at)
		if !t.state.prompted {
			t.state.prompted = true
			if kind == "user.message" {
				t.state.promptText = textOf(e["content"])
			} else {
				t.state.promptText = str(e["description"])
			}
			s.sendPrompt(t)
		}
	case "user.custom_tool_result":
		s.settle(s.open(kind, at), str(e["custom_tool_use_id"]), e, at, "")
	case "user.tool_result":
		s.settle(s.open(kind, at), str(e["tool_use_id"]), e, at, "")
	case "user.tool_confirmation":
		if e["result"] == "deny" {
			s.settle(s.open(kind, at), str(e["tool_use_id"]), e, at, "denied")
		} else {
			s.open(kind, at).state.blocking.delete(str(e["tool_use_id"]))
		}
	case "user.interrupt":
		s.open(kind, at).state.interrupted = true
	case "span.model_request_start":
		t := s.open(kind, at)
		if id != "" {
			t.state.starts.set(id, requestStart{at: at, model: s.cfg.model, agent: s.cfg.agentName})
		}
		t.state.requestSeen = true
		t.state.modelError, t.state.requestText = "", ""
		for _, entry := range t.state.provisional.entries() {
			entry.value.request = false
		}
	case "span.model_request_end":
		s.call(s.open(kind, at), e, at)
	case "span.outcome_evaluation_end":
		t := s.open(kind, at)
		t.state.hasGrader = true
		c := turnCall{agent: grader, endAt: at}.withUsage(obj(e["usage"]))
		if startID := str(e["outcome_evaluation_start_id"]); startID != "" {
			start, _ := t.state.starts.get(startID)
			c.startAt = start.at
			t.state.starts.delete(startID)
		}
		s.push(t, c)
	case "agent.message":
		t := s.open(kind, at)
		preview, _ := t.state.provisional.get(id)
		if id != "" {
			t.state.provisional.delete(id)
		}
		if text := textOf(e["content"]); text != "" || preview != nil {
			t.state.answer, t.state.answerEventID = text, ""
			if text != "" && (preview == nil || preview.request) {
				t.state.requestText += text
			}
		}
	case "agent.tool_use", "agent.mcp_tool_use", "agent.custom_tool_use":
		s.tool(s.open(kind, at), kind, e, id, at)
	case "agent.tool_result":
		s.settle(s.open(kind, at), str(e["tool_use_id"]), e, at, "")
	case "agent.mcp_tool_result":
		s.settle(s.open(kind, at), str(e["mcp_tool_use_id"]), e, at, "")
	case "agent.thread_context_compacted":
		s.open(kind, at).state.compactions++
	case "session.error":
		// An error while the session runs (a repository that cannot be cloned), or that ends a
		// carried session, belongs to a turn.
		if s.turn == nil && (s.hasCarriedState() || s.isRunning()) {
			s.open(kind, at)
		}
		t := s.turn
		// A last error still counts in the turn's latency.
		if t != nil && at.ok {
			t.lastAt = at
		}
		s.sessionError(t, e)
	case "session.status_idle":
		s.idle(e, at)
	case "session.status_terminated":
		if t := s.ending(kind, at); t != nil {
			switch {
			case t.state.errorType != "":
				s.finish(turnEnd{status: rudderstackai.StatusError, errorType: t.state.errorType}, at)
			case t.state.interrupted || t.abandoned:
				s.finish(turnEnd{status: rudderstackai.StatusCancelled, errorType: "cancelled"}, at)
			default:
				s.finish(turnEnd{status: rudderstackai.StatusSuccess}, at)
			}
		}
	case "session.deleted":
		if s.ending(kind, at) != nil {
			s.finish(turnEnd{status: rudderstackai.StatusCancelled, errorType: "cancelled"}, at)
		}
	}
}

// preview opens primary-thread work and retains text until its buffered event arrives.
func (s *sessionTracker) preview(e map[string]any, kind string) {
	start := obj(e["event"])
	id := str(e["event_id"])
	if kind == "event_start" {
		id = str(start["id"])
	}
	if id == "" || seenEvents.has(s.scope+id) {
		return
	}
	if kind == "event_start" {
		typ := str(start["type"])
		if typ != "agent.message" && typ != "agent.thinking" {
			return
		}
		if thread := str(start["session_thread_id"]); thread != "" {
			childThreads.add(s.scope + thread)
			return
		}
		t := s.open(typ, timeOf(start))
		if _, exists := t.state.provisional.get(id); typ == "agent.message" && !exists {
			t.state.provisional.set(id, &provisionalMessage{request: true})
		}
		return
	}
	delta := obj(e["delta"])
	content := obj(delta["content"])
	text, ok := content["text"].(string)
	if delta["type"] != "content_delta" || content["type"] != "text" || !ok {
		return
	}
	t := s.open("agent.message", timeOf(e))
	preview, exists := t.state.provisional.get(id)
	if !exists {
		preview = &provisionalMessage{request: true}
	}
	t.state.provisional.set(id, preview)
	// Keep one extra code point so the core's normal 32,000-rune size rule drops oversized text.
	runes := []rune(preview.text + text)
	preview.text = string(runes[:min(len(runes), 32_001)])
	t.state.answerEventID = id
}

func (s *sessionTracker) answer(t *turn) string {
	if t.state.answerEventID != "" {
		if preview, ok := t.state.provisional.get(t.state.answerEventID); ok {
			return preview.text
		}
	}
	return t.state.answer
}

// close: the stream ended or failed; a turn still open is paused, failed or cancelled.
func (s *sessionTracker) close(err error) {
	t := s.turn
	// A running session whose stream stopped before any other event still had a turn.
	if t == nil && s.isRunning() {
		t = s.open("session.status_running", s.runningAt)
	}
	if t == nil {
		return
	}
	switch {
	case err != nil:
		// Ends as an error ends an answer: the app's own is app_error, a deadline is timeout.
		errorType := kit.AppErrorType(err)
		s.finish(turnEnd{status: kit.StatusForErrorType(errorType), errorType: errorType}, msTime{})
	case t.state.errorType != "":
		// A terminal session.error read before the stream ended: the session will not go on.
		s.finish(turnEnd{status: rudderstackai.StatusError, errorType: t.state.errorType}, msTime{})
	case t.waiting != "":
		s.finish(turnEnd{status: rudderstackai.StatusPaused, pauseReason: t.waiting}, msTime{})
	default:
		s.finish(turnEnd{status: rudderstackai.StatusCancelled, errorType: "cancelled"}, msTime{})
	}
}

// hasCarriedState reports whether this stream can continue this session's paused or caller-owned turn.
func (s *sessionTracker) hasCarriedState() bool {
	if s.cfg.sessionID == "" {
		return false
	}
	if s.hasPausedTurn() {
		return true
	}
	owner := s.runningBorrowed
	if !s.runningSet {
		owner = s.owner()
	}
	borrowedTurns.Lock()
	defer borrowedTurns.Unlock()
	borrowed := borrowedTurns.m[s.cfg.sessionID]
	return borrowed != nil && borrowed.i == owner
}

// ending is the turn an ending event ends: the open one, else this session's carried one.
func (s *sessionTracker) ending(kind string, at msTime) *turn {
	if s.turn != nil {
		return s.turn
	}
	paused := s.hasPausedTurn()
	// A running session that ends before any other event (an agent stopped before its first
	// request) still has a turn.
	if !s.hasCarriedState() && !s.isRunning() {
		return nil
	}
	t := s.open(kind, at)
	t.abandoned = paused
	return t
}

func (s *sessionTracker) open(kind string, at msTime) *turn {
	t := s.turn
	if t == nil {
		start := at
		if s.runningAt.ok && at.ok && s.runningAt.v < at.v {
			start = s.runningAt
		}
		// Whose answer the turn is in is fixed when it starts: its running event, else this one.
		borrowed := s.runningBorrowed
		if !s.runningSet {
			borrowed = s.owner()
		}
		started := s.runningAnswer
		s.runningAt, s.runningSet, s.runningBorrowed, s.runningAnswer = msTime{}, false, nil, nil
		// A turn that settles earlier work continues it, also when the settling event was sent
		// before this stream opened (the paused turn is then the only trace of it).
		// Per the Managed Agents docs, "while the session is idle with requires_action, a
		// user.message returns 400, so answer the calls first": a prompt after a pause means the
		// paused turn was answered and ended off this stream. Its answer ends; the prompt's is new.
		if promptEvents[kind] {
			s.endPaused()
			deleteBorrowedTurn(s.cfg.sessionID)
		}
		continuation := settleEvents[kind] || (!promptEvents[kind] && s.hasPausedTurn())
		t = &turn{
			startAt: start, borrowed: borrowed,
			state: &turnState{
				continuation: continuation,
				provisional:  newBoundedMap[*provisionalMessage]("claude-managed-agents provisional"),
				starts:       newBoundedMap[requestStart]("claude-managed-agents starts"),
				tools:        newBoundedMap[*turnTool]("claude-managed-agents tool drafts"),
				recorded:     newBoundedMap[msTime]("claude-managed-agents tools"),
				blocking:     newBoundedMap[rudderstackai.PauseReason]("claude-managed-agents blocking"),
			},
		}
		s.turn = t
		// A paused answer continues as itself wherever the rest is read: another caller's
		// interaction never takes over its tools, identity or client.
		if t.state.continuation && s.hasPausedTurn() {
			t.borrowed = nil
		}
		if t.borrowed != nil {
			s.adoptBorrowed(t)
		} else if t.state.continuation {
			s.resume(t)
		}
		// A new answer starts with the turn, so every setting (identity, capture) resolves now.
		if t.borrowed == nil && t.resumed == nil {
			if t.interaction = started; started == nil {
				t.interaction = s.startAnswer(at)
			}
		} else if started != nil {
			contain(func() {
				started.End(rudderstackai.EndOptions{Status: rudderstackai.StatusCancelled, ErrorType: "cancelled"})
			})
		}
		// A turn whose prompt was sent before this stream (initial_events) still has its prompt event.
		if !promptEvents[kind] {
			s.sendPrompt(t)
		}
	}
	// Any event after a requires_action idle means the app answered.
	t.waiting = ""
	if at.ok {
		t.lastAt = at
	}
	return t
}

// isRunning reports a running event read outside a turn.
func (s *sessionTracker) isRunning() bool { return s.runningSet }

// owner is the caller's interaction the turn starting now reports into, if it is still open.
func (s *sessionTracker) owner() *rudderstackai.Interaction {
	if active := rudderstackai.FromContext(s.ctx); active != nil && !active.Ended() {
		return active
	}
	return nil
}

// running handles a running event outside a turn: the next turn starts here, and so does its answer.
func (s *sessionTracker) running(at msTime) {
	s.runningAt, s.runningSet, s.runningBorrowed = at, true, s.owner()
	// A paused answer may continue with the next event instead: that event decides.
	if s.runningBorrowed == nil && !s.hasPausedTurn() {
		s.runningAnswer = s.startAnswer(at)
	}
}

// startAnswer starts a new answer in the context the tracker was given, with its prompt event held
// back until the prompt arrives (sendPrompt); nil when starting it fails.
func (s *sessionTracker) startAnswer(at msTime) (i *rudderstackai.Interaction) {
	contain(func() {
		o := s.cfg.interaction
		o.ConversationID = s.conversationID()
		o.NoPromptEvent = true
		_, i = kit.StartInteraction(s.ctx, o, managedAgentsIntegration, s.cfg.analytics, clockAt(at)(at))
		s.conversation = i.ConversationID() // minted for the first turn when nothing names one
		kit.Adopt(i, managedAgentsIntegration, s.cfg.analytics)
	})
	return i
}

// sendPrompt sends the turn's prompt event once, as StartInteraction would have: with the prompt's
// text when the turn has one, never for a continuation, a resumed answer or the caller's own.
func (s *sessionTracker) sendPrompt(t *turn) {
	if t.state.promptSent || t.interaction == nil || t.interaction == t.borrowed || t.resumed != nil {
		return
	}
	t.state.promptSent = true
	o := s.cfg.interaction
	o.PromptText = first(o.PromptText, t.state.promptText)
	o.NoPromptEvent = o.NoPromptEvent || t.state.continuation
	contain(func() { kit.StartPrompt(t.interaction, o) })
}

// hasPausedTurn reports whether this session has a paused turn to resume.
func (s *sessionTracker) hasPausedTurn() bool {
	if s.cfg.sessionID == "" {
		return false
	}
	pausedTurns.Lock()
	defer pausedTurns.Unlock()
	return pausedTurns.m[s.cfg.sessionID] != nil
}

// adoptBorrowed takes over the tools an earlier stream of this session left open in the same
// interaction.
func (s *sessionTracker) adoptBorrowed(t *turn) {
	if s.cfg.sessionID == "" {
		return
	}
	borrowedTurns.Lock()
	open := borrowedTurns.m[s.cfg.sessionID]
	if open != nil && open.i == t.borrowed {
		delete(borrowedTurns.m, s.cfg.sessionID)
		borrowedTurns.order = slices.DeleteFunc(borrowedTurns.order, func(id string) bool { return id == s.cfg.sessionID })
	} else {
		open = nil
	}
	borrowedTurns.Unlock()
	if open != nil {
		t.state = open.state
	}
}

func deleteBorrowedTurn(sessionID string) {
	borrowedTurns.Lock()
	delete(borrowedTurns.m, sessionID)
	borrowedTurns.order = slices.DeleteFunc(borrowedTurns.order, func(id string) bool { return id == sessionID })
	borrowedTurns.Unlock()
}

// endPaused ends this session's paused answer, which can no longer continue, through the core's
// end path: the core decides how a tool the pause left waiting counts.
func (s *sessionTracker) endPaused() {
	if s.cfg.sessionID == "" {
		return
	}
	pausedTurns.Lock()
	paused := pausedTurns.m[s.cfg.sessionID]
	delete(pausedTurns.m, s.cfg.sessionID)
	pausedTurns.order = slices.DeleteFunc(pausedTurns.order, func(id string) bool { return id == s.cfg.sessionID })
	pausedTurns.Unlock()
	if paused == nil || paused.i.Ended() {
		return
	}
	contain(func() {
		calls := make([]rudderstackai.ToolCall, 0, paused.state.tools.len())
		for _, entry := range paused.state.tools.entries() {
			tool := entry.value
			calls = append(calls, rudderstackai.ToolCall{
				ID: tool.id, Name: tool.name, MCPServer: tool.mcpServer, AgentName: tool.agent, Args: tool.args,
			})
		}
		if len(calls) > 0 {
			paused.i.RecordToolCalls(calls...)
		}
		paused.i.End(rudderstackai.EndOptions{Status: rudderstackai.StatusCancelled, ErrorType: "cancelled"})
	})
}

// resume resumes this session's paused answer now, with the tools it was waiting on.
func (s *sessionTracker) resume(t *turn) {
	if s.cfg.sessionID == "" {
		return
	}
	pausedTurns.Lock()
	paused := pausedTurns.m[s.cfg.sessionID]
	delete(pausedTurns.m, s.cfg.sessionID)
	pausedTurns.order = slices.DeleteFunc(pausedTurns.order, func(id string) bool { return id == s.cfg.sessionID })
	pausedTurns.Unlock()
	if paused == nil || paused.i.Ended() {
		return
	}
	paused.i.Resume()
	t.resumed, t.interaction = paused.i, paused.i
	t.state = paused.state
}

func (s *sessionTracker) call(t *turn, e map[string]any, at msTime) {
	usage := obj(e["model_usage"])
	failed := e["is_error"] == true
	startID := str(e["model_request_start_id"])
	start, hasStart := t.state.starts.get(startID)
	model, agent := s.cfg.model, s.cfg.agentName
	if hasStart {
		model, agent = start.model, start.agent
	} else if model == "" {
		kit.WarnOnce("claude-managed-agents-model", "claude-managed-agents: pass WithSession or WithSessionModel to TrackSessionEvents; calls are unpriced without a model")
	}
	t.state.requestSeen = true
	if !failed && model != "" {
		t.state.lastModel = model
	}
	outputText := t.state.requestText
	for _, entry := range t.state.provisional.entries() {
		if entry.value.request {
			outputText += entry.value.text
		}
	}
	c := turnCall{model: model, agent: agent, outputText: outputText, startAt: start.at, endAt: at}.withUsage(usage)
	if speed := str(usage["speed"]); speed == "fast" || speed == "standard" {
		c.serviceTier = speed
	}
	if failed {
		c.errorType = t.state.modelError
		if c.errorType == "" {
			c.errorType = "api_error"
		}
	}
	if startID != "" {
		t.state.starts.delete(startID)
	}
	s.push(t, c)
	for _, entry := range t.state.provisional.entries() {
		entry.value.request = false
	}
	t.state.modelError, t.state.requestText = "", ""
}

// push buffers a call; past the call cap the turn's buffered calls and tools go to its answer.
func (s *sessionTracker) push(t *turn, c turnCall) {
	t.state.calls = append(t.state.calls, c)
	if s.report(t) {
		return
	}
	s.spillIfFull(t)
}

// report records what a turn in the caller's interaction holds as it arrives, so the caller's pause
// snapshot or end holds it. It returns whether it did.
func (s *sessionTracker) report(t *turn) bool {
	if t.borrowed == nil {
		return false
	}
	// The caller ended its answer mid-turn: the rest of the turn has nowhere to go.
	if t.borrowed.Ended() {
		t.state.calls = nil
		t.state.tools.clear()
		return true
	}
	contain(func() {
		s.flush(t, clockAt(t.lastAt), false, map[string]bool{})
		// The caller may pause or end its answer before the turn ends: correct it now.
		if t.state.hasGrader && t.state.lastModel != "" {
			kit.PrimaryModel(t.borrowed, t.state.lastModel, "anthropic")
		}
	})
	return true
}

func (s *sessionTracker) tool(t *turn, kind string, e map[string]any, id string, at msTime) {
	tool := &turnTool{id: id, agent: s.cfg.agentName, name: str(e["name"]), args: e["input"], at: at}
	if kind == "agent.mcp_tool_use" {
		tool.mcpServer = str(e["mcp_server_name"])
	}
	switch {
	case e["evaluated_permission"] == "deny":
		tool.err, tool.errorType, tool.settled = rudderstackai.Ptr(true), "denied", true
	case id != "" && kind == "agent.custom_tool_use":
		t.state.blocking.set(id, rudderstackai.PauseDeferredTool)
	case id != "" && e["evaluated_permission"] == "ask":
		t.state.blocking.set(id, rudderstackai.PauseApproval)
	}
	key := id
	if key == "" {
		t.state.localTools++
		key = "\x00local:" + strconv.Itoa(t.state.localTools)
	}
	_, exists := t.state.tools.get(key)
	full := !exists && t.state.tools.len() >= kit.MaxMapEntries()
	// Flush a full draft buffer before bounded insertion can discard unrecorded work.
	if full {
		s.flush(t, clockAt(at), false, map[string]bool{})
	}
	t.state.tools.set(key, tool)
	if full {
		s.flush(t, clockAt(at), false, map[string]bool{})
	}
	if s.report(t) {
		return
	}
	s.spillIfFull(t)
}

func (s *sessionTracker) settle(t *turn, id string, e map[string]any, at msTime, errorType string) {
	if id == "" {
		return
	}
	t.state.blocking.delete(id)
	if tool, ok := t.state.tools.get(id); ok {
		if tool.settled {
			return
		}
		tool.settled = true
		// A denial is timed from the tool use to the confirmation, like a result.
		if tool.at.ok && at.ok {
			d := time.Duration(max(0, at.v-tool.at.v)) * time.Millisecond
			tool.duration = &d
		}
		if errorType != "" {
			tool.err, tool.errorType = rudderstackai.Ptr(true), errorType
			return
		}
		// Read as the Anthropic wrapper reads a tool_result, so vendor failures count as errors.
		tool.hasResult, tool.result = true, kit.ToolResultValue(e["content"])
		tool.err = rudderstackai.Ptr(e["is_error"] == true)
		return
	}
	// A tool already recorded into the answer (past the call cap, or in the caller's interaction).
	i := t.interaction
	if i == nil {
		i = t.borrowed
	}
	startedAt, ok := t.state.recorded.get(id)
	if !ok || i == nil {
		return
	}
	t.state.recorded.delete(id)
	var duration *time.Duration
	if startedAt.ok && at.ok {
		d := time.Duration(max(0, at.v-startedAt.v)) * time.Millisecond
		duration = &d
	}
	if errorType != "" {
		kit.RecordToolResult(i, id, rudderstackai.ToolResult{Error: rudderstackai.Ptr(true), ErrorType: errorType, Duration: duration}, false)
		return
	}
	result := rudderstackai.ToolResult{Error: rudderstackai.Ptr(e["is_error"] == true), Result: kit.ToolResultValue(e["content"]), Duration: duration}
	kit.RecordToolResult(i, id, result, true)
}

func (s *sessionTracker) sessionError(t *turn, e map[string]any) {
	errObj := obj(e["error"])
	kind := str(errObj["type"])
	errorType, ok := sessionErrors[kind]
	if !ok {
		errorType = "api_error"
	}
	if t != nil {
		t.state.modelError = errorType
	}
	// A retried error is not the turn's outcome unless it recurs as exhausted or terminal.
	if str(obj(errObj["retry_status"])["type"]) == "retrying" {
		return
	}
	s.lastError = errorType
	if t != nil {
		t.state.errorType = errorType
	}
}

func (s *sessionTracker) idle(e map[string]any, at msTime) {
	stop := obj(e["stop_reason"])
	// A carried turn or pending running event can pause before any other primary event.
	t := s.turn
	if str(stop["type"]) != "requires_action" {
		t = s.ending("session.status_idle", at)
	} else if t == nil && (s.hasCarriedState() || s.isRunning()) {
		t = s.open("session.status_idle", at)
	}
	if t == nil {
		return
	}
	switch str(stop["type"]) {
	case "requires_action":
		ids, _ := stop["event_ids"].([]any)
		t.waiting = rudderstackai.PauseOther
		// Mark every named tool first, so one named after an approval is carried across too.
		for _, id := range ids {
			// A tool the session waits on is the app's to run or answer (a sandbox tool too).
			_, open := t.state.tools.get(str(id))
			if _, recorded := t.state.recorded.get(str(id)); open || recorded {
				if _, blocked := t.state.blocking.get(str(id)); !blocked {
					t.state.blocking.set(str(id), rudderstackai.PauseDeferredTool)
				}
			}
		}
		found := false
		for _, id := range ids {
			reason, ok := t.state.blocking.get(str(id))
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
		errorType := first(t.state.errorType, t.state.modelError, s.lastError, "api_error")
		s.finish(turnEnd{status: rudderstackai.StatusError, errorType: errorType}, at)
	case "budget_reached":
		s.finish(turnEnd{status: rudderstackai.StatusIncomplete, errorType: "max_budget_usd"}, at)
	default:
		if t.state.interrupted {
			s.finish(turnEnd{status: rudderstackai.StatusCancelled, errorType: "cancelled"}, at)
		} else {
			s.finish(turnEnd{status: rudderstackai.StatusSuccess}, at)
		}
	}
}

func (s *sessionTracker) finish(end turnEnd, at msTime) {
	t := s.turn
	s.turn, s.runningAt, s.runningSet, s.runningBorrowed, s.runningAnswer = nil, msTime{}, false, nil, nil
	if t == nil {
		return
	}
	if !at.ok {
		at = t.lastAt
	}
	contain(func() { s.end(t, end, at) })
}

// spillIfFull records what the turn holds once it reaches the call cap, so it never grows past it.
func (s *sessionTracker) spillIfFull(t *turn) {
	// Tools still waiting on their result stay with the turn, so a pause that names one (the app
	// runs it) still carries it; up to MaxMapEntries of them.
	waiting := map[string]bool{}
	for _, entry := range t.state.tools.entries() {
		key, tool := entry.key, entry.value
		if !tool.settled && tool.id != "" && len(waiting) < kit.MaxMapEntries() {
			waiting[key] = true
		}
	}
	if len(t.state.calls)+t.state.tools.len()-len(waiting) >= kit.MaxCallsPerInteraction() {
		contain(func() { s.flush(t, clockAt(t.lastAt), false, waiting) })
	}
}

// interactionFor is the answer the turn reports into: the caller's, the resumed one, else its own.
func (s *sessionTracker) interactionFor(t *turn, local func(msTime) time.Time) *rudderstackai.Interaction {
	if t.interaction == nil {
		// The caller's answer stays the turn's, also once the caller ended it.
		if t.interaction = t.borrowed; t.interaction == nil {
			t.interaction = s.startAnswer(t.lastAt)
		}
		kit.Adopt(t.interaction, managedAgentsIntegration, s.cfg.analytics)
	}
	if t.resumed != nil && !t.backdated {
		t.backdated = true
		// Read after the fact (history): the resumed segment keeps its own latency.
		kit.Backdate(t.resumed, local(t.startAt))
	}
	return t.interaction
}

// clockAt maps server times onto the local clock so that endAt is now.
func clockAt(endAt msTime) func(msTime) time.Time {
	now := time.Now()
	return func(m msTime) time.Time {
		if !m.ok || !endAt.ok {
			return time.Time{}
		}
		return now.Add(-time.Duration(max(0, endAt.v-m.v)) * time.Millisecond)
	}
}

// flush records the turn's buffered calls and tools into its answer. Tools in keep (waiting on
// the app) stay, for a pause to carry them to the next segment.
func (s *sessionTracker) flush(t *turn, local func(msTime) time.Time, final bool, keep map[string]bool) *rudderstackai.Interaction {
	i := s.interactionFor(t, local)
	calls := t.state.calls
	t.state.calls = nil
	for _, c := range calls {
		i.RecordCall(rudderstackai.LLMCall{
			Integration: managedAgentsIntegration, Provider: "anthropic", Model: c.model, AgentName: c.agent,
			InputTokens: c.input, OutputTokens: c.output, CachedInputTokens: c.cached, CacheWriteTokens: c.cacheWrite,
			ServiceTier: c.serviceTier, ErrorType: c.errorType, UsageUnknown: c.errorType != "",
			OutputText: c.outputText, StartedAt: local(c.startAt), EndedAt: local(c.endAt),
		})
	}
	var tools []*turnTool
	for _, entry := range t.state.tools.entries() {
		if keep[entry.key] {
			continue
		}
		tools = append(tools, entry.value)
		t.state.tools.delete(entry.key)
	}
	if len(tools) == 0 {
		return i
	}
	toolCalls := make([]rudderstackai.ToolCall, 0, len(tools))
	for _, tool := range tools {
		call := rudderstackai.ToolCall{
			ID: tool.id, Name: tool.name, MCPServer: tool.mcpServer, AgentName: tool.agent, Args: tool.args,
		}
		// Without an ID no result can settle it later: a refusal known up front goes with the call.
		if tool.id == "" && tool.settled {
			call.Error, call.ErrorType, call.Duration = tool.err, tool.errorType, tool.duration
		}
		toolCalls = append(toolCalls, call)
	}
	i.RecordToolCalls(toolCalls...)
	for _, tool := range tools {
		switch {
		case tool.id == "":
		case tool.settled:
			kit.RecordToolResult(i, tool.id, rudderstackai.ToolResult{
				Error: tool.err, ErrorType: tool.errorType, Duration: tool.duration, Result: tool.result,
			}, tool.hasResult)
		case !final:
			// Recorded before its result: the result settles it in the answer later.
			t.state.recorded.set(tool.id, tool.at)
		}
	}
	return i
}

// end records the turn's calls and tools and ends (or pauses) its answer.
func (s *sessionTracker) end(t *turn, end turnEnd, endAt msTime) {
	// Tools still waiting on the app carry over to the segment that resumes the pause.
	keep := map[string]bool{}
	if end.status == rudderstackai.StatusPaused && t.borrowed == nil {
		for _, entry := range t.state.tools.entries() {
			key, tool := entry.key, entry.value
			_, ok := t.state.blocking.get(key)
			if ok && !tool.settled {
				keep[key] = true
			}
		}
	}
	local := clockAt(endAt)
	i := s.flush(t, local, true, keep)
	// Read after the fact (history), the answer started at the turn's first event, also when a spill opened it earlier.
	if i != t.borrowed {
		kit.Backdate(i, local(t.startAt))
	}
	// The grader's model is unknown: it never becomes primary_model.
	if t.state.hasGrader && t.state.lastModel != "" {
		kit.PrimaryModel(i, t.state.lastModel, "anthropic")
	}
	if t.state.compactions > 0 {
		i.RecordCompaction(t.state.compactions)
	}
	t.state.compactions = 0
	if t.borrowed != nil && i == t.borrowed {
		kit.KeepAnswer(i, s.answer(t))
		kit.HintStatus(i, end.status, end.errorType, end.pauseReason)
		// Every unfinished turn carries whole state, even when only an interrupt or blocking remains.
		if s.cfg.sessionID != "" {
			if end.status == rudderstackai.StatusPaused || end.status == rudderstackai.StatusCancelled {
				borrowedTurns.Lock()
				if _, ok := borrowedTurns.m[s.cfg.sessionID]; !ok {
					borrowedTurns.order = append(borrowedTurns.order, s.cfg.sessionID)
				}
				borrowedTurns.m[s.cfg.sessionID] = &borrowedTurn{i: i, state: t.state}
				evicted := false
				for len(borrowedTurns.m) > kit.MaxMapEntries() {
					delete(borrowedTurns.m, borrowedTurns.order[0])
					borrowedTurns.order = borrowedTurns.order[1:]
					evicted = true
				}
				borrowedTurns.Unlock()
				if evicted {
					kit.LimitReached("claude-managed-agents borrowed turns")
				}
			} else {
				deleteBorrowedTurn(s.cfg.sessionID)
			}
		}
		return
	}
	if end.status != rudderstackai.StatusPaused {
		i.End(rudderstackai.EndOptions{Status: end.status, ErrorType: end.errorType, ResponseText: s.answer(t)})
		return
	}
	i.Pause(rudderstackai.PauseOptions{Reason: end.pauseReason, ResponseText: s.answer(t)})
	if s.cfg.sessionID == "" {
		return
	}
	pausedTurns.Lock()
	if _, ok := pausedTurns.m[s.cfg.sessionID]; !ok {
		pausedTurns.order = append(pausedTurns.order, s.cfg.sessionID)
	}
	pausedTurns.m[s.cfg.sessionID] = &pausedTurn{i: i, state: t.state}
	evicted := false
	for len(pausedTurns.m) > kit.MaxMapEntries() && len(pausedTurns.order) > 0 {
		delete(pausedTurns.m, pausedTurns.order[0])
		pausedTurns.order = pausedTurns.order[1:]
		evicted = true
	}
	pausedTurns.Unlock()
	if evicted { // reported outside the lock: an onError callback may track another stream
		kit.LimitReached("claude-managed-agents paused turns")
	}
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

// boundedSet keeps at most MaxMapEntries IDs, dropping the oldest. Streams of one session share it.
type boundedSet struct {
	where string
	mu    sync.Mutex
	m     map[string]struct{}
	order []string
}

func newBoundedSet(where string) *boundedSet {
	return &boundedSet{where: where, m: map[string]struct{}{}}
}

func (b *boundedSet) has(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.m[id]
	return ok
}

// clear forgets every ID (tests only).
func (b *boundedSet) clear() {
	b.mu.Lock()
	b.m, b.order = map[string]struct{}{}, nil
	b.mu.Unlock()
}

// add adds id; false when it was there already.
func (b *boundedSet) add(id string) bool {
	b.mu.Lock()
	if _, ok := b.m[id]; ok {
		b.mu.Unlock()
		return false
	}
	b.m[id] = struct{}{}
	b.order = append(b.order, id)
	evicted := false
	for limit := kit.MaxMapEntries(); len(b.m) > limit && len(b.order) > 0; {
		delete(b.m, b.order[0])
		b.order = b.order[1:]
		evicted = true
	}
	b.mu.Unlock()
	if evicted { // reported outside the lock: an onError callback may track another stream
		kit.LimitReached(b.where)
	}
	return true
}

// boundedMap keeps at most MaxMapEntries keys, dropping the oldest. One turn owns it.
type boundedMap[V any] struct {
	where string
	m     map[string]V
	order []string
}

func newBoundedMap[V any](where string) *boundedMap[V] {
	return &boundedMap[V]{where: where, m: map[string]V{}}
}

func (b *boundedMap[V]) get(key string) (V, bool) { v, ok := b.m[key]; return v, ok }

func (b *boundedMap[V]) len() int { return len(b.m) }

// mapEntry is a boundedMap entry.
type mapEntry[V any] struct {
	key   string
	value V
}

// entries is a copy of the entries, oldest first.
func (b *boundedMap[V]) entries() []mapEntry[V] {
	out := make([]mapEntry[V], 0, len(b.m))
	seen := make(map[string]bool, len(b.m))
	for _, key := range b.order {
		if v, ok := b.m[key]; ok && !seen[key] {
			seen[key] = true
			out = append(out, mapEntry[V]{key, v})
		}
	}
	return out
}

func (b *boundedMap[V]) delete(key string) {
	delete(b.m, key)
	b.order = slices.DeleteFunc(b.order, func(k string) bool { return k == key })
}

func (b *boundedMap[V]) clear() { b.m, b.order = map[string]V{}, nil }

func (b *boundedMap[V]) set(key string, v V) {
	if _, ok := b.m[key]; !ok {
		b.order = append(b.order, key)
	}
	b.m[key] = v
	evicted := false
	for len(b.m) > kit.MaxMapEntries() && len(b.order) > 0 {
		delete(b.m, b.order[0])
		b.order = b.order[1:]
		evicted = true
	}
	// Deleted keys leave the order list: compact it once it holds mostly stale keys.
	if len(b.order) > 2*len(b.m)+16 {
		b.order = slices.DeleteFunc(b.order, func(k string) bool { _, ok := b.m[k]; return !ok })
	}
	if evicted {
		kit.LimitReached(b.where)
	}
}
