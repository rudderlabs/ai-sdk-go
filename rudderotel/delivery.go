package rudderotel

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

// otelSpans is the dedup state every RudderStack processor shares, one entry per OTel span that
// takes part: an outer model-call span an SDK call may adopt, an inner one an SDK call covers,
// the descendants of a covered span (they export under its SDK owner), and an open model-call
// span an earlier processor started a child under before RudderStack saw it start.
//
// An entry is open from the span's start to its first end, or until every processor that
// started it shut down, and an open entry is never evicted. At most Limits.MaxMapEntries entries
// are open: a model-call span starting while they are full gets none, so it is never covered or
// adopted and exports as without dedup. Descendants of a covered span always get one, so its tree
// stays intact. At the span's end, an entry later readers may need moves to the ended entries, at
// most MaxMapEntries, oldest out: a replaced or covered span, for processors that end it after
// the first, and a covered tree, for children started after their parent ended. Export reads no
// map: a delivered span holds its entry, and an entry its parent's.
//
// No app code runs while mu is held: an SDK call's size check, whose mapper can call the redact
// hook, runs before the lock, and the span's state is checked again under it.
var otelSpans = struct {
	mu    sync.Mutex
	open  map[spanKey]*otelSpan
	ended map[spanKey]*otelSpan
	// order is the ended keys, oldest first.
	order []spanKey
	// unseen counts open entries of spans no RudderStack processor saw start.
	unseen int
	// n mirrors len(open)+len(ended), so a span's end skips the lock while nothing takes part.
	n atomic.Int64
}{open: map[spanKey]*otelSpan{}, ended: map[spanKey]*otelSpan{}}

type otelSpan struct {
	// parent is the app parent's entry, for a span in a tree.
	parent *otelSpan
	// tree: a covered span, a descendant of one, or a span no processor saw start and its
	// descendants. Children of a tree entry get entries too.
	tree bool
	// covered: an SDK call span sends the span, so every processor drops it at its end, and its
	// descendants export in the owner's trace, its direct children under the owner.
	covered    bool
	ownerTrace trace.TraceID
	ownerSpan  trace.SpanID
	// published: a child was sent under the span's original IDs, so it is never covered.
	published bool
	// decided: a processor saw the span start and made the dedup decision.
	decided bool
	// candidate: an outer model-call span the first SDK call of interaction starting inside it
	// adopts, while open, unless that call's active SDK span started after seq.
	candidate   bool
	seq         uint64
	interaction *rudderstackai.Interaction
	parentID    string
	// replaced: an SDK call adopted the span's IDs; every processor drops it at its end.
	replaced bool
	// unseen is the span of an open entry no RudderStack processor saw start.
	unseen trace.Span
	// processors that started the span (or tracked it) and have not shut down.
	processors []*SpanProcessor
}

func (e *otelSpan) join(p *SpanProcessor) {
	if !slices.Contains(e.processors, p) {
		e.processors = append(e.processors, p)
	}
}

// errDedupFull is reported, as limit_reached, when a span gets no entry because MaxMapEntries
// entries are open.
var errDedupFull = errors.New("rudderotel: MaxMapEntries open spans tracked for span dedup; new spans export without dedup")

func reportFull() {
	hook.Report(rudderstackai.KindLimitReached, "MaxMapEntries (span dedup)", errDedupFull)
}

// lookup is k's entry, open or ended. Holds the lock.
func lookup(k spanKey) *otelSpan {
	if e := otelSpans.open[k]; e != nil {
		return e
	}
	return otelSpans.ended[k]
}

// room reports whether a new open entry fits within limit. A full map first drops the entries
// of spans no processor saw start that ended meanwhile. Holds the lock.
func room(limit int) bool {
	if len(otelSpans.open) < limit {
		return true
	}
	if otelSpans.unseen > 0 {
		for k, e := range otelSpans.open {
			if e.unseen != nil && !e.unseen.IsRecording() {
				release(k, e, limit)
			}
		}
	}
	return len(otelSpans.open) < limit
}

