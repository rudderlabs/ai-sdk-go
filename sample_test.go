package rudderstackai

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

const (
	keptID    = "3f2c9a4e-8b1d-4c6f-9e2a-7d5b1c0e4a93" // bucket 0.39
	droppedID = "11111111-1111-4111-8111-111111111111" // bucket 0.74
)

// sampledAnswer runs one interaction with n model calls under id.
func sampledAnswer(t *testing.T, id string, n int) {
	t.Helper()
	run(t, InteractionOptions{InteractionID: id}, func(_ context.Context, i *Interaction) {
		for range n {
			i.RecordCall(LLMCall{Provider: "openai", Model: "gpt-4o-mini", InputTokens: 5, OutputTokens: 5})
		}
	})
}

func toolCalled(id string) map[string]any {
	return map[string]any{"server_name": "s", "interaction_id": id, "request_id": "r", "tool_name": "t", "tool_status": "success"}
}

func TestSamplingSharedFixture(t *testing.T) {
	var f struct {
		Cases []struct {
			InteractionID string  `json:"interaction_id"`
			Hash          uint32  `json:"hash"`
			Rate          float64 `json:"rate"`
			Keep          bool    `json:"keep"`
		} `json:"cases"`
	}
	loadFixture(t, "spec/fixtures/sampling-v0.1.json", &f)
	if len(f.Cases) == 0 {
		t.Fatal("no cases")
	}
	for _, c := range f.Cases {
		t.Run(fmt.Sprintf("%s@%v", c.InteractionID, c.Rate), func(t *testing.T) {
			if got := sampleBucket(c.InteractionID); got != float64(c.Hash)/(1<<32) {
				t.Fatalf("bucket %v, want %v", got, float64(c.Hash)/(1<<32))
			}
			r := setupAI(t, WithLLMCallEvents(true), WithSampleRates(map[string]float64{"ai_llm_response_received": c.Rate}))
			sampledAnswer(t, c.InteractionID, 1)
			want := 0
			if c.Keep {
				want = 1
			}
			if got := len(r.named("ai_llm_response_received")); got != want {
				t.Fatalf("kept %d, want %d", got, want)
			}
		})
	}
}

func TestSampleRateStamp(t *testing.T) {
	r := setupAI(t, WithLLMCallEvents(true), WithSampleRates(map[string]float64{"ai_llm_response_received": 0.1}))
	sampledAnswer(t, "interaction-ü-✓", 1) // bucket 0.0175
	if got := r.props(t, "ai_llm_response_received")["sample_rate"]; got != 0.1 {
		t.Fatalf("sample_rate %v", got)
	}
}

func TestSampleLimitEventsByInteraction(t *testing.T) {
	r := setupAI(t, WithSampleRates(map[string]float64{"ai_limit_reached": 0.1}))
	for _, id := range []string{"interaction-ü-✓", "i1"} {
		TrackLimitReached(context.Background(), TrackLimitReachedOptions{UserID: "u", LimitType: "credits", InteractionID: id})
	}
	p := r.props(t, "ai_limit_reached")
	if p["interaction_id"] != "interaction-ü-✓" || p["sample_rate"] != 0.1 {
		t.Fatalf("properties = %v", p)
	}
	if err := validateEvent("ai_limit_reached", p); err != nil {
		t.Fatal(err)
	}
	for _, rate := range []any{0, -0.1, 1.1, "0.1"} {
		invalid := map[string]any{}
		for key, value := range p {
			invalid[key] = value
		}
		invalid["sample_rate"] = rate
		if err := validateEvent("ai_limit_reached", invalid); err == nil {
			t.Errorf("sample_rate %v accepted", rate)
		}
	}
}

func TestSampleAccountWideLimitEventsAtRandom(t *testing.T) {
	r := setupAI(t, WithSampleRates(map[string]float64{"ai_limit_reached": 0.5}))
	draws := []float64{0.4, 0.6}
	prev := sampleRandom
	sampleRandom = func() float64 { d := draws[0]; draws = draws[1:]; return d }
	t.Cleanup(func() { sampleRandom = prev })
	for range 2 {
		TrackLimitReached(context.Background(), TrackLimitReachedOptions{UserID: "u", LimitType: "credits"})
	}
	p := r.props(t, "ai_limit_reached")
	if len(p) != 2 || p["limit_type"] != "credits" || p["sample_rate"] != 0.5 {
		t.Fatalf("properties = %v", p)
	}
}

func TestNoSampleRateWithoutARateOrAtRate1(t *testing.T) {
	r := setupAI(t, WithLLMCallEvents(true), WithSampleRates(map[string]float64{"ai_user_prompt_created": 1}))
	sampledAnswer(t, "i1", 1)
	if len(r.all()) == 0 {
		t.Fatal("no events")
	}
	for _, e := range r.all() {
		if _, ok := e.Properties["sample_rate"]; ok {
			t.Fatalf("%s carries sample_rate", e.Event)
		}
	}
}

