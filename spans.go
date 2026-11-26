package rudderstackai

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	analytics "github.com/rudderlabs/analytics-go/v4"

	"github.com/rudderlabs/ai-sdk-go/internal/capture"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
	"github.com/rudderlabs/ai-sdk-go/internal/spanmodel"
)

// SpanOptions describes an execute_tool, retrieval or invoke_agent operation.
type SpanOptions struct {
	Name, Operation string
	Attributes      map[string]any
}

// Span is an SDK span. Pass the context returned by StartSpan to nested work.
// TraceID and SpanID are immutable W3C identifiers; empty for a disabled span.
type Span struct {
	TraceID, SpanID                                    string
	mu                                                 sync.Mutex
	ended                                              bool
	reporting                                          int
	parent                                             *Span
	parentID                                           string
	traceState                                         string
	started                                            time.Time
	name, operation                                    string
	attributes                                         map[string]any
	interaction                                        *Interaction
	client                                             analytics.Client
	userID, anonymousID, interactionID, conversationID string
	// capture: span content kinds kept; llmCalls: the call event carries its content.
	capture    capture.Spans
	llmCalls   bool
	msgContext map[string]any
	// seq: start order, compared with OTel model-call spans to tell which one is nested.
	seq uint64
	// call: a provider wrapper's model call.
	call bool
	// replaced: it adopted an OTel span's IDs. covered: an OTel model-call span inside it is not
	// exported. Either way delivery sheds optional fields instead of dropping it.
	replaced bool
	covered  atomic.Bool
	// admission: set once, by the first size check to finish.
	admission atomic.Pointer[admission]
}

// admission is whether a span can be sent (it has a client and its mandatory core fits the
// budget), and that core.
type admission struct {
	sendable bool
	core     *analytics.Track
}

// canSend reports whether this span can replace or cover an OTel span: it has an analytics
// client, and its mandatory core as known now, plus room for completion, fits the span size
// budget (FIXTURE-FORMAT.md, SDK span delivery budget). The check maps the span as delivery
// does, which can call app code (the redact hook, on content) that starts a model span inside
// this call and so asks again: no lock is held, and the first answer stored wins.
func (s *Span) canSend() bool {
	a := s.admission.Load()
	if a == nil {
		core, sendable := s.admit(s.TraceID, s.SpanID, s.parentID)
		s.admission.CompareAndSwap(nil, &admission{sendable: sendable, core: core})
		a = s.admission.Load()
	}
	return a.sendable
}

func (s *Span) admit(traceID, spanID, parentID string) (*analytics.Track, bool) {
	return s.admitAt(time.Now(), traceID, spanID, parentID)
}

func (s *Span) admitAt(ended time.Time, traceID, spanID, parentID string) (*analytics.Track, bool) {
	if s.client == nil {
		return nil, false
	}
	n := s.model("chat", "chat", "wrapper", nil, nil, nil, ended)
	n.Context.TraceID, n.Context.SpanID = traceID, spanID
	n.Parent = spanmodel.Context{}
	if parentID != "" {
		n.Parent = spanmodel.Context{TraceID: traceID, SpanID: parentID}
	}
	return spanmodel.DeliveryCore(n, s.trackOptions(false))
}

func (s *Span) trackOptions(masked bool) spanmodel.Options {
	return spanmodel.Options{Capture: &s.capture, ContentMasked: masked, SDKIdentity: true, UserID: s.userID, AnonymousID: productAnonymousID(s.userID, s.anonymousID), MessageContext: s.msgContext, LibraryName: LibraryName, LibraryVersion: Version}
}

// ownsCall is the active SDK call span of interaction i that encloses ctx's innermost active SDK
// span, or is it. A manual span inside the call does not hide it; the walk stops at the
// interaction boundary.
func ownsCall(ctx context.Context, i any) *Span {
	for s := activeSDKSpan(ctx); s != nil && any(s.interaction) == i; s = s.parent {
		s.mu.Lock()
		ended := s.ended
		s.mu.Unlock()
		if !ended && s.call && s.canSend() {
			return s
		}
	}
	return nil
}

