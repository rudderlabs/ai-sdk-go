package rudderotel

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

type spanKey struct {
	trace trace.TraceID
	span  trace.SpanID
}

func key(s trace.SpanContext) spanKey { return spanKey{s.TraceID(), s.SpanID()} }

type spanState struct {
	tags       map[string]string
	owner      *rudderstackai.Interaction
	suppressed bool
	// covered: a model-call span inside a call an SDK span already sends; recorded, never exported.
	covered bool
}

// otelCalls are the OTel model-call spans a processor saw start outside any SDK call span,
// while they are open: an SDK call starting inside one joins it instead of sending its own span.
var otelCalls = struct {
	mu sync.Mutex
	m  map[spanKey]otelCall
	// n mirrors len(m), so OnEnd skips the lock while nothing is registered (spans off).
	n atomic.Int64
}{m: map[spanKey]otelCall{}}

type otelCall struct {
	seq         uint64
	interaction *rudderstackai.Interaction
	parentID    string
	// owner holds the joined SDK spans' sends until OnEnd knows whether the span is exported.
	owner *hook.OTelOwner
	// processor registered it; its Shutdown drops the entry so a stale span owns nothing.
	processor *SpanProcessor
}

func init() {
	hook.Tracing.OTelCallOwner = func(ctx context.Context) (hook.OTelCall, bool) {
		s := trace.SpanFromContext(ctx)
		if !s.IsRecording() {
			return hook.OTelCall{}, false
		}
		sc := s.SpanContext()
		otelCalls.mu.Lock()
		c, ok := otelCalls.m[key(sc)]
		otelCalls.mu.Unlock()
		if !ok {
			return hook.OTelCall{}, false
		}
		return hook.OTelCall{TraceID: sc.TraceID().String(), SpanID: sc.SpanID().String(), ParentID: c.parentID, Seq: c.seq, Interaction: c.interaction, Owner: c.owner}, true
	}
}

// dedup keeps one span per model call: the outer of an SDK call span and an OTel model-call
// span owns it. Spans off, it is never called.
func (p *SpanProcessor) dedup(ctx context.Context, s sdktrace.ReadWriteSpan, interaction *rudderstackai.Interaction) (covered bool) {
	if interaction == nil || interaction.Ended() || spanRole(s) != "llm" {
		return false
	}
	if hook.Tracing.SDKOwnsCall(ctx, interaction) {
		return true
	}
	// Only a span this processor will export can own a call; otherwise the SDK sends its own.
	// A panicking filter counts as a rejection.
	exported := false
	safe("tracing.filter", func() { exported = p.exports(s) })
	if !s.SpanContext().IsSampled() || !exported {
		return false
	}
	otelCalls.mu.Lock()
	defer otelCalls.mu.Unlock()
	// Bounded like the SDK's other maps: past the cap a span is simply not joined.
	if len(otelCalls.m) < max(hook.MaxMapEntries(), 1) {
		parentID := ""
		if s.Parent().IsValid() {
			parentID = s.Parent().SpanID().String()
		}
		otelCalls.m[key(s.SpanContext())] = otelCall{seq: hook.Tracing.NextSpanSeq(), interaction: interaction, parentID: parentID, owner: &hook.OTelOwner{}, processor: p}
		otelCalls.n.Store(int64(len(otelCalls.m)))
	}
	return false
}

// exports applies the exporter's filter to a span as it starts.
func (p *SpanProcessor) exports(s sdktrace.ReadOnlySpan) bool {
	if p.options.Filter != nil {
		return p.options.Filter(s)
	}
	return p.options.ExportAllSpans || IsAISpan(s)
}

// takeOTelCall removes and returns the owner p registered for a span that ended.
func (p *SpanProcessor) takeOTelCall(k spanKey) *hook.OTelOwner {
	if otelCalls.n.Load() == 0 {
		return nil
	}
	otelCalls.mu.Lock()
	defer otelCalls.mu.Unlock()
	c, ok := otelCalls.m[k]
	if !ok || c.processor != p {
		return nil
	}
	delete(otelCalls.m, k)
	otelCalls.n.Store(int64(len(otelCalls.m)))
	return c.owner
}

