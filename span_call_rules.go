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
		for parent := s.ParentSpanID; parent != "" && !visited[parent]; parent = parents[s.TraceID+":"+parent] {
			visited[parent] = true
			if !slices.Contains(s.Ancestors, parent) {
				s.Ancestors = append(s.Ancestors, parent)
			}
		}
	}
}

// Resolve surviving evidence before its users. A cycle cannot remove every call.
func skippedSpanCalls(calls []*recordedCall, endedAt float64) map[*recordedCall]bool {
	const (
		keep  = 1
		skip  = 2
		cycle = 3
	)
	decisions := map[*recordedCall]int{}
	visiting := map[*recordedCall]bool{}
	descendant := func(child, parent *hook.SpanSource) bool {
		return child.TraceID == parent.TraceID && slices.Contains(child.Ancestors, parent.SpanID)
	}
	var decide func(*recordedCall) int
	decide = func(call *recordedCall) int {
		// Wrapper reports are authoritative, even before their call event is released.
		// Keep their span tags available as evidence for pure, unmatched span calls.
		if !call.fromSpan || call.twinMatched || call.callEventEmitted {
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
		if node == nil {
			return keep
		}
		result := keep
		for _, other := range calls {
			evidence := other.spanSource
			if evidence == nil || other == call || evidence.TraceID != node.TraceID {
				continue
			}
			prep := node.LLM && !node.Usage && node.ResponseID == "" && !node.Output && !node.Error && evidence.Usage && evidence.Start >= node.Start-1 && evidence.Start <= endedAt && !descendant(evidence, node) && !descendant(node, evidence)
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
			nested := descendant(evidence, node) && (evidence.ResponseID != "" || evidence.Usage) && matching
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
	i.spanParents, i.spanParentOrder = nil, nil
	before := slices.Clone(i.calls)
	if len(skipped) == 0 {
		for _, c := range before {
			c.answerText = ""
		}
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
	var lastSpan *recordedCall
	for _, c := range before {
		if c.spanSource != nil && c.errorType == "" {
			lastSpan = c
		}
	}
	if skipped[lastSpan] {
		i.spanAnswer, i.answer = "", ""
		for _, c := range i.calls {
			if c.errorType == "" {
				if c.answerText != "" {
					i.answer = c.answerText
				}
				if c.spanSource != nil && i.capture.AnswerText {
					i.spanAnswer = c.answerText
				}
			}
		}
	}
	for _, c := range before {
		c.answerText = ""
		if skipped[c] {
			c.content = nil
			c.callEventPending = false
		}
	}
}