type sdkSpanKey struct{}

var spanSeq atomic.Uint64

// emitSpans mirrors the EmitSpans setting for hot paths that must not copy the config.
var emitSpans atomic.Bool

// WithEmitSpans enables wrapper and manual ai_otel_span events. Off by default,
// independent of WithLLMCallEvents; no OpenTelemetry dependency is required.
func WithEmitSpans(on bool) ConfigOption {
	return func(c *config, _ *pending) error { c.emitSpans = on; return nil }
}

func randomSpanID(bytes int) string {
	b := make([]byte, bytes)
	for {
		if _, err := rand.Read(b); err != nil {
			panic(err)
		}
		id := hex.EncodeToString(b)
		if strings.Trim(id, "0") != "" {
			return id
		}
	}
}
func validSDKTrace(id string) bool {
	if len(id) != 32 || strings.Trim(id, "0") == "" {
		return false
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
func activeSDKSpan(ctx context.Context) *Span {
	s, _ := ctx.Value(sdkSpanKey{}).(*Span)
	for s != nil {
		s.mu.Lock()
		ended := s.ended
		s.mu.Unlock()
		if !ended && (s.interaction == nil || !s.interaction.Ended()) {
			return s
		}
		s = s.parent
	}
	return nil
}

type spanContextCapture struct {
	done  bool
	value map[string]any
}

func (c *spanContextCapture) resolve(v map[string]any) map[string]any {
	if !c.done {
		c.value, _ = captureContent(v).(map[string]any)
		c.done = true
	}
	return c.value
}
func newSDKSpan(ctx context.Context, i *Interaction, started time.Time, captured ...*spanContextCapture) *Span {
	if i != nil {
		leave := i.enterCallback(false)
		defer leave()
	}
	if i != nil && i.Ended() {
		i = nil
	}
	c := cfg()
	s := &Span{started: started, interaction: i, attributes: map[string]any{}, capture: capture.Resolve(c.captureLevel()).Spans, client: c.analytics}
	parent := activeSDKSpan(ctx)
	if parent != nil && parent.interaction != i {
		parent = nil
	}
	var msgContext map[string]any
	var externalTrace, externalParent, externalState string
	if parent == nil && hook.Tracing.ParentContext != nil {
		externalTrace, externalParent, externalState = hook.Tracing.ParentContext(ctx)
		if !validSDKTrace(externalTrace) || !validSDKSpanID(externalParent) {
			externalTrace, externalParent, externalState = "", "", ""
		}
	}
	if i != nil {
		i.mu.Lock()
		if i.sdkTraceID == "" {
			i.sdkTraceID = externalTrace
			if parent != nil {
				i.sdkTraceID = parent.TraceID
			}
			if i.sdkTraceID == "" {
				i.sdkTraceID = randomSpanID(16)
			}
		}
		s.TraceID = i.sdkTraceID
		s.userID, s.anonymousID, s.interactionID, s.conversationID = i.userID, i.anonymousID, i.id, i.conversationID
		s.client, s.capture, s.llmCalls = i.analytics(), i.capture.Spans, i.capture.LLMCalls
		msgContext = i.msgContext
		i.mu.Unlock()
	} else {
		cv := valuesFrom(ctx)
		id := resolveIdentity(identityInputs{}, cv, c)
		s.userID, s.anonymousID = id.userID, id.anonymousID
		if cv != nil {
			s.conversationID = cv.conversationID
			msgContext = withSessionID(cv.context, id.sessionID)
			if cv.analytics != nil {
				s.client = cv.analytics
			}
		}
		s.TraceID = externalTrace
		if parent != nil {
			s.TraceID = parent.TraceID
		}
	}
	if s.TraceID == "" {
		s.TraceID = randomSpanID(16)
	}
	if parent != nil && parent.TraceID == s.TraceID {
		s.parent, s.parentID, s.traceState = parent, parent.SpanID, parent.traceState
	} else if externalTrace == s.TraceID {
		s.parentID, s.traceState = externalParent, externalState
	}
	if len(captured) > 0 {
		s.msgContext = captured[0].resolve(msgContext)
	} else {
		s.msgContext, _ = captureContent(msgContext).(map[string]any)
	}
	s.SpanID = randomSpanID(8)
	s.seq = spanSeq.Add(1)
	return s
}

// otelReplacementSpan is the SDK span of a model call starting inside an active OTel model-call
// span a span processor saw start in this interaction, nested inside every active SDK span. The
// first such call adopts the OTel span's trace, span and parent IDs, so RudderStack gets this
// span in its place; later calls inside are its children. The app's span is never modified.
func otelReplacementSpan(ctx context.Context, i *Interaction, started time.Time, captured *spanContextCapture) *Span {
	if i != nil {
		leave := i.enterCallback(false)
		defer leave()
	}
	if hook.Tracing.AdoptOTelCall == nil || i == nil || i.Ended() {
		return nil
	}
	active := activeSDKSpan(ctx)
	if active != nil && active.interaction != i {
		active = nil
	}
	var activeSeq uint64
	if active != nil {
		activeSeq = active.seq
	}
	// Keeps the active SDK parent, so work on a retained context still finds it once this ends.
	s := &Span{started: started, interaction: i, attributes: map[string]any{}, parent: active, seq: spanSeq.Add(1), call: true}
	i.mu.Lock()
	s.userID, s.anonymousID, s.interactionID, s.conversationID = i.userID, i.anonymousID, i.id, i.conversationID
	s.client, s.capture, s.llmCalls = i.analytics(), i.capture.Spans, i.capture.LLMCalls
	msgContext := i.msgContext
	i.mu.Unlock()

	// An SDK span that cannot be sent under the IDs it would take never replaces the OTel span:
	// the processor keeps it. Measured before the OTel span is marked replaced.
	childID := randomSpanID(8)
	var core *analytics.Track
	o, ok := hook.Tracing.AdoptOTelCall(ctx, i, activeSeq, func(o hook.OTelCall) (admitted bool) {
		s.msgContext = captured.resolve(msgContext)
		if o.Adopted {
			core, admitted = s.admit(o.TraceID, o.SpanID, o.ParentID)
		} else {
			core, admitted = s.admit(o.TraceID, childID, o.SpanID)
		}
		return admitted
	})
	if !ok {
		return nil
	}
	s.admission.Store(&admission{sendable: true, core: core})
	s.replaced = o.Adopted
	s.TraceID, s.SpanID, s.parentID = o.TraceID, o.SpanID, o.ParentID
	if !o.Adopted {
		s.SpanID, s.parentID = childID, o.SpanID
	}
	if o.Adopted {
		// The first SDK span fixes the interaction's trace, persisted across pause and JSON restore.
		i.mu.Lock()
		i.sdkTraceID = o.TraceID
		i.mu.Unlock()
	}
	return s
}

func productAnonymousID(userID, anonymousID string) string {
	if userID == "" && anonymousID == "" {
		return systemAnonymousID
	}
	return anonymousID
}

func validSDKSpanID(id string) bool {
	return len(id) == 16 && validSDKTrace(strings.Repeat("0", 16)+id)
}

// StartSpan starts a manual span. End records err without changing or wrapping it.
// When EmitSpans is disabled, it returns ctx and a no-op handle.
func StartSpan(ctx context.Context, options SpanOptions) (out context.Context, span *Span) {
	if ctx == nil {
		ctx = context.Background()
	}
	defer func() {
		if r := recover(); r != nil {
			report(KindInternal, "span.start", recovered(r))
			out, span = ctx, &Span{ended: true}
		}
	}()
	if !cfg().emitSpans {
		return ctx, &Span{ended: true}
	}
	i := FromContext(ctx)
	if i != nil && i.Ended() {
		i = nil
	}
	if isUntracked(ctx) || (i != nil && i.untracked) {
		return ctx, &Span{ended: true}
	}
	s := newSDKSpan(ctx, i, time.Now())
	s.name, s.operation = options.Name, options.Operation
	s.attributes = maps.Clone(options.Attributes)
	return context.WithValue(ctx, sdkSpanKey{}, s), s
}

// SetAttributes adds or replaces attributes until the span ends.
func (s *Span) SetAttributes(a map[string]any) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return
	}
	if s.attributes == nil {
		s.attributes = map[string]any{}
	}
	maps.Copy(s.attributes, a)
}

