package rudderstackai

import (
	"cmp"
	"container/list"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"
	"weak"

	"github.com/google/uuid"
)

// Limits caps the memory the SDK holds. A zero field keeps its default. Identical defaults in
// Node.js and Python.
type Limits struct {
	// MaxOpenInteractions is how many interactions may be open or paused at once (default 10,000);
	// a paused run holds its slot until it ends or the app drops it and it is garbage-collected. Past it,
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
	// failed attempts held for a retry, each MCP map per server, and the IDs of recently ended
	// interactions that late span interaction IDs are checked against. The oldest entries go first.
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
	token                                   string
	order                                   uint64
}

func slotFor(i *Interaction) pausedSlot {
	return pausedSlot{i.id, i.conversationID, i.userID, i.anonymousID, i.pauseToken, i.ownerOrder}
}

// open holds the open interactions, oldest first, for MaxOpenInteractions and the TTL sweep.
var open = struct {
	mu     sync.Mutex
	list   *list.List
	paused map[weak.Pointer[Interaction]]pausedSlot // One slot per object, without retaining it.
	// restored holds paused originals whose snapshot was restored here: uncounted, kept for
	// product span lookup until the app ends them, resumes them or they are collected.
	restored map[weak.Pointer[Interaction]]restoredOriginal
	// nextOrder orders registrations and retirements (an end, or a collected paused original).
	nextOrder uint64
	// retired is each ID's latest retirement, oldest first, up to MaxMapEntries; retiredFloor is
	// the newest one dropped for the cap, so a lookup past the cap fails closed.
	retired      map[string]*list.Element
	retiredOrder *list.List
	retiredFloor uint64
	lastSweep    time.Time
}{list: list.New(), paused: map[weak.Pointer[Interaction]]pausedSlot{}, restored: map[weak.Pointer[Interaction]]restoredOriginal{},
	retired: map[string]*list.Element{}, retiredOrder: list.New()}

// restoredOriginal is a held original's ID and the run it continues in. The continuation does not
// retain the original.
type restoredOriginal struct {
	id   string
	cont *continuation
}

type retirement struct {
	id  string
	seq uint64
}

// forgetRestoredLocked drops a held original that ended or was collected. That retires its ID
// only when the run it continued in is no longer registered: otherwise the run goes on.
func forgetRestoredLocked(ref weak.Pointer[Interaction]) (found, retires bool) {
	r, found := open.restored[ref]
	if !found {
		return false, false
	}
	delete(open.restored, ref)
	return true, !registeredLocked(r.cont.current.Load())
}

// registeredLocked reports whether i is on the open list or holds a paused slot.
func registeredLocked(i *Interaction) bool {
	if i == nil {
		return false
	}
	if i.elem != nil {
		return true
	}
	_, ok := open.paused[weak.Make(i)]
	return ok
}

// retireLocked records that a run with this ID ended or was collected.
func retireLocked(id string) {
	open.nextOrder++
	if e, ok := open.retired[id]; ok {
		open.retiredOrder.Remove(e)
	}
	open.retired[id] = open.retiredOrder.PushBack(retirement{id, open.nextOrder})
	for open.retiredOrder.Len() > max(cfg().limits.MaxMapEntries, 1) {
		oldest := open.retiredOrder.Remove(open.retiredOrder.Front()).(retirement)
		delete(open.retired, oldest.id)
		open.retiredFloor = max(open.retiredFloor, oldest.seq)
	}
}

// retiredSinceLocked reports whether a run with this ID may have retired after seq.
func retiredSinceLocked(id string, seq uint64) bool {
	last := open.retiredFloor
	if e, ok := open.retired[id]; ok {
		last = e.Value.(retirement).seq
	}
	return last > seq
}

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
		prunePausedLocked()
		if open.list.Len()+len(open.paused) >= limit {
			return false
		}
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
	ref := weak.Make(i)
	delete(open.paused, ref)
	if found, retires := forgetRestoredLocked(ref); !found || retires {
		retireLocked(i.id)
	}
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
	i.pauseToken = uuid.NewString()
	if len(open.paused) >= cfg().limits.MaxOpenInteractions {
		prunePausedLocked()
	}
	ref := weak.Make(i)
	open.paused[ref] = slotFor(i)
	// A dropped paused run frees its slot and token entry once collected. Registered once per
	// object: the weak key is stable, so it removes whichever entry the run holds then.
	if i.pauseCleanup {
		return
	}
	i.pauseCleanup = true
	if onPauseCleanup != nil {
		onPauseCleanup()
	}
	runtime.AddCleanup(i, func(slot pausedRef) {
		open.mu.Lock()
		defer open.mu.Unlock()
		_, paused := open.paused[slot.ref]
		delete(open.paused, slot.ref)
		if _, retires := forgetRestoredLocked(slot.ref); paused || retires {
			retireLocked(slot.id)
		}
	}, pausedRef{ref, i.id})
}

