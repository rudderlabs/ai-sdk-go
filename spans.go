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
	"time"

	analytics "github.com/rudderlabs/analytics-go/v4"

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
	parent                                             *Span
	parentID                                           string
	traceState                                         string
	started                                            time.Time
	name, operation                                    string
	attributes                                         map[string]any
	interaction                                        *Interaction
	client                                             analytics.Client
	userID, anonymousID, interactionID, conversationID string
	capture                                            bool
	msgContext                                         map[string]any
}
type sdkSpanKey struct{}

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
func newSDKSpan(ctx context.Context, i *Interaction, started time.Time) *Span {
	if i != nil && i.Ended() {
		i = nil
	}
	c := cfg()
	s := &Span{started: started, interaction: i, attributes: map[string]any{}, capture: c.captureContent, client: c.analytics}
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
		s.client, s.capture = i.analytics(), i.captureContent
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
	s.msgContext, _ = stripBinary(msgContext).(map[string]any)
	s.SpanID = randomSpanID(8)
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
	s := newSDKSpan(ctx, i, started)
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

func finishPendingWrapperSpans() {
	pendingWrapperSpans.mu.Lock()
	spans := maps.Clone(pendingWrapperSpans.spans)
	pendingWrapperSpans.mu.Unlock()
	for s, metadata := range spans {
		hook.Tracing.FinishRaw(s, metadata.provider, metadata.model, 0, time.Now(), errors.New("cancelled"))
	}
}

func (s *Span) finish(name, operation, source string, a map[string]any, err error, masked bool, ended time.Time) {
	safely(func() { s.finishUnsafe(name, operation, source, a, err, masked, ended) })
}

func (s *Span) finishUnsafe(name, operation, source string, a map[string]any, err error, masked bool, ended time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	pendingWrapperSpans.mu.Lock()
	delete(pendingWrapperSpans.spans, s)
	pendingWrapperSpans.mu.Unlock()
	attrs := maps.Clone(s.attributes)
	s.mu.Unlock()
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
	safely(func() {
		if s.client == nil {
			return
		}
		m, e := spanmodel.ToTrack(n, spanmodel.Options{CaptureContent: &s.capture, ContentMasked: masked, SDKIdentity: true, UserID: s.userID, AnonymousID: productAnonymousID(s.userID, s.anonymousID), MessageContext: s.msgContext, LibraryName: LibraryName, LibraryVersion: Version})
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
	})
}
func (i *Interaction) recordWrapperCall(call LLMCall, s *Span, status int) bool {
	call.wrapperSpan = s
	call.httpStatus = status
	return i.RecordCall(call)
}
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