// End emits a manual span once. Subsequent calls are no-ops.
func (s *Span) End(err error) {
	if s == nil {
		return
	}
	s.finish(s.name, s.operation, "manual", nil, err, false, time.Time{})
}
func beginWrapperSpan(ctx context.Context, i *Interaction, started time.Time) (context.Context, *Span) {
	if !cfg().emitSpans || isUntracked(ctx) || (i != nil && !i.Ended() && i.untracked) {
		return ctx, nil
	}
	captured := &spanContextCapture{}
	s := otelReplacementSpan(ctx, i, started, captured)
	if s == nil {
		s = newSDKSpan(ctx, i, started, captured)
		s.call = true
	}
	ctx = context.WithValue(ctx, keyInteraction, s.interaction)
	pendingWrapperSpans.mu.Lock()
	pendingWrapperSpans.spans[s] = wrapperMetadata{}
	pendingWrapperSpans.mu.Unlock()
	return context.WithValue(ctx, sdkSpanKey{}, s), s
}

type wrapperMetadata struct{ provider, model string }

// Only wrapper spans are drained by Flush; manual spans remain caller-owned.
var pendingWrapperSpans = struct {
	mu    sync.Mutex
	spans map[*Span]wrapperMetadata
}{spans: make(map[*Span]wrapperMetadata)}

