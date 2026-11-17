package rudderstackai

import (
	"cmp"
	"container/list"
	"errors"
	"fmt"
	"sync"
	"time"
	"weak"
)

// Limits caps the memory the SDK holds. A zero field keeps its default. Identical defaults in
// Node.js and Python.
type Limits struct {
	// MaxOpenInteractions is how many interactions may be open or paused at once (default 10,000);
	// a paused run holds its slot until it ends. Past it,
	// new interactions are not tracked: your code runs as usual and limit_reached is reported.
	MaxOpenInteractions int
	// InteractionTTL is how long an interaction may stay open (default 1 hour). Past it, the next
	// start or end of any interaction ends it with response_status "incomplete" and end_reason
	// "ttl_expired".
	InteractionTTL time.Duration
	// MaxCallsPerInteraction caps the model calls and the tool calls kept per interaction
	// (default 1,000 each). Past it, totals keep counting; per-call detail is no longer kept.
	MaxCallsPerInteraction int
	// MaxMapEntries caps each of the SDK's own lookup maps (default 10,000): remembered clients,
	// failed attempts held for a retry, and each MCP map per server. The oldest entries go first.
	// It also caps the open model-call spans rudderotel tracks for span dedup: once full, a new
	// one exports without dedup, and none is evicted.
	MaxMapEntries int
}

// defaultLimits are the limits without [WithLimits].
var defaultLimits = Limits{
	MaxOpenInteractions:    10_000,
	InteractionTTL:         time.Hour,
	MaxCallsPerInteraction: 1_000,
	MaxMapEntries:          10_000,
}

// WithLimits sets the memory caps. A zero field keeps its default; a negative one is an error.
func WithLimits(l Limits) ConfigOption {
	return func(c *config, _ *pending) error {
		if l.MaxOpenInteractions < 0 || l.InteractionTTL < 0 || l.MaxCallsPerInteraction < 0 || l.MaxMapEntries < 0 {
			return errors.New("rudderstackai: Limits fields must not be negative")
		}
		d := defaultLimits
		c.limits = Limits{
			MaxOpenInteractions:    cmp.Or(l.MaxOpenInteractions, d.MaxOpenInteractions),
			InteractionTTL:         cmp.Or(l.InteractionTTL, d.InteractionTTL),
			MaxCallsPerInteraction: cmp.Or(l.MaxCallsPerInteraction, d.MaxCallsPerInteraction),
			MaxMapEntries:          cmp.Or(l.MaxMapEntries, d.MaxMapEntries),
		}
		return nil
	}
}

// limitReached reports a cap that was hit.
func limitReached(name string) {
	report(KindLimitReached, name, fmt.Errorf("rudderstackai: %s reached", name))
}

// pausedSlot identifies a counted run without retaining its interaction. A restore
// must match the run's identity, not just an application-chosen interaction ID.
type pausedSlot struct {
	id, conversationID, userID, anonymousID string
	order                                   uint64
}

func slotFor(i *Interaction) pausedSlot {
	return pausedSlot{i.id, i.conversationID, i.userID, i.anonymousID, i.ownerOrder}
}

// open holds the open interactions, oldest first, for MaxOpenInteractions and the TTL sweep.
var open = struct {
	mu        sync.Mutex
	list      *list.List
	paused    map[weak.Pointer[Interaction]]pausedSlot // One slot per object, without retaining it.
	nextOrder uint64
	lastSweep time.Time
}{list: list.New(), paused: map[weak.Pointer[Interaction]]pausedSlot{}}

// register opens i; false when MaxOpenInteractions is reached. A restore of a run that holds a
// slot here (paused in this process) takes it over; any other restore is admitted like a new run.
func register(i *Interaction) bool {
	if i.resumed && takePausedSlot(i) {
		return true
	}
	if !opened(i, cfg().limits.MaxOpenInteractions) {
		limitReached("MaxOpenInteractions")
		return false
	}
	return true
}

func opened(i *Interaction, limit int) bool {
	open.mu.Lock()
	defer open.mu.Unlock()
	if open.list.Len()+len(open.paused) >= limit {
		return false
	}
	open.nextOrder++
	i.ownerOrder = open.nextOrder
	i.opened = time.Now()
	i.elem = open.list.PushBack(i)
	return true
}