// release closes k's open entry, keeping it among the ended entries when later readers may need
// it. Holds the lock.
func release(k spanKey, e *otelSpan, limit int) {
	delete(otelSpans.open, k)
	if e.unseen != nil {
		e.unseen = nil
		otelSpans.unseen--
	}
	e.interaction, e.processors = nil, nil
	if e.covered || e.replaced || e.tree {
		if _, kept := otelSpans.ended[k]; !kept {
			otelSpans.order = append(otelSpans.order, k)
		}
		otelSpans.ended[k] = e
		for len(otelSpans.order) > limit {
			delete(otelSpans.ended, otelSpans.order[0])
			otelSpans.order = otelSpans.order[1:]
		}
	}
	countEntries()
}

// countEntries refreshes n. Holds the lock.
func countEntries() { otelSpans.n.Store(int64(len(otelSpans.open) + len(otelSpans.ended))) }

func limit() int { return max(hook.MaxMapEntries(), 1) }

// isModelSpan reports whether s is a model-call span, the only kind that can be covered.
func isModelSpan(s trace.Span) bool {
	r, ok := s.(sdktrace.ReadOnlySpan)
	return ok && spanRole(r) == "llm"
}

// track gives s an entry in its app parent's tree, as OTel set the parent from ctx, when that
// parent is covered or in a tree. The parent gets one too when it is an open model-call span no
// processor saw start yet: an earlier processor is starting children inside its own OnStart, and
// its later coverage, or a child already sent under it, decides where they export.
func (p *SpanProcessor) track(ctx context.Context, s sdktrace.ReadWriteSpan) {
	parentSpan := trace.SpanFromContext(ctx)
	parent := parentSpan.SpanContext()
	if !parent.IsValid() || s.Parent().SpanID() != parent.SpanID() || s.Parent().TraceID() != parent.TraceID() {
		return
	}
	k, pk := key(s.SpanContext()), key(parent)
	unseen := !p.started(pk) && parentSpan.IsRecording() && isModelSpan(parentSpan)
	if otelSpans.n.Load() == 0 && !unseen {
		return
	}
	limit := limit()
	if !p.trackLocked(k, pk, parentSpan, unseen, limit) {
		reportFull()
	}
}

// trackLocked is track under the lock; false when the parent needed an entry and none was free.
func (p *SpanProcessor) trackLocked(k, pk spanKey, parentSpan trace.Span, unseen bool, limit int) bool {
	otelSpans.mu.Lock()
	defer otelSpans.mu.Unlock()
	// Under the lock, so an entry never outlives this processor's Shutdown.
	if p.closed.Load() {
		return true
	}
	if e := otelSpans.open[k]; e != nil {
		e.join(p)
		return true
	}
	pe := lookup(pk)
	switch {
	case pe == nil && !unseen:
		return true
	case pe == nil:
		if !room(limit) {
			return false
		}
		pe = &otelSpan{tree: true, unseen: parentSpan, processors: []*SpanProcessor{p}}
		otelSpans.open[pk] = pe
		otelSpans.unseen++
	case !pe.tree:
		return true
	}
	// A descendant of a covered span: always admitted, it is what keeps the tree intact.
	otelSpans.open[k] = &otelSpan{parent: pe, tree: true, processors: []*SpanProcessor{p}}
	countEntries()
	return true
}

// dedup registers a model-call span an SDK call may replace. A span starting inside an SDK call
// span is covered: it is recorded, never exported. Otherwise the first SDK call starting inside
// it while it is open adopts it. The first processor to see the span start decides; one that
// finds no free entry leaves the span as without dedup. Spans off, it is never called.
func (p *SpanProcessor) dedup(ctx context.Context, s sdktrace.ReadWriteSpan, interaction *rudderstackai.Interaction) {
	if interaction == nil || interaction.Ended() || spanRole(s) != "llm" {
		return
	}
	// Before the lock: finding the owner runs the call's size check, which can run app code.
	var owner *hook.SDKCall
	if o, owns := hook.Tracing.SDKOwnsCall(ctx, interaction); owns {
		owner = &o
	}
	parentID := ""
	if s.Parent().IsValid() {
		parentID = s.Parent().SpanID().String()
	}
	if !p.dedupLocked(s, parentID, owner, interaction, limit()) {
		reportFull()
	}
}

