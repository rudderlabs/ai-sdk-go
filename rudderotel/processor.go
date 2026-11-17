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
}

// coveredCalls are the OTel model-call spans a processor saw start inside a call an SDK span
// sends: recorded, never exported. The mark is shared, so every processor drops such a span at
// its end, whether or not it recognized the span itself. An entry goes when the last processor
// that marked it ends the span; ended entries stay briefly, so a processor that did not mark
// the span and ends it later still drops it.
var coveredCalls = struct {
	mu     sync.Mutex
	m      map[spanKey]map[*SpanProcessor]struct{}
	recent []spanKey
	// n mirrors len(m), so OnEnd skips the lock while nothing is covered (spans off).
	n atomic.Int64
}{m: map[spanKey]map[*SpanProcessor]struct{}{}}

// coveredRecent bounds the ended entries kept for processors that did not mark the span.
const coveredRecent = 256

func (p *SpanProcessor) markCovered(k spanKey) {
	coveredCalls.mu.Lock()
	defer coveredCalls.mu.Unlock()
	// Under the lock, so a mark never outlives this processor's Shutdown.
	if p.closed.Load() {
		return
	}
	if coveredCalls.m[k] == nil {
		coveredCalls.m[k] = map[*SpanProcessor]struct{}{}
	}
	coveredCalls.m[k][p] = struct{}{}
	coveredCalls.n.Store(int64(len(coveredCalls.m)))
}

// takeCovered is p reaching the end of a span: it reports whether any processor covered it.
func (p *SpanProcessor) takeCovered(k spanKey) bool {
	if coveredCalls.n.Load() == 0 {
		return false
	}
	coveredCalls.mu.Lock()
	defer coveredCalls.mu.Unlock()
	marked, ok := coveredCalls.m[k]
	if !ok {
		return false
	}
	if _, in := marked[p]; in {
		delete(marked, p)
		if len(marked) == 0 {
			coveredCalls.recent = append(coveredCalls.recent, k)
			for len(coveredCalls.recent) > coveredRecent {
				if old := coveredCalls.m[coveredCalls.recent[0]]; len(old) == 0 {
					delete(coveredCalls.m, coveredCalls.recent[0])
				}
				coveredCalls.recent = coveredCalls.recent[1:]
			}
			coveredCalls.n.Store(int64(len(coveredCalls.m)))
		}
	}
	return true
}

// forgetCovered drops p from every span it marked and has not ended.
func forgetCovered(p *SpanProcessor) {
	coveredCalls.mu.Lock()
	defer coveredCalls.mu.Unlock()
	for k, marked := range coveredCalls.m {
		if _, in := marked[p]; in {
			delete(marked, p)
			if len(marked) == 0 {
				delete(coveredCalls.m, k)
			}
		}
	}
	coveredCalls.n.Store(int64(len(coveredCalls.m)))
}

// otelCalls are the OTel model-call spans a processor saw start outside any SDK call span,
// while they are open: the first SDK call starting inside one adopts its IDs, and every
// processor drops it at its end. An entry goes when its last processor ends the span (or shuts
// down), the only point after which the span can no longer be adopted.
var otelCalls = struct {
	mu sync.Mutex
	m  map[spanKey]*otelCall
	// n mirrors len(m), so OnEnd skips the lock while nothing is registered (spans off).
	n atomic.Int64
}{m: map[spanKey]*otelCall{}}

type otelCall struct {
	seq         uint64
	interaction *rudderstackai.Interaction
	parentID    string
	// replaced: an SDK call adopted the span's IDs; every processor drops it at its end.
	replaced bool
	// ended: a processor reached the span's end, so the decision is final and nothing adopts it.
	ended bool
	// processors registered the span and have not reached its end yet.
	processors map[*SpanProcessor]struct{}
}

