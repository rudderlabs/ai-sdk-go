package rudderstackai

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// answer runs one successful user interaction with a prompt and an answer.
func answer(t *testing.T, o InteractionOptions, out string) *Interaction {
	t.Helper()
	var inner *Interaction
	run(t, o, func(_ context.Context, i *Interaction) {
		inner = i
		i.RecordCall(LLMCall{Provider: "openai", Model: "m", OutputText: out})
	})
	return inner
}

func TestCustomClassifier(t *testing.T) {
	var mu sync.Mutex
	var seen []ClassifyTurn
	r := setupAI(t, WithClassifier(&ClassifierOptions{
		Intents: []string{"refund"},
		Classify: func(_ context.Context, turn ClassifyTurn) (ClassifyResult, error) {
			mu.Lock()
			seen = append(seen, turn)
			mu.Unlock()
			if turn.Prompt == "odd" {
				return ClassifyResult{Intent: "shopping", Outcome: "great", IntentSummary: "Mail a@b.com now. Second sentence."}, nil
			}
			return ClassifyResult{Intent: "refund", Outcome: "resolved", IntentSummary: "User wants a refund. Then more."}, nil
		},
	}))
	i := answer(t, InteractionOptions{PromptText: "I want a refund"}, "Done")
	answer(t, InteractionOptions{PromptText: "odd"}, "")
	got := r.wait(t, "ai_interaction_classified", 2)
	byID := map[string]map[string]any{}
	for _, m := range got {
		byID[m.Properties["interaction_id"].(string)] = m.Properties
	}
	p := byID[i.ID()]
	if p["intent"] != "refund" || p["outcome"] != "resolved" || p["intent_summary"] != "User wants a refund." || p["classifier"] != "custom" || p["trigger"] != "user" {
		t.Fatalf("classified: %v", p)
	}
	for id, p := range byID {
		if id != i.ID() && (p["intent"] != "other" || p["outcome"] != "unclear" || p["intent_summary"] != "Mail [email] now.") {
			t.Fatalf("closed lists: %v", p)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for _, turn := range seen {
		if turn.InteractionID == i.ID() && (turn.Prompt != "I want a refund" || turn.Answer != "Done" || !reflect.DeepEqual(turn.Outcomes, DefaultOutcomes)) {
			t.Fatalf("turn: %+v", turn)
		}
	}
}

func TestClassifierSkips(t *testing.T) {
	r := setupAI(t, WithClassifier(&ClassifierOptions{Classify: func(context.Context, ClassifyTurn) (ClassifyResult, error) {
		return ClassifyResult{Intent: "x", Outcome: "resolved"}, nil
	}}))
	logs := captureLogs(t)
	answer(t, InteractionOptions{PromptText: "p", Classify: Ptr(false)}, "a")
	answer(t, InteractionOptions{PromptText: "p", Trigger: TriggerSystem}, "a")
	answer(t, InteractionOptions{}, "") // no text at all
	_ = RunInteraction(context.Background(), InteractionOptions{UserID: "u", PromptText: "p"}, func(context.Context, *Interaction) error {
		return errors.New("failed")
	})
	waitBackgroundWork(5 * time.Second)
	if n := len(r.named("ai_interaction_classified")); n != 0 {
		t.Fatalf("%d classified", n)
	}
	if logs.count("needs prompt or answer text") != 1 {
		t.Fatalf("logs: %s", logs)
	}
	// Free-form intents without a closed list are kept.
	answer(t, InteractionOptions{PromptText: "p"}, "a")
	if p := r.wait(t, "ai_interaction_classified", 1)[0].Properties; p["intent"] != "x" {
		t.Fatal(p)
	}
}

type fakeModel struct {
	reply ClassifierReply
	err   error
	block bool
	mu    sync.Mutex
	sys   string
	user  string
}

func (f *fakeModel) Classify(ctx context.Context, system, user string) (ClassifierReply, error) {
	f.mu.Lock()
	f.sys, f.user = system, user
	f.mu.Unlock()
	if f.block {
		<-ctx.Done()
		return ClassifierReply{}, ctx.Err()
	}
	return f.reply, f.err
}

func TestModelClassifier(t *testing.T) {
	model := &fakeModel{reply: ClassifierReply{
		Text:  "Sure:\n{\"intent\":\"billing\",\"outcome\":\"resolved\",\"intent_summary\":\"Asks about a bill.\"}",
		Model: "gpt-4o-mini", Provider: "openai", UsageKnown: true, InputTokens: 1000, OutputTokens: 100,
	}}
	r := setupAI(t, WithClassifier(&ClassifierOptions{Client: model, Intents: []string{"billing"}, IntentSummary: true}))
	mustPrice(t, "gpt-4o-mini", rate(0.15, 0.6))
	answer(t, InteractionOptions{PromptText: "why <b>this</b> bill?"}, "because")
	p := r.wait(t, "ai_interaction_classified", 1)[0].Properties
	if p["intent"] != "billing" || p["classifier"] != "model" || p["classifier_model"] != "gpt-4o-mini" ||
		p["intent_summary"] != "Asks about a bill." || p["classifier_cost_usd"] != 0.00021 {
		t.Fatalf("model classified: %v", p)
	}
	model.mu.Lock()
	defer model.mu.Unlock()
	if !strings.Contains(model.sys, `["billing"]`) || !strings.Contains(model.sys, "intent_summary") ||
		!strings.Contains(model.user, "<user_prompt>why &lt;b&gt;this&lt;/b&gt; bill?</user_prompt>") {
		t.Fatalf("prompt:\n%s\n%s", model.sys, model.user)
	}
}

func TestModelClassifierFailures(t *testing.T) {
	for name, model := range map[string]*fakeModel{
		"unparseable": {reply: ClassifierReply{Text: "no json here"}},
		"bad json":    {reply: ClassifierReply{Text: "{not json}"}},
		"error":       {err: errors.New("down")},
		"timeout":     {block: true},
	} {
		t.Run(name, func(t *testing.T) {
			r := setupAI(t, WithClassifier(&ClassifierOptions{Client: model, Intents: []string{"x"}, Timeout: 50 * time.Millisecond}))
			logs := captureLogs(t)
			answer(t, InteractionOptions{PromptText: "p"}, "a")
			answer(t, InteractionOptions{PromptText: "p"}, "a")
			if !Flush(context.Background()) {
				t.Fatal("classification did not finish")
			}
			if len(r.named("ai_interaction_classified")) != 0 || logs.count("user_callback error in classifier") != 1 {
				t.Fatalf("events %v logs %s", r.names(), logs)
			}
		})
	}
}

func TestClassifierOptionsValidation(t *testing.T) {
	setupAI(t)
	model := &fakeModel{}
	fn := func(context.Context, ClassifyTurn) (ClassifyResult, error) { return ClassifyResult{}, nil }
	for name, o := range map[string]ClassifierOptions{
		"neither":          {},
		"both":             {Client: model, Classify: fn, Intents: []string{"x"}},
		"empty intents":    {Classify: fn, Intents: []string{}},
		"blank outcome":    {Classify: fn, Outcomes: []string{"ok", ""}},
		"client no intent": {Client: model},
		"negative timeout": {Classify: fn, Timeout: -time.Second},
	} {
		if err := Configure(WithClassifier(&o)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if err := Configure(WithClassifier(&ClassifierOptions{Classify: fn})); err != nil || cfg().classifier.Timeout != defaultClassifyTimeout {
		t.Fatal(err)
	}
	if err := Configure(WithClassifier(nil)); err != nil || cfg().classifier != nil {
		t.Fatal("classifier not cleared")
	}
}

func TestTurnText(t *testing.T) {
	setupAI(t)
	if clip("abcdef", 10) != "abcdef" || clip("abcdefghij", 5) != "ab…ij" {
		t.Fatal(clip("abcdefghij", 5))
	}
	msgs := []any{
		map[string]any{"role": "system", "content": "be nice"},
		map[string]any{"role": "human", "content": "first question"},
		map[string]any{"role": "ai", "content": []any{"part one", map[string]any{"type": "text", "text": "part two"}, map[string]any{"type": "tool_use", "text": "hidden"}}},
		map[string]any{"role": "tool", "content": "tool output"},
		map[string]any{"role": "model", "parts": []any{map[string]any{"text": "gemini says"}}},
		map[string]any{"content": "no role"},
		"not a message",
		map[string]any{"role": "user", "content": "latest prompt"},
	}
	if got := textMessages("just text"); len(got) != 1 || got[0].Role != "user" {
		t.Fatal(got)
	}
	if textMessages("") != nil {
		t.Fatal("empty string")
	}
	i := &Interaction{id: "i", conversationID: "c"}
	turn := buildTurn(i, turnSnapshot{messages: msgs, answer: "A", toolNames: []string{"t", "t", "u"}}, nil, DefaultOutcomes)
	want := []TurnMessage{{"user", "first question"}, {"assistant", "part one\npart two"}, {"assistant", "gemini says"}}
	if turn.Prompt != "latest prompt" || !reflect.DeepEqual(turn.History, want) || !reflect.DeepEqual(turn.ToolNames, []string{"t", "u"}) {
		t.Fatalf("turn: %+v", turn)
	}
	// History fills what the prompt and answer leave, latest first.
	long := strings.Repeat("x", turnCharCap)
	turn = buildTurn(i, turnSnapshot{prompt: long, answer: long, messages: []any{
		map[string]any{"role": "user", "content": "old"},
		map[string]any{"role": "assistant", "content": long},
		map[string]any{"role": "user", "content": "now"},
	}}, nil, nil)
	if n := len([]rune(turn.Prompt)); n != turnCharCap/3 {
		t.Fatalf("prompt %d", n)
	}
	if len(turn.History) != 1 || len([]rune(turn.History[0].Content)) != turnCharCap-2*(turnCharCap/3) {
		t.Fatalf("history: %d", len(turn.History))
	}
	system, user := ClassifierPrompt(ClassifyTurn{Prompt: "p", History: []TurnMessage{{"user", "<x>"}}, ToolNames: []string{"search"}}, false)
	if strings.Contains(system, "intent_summary") || !strings.Contains(system, "exactly one of []") ||
		!strings.Contains(user, "[user] &lt;x&gt;") || !strings.Contains(user, "<tools_called>search</tools_called>") {
		t.Fatalf("%s\n%s", system, user)
	}
}

func TestParseReplyAndSummary(t *testing.T) {
	setupAI(t)
	if _, ok := parseReply("}{"); ok {
		t.Fatal("bad braces parsed")
	}
	if r, ok := parseReply(`{"intent":"a","outcome":"b","intentSummary":"c"}`); !ok || r.IntentSummary != "c" {
		t.Fatal(r)
	}
	long := strings.Repeat("word ", 40)
	if got := summaryOf(long); len(strings.Fields(got)) != summaryWords {
		t.Fatalf("words: %q", got)
	}
	if got := summaryOf(strings.Repeat("a", 400)); len(got) != summaryChars {
		t.Fatalf("chars: %d", len(got))
	}
	if summaryOf("   ") != "" {
		t.Fatal("blank summary")
	}
	if err := Configure(WithRedactHook(func(string) (string, error) { return "", errors.New("no") })); err != nil {
		t.Fatal(err)
	}
	if summaryOf("fine") != "" {
		t.Fatal("a failed hook must withhold the summary")
	}
	if err := Configure(WithRedactHook(func(s string) (string, error) { return s + " call 555-123-4567", nil })); err != nil {
		t.Fatal(err)
	}
	if summaryOf("fine") != "" {
		t.Fatal("a summary still holding sensitive data must be withheld")
	}
	if err := Configure(WithRedactHook(func(string) (string, error) { panic("hook") })); err != nil {
		t.Fatal(err)
	}
	if _, ok := redactText("x"); ok {
		t.Fatal("a panicking hook must drop the text")
	}
	if _, ok := redactText(strings.Repeat("a", maxMaskedCodePoints+1)); ok {
		t.Fatal("over the cap")
	}
	if err := Configure(WithRedaction(false)); err != nil {
		t.Fatal(err)
	}
	if got, ok := redactText("a@b.com"); !ok || got != "a@b.com" {
		t.Fatal("redaction off")
	}
}

func TestBackgroundQueueLimit(t *testing.T) {
	setupAI(t)
	logs := captureLogs(t)
	release := make(chan struct{})
	for range maxInFlight + maxQueued {
		if enqueue(func() { <-release }) == nil {
			t.Fatal("dropped too early")
		}
	}
	if enqueue(func() {}) != nil {
		t.Fatal("queue past its cap")
	}
	if waitBackgroundWork(10 * time.Millisecond) {
		t.Fatal("work reported finished")
	}
	close(release)
	if !waitBackgroundWork(10 * time.Second) {
		t.Fatal("queue did not drain")
	}
	if logs.count("queue full") != 1 {
		t.Fatalf("logs: %s", logs)
	}
	v, err := withTimeout(time.Second, func(context.Context) (int, error) { panic("job") })
	if v != 0 || err == nil || !strings.Contains(err.Error(), "panic") {
		t.Fatal(v, err)
	}
}

// Work queued while Flush waits must not crash: a sync.WaitGroup panics when Add follows a
// Wait that has not returned.
func TestBackgroundWorkAddedWhileWaiting(t *testing.T) {
	setupAI(t)
	stop := make(chan struct{})
	waiters := make(chan struct{})
	go func() {
		defer close(waiters)
		for {
			select {
			case <-stop:
				return
			default:
				waitBackgroundWork(time.Millisecond)
			}
		}
	}()
	for range 20000 {
		if done := enqueue(func() {}); done != nil {
			<-done
		}
	}
	close(stop)
	<-waiters
	if !waitBackgroundWork(time.Second) {
		t.Fatal("work left pending")
	}
}

func TestEvaluator(t *testing.T) {
	r := setupAI(t, WithCaptureContent(true), WithEvaluator(func(_ context.Context, turn EvaluationTurn) ([]Score, error) {
		if turn.Prompt == "fail" {
			return nil, errors.New("judge down")
		}
		return []Score{
			{EvaluationName: "groundedness", ScoreValue: Ptr(0.9), Evaluator: EvaluatorModel, Sampled: Ptr(true), Explanation: "  " + strings.Repeat("é", 300) + " a@b.com"},
			{EvaluationName: "tone", ScoreLabel: "pass", Evaluator: EvaluatorCode},
			{ScoreValue: Ptr(1.0), Evaluator: EvaluatorHuman},
			{EvaluationName: "x", ScoreValue: Ptr(math.NaN()), Evaluator: EvaluatorHuman},
			{EvaluationName: "x", ScoreValue: Ptr(1.0), Evaluator: "robot"},
		}, nil
	}))
	logs := captureLogs(t)
	i := answer(t, InteractionOptions{PromptText: "p"}, "a")
	answer(t, InteractionOptions{PromptText: "fail"}, "a")
	FlushTimeout(time.Second)
	scores := r.named("ai_interaction_scored")
	if len(scores) != 2 {
		t.Fatalf("scores: %d", len(scores))
	}
	g := scores[0].Properties
	if scores[0].Properties["evaluation_name"] != "groundedness" {
		g = scores[1].Properties
	}
	if g["interaction_id"] != i.ID() || g["score_value"] != 0.9 || g["sampled"] != true || len([]rune(g["explanation"].(string))) != explanationChars {
		t.Fatalf("score: %v", g)
	}
	if logs.count("user_callback error in evaluator") != 1 || logs.count("judge down") != 0 || logs.count("ai_interaction_scored dropped") != 3 {
		t.Fatalf("logs: %s", logs)
	}
}

func TestEvaluatorTimeout(t *testing.T) {
	r := setupAI(t, WithEvaluator(func(ctx context.Context, _ EvaluationTurn) ([]Score, error) {
		if FromContext(ctx) != nil || !isUntracked(ctx) {
			return nil, errors.New("evaluator context is tracked")
		}
		<-ctx.Done()
		return []Score{{EvaluationName: "late", ScoreLabel: "x", Evaluator: EvaluatorCode}}, nil
	}))
	logs := captureLogs(t)
	answer(t, InteractionOptions{Trigger: TriggerSystem}, "")
	if !waitBackgroundWork(evaluateTimeout + 5*time.Second) {
		t.Fatal("evaluation never finished")
	}
	if len(r.named("ai_interaction_scored")) != 0 || logs.count("user_callback error in evaluator") != 1 {
		t.Fatalf("events %v logs %s", r.names(), logs)
	}
}