// forgetOTelCalls drops every span a shut-down processor registered. They will not be
// exported, so calls they owned send their own spans.
func forgetOTelCalls(p *SpanProcessor) {
	if otelCalls.n.Load() == 0 {
		return
	}
	var owners []*hook.OTelOwner
	otelCalls.mu.Lock()
	for k, c := range otelCalls.m {
		if c.processor == p {
			delete(otelCalls.m, k)
			owners = append(owners, c.owner)
		}
	}
	otelCalls.n.Store(int64(len(otelCalls.m)))
	otelCalls.mu.Unlock()
	for _, o := range owners {
		safe("tracing.owner", func() { o.Settle(false) })
	}
}

// SpanProcessor adds interaction identity and optionally records product calls before export.
type SpanProcessor struct {
	inner      sdktrace.SpanProcessor
	exporter   *Exporter
	options    Options
	productNow func() time.Time
	mu         sync.Mutex
	closed     atomic.Bool
	// Go has no weak-key map for interface spans. Bounded identities allow descendants of
	// already-ended non-AI spans to inherit without retaining span objects themselves.
	states map[spanKey]spanState
	held   map[spanKey]spanState
	order  []spanKey
}

var _ sdktrace.SpanProcessor = (*SpanProcessor)(nil)

// NewSpanProcessor creates a processor for sdktrace.WithSpanProcessor.
func NewSpanProcessor(options Options) (*SpanProcessor, error) {
	exporter, err := NewExporter(options)
	if err != nil {
		return nil, err
	}
	var inner sdktrace.SpanProcessor
	if options.Simple {
		inner = sdktrace.NewSimpleSpanProcessor(exporter)
	} else {
		inner = sdktrace.NewBatchSpanProcessor(exporter, options.BatchOptions...)
	}
	p := &SpanProcessor{inner: inner, exporter: exporter, options: options, states: map[spanKey]spanState{}, held: map[spanKey]spanState{}}
	return p, nil
}

func (p *SpanProcessor) state(k spanKey) spanState {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.states[k]; ok {
		return s
	}
	return p.held[k]
}

func (p *SpanProcessor) remember(k spanKey, s spanState) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.states[k] = s
}

func (p *SpanProcessor) finish(k spanKey) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, exists := p.states[k]
	delete(p.states, k)
	if !exists || p.closed.Load() {
		return
	}
	// Active spans retain suppression and pending tags until End, even when the
	// bounded cache of ended parents fills up.
	held := spanState{tags: map[string]string{}, suppressed: s.suppressed}
	for _, k := range []string{"user.id", "session.id"} {
		if v := s.tags[k]; v != "" {
			held.tags[k] = v
		}
	}
	if len(held.tags) == 0 && !held.suppressed {
		return
	}
	limit := max(hook.MaxMapEntries(), 1)
	for len(p.order) >= limit {
		delete(p.held, p.order[0])
		p.order = p.order[1:]
	}
	p.held[k] = held
	p.order = append(p.order, k)
}

// OnStart tags only AI spans; non-AI identity is kept aside for AI descendants.
func (p *SpanProcessor) OnStart(ctx context.Context, s sdktrace.ReadWriteSpan) {
	if p.closed.Load() {
		return
	}
	safe("tracing.onStart", func() {
		traceID := ""
		if p.options.LinkTraceID {
			traceID = s.SpanContext().TraceID().String()
		}
		tags, system, suppressed, owner := hook.Tracing.Identity(ctx, traceID)
		parent := trace.SpanFromContext(ctx)
		inherited := p.state(key(parent.SpanContext()))
		suppressed = suppressed || inherited.suppressed
		if suppressed {
			p.remember(key(s.SpanContext()), spanState{suppressed: true})
			return
		}
		if !system && (p.options.PropagateIdentity == nil || *p.options.PropagateIdentity) {
			parentAttrs := map[string]attribute.Value{}
			if readable, ok := parent.(sdktrace.ReadOnlySpan); ok {
				parentAttrs = attributes(readable.Attributes())
			}
			for _, k := range []string{"user.id", "session.id"} {
				if _, exists := tags[k]; exists {
					continue
				}
				v := asString(parentAttrs[k])
				if v == "" {
					v = inherited.tags[k]
				}
				if v != "" {
					tags[k] = v
				}
			}
		}
		own := attributes(s.Attributes())
		for k := range tags {
			if _, exists := own[k]; exists {
				delete(tags, k)
			}
		}
		var interaction *rudderstackai.Interaction
		if owner != nil {
			interaction = owner.(*rudderstackai.Interaction)
		}
		if p.options.ProductEvents && interaction != nil && (firstString(own, InteractionIDAttribute) == "" || firstString(own, InteractionIDAttribute) == interaction.ID()) && spanRole(s) != "" && !interaction.Ended() {
			interaction.Resume()
		}
		// Preserve the own identity too, for child spans.
		held := make(map[string]string, len(tags)+2)
		for k, v := range tags {
			held[k] = v
		}
		for _, k := range []string{"user.id", "session.id"} {
			if v := asString(own[k]); v != "" {
				held[k] = v
			}
		}
		if IsAISpan(s) {
			for k, v := range tags {
				s.SetAttributes(attribute.String(k, v))
			}
		}
		// After tagging, so a filter that reads user.id sees the tagged span.
		covered := interaction != nil && hook.Tracing.EmitSpans() && p.dedup(ctx, s, interaction)
		p.remember(key(s.SpanContext()), spanState{tags: held, owner: interaction, covered: covered})
		if p.options.ProductEvents {
			p.recordAncestry(s, interaction)
		}
		p.inner.OnStart(ctx, s)
	})
}

