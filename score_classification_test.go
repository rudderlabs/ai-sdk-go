package rudderstackai

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

func TestTrackClassificationAllFields(t *testing.T) {
	r := setupAI(t)
	ctx := WithValues(context.Background(), ContextValues{AnonymousID: "anon", ConversationID: "conv", Feature: "f", Surface: SurfaceEmail, AgentVersion: "v1", Variant: "b"})
	TrackClassification(ctx, TrackClassificationOptions{
		InteractionID: "i", Intent: "refund", Outcome: "resolved", IntentSummary: "Wants a refund.",
		Classifier: "custom", ClassifierModel: "haiku", ClassifierDurationMS: Ptr(640.4), ClassifierCostUSD: Ptr(0.0004),
		Trigger: TriggerSystem, UserID: "u", AccountID: "acct", OnBehalfOf: "boss",
	})
	ms := r.named("ai_interaction_classified")
	if len(ms) != 1 {
		t.Fatalf("events: %v", r.names())
	}
	p := ms[0].Properties
	want := map[string]any{
		"conversation_id": "conv", "interaction_id": "i", "intent": "refund", "outcome": "resolved",
		"intent_summary": "Wants a refund.", "classifier": "custom", "classifier_model": "haiku",
		"classifier_duration_ms": float64(640), "classifier_cost_usd": 0.0004, "trigger": "system",
		"feature": "f", "surface": "email", "account_id": "acct", "on_behalf_of": "boss", "agent_version": "v1", "variant": "b",
	}
	for k, v := range want {
		if got := p[k]; got != v {
			t.Errorf("%s = %#v, want %#v", k, got, v)
		}
	}
	if ms[0].UserId != "u" {
		t.Fatalf("user: %+v", ms[0])
	}
}

func TestTrackClassificationDefaultsAndAmbient(t *testing.T) {
	r := setupAI(t)
	ctx := WithValues(context.Background(), ContextValues{AnonymousID: "anon", ConversationID: "conv"})
	TrackClassification(ctx, TrackClassificationOptions{InteractionID: "i", Intent: "x", Outcome: "resolved"})
	m := r.named("ai_interaction_classified")[0]
	if m.Properties["classifier"] != "model" || m.Properties["conversation_id"] != "conv" || m.AnonymousId != "anon" {
		t.Fatalf("%+v", m)
	}
	for _, k := range []string{"intent_summary", "classifier_model", "classifier_duration_ms", "classifier_cost_usd", "trigger"} {
		if _, ok := m.Properties[k]; ok {
			t.Errorf("%s sent unset", k)
		}
	}
}

func TestTrackClassificationLabels(t *testing.T) {
	cases := []struct {
		name         string
		opts         []ConfigOption
		intent, out  string
		wantI, wantO string
	}{
		{"default list keeps known outcome, any intent", nil, "anything", "escalated", "anything", "escalated"},
		{"default list: unknown outcome is unclear", nil, "anything", "great", "anything", "unclear"},
		{"configured intents: unknown is other", []ConfigOption{WithClassifier(&ClassifierOptions{Classify: noopClassify, Intents: []string{"refund"}, Outcomes: []string{"won"}})}, "billing", "won", "other", "won"},
		{"configured lists keep members", []ConfigOption{WithClassifier(&ClassifierOptions{Classify: noopClassify, Intents: []string{"refund"}, Outcomes: []string{"won"}})}, "refund", "resolved", "refund", "unclear"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := setupAI(t, c.opts...)
			TrackClassification(context.Background(), TrackClassificationOptions{InteractionID: "i", ConversationID: "c", Intent: c.intent, Outcome: c.out})
			p := r.props(t, "ai_interaction_classified")
			if p["intent"] != c.wantI || p["outcome"] != c.wantO {
				t.Fatalf("%v", p)
			}
		})
	}
}

func noopClassify(context.Context, ClassifyTurn) (ClassifyResult, error) {
	return ClassifyResult{}, nil
}

