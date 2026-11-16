package hook

import "sync"

// OTelCall is an active OTel model-call span a span processor registered: an SDK call
// starting inside it joins it instead of sending its own span.
type OTelCall struct {
	TraceID, SpanID, ParentID string
	Seq                       uint64
	Interaction               any
	Owner                     *OTelOwner
}

// OTelOwner holds the joined SDK spans' sends until every span processor that registered the
// owning span has decided whether it exports it: they run once, only if none did, so the call
// keeps a span.
type OTelOwner struct {
	mu                sync.Mutex
	registered        int
	settled, exported bool
	held              []func()
}

// Register counts one more processor that decides on the owning span.
func (o *OTelOwner) Register() {
	o.mu.Lock()
	if !o.settled {
		o.registered++
	}
	o.mu.Unlock()
}

// Defer sends now when the owner settled with no exporter, holds the send while it is unsettled,
// and drops it when a processor exported the owner. Holds at most MaxMapEntries sends.
func (o *OTelOwner) Defer(send func()) {
	o.mu.Lock()
	if !o.settled {
		if len(o.held) < max(MaxMapEntries(), 1) {
			o.held = append(o.held, send)
		}
		o.mu.Unlock()
		return
	}
	exported := o.exported
	o.mu.Unlock()
	if !exported {
		send()
	}
}

// Settle records one registered processor's decision. Once all have decided, the held sends
// run if none exported the owner.
func (o *OTelOwner) Settle(exported bool) {
	o.mu.Lock()
	if o.settled {
		o.mu.Unlock()
		return
	}
	o.exported = o.exported || exported
	if o.registered--; o.registered > 0 {
		o.mu.Unlock()
		return
	}
	o.settled = true
	held, anyExported := o.held, o.exported
	o.held = nil
	o.mu.Unlock()
	if !anyExported {
		for _, send := range held {
			send()
		}
	}
}
