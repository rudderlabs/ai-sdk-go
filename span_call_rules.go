package rudderstackai

import (
	"encoding/json"
	"math"
	"slices"
	"time"

	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

func spanSourceSnapshot(s *hook.SpanSource) map[string]any {
	b, _ := json.Marshal(s)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}

// Invalid optional evidence is ignored, as in Node; older snapshots count conservatively.
func restoredSpanSource(value any) *hook.SpanSource {
	s, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	for _, k := range []string{"traceId", "spanId"} {
		if _, ok := s[k].(string); !ok {
			return nil
		}
	}
	for _, k := range []string{"parentSpanId", "responseId", "requestModel", "responseModel"} {
		if v, exists := s[k]; exists {
			if _, ok := v.(string); !ok {
				return nil
			}
		}
	}
	for _, k := range []string{"start", "end"} {
		switch s[k].(type) {
		case float64, int, int64, json.Number:
		default:
			return nil
		}
		b, err := json.Marshal(s[k])
		if err != nil {
			return nil
		}
		var n float64
		if json.Unmarshal(b, &n) != nil || n < 0 || math.IsInf(n, 0) || math.IsNaN(n) {
			return nil
		}
	}
	for _, k := range []string{"usage", "output", "error", "llm"} {
		if _, ok := s[k].(bool); !ok {
			return nil
		}
	}
	if v, exists := s["ancestorsComplete"]; exists {
		if _, ok := v.(bool); !ok {
			return nil
		}
	}
	switch a := s["ancestors"].(type) {
	case []string:
		if a == nil {
			return nil
		}
	case []any:
		for _, id := range a {
			if _, ok := id.(string); !ok {
				return nil
			}
		}
	default:
		return nil
	}
	b, err := json.Marshal(s)
	if err != nil {
		return nil
	}
	var source hook.SpanSource
	if json.Unmarshal(b, &source) != nil {
		return nil
	}
	return &source
}

func (i *Interaction) recordSpanAncestry(traceID, spanID, parentID string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.ended {
		return
	}
	if i.spanParents == nil {
		i.spanParents = map[string]string{}
	}
	key := traceID + ":" + spanID
	if _, exists := i.spanParents[key]; exists {
		i.spanParentOrder = slices.DeleteFunc(i.spanParentOrder, func(k string) bool { return k == key })
	}
	i.spanParents[key] = parentID
	i.spanParentOrder = append(i.spanParentOrder, key)
	for len(i.spanParentOrder) > max(cfg().limits.MaxMapEntries, 1) {
		delete(i.spanParents, i.spanParentOrder[0])
		i.spanParentOrder = i.spanParentOrder[1:]
	}
}

func (i *Interaction) completeSpanAncestryLocked() {
	parents := make(map[string]string, len(i.spanParents)+len(i.calls))
	for k, v := range i.spanParents {
		parents[k] = v
	}
	for _, c := range i.calls {
		if s := c.spanSource; s != nil {
			parents[s.TraceID+":"+s.SpanID] = s.ParentSpanID
		}
	}
	for _, c := range i.calls {
		s := c.spanSource
		if s == nil {
			continue
		}
		visited := map[string]bool{s.SpanID: true}
		parent := s.ParentSpanID
		for parent != "" && !visited[parent] {
			visited[parent] = true
			if !slices.Contains(s.Ancestors, parent) {
				s.Ancestors = append(s.Ancestors, parent)
			}
			// A missing edge cannot establish that this ancestor is a root.
			next, known := parents[s.TraceID+":"+parent]
			if !known {
				break
			}
			parent = next
		}
		s.AncestorsComplete = s.AncestorsComplete || parent == ""
	}
}

// Wrappers and merged wrapper twins remain authoritative, even before emission.
func isSkippableSpanCall(call *recordedCall) bool {
	return call.spanSource != nil && call.fromSpan && !call.twinMatched && !call.callEventEmitted
}

// Resolve surviving evidence before its users. A cycle cannot remove every call.
func skippedSpanCalls(calls []*recordedCall, endedAt float64) map[*recordedCall]bool {
	const (
		keep            = 1
		skip            = 2
		cycle           = 3
		unknownAncestry = 0
		unrelated       = 1
		related         = 2
	)
	decisions := map[*recordedCall]int{}
	visiting := map[*recordedCall]bool{}
	descendant := func(child, parent *hook.SpanSource) int {
		if child.TraceID != parent.TraceID {
			return unrelated
		}
		if slices.Contains(child.Ancestors, parent.SpanID) {
			return related
		}
		// A shared ancestor proves separate branches even if its own edge is missing.
		if child.AncestorsComplete || slices.ContainsFunc(child.Ancestors, func(id string) bool {
			return slices.Contains(parent.Ancestors, id)
		}) {
			return unrelated
		}
		return unknownAncestry
	}
	var decide func(*recordedCall) int
	decide = func(call *recordedCall) int {
		// Wrapper reports are authoritative, even before their call event is released.
		// Keep their span tags available as evidence for pure, unmatched span calls.
		if !isSkippableSpanCall(call) {
			return keep
		}
		if known := decisions[call]; known != 0 {
			return known
		}
		if visiting[call] {
			return cycle
		}
		visiting[call] = true
		node := call.spanSource
		result := keep
		for _, other := range calls {
			evidence := other.spanSource
			if evidence == nil || other == call || evidence.TraceID != node.TraceID {
				continue
			}
			prep := node.LLM && !node.Usage && node.ResponseID == "" && !node.Output && !node.Error && evidence.Usage && evidence.Start >= node.Start-1 && evidence.Start <= endedAt && descendant(evidence, node) == unrelated && descendant(node, evidence) == unrelated
			sameModel := false
			for _, model := range []string{node.RequestModel, node.ResponseModel} {
				if model != "" && (model == evidence.RequestModel || model == evidence.ResponseModel) {
					sameModel = true
				}
			}
			overlaps := evidence.Start <= node.End && node.Start <= evidence.End
			matching := evidence.ResponseID != "" || (sameModel && overlaps)
			if node.Error {
				matching = evidence.Error && sameModel && overlaps
			}
			nested := descendant(evidence, node) == related && (node.ResponseID == "" || node.ResponseID == evidence.ResponseID) && (evidence.ResponseID != "" || evidence.Usage) && matching
			if (prep || nested) && decide(other) == keep {
				result = skip
				break
			}
		}
		delete(visiting, call)
		decisions[call] = result
		return result
	}
	for _, call := range calls {
		if call.spanSource != nil {
			decide(call)
		}
	}
	skipped := map[*recordedCall]bool{}
	for c, d := range decisions {
		if d == skip {
			skipped[c] = true
		}
	}
	return skipped
}

// The existing interaction lock protects the entire pass; no callbacks or recording occur here.
func (i *Interaction) filterSpanCallsLocked() {
	i.completeSpanAncestryLocked()
	now := time.Now()
	if i.eventClock != nil {
		now = i.eventClock()
	}
	skipped := skippedSpanCalls(i.calls, float64(now.UnixMilli()))
	before := slices.Clone(i.calls)
	if len(skipped) == 0 {
		return
	}
	removedBefore := func(sequence int) int {
		n := 0
		for _, c := range before {
			if skipped[c] && c.reportSequence <= sequence {
				n++
			}
		}
		return n
	}
	if i.hint != nil {
		i.hint.calls -= removedBefore(i.hint.calls)
	}
	i.successfulCallSequence -= removedBefore(i.successfulCallSequence)
	removed := 0
	for c := range skipped {
		removed += c.callCount
	}
	i.callSeq -= removed
	i.callCount -= len(skipped)
	i.calls = slices.DeleteFunc(i.calls, func(c *recordedCall) bool { return skipped[c] })
	// A filtered report cannot settle an approval wait. Preserve surviving call orders,
	// including folded calls, so genuine later model calls still settle their tools.
	clear(i.agentCalls)
	i.lastAgent = ""
	latestOrder := 0
	for _, c := range append(slices.Clone(i.calls), i.overflow...) {
		i.agentCalls[c.agentName] = max(i.agentCalls[c.agentName], c.order)
		if c.order > latestOrder {
			latestOrder = c.order
			i.lastAgent = c.agentName
		}
	}
	index := before[0].callIndex
	for _, c := range i.calls {
		c.reportSequence -= removedBefore(c.reportSequence)
		if !c.callEventEmitted {
			c.callIndex = index
		}
		index = c.callIndex + c.callCount
	}
	if skipped[i.lastCall] {
		i.lastCall = nil
		for _, c := range i.calls {
			i.lastCall = c
		}
	}
	if skipped[i.lastOK] {
		i.lastOK = nil
		i.successfulCallSequence = 0
		for _, c := range i.calls {
			if c.errorType == "" {
				i.lastOK = c
				i.successfulCallSequence = c.reportSequence
			}
		}
	}
	for _, c := range i.pendingOverflowCalls {
		if !c.callEventEmitted {
			c.callIndex -= removed
		}
	}
	if skipped[i.spanAnswerSource] {
		i.spanAnswer, i.spanAnswerSource = "", nil
		for _, c := range i.calls {
			if c.spanSource != nil && c.errorType == "" && i.capture.AnswerText {
				i.spanAnswer, i.spanAnswerSource = c.answerText, c
			}
		}
	}
	if skipped[i.answerSource] {
		i.answer, i.answerSource = "", nil
		for _, c := range i.calls {
			if c.errorType == "" && c.answerText != "" {
				i.answer, i.answerSource = c.answerText, c
			}
		}
	}
	for _, c := range before {
		if skipped[c] {
			c.answerFiltered = true
			c.answerText = ""
			c.content = nil
			c.callEventPending = false
		}
	}
}

// Capture may record more calls or change the inferred answer. Reapply the rules
// after each redaction and tool callback, keeping evidence until capture settles.
// The caller holds mu; callbacks run unlocked and this returns with mu held.
func (i *Interaction) captureResponseLocked(explicit string, freeze bool) (text string, ok bool) {
	leave := i.enterCallback(false)
	defer leave()
	i.filterSpanCallsLocked()
	selected := first(explicit, i.spanAnswer)
	var source *recordedCall
	if explicit == "" {
		source = i.spanAnswerSource
	}
	drained := 0
	for {
		captured := selected
		text, ok = "", false
		i.mu.Unlock()
		if captured != "" && i.capture.AnswerText {
			text, ok = redactText(captured)
		}
		i.mu.Lock()
		for drained < len(i.tools) {
			tools := slices.Clone(i.tools[drained:])
			drained = len(i.tools)
			i.mu.Unlock()
			i.fireToolEvents(tools, true)
			i.mu.Lock()
		}
		i.filterSpanCallsLocked()
		selected = first(explicit, i.spanAnswer)
		// Only filtering the frozen answer's own call permits pause-time replacement.
		if freeze && (source == nil || !source.answerFiltered) {
			selected = captured
		} else {
			source = i.spanAnswerSource
		}
		if selected == captured {
			break
		}
	}
	i.spanParents, i.spanParentOrder = nil, nil
	for _, c := range i.calls {
		c.answerText = ""
	}
	return text, ok
}