func TestTrackClassificationIntentSummary(t *testing.T) {
	r := setupAI(t)
	TrackClassification(context.Background(), TrackClassificationOptions{InteractionID: "i", ConversationID: "c", Intent: "x", Outcome: "resolved", IntentSummary: "Mail jane@example.com about it. Second sentence."})
	if s := r.props(t, "ai_interaction_classified")["intent_summary"]; s != "Mail [email] about it." {
		t.Fatalf("masked: %v", s)
	}
	r.reset()
	TrackClassification(context.Background(), TrackClassificationOptions{InteractionID: "i", ConversationID: "c", Intent: "x", Outcome: "resolved", IntentSummary: strings.Repeat("a", 400)})
	if s, _ := r.props(t, "ai_interaction_classified")["intent_summary"].(string); len([]rune(s)) != 280 {
		t.Fatalf("cap: %d", len(s))
	}
	// Redaction off leaves a sensitive value in place, so the pattern check withholds it.
	r = setupAI(t, WithRedaction(false))
	TrackClassification(context.Background(), TrackClassificationOptions{InteractionID: "i", ConversationID: "c", Intent: "x", Outcome: "resolved", IntentSummary: "Mail jane@example.com."})
	if _, ok := r.props(t, "ai_interaction_classified")["intent_summary"]; ok {
		t.Fatal("sensitive summary sent")
	}
}

func TestTrackClassificationValidation(t *testing.T) {
	r := setupAI(t)
	logs := captureLogs(t)
	bg := context.Background()
	TrackClassification(bg, TrackClassificationOptions{ConversationID: "c", Intent: "x", Outcome: "resolved"})
	TrackClassification(bg, TrackClassificationOptions{InteractionID: "i", Intent: "x", Outcome: "resolved"})
	TrackClassification(bg, TrackClassificationOptions{InteractionID: "i", ConversationID: "c", Outcome: "resolved"})
	if len(r.all()) != 0 || logs.count("ai_interaction_classified dropped") != 2 {
		t.Fatalf("drops: %v %s", r.names(), logs)
	}
	// Bad optional values lose only themselves, with one warning each.
	TrackClassification(bg, TrackClassificationOptions{
		InteractionID: "i", ConversationID: "c", Intent: "x", Outcome: "resolved", Classifier: "bogus",
		ClassifierDurationMS: Ptr(-1.0), ClassifierCostUSD: Ptr(math.NaN()), Trigger: "bogus",
	})
	TrackClassification(bg, TrackClassificationOptions{
		InteractionID: "i", ConversationID: "c", Intent: "x", Outcome: "resolved", Classifier: "bogus",
		ClassifierDurationMS: Ptr(-1.0), ClassifierCostUSD: Ptr(-0.5),
	})
	for _, m := range r.named("ai_interaction_classified") {
		p := m.Properties
		if p["classifier"] != "model" || p["classifier_duration_ms"] != nil || p["classifier_cost_usd"] != nil || p["trigger"] != nil {
			t.Fatalf("%v", p)
		}
	}
	if n := len(r.named("ai_interaction_classified")); n != 2 || logs.count("invalid classifier") != 1 {
		t.Fatalf("events %d logs %s", n, logs)
	}
}

func TestTrackClassificationSampling(t *testing.T) {
	r := setupAI(t, WithSampleRates(map[string]float64{"ai_interaction_classified": 0.000001}))
	for n := 0; n < 20; n++ {
		TrackClassification(context.Background(), TrackClassificationOptions{InteractionID: "i" + string(rune('a'+n)), ConversationID: "c", Intent: "x", Outcome: "resolved"})
	}
	if n := len(r.named("ai_interaction_classified")); n != 0 {
		t.Fatalf("sampled out expected, got %d", n)
	}
	r = setupAI(t, WithSampleRates(map[string]float64{"ai_interaction_classified": 0.5}))
	for n := 0; n < 40; n++ {
		TrackClassification(context.Background(), TrackClassificationOptions{InteractionID: "id-" + string(rune('a'+n)), ConversationID: "c", Intent: "x", Outcome: "resolved"})
	}
	ms := r.named("ai_interaction_classified")
	if len(ms) == 0 || len(ms) == 40 || ms[0].Properties["sample_rate"] != 0.5 {
		t.Fatalf("kept %d", len(ms))
	}
}

