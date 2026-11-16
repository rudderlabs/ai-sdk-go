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

// OTelOwner holds the joined SDK spans' sends until the processor knows whether it exports
// the owning span: they run only if it does not, so the call keeps a span.
type OTelOwner struct {
	mu                sync.Mutex
	settled, exported bool
	held              []func()
}

// Defer sends now when the owner was not exported, holds the send while it is unsettled, and
// drops it when the owner was exported. Holds at most MaxMapEntries sends.
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

// Settle records whether the owning span is exported, once, and runs the held sends if not.
func (o *OTelOwner) Settle(exported bool) {
	o.mu.Lock()
	if o.settled {
		o.mu.Unlock()
		return
	}
	o.settled, o.exported = true, exported
	held := o.held
	o.held = nil
	o.mu.Unlock()
	if !exported {
		for _, send := range held {
			send()
		}
	}
}