func init() {
	hook.Tracing.AdoptOTelCall = func(ctx context.Context, interaction any, activeSeq uint64, fits func(hook.OTelCall) bool) (hook.OTelCall, bool) {
		s := trace.SpanFromContext(ctx)
		sc := s.SpanContext()
		otelCalls.mu.Lock()
		defer otelCalls.mu.Unlock()
		// Checked under the lock: a span's end is observed by its processors after it stops
		// recording, so a call that sees it recording is adopted before any processor's end.
		if !s.IsRecording() {
			return hook.OTelCall{}, false
		}
		c := otelCalls.m[key(sc)]
		if c == nil || c.ended || any(c.interaction) != interaction || activeSeq > c.seq {
			return hook.OTelCall{}, false
		}
		call := hook.OTelCall{TraceID: sc.TraceID().String(), SpanID: sc.SpanID().String(), ParentID: c.parentID, Adopted: !c.replaced}
		if fits != nil && !fits(call) {
			return hook.OTelCall{}, false
		}
		c.replaced = true
		return call, true
	}
}

// dedup registers an OTel model-call span an SDK call may replace. A span starting inside an
// SDK call span is covered: it is recorded, never exported. Spans off, it is never called.
func (p *SpanProcessor) dedup(ctx context.Context, s sdktrace.ReadWriteSpan, interaction *rudderstackai.Interaction) (covered bool) {
	if interaction == nil || interaction.Ended() || spanRole(s) != "llm" {
		return false
	}
	k := key(s.SpanContext())
	if owner, owns := hook.Tracing.SDKOwnsCall(ctx, interaction); owns {
		owner.Cover()
		p.markCovered(k)
		return true
	}
	otelCalls.mu.Lock()
	defer otelCalls.mu.Unlock()
	// Under the registry lock, so a registration never outlives this processor's Shutdown.
	if p.closed.Load() {
		return false
	}
	c := otelCalls.m[k]
	if c == nil {
		parentID := ""
		if s.Parent().IsValid() {
			parentID = s.Parent().SpanID().String()
		}
		c = &otelCall{seq: hook.Tracing.NextSpanSeq(), interaction: interaction, parentID: parentID, processors: map[*SpanProcessor]struct{}{}}
		otelCalls.m[k] = c
		otelCalls.n.Store(int64(len(otelCalls.m)))
	}
	c.processors[p] = struct{}{}
	return false
}

// takeOTelCall is p reaching the end of a registered span: it reports whether an SDK call
// replaced it, and p's entry goes. The first end makes the decision final, so a span one
// processor exported can never be adopted by a call whose span another processor then drops.
func (p *SpanProcessor) takeOTelCall(k spanKey) (replaced bool) {
	if otelCalls.n.Load() == 0 {
		return false
	}
	otelCalls.mu.Lock()
	defer otelCalls.mu.Unlock()
	c := otelCalls.m[k]
	if c == nil {
		return false
	}
	if _, in := c.processors[p]; !in {
		return false
	}
	c.ended = true
	p.unregister(k, c)
	return c.replaced
}

// unregister removes p from an entry, and the entry once no processor is left. Holds the lock.
func (p *SpanProcessor) unregister(k spanKey, c *otelCall) {
	delete(c.processors, p)
	if len(c.processors) == 0 {
		delete(otelCalls.m, k)
		otelCalls.n.Store(int64(len(otelCalls.m)))
	}
}

// forgetOTelCalls drops every span a shut-down processor registered and has not ended.
func forgetOTelCalls(p *SpanProcessor) {
	otelCalls.mu.Lock()
	defer otelCalls.mu.Unlock()
	for k, c := range otelCalls.m {
		if _, in := c.processors[p]; in {
			p.unregister(k, c)
		}
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
		if interaction != nil && hook.Tracing.EmitSpans() {
			p.dedup(ctx, s, interaction)
		}
		p.remember(key(s.SpanContext()), spanState{tags: held, owner: interaction})
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
	// An SDK call that adopted this span's IDs sends the span instead: every processor drops it
	// here, synchronously, whatever its filter says.
	replaced := p.takeOTelCall(key(s.SpanContext()))
	// Likewise a span covered by a call an SDK span sends, as marked by any processor.
	covered := p.takeCovered(key(s.SpanContext()))
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
		if !replaced && !covered {
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
	forgetCovered(p)
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