func TestTrackClassificationAfterInteractionEnded(t *testing.T) {
	r := setupAI(t, WithCaptureContent(false))
	ctx, i := StartInteraction(context.Background(), InteractionOptions{AnonymousID: "a", NoPromptEvent: true})
	i.End(EndOptions{Status: StatusError})
	waitBackgroundWork(5 * time.Second)
	r.reset()
	TrackClassification(ctx, TrackClassificationOptions{InteractionID: i.ID(), ConversationID: i.ConversationID(), Intent: "x", Outcome: "unresolved", ClassifierCostUSD: Ptr(0.5)})
	ms := r.named("ai_interaction_classified")
	if len(ms) != 1 {
		t.Fatalf("events: %v", r.names())
	}
	if _, ok := ms[0].Properties["total_cost_usd"]; ok || len(r.named("ai_response_received")) != 0 {
		t.Fatalf("%v", ms[0].Properties)
	}
}

func TestTrackClassificationDurationRounding(t *testing.T) {
	cases := []struct {
		in   float64
		want float64
	}{{0.49999999999999994, 0}, {0.5, 1}, {2.5, 3}, {640.4, 640}, {4503599627370497, 4503599627370497}, {1e100, 1e100}}
	for _, c := range cases {
		r := setupAI(t)
		TrackClassification(context.Background(), TrackClassificationOptions{InteractionID: "i", ConversationID: "c", Intent: "x", Outcome: "resolved", ClassifierDurationMS: Ptr(c.in)})
		got, ok := r.props(t, "ai_interaction_classified")["classifier_duration_ms"].(float64)
		if !ok || got != c.want {
			t.Errorf("%v: got %#v, want %v", c.in, r.props(t, "ai_interaction_classified")["classifier_duration_ms"], c.want)
		}
	}
	b, err := json.Marshal(map[string]any{"d": float64(640)})
	if err != nil || string(b) != `{"d":640}` {
		t.Fatalf("whole float encoding: %s %v", b, err)
	}
}

func TestTrackClassificationWarningCategories(t *testing.T) {
	r := setupAI(t)
	logs := captureLogs(t)
	bg := context.Background()
	TrackClassification(bg, TrackClassificationOptions{Intent: "x", Outcome: "resolved"})
	TrackClassification(bg, TrackClassificationOptions{InteractionID: "i", ConversationID: "c", Outcome: "resolved"})
	if len(r.all()) != 0 || logs.count("InteractionID and ConversationID are required") != 1 || logs.count("Intent is required") != 1 {
		t.Fatalf("warnings: %s", logs)
	}
}

func TestTrackClassificationDefaultOutcomes(t *testing.T) {
	r := setupAI(t)
	TrackClassification(context.Background(), TrackClassificationOptions{InteractionID: "i", ConversationID: "c", Intent: "x", Outcome: "unsupported"})
	if o := r.props(t, "ai_interaction_classified")["outcome"]; o != "unsupported" {
		t.Fatalf("outcome: %v", o)
	}
}

func TestTrackClassificationSummaryUnicodeSpace(t *testing.T) {
	r := setupAI(t)
	TrackClassification(context.Background(), TrackClassificationOptions{InteractionID: "i", ConversationID: "c", Intent: "x", Outcome: "resolved", IntentSummary: "First.\u00a0Second."})
	if s := r.props(t, "ai_interaction_classified")["intent_summary"]; s != "First." {
		t.Fatalf("summary: %q", s)
	}
}