// pausedRef is what a paused run's cleanup needs, without retaining the run.
type pausedRef struct {
	ref weak.Pointer[Interaction]
	id  string
}

// onPauseCleanup is a test seam counting cleanup registrations.
var onPauseCleanup func()

// prunePausedLocked drops the entries of paused runs that were garbage-collected; the cleanup
// above does the same, but only after the GC cycle, so admission checks prune too.
func prunePausedLocked() {
	for ref, slot := range open.paused {
		if ref.Value() == nil {
			delete(open.paused, ref)
			retireLocked(slot.id)
		}
	}
}

// takePausedSlot transfers a counted slot only to the restore of the exact pause the
// snapshot names: its token and identity both match. Anything else (an older snapshot
// without a token, one from another process or tenant) goes through ordinary admission.
func takePausedSlot(i *Interaction) bool {
	open.mu.Lock()
	defer open.mu.Unlock()
	var match weak.Pointer[Interaction]
	var matchedSlot pausedSlot
	found := false
	for ref, slot := range open.paused {
		if ref.Value() == nil || i.pauseToken == "" || slot.token != i.pauseToken || slot.id != i.id || slot.conversationID != i.conversationID ||
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
	original := match.Value()
	original.continueAs(i)
	if original != nil {
		// The original stays visible to product span lookup as this run: not a retirement.
		open.restored[match] = restoredOriginal{matchedSlot.id, original.cont.Load()}
	}
	i.ownerOrder = matchedSlot.order
	i.opened = time.Now()
	i.elem = open.list.PushBack(i)
	return true
}

// liveOwner is the one live run with this ID, or nil. Open and paused interactions count as
// themselves; a restored original counts as its newest segment, so it never makes its own run
// ambiguous but keeps another tenant's same-ID run from owning the span. With late, a run with the
// ID that retired after since (the span's start) also leaves it without an owner.
func liveOwner(id string, since uint64, late bool) *Interaction {
	owner := func() *Interaction {
		open.mu.Lock()
		defer open.mu.Unlock()
		var owner *Interaction
		ambiguous := false
		add := func(i *Interaction) {
			if owner != nil && owner != i {
				ambiguous = true
			}
			owner = i
		}
		for e := open.list.Front(); e != nil; e = e.Next() {
			if i := e.Value.(*Interaction); i.id == id {
				add(i)
			}
		}
		for ref, slot := range open.paused {
			if slot.id != id {
				continue
			}
			if i := ref.Value(); i != nil {
				add(i)
			} else {
				// Collected before its cleanup ran: it retired by now.
				delete(open.paused, ref)
				retireLocked(id)
			}
		}
		for ref, r := range open.restored {
			if r.id != id {
				continue
			}
			if i := ref.Value(); i != nil {
				add(i.latest())
			} else if _, retires := forgetRestoredLocked(ref); retires {
				retireLocked(id)
			}
		}
		if ambiguous || (late && retiredSinceLocked(id, since)) {
			return nil
		}
		return owner
	}()
	// After open.mu: Pause holds the interaction's lock while it takes open.mu.
	if owner == nil || owner.Ended() {
		return nil
	}
	return owner
}

// reopen puts a resumed interaction back on the open list, whatever MaxOpenInteractions says: it
// held a slot before its pause.
func reopen(i *Interaction) {
	open.mu.Lock()
	defer open.mu.Unlock()
	delete(open.paused, weak.Make(i))
	delete(open.restored, weak.Make(i))
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
	open.restored = map[weak.Pointer[Interaction]]restoredOriginal{}
	open.retired = map[string]*list.Element{}
	open.retiredOrder = list.New()
	open.retiredFloor = 0
	open.lastSweep = time.Time{}
	open.nextOrder = 0
}

func ttlSelected(i *Interaction) bool {
	open.mu.Lock()
	defer open.mu.Unlock()
	return i.elem == nil
}