func finishPendingWrapperSpans(ctx context.Context) bool {
	pendingWrapperSpans.mu.Lock()
	spans := maps.Clone(pendingWrapperSpans.spans)
	pendingWrapperSpans.mu.Unlock()
	var wg sync.WaitGroup
	for s, metadata := range spans {
		done := hook.Track()
		wg.Go(func() {
			defer done()
			safely(func() {
				s.finishUnsafe(strings.TrimSpace("chat "+metadata.model), "chat", "wrapper", map[string]any{"gen_ai.provider.name": metadata.provider, "gen_ai.request.model": metadata.model, "gen_ai.response.model": metadata.model, "http.response.status_code": 0}, errors.New("cancelled"), true, time.Now(), true)
			})
		})
	}
	settled := make(chan struct{})
	go func() { wg.Wait(); close(settled) }()
	select {
	case <-settled:
		return ctx.Err() == nil
	case <-ctx.Done():
		return false
	}
}

func (s *Span) finish(name, operation, source string, a map[string]any, err error, masked bool, ended time.Time) {
	safely(func() { s.finishUnsafe(name, operation, source, a, err, masked, ended, false) })
}

func (s *Span) finishUnsafe(name, operation, source string, a map[string]any, err error, masked bool, ended time.Time, pendingOnly bool) {
	if s == nil {
		return
	}
	if s.interaction != nil {
		leave := s.interaction.enterCallback(false)
		defer leave()
	}
	s.mu.Lock()
	if s.ended || (pendingOnly && s.reporting > 0) {
		s.mu.Unlock()
		return
	}
	// Register completion before the pending entry disappears, so a concurrent
	// drain always sees either the span or its in-flight mapping and enqueue.
	done := hook.Track()
	defer done()
	s.ended = true
	pendingWrapperSpans.mu.Lock()
	delete(pendingWrapperSpans.spans, s)
	pendingWrapperSpans.mu.Unlock()
	attrs := maps.Clone(s.attributes)
	s.attributes = nil
	s.mu.Unlock()
	n := s.model(name, operation, source, attrs, a, err, ended)
	// Resolve after copying under the lock, while the interaction callback guard is active.
	n.Attributes = captureProperties(spanmodel.AdmitAttributes(n.Attributes, n.Scope.Name, s.capture))
	safely(func() { s.send(n, masked) })
}