// taggedSpan adds late identity to the exported snapshot without mutating other exporters' spans.
type taggedSpan struct {
	sdktrace.ReadOnlySpan
	attrs []attribute.KeyValue
}

func (s taggedSpan) Attributes() []attribute.KeyValue { return s.attrs }

// OnEnd records only spans of an open interaction and skips suppressed classifier spans.
func (p *SpanProcessor) OnEnd(s sdktrace.ReadOnlySpan) {
	defer p.finish(key(s.SpanContext()))
	// An owner always settles: unless the filter accepts the finished span (a panic, a closed
	// processor or any failure counts as a rejection), its joined calls send their own spans.
	exported := false
	if owner := p.takeOTelCall(key(s.SpanContext())); owner != nil {
		defer safe("tracing.owner", func() { owner.Settle(exported) })
		if !p.closed.Load() {
			defer safe("tracing.filter", func() { exported = p.exports(s) })
		}
	}
	if p.closed.Load() {
		return
	}
	safe("tracing.onEnd", func() {
		state := p.state(key(s.SpanContext()))
		if state.suppressed {
			return
		}
		if IsAISpan(s) && len(state.tags) > 0 {
			attrs := slices.Clone(s.Attributes())
			own := attributes(attrs)
			for k, v := range state.tags {
				if _, exists := own[k]; !exists {
					attrs = append(attrs, attribute.String(k, v))
				}
			}
			s = taggedSpan{ReadOnlySpan: s, attrs: attrs}
		}
		if p.options.ProductEvents {
			safe("tracing.productEvents", func() { p.recordAncestry(s, state.owner); recordSpan(s, p.exporter.client, state.owner) })
		}
		if !state.covered {
			p.inner.OnEnd(s)
		}
	})
}

func flushContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, 10*time.Second)
}

// ForceFlush waits for classifications, exports the batch, then drains the client queue.
func (p *SpanProcessor) ForceFlush(ctx context.Context) error {
	ctx, cancel := flushContext(ctx)
	defer cancel()
	var waitErr error
	if !hook.Tracing.Wait(ctx) {
		waitErr = ctx.Err()
	}
	return errors.Join(waitErr, p.inner.ForceFlush(ctx), p.exporter.ForceFlush(ctx))
}

// Shutdown waits for classifications and exports pending spans, without ending interactions.
func (p *SpanProcessor) Shutdown(ctx context.Context) error {
	ctx, cancel := flushContext(ctx)
	defer cancel()
	var waitErr error
	if !hook.Tracing.Wait(ctx) {
		waitErr = ctx.Err()
	}
	p.closed.Store(true)
	forgetOTelCalls(p)
	err := errors.Join(waitErr, p.inner.Shutdown(ctx))
	p.mu.Lock()
	p.states = map[spanKey]spanState{}
	p.held = map[spanKey]spanState{}
	p.order = nil
	p.mu.Unlock()
	return err
}

func (p *SpanProcessor) recordAncestry(s sdktrace.ReadOnlySpan, owner *rudderstackai.Interaction) {
	id := firstString(attributes(s.Attributes()), InteractionIDAttribute)
	if id != "" && (owner == nil || owner.ID() != id) {
		owner = nil
		if found := hook.Tracing.Open(id); found != nil {
			owner = found.(*rudderstackai.Interaction)
		}
	}
	if owner == nil {
		return
	}
	parent := ""
	if s.Parent().IsValid() {
		parent = s.Parent().SpanID().String()
	}
	hook.Tracing.RecordAncestry(owner, s.SpanContext().TraceID().String(), s.SpanContext().SpanID().String(), parent)
}
