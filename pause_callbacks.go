package rudderstackai

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// pausePricingWaitBound bounds waiting for all pricing attempts, including capture repricing.
// It is not public configuration; tests can shorten an interaction's private seam.
const pausePricingWaitBound = 5 * time.Second

// callbackCaller reads the runtime's first stack line. A failed parse chooses the wait path.
func callbackCaller() uint64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	return parseCallbackCaller(string(buf[:n]))
}

func parseCallbackCaller(stack string) uint64 {
	line, _, _ := strings.Cut(stack, "\n")
	fields := strings.Fields(line)
	if len(fields) < 2 || fields[0] != "goroutine" {
		return 0
	}
	id, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	return id
}

var errorCallers = struct {
	sync.Mutex
	depths  map[uint64]int
	entries map[uint64]uint64
}{depths: make(map[uint64]int), entries: make(map[uint64]uint64)}

var callbackSequence atomic.Uint64

func inErrorHandler(id, entry uint64) bool {
	if id == 0 {
		return false
	}
	errorCallers.Lock()
	defer errorCallers.Unlock()
	return errorCallers.depths[id] > 0 && entry <= errorCallers.entries[id]
}

func outsideSDKCallbacks(fn func()) {
	id := callbackCaller()
	errorCallers.Lock()
	previousEntry := errorCallers.entries[id]
	errorCallers.entries[id] = callbackSequence.Add(1)
	errorCallers.depths[id]++
	errorCallers.Unlock()
	defer func() {
		errorCallers.Lock()
		defer errorCallers.Unlock()
		errorCallers.depths[id]--
		errorCallers.entries[id] = previousEntry
		if errorCallers.depths[id] == 0 {
			delete(errorCallers.depths, id)
			delete(errorCallers.entries, id)
		}
	}()
	fn()
}

type pricingReservation struct{ active bool }

func (i *Interaction) enterCallback(pricing bool) func() {
	id := callbackCaller()
	var token *pricingReservation
	var previousEntry uint64
	i.callbackMu.Lock()
	if i.callbackCallers == nil {
		i.callbackCallers = make(map[uint64]int)
	}
	if i.pricingSettled == nil {
		i.pricingSettled = sync.NewCond(&i.callbackMu)
	}
	if id != 0 {
		if i.callbackEntries == nil {
			i.callbackEntries = make(map[uint64]uint64)
		}
		previousEntry = i.callbackEntries[id]
		i.callbackEntries[id] = callbackSequence.Add(1)
		i.callbackCallers[id]++
	}
	if pricing {
		token = &pricingReservation{active: true}
		if i.pricingTokens == nil {
			i.pricingTokens = make(map[uint64][]*pricingReservation)
		}
		i.pricingTokens[id] = append(i.pricingTokens[id], token)
		i.pricingCount++
	}
	i.callbackMu.Unlock()
	return func() {
		i.callbackMu.Lock()
		defer i.callbackMu.Unlock()
		if id != 0 {
			i.callbackCallers[id]--
			i.callbackEntries[id] = previousEntry
			if i.callbackCallers[id] == 0 {
				delete(i.callbackCallers, id)
				delete(i.callbackEntries, id)
			}
		}
		if token != nil {
			tokens := i.pricingTokens[id]
			for index, current := range tokens {
				if current == token {
					i.pricingTokens[id] = append(tokens[:index], tokens[index+1:]...)
					break
				}
			}
			if len(i.pricingTokens[id]) == 0 {
				delete(i.pricingTokens, id)
			}
			if token.active {
				i.pricingCount--
				i.pricingSettled.Broadcast()
			}
		}
	}
}

func (i *Interaction) waitPricing(deadline time.Time) string {
	id := callbackCaller()
	i.callbackMu.Lock()
	exempt := inErrorHandler(id, i.callbackEntries[id])
	defer i.callbackMu.Unlock()
	if id != 0 && i.callbackCallers[id] > 0 && !exempt {
		return "in_callback"
	}
	// Error handlers may pause their own pricing call without waiting on themselves.
	if i.pricingCount == 0 || (exempt && i.callbackCallers[id] > 0) {
		return ""
	}
	timer := time.AfterFunc(max(time.Until(deadline), 0), func() {
		i.callbackMu.Lock()
		defer i.callbackMu.Unlock()
		i.pricingSettled.Broadcast()
	})
	defer timer.Stop()
	for i.pricingCount > 0 {
		if !time.Now().Before(deadline) {
			return "pricing_in_flight"
		}
		i.pricingSettled.Wait()
	}
	return ""
}

func reportPauseRefused(reason string) {
	report(KindPauseRefused, "interaction.pause", fmt.Errorf("pause refused: %s", reason), reason)
}

// settlePricing releases the current report's wait reservation before delivery or notices.
// Its callback scope stays active so transport re-entry is still refused.
func (i *Interaction) settlePricing() {
	id := callbackCaller()
	if id == 0 {
		return
	}
	i.callbackMu.Lock()
	defer i.callbackMu.Unlock()
	tokens := i.pricingTokens[id]
	if len(tokens) == 0 {
		return
	}
	token := tokens[len(tokens)-1]
	if token.active {
		token.active = false
		i.pricingCount--
		i.pricingSettled.Broadcast()
	}
}