// model is the span as a model span: the attributes and the identity it carries, its IDs, times,
// status and exception event. Delivery and the size check build it the same way.
func (s *Span) model(name, operation, source string, attrs, a map[string]any, err error, ended time.Time) spanmodel.Span {
	attrs = maps.Clone(attrs)
	if attrs == nil {
		attrs = map[string]any{}
	}
	maps.Copy(attrs, a)
	attrs["gen_ai.operation.name"], attrs["rudderstack.span_source"] = operation, source
	put(attrs, "user.id", s.userID)
	put(attrs, "rudderstack.anonymous_id", s.anonymousID)
	put(attrs, "rudderstack.interaction_id", s.interactionID)
	put(attrs, "gen_ai.conversation.id", s.conversationID)
	if ended.IsZero() {
		ended = time.Now()
	}
	if ended.Before(s.started) {
		ended = s.started
	}
	n := spanmodel.Span{Name: name, Kind: 1, Context: spanmodel.Context{TraceID: s.TraceID, SpanID: s.SpanID, TraceState: s.traceState}, Start: s.started, End: ended, StatusCode: 1, Attributes: attrs, Scope: spanmodel.Scope{Name: LibraryName, Version: Version}}
	if operation == "chat" {
		n.Kind = 3
	}
	if s.parentID != "" {
		n.Parent = spanmodel.Context{TraceID: s.TraceID, SpanID: s.parentID}
	}
	if err != nil {
		n.StatusCode = 2
		safely(func() { n.StatusMessage = err.Error() })
		n.Events = []spanmodel.Event{{Name: "exception", Time: ended, Attributes: map[string]any{"exception.type": func() string {
			if source == "wrapper" {
				return "string"
			}
			return fmt.Sprintf("%T", err)
		}(), "exception.message": n.StatusMessage}}}
	}
	return n
}

func (s *Span) send(n spanmodel.Span, masked bool) {
	if s.client == nil {
		return
	}
	options := s.trackOptions(masked)
	if a := s.admission.Load(); a != nil && (s.replaced || s.covered.Load()) {
		options.DeliveryCore = a.core
	}
	m, e := spanmodel.ToTrack(n, options)
	if e != nil {
		report(KindInternal, "span.map", e)
		return
	}
	if m != nil {
		rememberClient(s.client)
		if e := s.client.Enqueue(*m); e != nil {
			report(KindInternal, "span.track", e)
		}
	}
}
func (i *Interaction) recordWrapperCall(call LLMCall, s *Span, status int) bool {
	call.wrapperSpan = s
	call.httpStatus = status
	return i.RecordCall(call)
}

// capturesContent: the call's content is acquired, for this span or its call event.
func (s *Span) capturesContent() bool { return s.capture.Content() || s.llmCalls }

