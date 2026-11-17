package rudderotel

import (
	"context"
	"sync"
	"sync/atomic"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

// otelDelivery holds what export needs to place a covered OTel model span's subtree under its
// SDK owner, shared by every processor and exporter. Each map keeps at most MaxMapEntries,
// oldest out. Recorded only while EmitSpans is on.
var otelDelivery = struct {
	mu sync.Mutex
	// owners: covered model span to the SDK call span that sends it.
	owners boundedMap[hook.SDKCall]
	// parents: each span's app parent, recorded at start.
	parents boundedMap[spanKey]
	// rawSent: spans already sent under their original IDs. published: their parents, which are
	// never covered after that, so a published tree keeps its parent.
	rawSent, published boundedMap[struct{}]
	// owned mirrors len(owners), so export skips resolution while nothing is covered.
	owned atomic.Int64
}{}

type boundedMap[V any] struct {
	m     map[spanKey]V
	order []spanKey
}

func (b *boundedMap[V]) put(k spanKey, v V) {
	if b.m == nil {
		b.m = map[spanKey]V{}
	}
	if _, exists := b.m[k]; !exists {
		for len(b.order) >= max(hook.MaxMapEntries(), 1) {
			delete(b.m, b.order[0])
			b.order = b.order[1:]
		}
		b.order = append(b.order, k)
	}
	b.m[k] = v
}

func (b *boundedMap[V]) get(k spanKey) (V, bool) {
	v, ok := b.m[k]
	return v, ok
}

// recordParent remembers a span's app parent, as OTel set it from ctx.
func recordParent(ctx context.Context, s sdktrace.ReadWriteSpan) {
	parent := trace.SpanContextFromContext(ctx)
	if !parent.IsValid() || s.Parent().SpanID() != parent.SpanID() || s.Parent().TraceID() != parent.TraceID() {
		return
	}
	k, pk := key(s.SpanContext()), key(parent)
	otelDelivery.mu.Lock()
	defer otelDelivery.mu.Unlock()
	otelDelivery.parents.put(k, pk)
	if _, sent := otelDelivery.rawSent.get(k); sent {
		otelDelivery.published.put(pk, struct{}{})
	}
}

// cover records that model span k is not exported and its SDK owner sends it, unless a child was
// already sent under k: that tree keeps k (buffered children still resolve at export).
func cover(k spanKey, owner hook.SDKCall) bool {
	otelDelivery.mu.Lock()
	defer otelDelivery.mu.Unlock()
	if _, published := otelDelivery.published.get(k); published && (k.trace.String() != owner.TraceID || k.span.String() != owner.SpanID) {
		return false
	}
	otelDelivery.owners.put(k, owner)
	otelDelivery.owned.Store(int64(len(otelDelivery.owners.m)))
	return true
}

// deliveredSpan is a span as RudderStack exports it: in a covered subtree, its trace is the SDK
// owner's and a direct child of the covered span has the owner as parent. Resolved only when the
// exporter sends it (a batch processor reads SpanContext earlier), so a child buffered before its
// parent was covered still moves. Other processors get the original span.
type deliveredSpan struct {
	sdktrace.ReadOnlySpan
	once     sync.Once
	done     atomic.Bool
	sc, psc  trace.SpanContext
	original bool
}

func newDeliveredSpan(s sdktrace.ReadOnlySpan) *deliveredSpan {
	return &deliveredSpan{ReadOnlySpan: s}
}

// resolve fixes the exported IDs; the exporter calls it right before sending.
func (d *deliveredSpan) resolve() {
	d.once.Do(func() {
		defer d.done.Store(true)
		d.sc, d.psc, d.original = d.ReadOnlySpan.SpanContext(), d.ReadOnlySpan.Parent(), true
		if otelDelivery.owned.Load() == 0 {
			return
		}
		k := key(d.sc)
		otelDelivery.mu.Lock()
		defer otelDelivery.mu.Unlock()
		parent, hasParent := otelDelivery.parents.get(k)
		owner, found := hook.SDKCall{}, false
		seen := map[spanKey]bool{}
		for current := k; !found && !seen[current]; {
			seen[current] = true
			if owner, found = otelDelivery.owners.get(current); found {
				break
			}
			next, ok := otelDelivery.parents.get(current)
			if !ok {
				break
			}
			current = next
		}
		if !found {
			return
		}
		traceID, err := trace.TraceIDFromHex(owner.TraceID)
		if err != nil {
			return
		}
		d.original = false
		d.sc = d.sc.WithTraceID(traceID)
		if !d.psc.IsValid() {
			return
		}
		d.psc = d.psc.WithTraceID(traceID)
		if _, covered := otelDelivery.owners.get(parent); hasParent && covered {
			if spanID, err := trace.SpanIDFromHex(owner.SpanID); err == nil {
				d.psc = d.psc.WithSpanID(spanID)
			}
		}
	})
}

func (d *deliveredSpan) SpanContext() trace.SpanContext {
	if d.done.Load() {
		return d.sc
	}
	return d.ReadOnlySpan.SpanContext()
}

func (d *deliveredSpan) Parent() trace.SpanContext {
	if d.done.Load() {
		return d.psc
	}
	return d.ReadOnlySpan.Parent()
}

// sentRaw records a span sent under its original IDs, and its parent as published.
func sentRaw(s sdktrace.ReadOnlySpan) {
	if d, ok := s.(*deliveredSpan); ok && !d.original {
		return
	}
	k := key(s.SpanContext())
	otelDelivery.mu.Lock()
	defer otelDelivery.mu.Unlock()
	otelDelivery.rawSent.put(k, struct{}{})
	if parent, ok := otelDelivery.parents.get(k); ok {
		otelDelivery.published.put(parent, struct{}{})
	}
}