// dedupLocked is dedup under the lock, for span s with parent span ID parentID; false when the
// span needed an entry and none was free.
func (p *SpanProcessor) dedupLocked(s trace.Span, parentID string, owner *hook.SDKCall, interaction *rudderstackai.Interaction, limit int) bool {
	otelSpans.mu.Lock()
	defer otelSpans.mu.Unlock()
	// Under the lock, so an entry never outlives this processor's Shutdown. A span that app code
	// ended during the owner's size check is left as without dedup.
	if p.closed.Load() || !s.IsRecording() {
		return true
	}
	k := key(s.SpanContext())
	e := otelSpans.open[k]
	switch {
	case e != nil && e.decided:
		e.join(p)
		return true
	case e == nil:
		if !room(limit) {
			return false
		}
		e = &otelSpan{}
		otelSpans.open[k] = e
	case e.unseen != nil:
		e.unseen = nil
		otelSpans.unseen--
	}
	defer countEntries()
	e.join(p)
	e.decided = true
	if owner == nil {
		e.candidate, e.seq, e.interaction, e.parentID = true, hook.Tracing.NextSpanSeq(), interaction, parentID
		return true
	}
	ownerTrace, traceErr := trace.TraceIDFromHex(owner.TraceID)
	ownerSpan, spanErr := trace.SpanIDFromHex(owner.SpanID)
	// A processor before this one may already have sent a child under the span: keep it. A call
	// that ended since its size check may have sent its span without the admitted core: it
	// covers nothing.
	if traceErr != nil || spanErr != nil || (e.published && (k.trace != ownerTrace || k.span != ownerSpan)) || !owner.Cover() {
		return true
	}
	e.covered, e.tree, e.ownerTrace, e.ownerSpan = true, true, ownerTrace, ownerSpan
	return true
}

func init() { hook.Tracing.AdoptOTelCall = adoptOTelCall }

// adoptOTelCall is hook.Tracing.AdoptOTelCall. fits measures the SDK span through the shared
// mapper, which can call app code (the redact hook, on content) that ends the span or starts
// others, so it runs with no lock held: the span is read before it and checked again after it,
// under the lock, and a span whose state changed meanwhile is not adopted and exports as without
// dedup.
func adoptOTelCall(ctx context.Context, interaction any, activeSeq uint64, fits func(hook.OTelCall) bool) (hook.OTelCall, bool) {
	s := trace.SpanFromContext(ctx)
	otelSpans.mu.Lock()
	e, call, ok := adoptable(s, interaction, activeSeq)
	otelSpans.mu.Unlock()
	if !ok || (fits != nil && !fits(call)) {
		return hook.OTelCall{}, false
	}
	otelSpans.mu.Lock()
	defer otelSpans.mu.Unlock()
	if again, now, ok := adoptable(s, interaction, activeSeq); !ok || again != e || now != call {
		return hook.OTelCall{}, false
	}
	e.replaced = true
	return call, true
}

// adoptable is the open entry of s an SDK call of interaction, nested inside the active SDK span
// with start order activeSeq, may adopt, and the call it would make. Holds the lock.
func adoptable(s trace.Span, interaction any, activeSeq uint64) (*otelSpan, hook.OTelCall, bool) {
	// Checked under the lock: a span's end is observed by its processors after it stops
	// recording, so a call that sees it recording is adopted before any processor's end.
	if !s.IsRecording() {
		return nil, hook.OTelCall{}, false
	}
	sc := s.SpanContext()
	e := otelSpans.open[key(sc)]
	if e == nil || !e.candidate || any(e.interaction) != interaction || activeSeq > e.seq {
		return nil, hook.OTelCall{}, false
	}
	return e, hook.OTelCall{TraceID: sc.TraceID().String(), SpanID: sc.SpanID().String(), ParentID: e.parentID, Adopted: !e.replaced}, true
}