func TestResponseReceivedUnsampledByDefault(t *testing.T) {
	r := setupAI(t, WithLLMCallEvents(true), WithSampleRates(map[string]float64{"ai_llm_response_received": 0, "mcp_tool_called": 0}))
	sampledAnswer(t, "i1", 1)
	if n := len(r.named("ai_llm_response_received")); n != 0 {
		t.Fatalf("llm calls %d", n)
	}
	if _, ok := r.response(t)["sample_rate"]; ok {
		t.Fatal("ai_response_received carries sample_rate")
	}
	if n := len(r.named("ai_user_prompt_created")); n != 1 {
		t.Fatalf("prompts %d", n)
	}
}

func TestSampledEventsOfOneInteractionStayTogether(t *testing.T) {
	r := setupAI(t, WithLLMCallEvents(true), WithSampleRates(map[string]float64{"ai_llm_response_received": 0.5}))
	sampledAnswer(t, keptID, 3)
	sampledAnswer(t, droppedID, 3)
	kept := r.named("ai_llm_response_received")
	if len(kept) != 3 {
		t.Fatalf("kept %d", len(kept))
	}
	for _, e := range kept {
		if e.Properties["interaction_id"] != keptID {
			t.Fatalf("kept %v", e.Properties["interaction_id"])
		}
	}
	if n := len(r.named("ai_response_received")); n != 2 {
		t.Fatalf("responses %d", n)
	}
}

func TestSampleKeepsRoughlyTheConfiguredShare(t *testing.T) {
	rates := map[string]float64{"ai_llm_response_received": 0.2}
	kept := 0
	for n := range 2000 {
		if _, ok := sample("ai_llm_response_received", map[string]any{"interaction_id": fmt.Sprintf("interaction-%d", n)}, rates); ok {
			kept++
		}
	}
	if share := float64(kept) / 2000; share <= 0.17 || share >= 0.23 {
		t.Fatalf("share %v", share)
	}
}

func TestSampleWithoutInteractionIDIsRandom(t *testing.T) {
	r := setupAI(t, WithSampleRates(map[string]float64{"mcp_client_seen": 0.5}))
	draws := []float64{0.4, 0.6}
	prev := sampleRandom
	sampleRandom = func() float64 { d := draws[0]; draws = draws[1:]; return d }
	t.Cleanup(func() { sampleRandom = prev })
	emit(r, "mcp_client_seen", who{userID: "u"}, map[string]any{"server_name": "s"})
	emit(r, "mcp_client_seen", who{userID: "u"}, map[string]any{"server_name": "s"})
	got := r.named("mcp_client_seen")
	if len(got) != 1 || got[0].Properties["sample_rate"] != 0.5 {
		t.Fatalf("got %v", got)
	}
}

func TestSampleMCPToolCalledByInteractionID(t *testing.T) {
	r := setupAI(t, WithSampleRates(map[string]float64{"mcp_tool_called": 0.1}))
	emit(r, "mcp_tool_called", who{userID: "u"}, toolCalled("interaction-ü-✓"))
	emit(r, "mcp_tool_called", who{userID: "u"}, toolCalled("i1"))
	got := r.named("mcp_tool_called")
	if len(got) != 1 || got[0].Properties["interaction_id"] != "interaction-ü-✓" {
		t.Fatalf("got %v", got)
	}
}

func TestSampleLeavesTheCallersPropertiesAlone(t *testing.T) {
	r := setupAI(t, WithSampleRates(map[string]float64{"mcp_tool_called": 0.5}))
	p := toolCalled(keptID)
	emit(r, "mcp_tool_called", who{userID: "u"}, p)
	if got := r.props(t, "mcp_tool_called")["sample_rate"]; got != 0.5 {
		t.Fatalf("sample_rate %v", got)
	}
	if _, ok := p["sample_rate"]; ok {
		t.Fatal("caller's map changed")
	}
}

func TestResponseReceivedCanBeSampledWhenAsked(t *testing.T) {
	r := setupAI(t, WithSampleRates(map[string]float64{"ai_response_received": 0.5}))
	sampledAnswer(t, keptID, 1)
	sampledAnswer(t, droppedID, 1)
	got := r.named("ai_response_received")
	if len(got) != 1 || got[0].Properties["sample_rate"] != 0.5 {
		t.Fatalf("got %v", got)
	}
}