// unregister forgets an ended interaction.
func unregister(i *Interaction) {
	open.mu.Lock()
	defer open.mu.Unlock()
	if i.elem != nil {
		open.list.Remove(i.elem)
		i.elem = nil
	}
	delete(open.paused, weak.Make(i))
}

// holdPausedSlot keeps a paused run counted toward MaxOpenInteractions, off the TTL list. The
// move is atomic, so the run never frees its slot in between.
func holdPausedSlot(i *Interaction) {
	open.mu.Lock()
	defer open.mu.Unlock()
	if i.elem != nil {
		open.list.Remove(i.elem)
		i.elem = nil
	}
	open.paused[weak.Make(i)] = slotFor(i)
}

// takePausedSlot transfers a counted slot only when the restored identity has a
// unique match. Ambiguous matches leave every original registered and go through
// ordinary admission, just like a snapshot from another process.
func takePausedSlot(i *Interaction) bool {
	open.mu.Lock()
	defer open.mu.Unlock()
	var match weak.Pointer[Interaction]
	var matchedSlot pausedSlot
	found := false
	for ref, slot := range open.paused {
		if slot.id != i.id || slot.conversationID != i.conversationID ||
			slot.userID != i.userID || slot.anonymousID != i.anonymousID {
			continue
		}
		if found {
			return false
		}
		match, matchedSlot, found = ref, slot, true
	}
	if !found {
		return false
	}
	delete(open.paused, match)
	match.Value().continueAs(i)
	i.ownerOrder = matchedSlot.order
	i.opened = time.Now()
	i.elem = open.list.PushBack(i)
	return true
}

// reopen puts a resumed interaction back on the open list, whatever MaxOpenInteractions says: it
// held a slot before its pause.
func reopen(i *Interaction) {
	open.mu.Lock()
	defer open.mu.Unlock()
	delete(open.paused, weak.Make(i))
	if i.elem == nil {
		i.opened = time.Now()
		i.elem = open.list.PushBack(i)
	}
}

// openCount is the number of open interactions.
func openCount() int {
	open.mu.Lock()
	defer open.mu.Unlock()
	return open.list.Len()
}

// sweepExpired ends interactions open longer than the TTL. It runs on interaction start and end,
// at most once a second (as in Node.js and Python), so no timer goroutine is needed.
func sweepExpired() {
	for _, i := range expiredInteractions(cfg().limits.InteractionTTL) {
		i.End(EndOptions{Status: StatusIncomplete, endReason: endReasonTTL})
	}
}

// Values of end_reason, for interactions the SDK ended itself.
const (
	endReasonTTL      = "ttl_expired"
	endReasonShutdown = "shutdown"
)

// takeOpen takes every open interaction off the open list.
func takeOpen() []*Interaction {
	open.mu.Lock()
	defer open.mu.Unlock()
	out := make([]*Interaction, 0, open.list.Len())
	for e := open.list.Front(); e != nil; e = e.Next() {
		i := e.Value.(*Interaction)
		i.elem = nil
		out = append(out, i)
	}
	open.list = list.New()
	return out
}

// expiredInteractions takes the interactions open longer than ttl off the open list.
func expiredInteractions(ttl time.Duration) []*Interaction {
	now := time.Now()
	open.mu.Lock()
	defer open.mu.Unlock()
	if now.Sub(open.lastSweep) < min(ttl, time.Second) {
		return nil
	}
	open.lastSweep = now
	var expired []*Interaction
	for e := open.list.Front(); e != nil; e = open.list.Front() {
		i := e.Value.(*Interaction)
		if now.Sub(i.opened) < ttl {
			break
		}
		open.list.Remove(e)
		i.elem = nil
		expired = append(expired, i)
	}
	return expired
}

func resetOpenForTest() {
	open.mu.Lock()
	defer open.mu.Unlock()
	// A new list: interactions of earlier tests that end later are not on it.
	open.list = list.New()
	open.paused = map[weak.Pointer[Interaction]]pausedSlot{}
	open.lastSweep = time.Time{}
	open.nextOrder = 0
}

func ttlSelected(i *Interaction) bool {
	open.mu.Lock()
	defer open.mu.Unlock()
	return i.elem == nil
}