// endSpan is a processor reaching s's end, whether or not it saw the span start. The first end
// closes the span's entry, so the decision is final: a span one processor exported is never
// adopted by a call whose span another drops. It reports whether an SDK call span sends the span
// instead (replaced or covered), and the entry delivery resolves from: the span's own, or else
// its parent's.
func endSpan(s sdktrace.ReadOnlySpan) (drop bool, own, parent *otelSpan) {
	if otelSpans.n.Load() == 0 {
		return false, nil, nil
	}
	k := key(s.SpanContext())
	limit := limit()
	otelSpans.mu.Lock()
	defer otelSpans.mu.Unlock()
	if own = otelSpans.open[k]; own != nil {
		release(k, own, limit)
	} else if own = otelSpans.ended[k]; own == nil {
		if s.Parent().IsValid() {
			if pe := lookup(key(s.Parent())); pe != nil && pe.tree {
				parent = pe
			}
		}
		return false, nil, parent
	}
	return own.covered || own.replaced, own, nil
}

// forgetSpans closes the open entries no live processor that started them is left for: they
// are never adopted, and nothing of theirs ends them.
func forgetSpans(p *SpanProcessor) {
	limit := limit()
	otelSpans.mu.Lock()
	defer otelSpans.mu.Unlock()
	for k, e := range otelSpans.open {
		if i := slices.Index(e.processors, p); i >= 0 {
			e.processors = slices.Delete(e.processors, i, i+1)
			if len(e.processors) == 0 {
				release(k, e, limit)
			}
		}
	}
}

// deliveredSpan is a span as RudderStack exports it: in a covered subtree, its trace is the SDK
// owner's and a direct child of the covered span has the owner as parent. Resolved only when the
// exporter sends it, after the app's filter (a batch processor reads SpanContext earlier), so a
// child buffered before its parent was covered still moves. Other processors get the original
// span.
type deliveredSpan struct {
	sdktrace.ReadOnlySpan
	// own is the span's entry; without one, parent is its parent's.
	own, parent *otelSpan
	once        sync.Once
	done        atomic.Bool
	sc, psc     trace.SpanContext
	original    bool
}

func newDeliveredSpan(s sdktrace.ReadOnlySpan, own, parent *otelSpan) *deliveredSpan {
	return &deliveredSpan{ReadOnlySpan: s, own: own, parent: parent}
}

// treeParent is the entry of the span's app parent, if it has one.
func (d *deliveredSpan) treeParent() *otelSpan {
	if d.own != nil {
		return d.own.parent
	}
	return d.parent
}

// resolve fixes the exported IDs; the exporter calls it right before sending.
func (d *deliveredSpan) resolve() {
	d.once.Do(func() {
		defer d.done.Store(true)
		d.sc, d.psc, d.original = d.ReadOnlySpan.SpanContext(), d.ReadOnlySpan.Parent(), true
		parent := d.treeParent()
		if parent == nil {
			return
		}
		otelSpans.mu.Lock()
		defer otelSpans.mu.Unlock()
		root := parent
		for root != nil && !root.covered {
			root = root.parent
		}
		if root == nil {
			return
		}
		d.original = false
		d.sc = d.sc.WithTraceID(root.ownerTrace)
		if !d.psc.IsValid() {
			return
		}
		d.psc = d.psc.WithTraceID(root.ownerTrace)
		if parent == root {
			d.psc = d.psc.WithSpanID(root.ownerSpan)
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

// sentRaw records a span sent under its original IDs: its parent is never covered after that,
// so a published tree keeps its parent.
func sentRaw(s sdktrace.ReadOnlySpan) {
	d, ok := s.(*deliveredSpan)
	if !ok || !d.original {
		return
	}
	parent := d.treeParent()
	if parent == nil {
		return
	}
	otelSpans.mu.Lock()
	defer otelSpans.mu.Unlock()
	parent.published = true
}