func TestSampleRatesReplacedAndNilClears(t *testing.T) {
	r := setupAI(t, WithLLMCallEvents(true), WithSampleRates(map[string]float64{"ai_llm_response_received": 0}))
	if err := Configure(WithSampleRates(map[string]float64{"ai_user_prompt_created": 0})); err != nil {
		t.Fatal(err)
	}
	sampledAnswer(t, "i1", 1)
	if len(r.named("ai_llm_response_received")) != 1 || len(r.named("ai_user_prompt_created")) != 0 {
		t.Fatalf("got %v", r.names())
	}
	if err := Configure(WithSampleRates(nil)); err != nil {
		t.Fatal(err)
	}
	sampledAnswer(t, "i1", 1)
	if len(r.named("ai_user_prompt_created")) != 1 {
		t.Fatalf("got %v", r.names())
	}
}

func TestInvalidSampleRatesChangeNothing(t *testing.T) {
	for name, rates := range map[string]map[string]float64{
		"above 1":    {"mcp_tool_called": 1.5},
		"negative":   {"mcp_tool_called": -0.1},
		"NaN":        {"mcp_tool_called": math.NaN()},
		"empty name": {"": 0.5},
	} {
		t.Run(name, func(t *testing.T) {
			r := setupAI(t, WithLLMCallEvents(true), WithSampleRates(map[string]float64{"ai_llm_response_received": 0}))
			err := Configure(WithSampleRates(rates), WithCaptureContent(true))
			if err == nil || !strings.Contains(err.Error(), "sample") {
				t.Fatalf("err %v", err)
			}
			if cfg().captureContent {
				t.Fatal("capture content changed")
			}
			sampledAnswer(t, "i1", 1)
			if n := len(r.named("ai_llm_response_received")); n != 0 {
				t.Fatalf("llm calls %d", n)
			}
		})
	}
}

// A Configure between fitting and emitting must not add sample_rate to an event already at budget.
func TestFittedToolEventKeepsItsRatesSnapshot(t *testing.T) {
	r := setupAI(t)
	const event = "order_placed"
	p := map[string]any{"server_name": "s", "interaction_id": keptID, "request_id": "r", "tool_name": "t", "tool_status": "success", "pad": "", "small": 1}
	p["pad"] = strings.Repeat("x", maxPropertiesBytes-propertiesBytes(p)-5) // fits, but not with sample_rate
	var rates map[string]float64                                            // snapshot taken before the concurrent Configure
	fitted := fitToolEventProperties(event, p, []string{"pad", "small"}, rates)
	if err := Configure(WithSampleRates(map[string]float64{event: 0.99})); err != nil {
		t.Fatal(err)
	}
	emitRates(r, event, who{userID: "u"}, fitted, rates)
	emitAIRates(r, event, "u", "", fitted, nil, "", time.Now(), rates)
	got := r.named(event)
	if len(got) != 2 {
		t.Fatalf("got %d events", len(got))
	}
	for _, e := range got {
		if _, ok := e.Properties["sample_rate"]; ok || e.Properties["pad"] == nil {
			t.Fatalf("event changed after fitting: %d bytes, sample_rate %v", len(fmt.Sprint(e.Properties)), e.Properties["sample_rate"])
		}
	}
}

func TestSampledResponseExtrasStayWithinBudget(t *testing.T) {
	r := setupAI(t, WithSampleRates(map[string]float64{"ai_response_received": 0.99}))
	p := map[string]any{"interaction_id": keptID, "conversation_id": "c", "response_status": "success", "trigger": "user", "llm_call_count": 0, "tool_call_count": 0, "latency_ms": 0, "padding": ""}
	p["padding"] = strings.Repeat("x", maxPropertiesBytes-propertiesBytes(p))
	emitAIRates(r, "ai_response_received", "u", "", p, nil, "", time.Now(), cfg().sampleRates)
	got := r.props(t, "ai_response_received")
	if propertiesBytes(got) > maxPropertiesBytes || got["sample_rate"] != 0.99 || got["response_status"] != "success" {
		t.Fatalf("%d bytes: rate %v", propertiesBytes(got), got["sample_rate"])
	}
}

func TestOverBudgetBlockedResponseKeepsBlockReason(t *testing.T) {
	r := setupAI(t, WithSampleRates(map[string]float64{"ai_response_received": 0.99}))
	p := map[string]any{"interaction_id": keptID, "conversation_id": "c", "response_status": "blocked", "block_reason": "policy", "trigger": "user", "llm_call_count": 0, "tool_call_count": 0, "latency_ms": 0, "padding": ""}
	p["padding"] = strings.Repeat("x", maxPropertiesBytes-propertiesBytes(p))
	emitAIRates(r, "ai_response_received", "u", "", p, nil, "", time.Now(), cfg().sampleRates)
	got := r.props(t, "ai_response_received")
	if propertiesBytes(got) > maxPropertiesBytes || got["block_reason"] != "policy" {
		t.Fatalf("%d bytes: block_reason %v", propertiesBytes(got), got["block_reason"])
	}
}
