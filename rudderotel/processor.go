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

// SpanProcessor adds interaction identity and optionally records product calls before export.
type SpanProcessor struct {
	inner    sdktrace.SpanProcessor
	exporter *Exporter
	options  Options
	mu       sync.Mutex
	closed   atomic.Bool
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
	return &SpanProcessor{inner: inner, exporter: exporter, options: options, states: map[spanKey]spanState{}, held: map[spanKey]spanState{}}, nil
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
		p.remember(key(s.SpanContext()), spanState{tags: held, owner: interaction})
		if IsAISpan(s) {
			for k, v := range tags {
				s.SetAttributes(attribute.String(k, v))
			}
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
			safe("tracing.productEvents", func() { recordSpan(s, p.exporter.client, state.owner) })
		}
		p.inner.OnEnd(s)
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
	err := errors.Join(waitErr, p.inner.Shutdown(ctx))
	p.mu.Lock()
	p.states = map[spanKey]spanState{}
	p.held = map[spanKey]spanState{}
	p.order = nil
	p.mu.Unlock()
	return err
}