func finishWrapperSpan(s *Span, call LLMCall, content *callContent) {
	if s == nil {
		return
	}
	a := map[string]any{"gen_ai.provider.name": call.Provider, "gen_ai.request.model": first(call.RequestedModel, call.Model), "gen_ai.response.model": call.Model}
	put(a, "gen_ai.response.id", call.ResponseID)
	if !call.UsageUnknown {
		a["gen_ai.usage.input_tokens"], a["gen_ai.usage.output_tokens"] = nonNegative(call.InputTokens), nonNegative(call.OutputTokens)
	}
	if call.FinishReason != "" {
		a["gen_ai.response.finish_reasons"] = []string{call.FinishReason}
	}
	if content != nil {
		if content.inputMessages != nil {
			a["gen_ai.input.messages"] = content.inputMessages
		}
		parts := []any{}
		if content.hasOutputText {
			parts = append(parts, map[string]any{"type": "text", "content": content.outputText})
		}
		for _, raw := range content.outputToolCalls {
			t := raw.(map[string]any)
			p := map[string]any{"type": "tool_call", "name": t["name"]}
			if v, ok := t["arguments"]; ok {
				p["arguments"] = v
			}
			parts = append(parts, p)
		}
		if len(parts) > 0 {
			a["gen_ai.output.messages"] = []any{map[string]any{"role": "assistant", "parts": parts}}
		}
	}
	var err error
	if call.ErrorType != "" {
		err = fmt.Errorf("%s", call.ErrorType)
	}
	s.finish(strings.TrimSpace("chat "+first(call.RequestedModel, call.Model)), "chat", "wrapper", a, err, true, call.EndedAt)
}
func init() {
	hook.Tracing.EmitSpans = emitSpans.Load
	hook.Tracing.NextSpanSeq = func() uint64 { return spanSeq.Add(1) }
	hook.Tracing.SDKOwnsCall = func(ctx context.Context, i any) (hook.SDKCall, bool) {
		s := ownsCall(ctx, i)
		if s == nil {
			return hook.SDKCall{}, false
		}
		return hook.SDKCall{TraceID: s.TraceID, SpanID: s.SpanID, Cover: func() bool {
			// Under the span's lock, so an end either sees the mark or comes first and is seen.
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.ended {
				return false
			}
			s.covered.Store(true)
			return true
		}}, true
	}
	hook.Tracing.ReserveWrapperCompletion = func(value any) func() {
		s, _ := value.(*Span)
		if s == nil {
			return func() {}
		}
		s.mu.Lock()
		done := hook.Track()
		s.reporting++
		s.mu.Unlock()
		var once sync.Once
		return func() {
			once.Do(func() {
				s.mu.Lock()
				s.reporting--
				s.mu.Unlock()
				done()
			})
		}
	}
	hook.Tracing.WrapperCaptureContent = func(value any) bool {
		s, _ := value.(*Span)
		return s != nil && s.capturesContent()
	}
	hook.Tracing.WrapperMetadata = func(value any, provider, model string) {
		s, _ := value.(*Span)
		if s == nil {
			return
		}
		pendingWrapperSpans.mu.Lock()
		if _, pending := pendingWrapperSpans.spans[s]; pending {
			pendingWrapperSpans.spans[s] = wrapperMetadata{provider: provider, model: model}
		}
		pendingWrapperSpans.mu.Unlock()
	}
	hook.Tracing.FinishRaw = func(value any, provider, model string, status int, ended time.Time, err error) {
		s, _ := value.(*Span)
		if s == nil {
			return
		}
		s.finish(strings.TrimSpace("chat "+model), "chat", "wrapper", map[string]any{"gen_ai.provider.name": provider, "gen_ai.request.model": model, "gen_ai.response.model": model, "http.response.status_code": status}, err, true, ended)
	}

	hook.Tracing.ActivateWrapper = func(ctx context.Context, value any) context.Context {
		s, _ := value.(*Span)
		if s == nil {
			return ctx
		}
		ctx = context.WithValue(ctx, keyInteraction, s.interaction)
		return context.WithValue(ctx, sdkSpanKey{}, s)
	}
	hook.Tracing.BeginWrapper = func(ctx context.Context, i any, started time.Time) (context.Context, any) {
		var s *Span
		safely(func() { ctx, s = beginWrapperSpan(ctx, i.(*Interaction), started) })
		if s == nil {
			return ctx, nil
		}
		return ctx, s
	}
	hook.Tracing.RecordWrapper = func(i, call, s any, status int) bool {
		span, _ := s.(*Span)
		return i.(*Interaction).recordWrapperCall(call.(LLMCall), span, status)
	}
}
