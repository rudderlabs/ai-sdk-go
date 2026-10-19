package rudderstackai

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRunTaskChainsToTheContextInteraction(t *testing.T) {
	r := setupAI(t)
	flusher := &sink{}
	var parent *Interaction
	err := RunInteraction(WithValues(context.Background(), ContextValues{Request: nil, ConversationID: "ignored"}), InteractionOptions{UserID: "u", ConversationID: "conv"}, func(ctx context.Context, i *Interaction) error {
		parent = i
		return RunTask(ctx, "sub-agent", TaskOptions{Analytics: flusher, OnBehalfOf: "u"}, func(ctx context.Context, task *Interaction) error {
			if FromContext(ctx) != task || task.Trigger() != TriggerSystem {
				t.Error("task not in its context")
			}
			return nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if flusher.flushes.Load() != 0 {
		t.Fatal("a chained task flushed")
	}
	task := flusher.named("ai_response_received")
	if len(task) != 1 || len(flusher.names()) != 1 {
		t.Fatalf("task events: %v", flusher.names())
	}
	p := task[0].Properties
	if p["parent_interaction_id"] != parent.ID() || p["conversation_id"] != "conv" || p["trigger_source"] != "agent" ||
		p["task_name"] != "sub-agent" || p["trigger"] != "system" || p["on_behalf_of"] != "u" ||
		task[0].UserId != "" || task[0].AnonymousId != systemAnonymousID {
		t.Fatalf("task: %+v", task[0])
	}
	if r.response(t)["interaction_id"] != parent.ID() {
		t.Fatal("parent response")
	}
}

func TestRunTaskOptions(t *testing.T) {
	setupAI(t)
	boom := errors.New("boom")
	cases := []struct {
		name    string
		o       TaskOptions
		inside  bool
		err     error
		flushes int32
		check   func(t *testing.T, p map[string]any, parentID string)
	}{
		{name: "standalone", flushes: 1, check: func(t *testing.T, p map[string]any, _ string) {
			if _, has := p["parent_interaction_id"]; has {
				t.Fatal(p)
			}
			if _, has := p["trigger_source"]; has {
				t.Fatal(p)
			}
		}},
		{name: "no parent", o: TaskOptions{NoParent: true, TriggerSource: "queue"}, inside: true, flushes: 1, check: func(t *testing.T, p map[string]any, parentID string) {
			if _, has := p["parent_interaction_id"]; has || p["trigger_source"] != "queue" {
				t.Fatal(p)
			}
		}},
		{name: "explicit parent", o: TaskOptions{ParentInteractionID: "p1"}, inside: true, flushes: 1, check: func(t *testing.T, p map[string]any, _ string) {
			if p["parent_interaction_id"] != "p1" || p["trigger_source"] != "agent" {
				t.Fatal(p)
			}
		}},
		{name: "flush off", o: TaskOptions{Flush: Ptr(false)}, err: boom, check: func(t *testing.T, p map[string]any, _ string) {
			if p["response_status"] != "error" {
				t.Fatal(p)
			}
		}},
		{name: "flush on when chained", o: TaskOptions{Flush: Ptr(true), UserID: "actor"}, inside: true, flushes: 1, check: func(t *testing.T, p map[string]any, parentID string) {
			if p["parent_interaction_id"] != parentID {
				t.Fatal(p)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			client := &sink{flushErr: errors.New("flush failed")}
			o := c.o
			o.Analytics = client
			parentID := ""
			var got error
			if c.inside {
				_ = RunInteraction(context.Background(), InteractionOptions{UserID: "u", NoPromptEvent: true, Analytics: &sink{}}, func(ctx context.Context, i *Interaction) error {
					parentID = i.ID()
					got = RunTask(ctx, "job", o, func(context.Context, *Interaction) error { return c.err })
					return nil
				})
			} else {
				got = RunTask(nil, "job", o, func(context.Context, *Interaction) error { return c.err }) //nolint:staticcheck // nil context is tolerated
			}
			if !errors.Is(got, c.err) || (got == nil) != (c.err == nil) {
				t.Fatalf("error %v", got)
			}
			if client.flushes.Load() != c.flushes {
				t.Fatalf("flushes %d, want %d", client.flushes.Load(), c.flushes)
			}
			events := client.named("ai_response_received")
			if len(events) != 1 {
				t.Fatalf("events %v", client.names())
			}
			if c.o.UserID != "" && events[0].UserId != c.o.UserID {
				t.Fatal("task actor")
			}
			c.check(t, events[0].Properties, parentID)
		})
	}
}

// RunTask's own flush waits as long as Flush: the configured classifier timeout plus a second.
func TestFlushTimeoutFollowsClassifier(t *testing.T) {
	resetForTest()
	t.Cleanup(resetForTest)
	if got := flushTimeout(); got != defaultClassifyTimeout+time.Second {
		t.Fatalf("default = %v", got)
	}
	classify := func(context.Context, ClassifyTurn) (ClassifyResult, error) { return ClassifyResult{}, nil }
	if err := Configure(WithClassifier(&ClassifierOptions{Classify: classify, Timeout: 30 * time.Second})); err != nil {
		t.Fatal(err)
	}
	if got := flushTimeout(); got != 31*time.Second {
		t.Fatalf("configured = %v", got)
	}
}

func TestFlush(t *testing.T) {
	r := setupAI(t, WithClassifier(&ClassifierOptions{Timeout: time.Second, Classify: func(context.Context, ClassifyTurn) (ClassifyResult, error) {
		time.Sleep(20 * time.Millisecond)
		return ClassifyResult{Intent: "x", Outcome: "resolved"}, nil
	}}))
	global := &sink{}
	used := &sink{flushErr: errors.New("nope")}
	if err := Configure(WithAnalytics(global)); err != nil {
		t.Fatal(err)
	}
	answer(t, InteractionOptions{PromptText: "p", Analytics: used}, "a")
	if !Flush(0) {
		t.Fatal("classification not finished")
	}
	if len(used.named("ai_interaction_classified")) != 1 || used.flushes.Load() != 1 || global.flushes.Load() != 1 {
		t.Fatalf("used %v flushes %d global %d", used.names(), used.flushes.Load(), global.flushes.Load())
	}
	if len(r.all()) != 0 {
		t.Fatal("recorder used")
	}
	if err := Configure(WithClassifier(nil)); err != nil {
		t.Fatal(err)
	}
	if !Flush(0) {
		t.Fatal("nothing pending")
	}
}

func TestTrackAction(t *testing.T) {
	r := setupAI(t)
	logs := captureLogs(t)
	TrackAction(context.Background(), TrackActionOptions{ActionType: ActionCopiedResponse, InteractionID: "i"})
	TrackAction(nil, TrackActionOptions{ActionType: ActionCopiedResponse, ConversationID: "c"}) //nolint:staticcheck // nil context is tolerated
	if len(r.all()) != 0 || logs.count("ai_user_action dropped") != 2 {
		t.Fatalf("dropped: %v %s", r.names(), logs)
	}
	ctx := WithValues(context.Background(), ContextValues{UserID: "u", ConversationID: "conv", AccountID: "acct", OnBehalfOf: "boss", Surface: SurfaceIDE, Context: map[string]any{"k": 1}})
	TrackAction(ctx, TrackActionOptions{ActionType: ActionFeedbackGiven, InteractionID: "i", ActionDetails: map[string]any{
		"feedback_type": 5, "feedback_value": 1, "feedback_text": "mail a@b.com", "card_kind": "", "target_type": "doc",
	}})
	m := r.named("ai_user_action")[0]
	p := m.Properties
	details := p["action_details"].(map[string]any)
	if p["conversation_id"] != "conv" || p["account_id"] != "acct" || p["on_behalf_of"] != "boss" || p["surface"] != "ide" || m.UserId != "u" ||
		details["feedback_value"] != 1 || details["target_type"] != "doc" || len(details) != 2 || m.Context.Extra["k"] != 1 || integrationOf(m) != IntegrationManual {
		t.Fatalf("action: %+v", m)
	}
	if logs.count("must be a string") != 2 {
		t.Fatalf("logs: %s", logs)
	}

	// Content capture of the active interaction applies to feedback text.
	r.reset()
	actx, i := StartInteraction(ctx, InteractionOptions{CaptureContent: Ptr(true), NoPromptEvent: true})
	TrackAction(actx, TrackActionOptions{ActionType: ActionFeedbackGiven, InteractionID: i.ID(), ActionDetails: map[string]any{"feedback_text": "mail a@b.com", "x": 1}, UserID: "other", Surface: SurfaceAPI})
	if d := r.props(t, "ai_user_action")["action_details"].(map[string]any); d["feedback_text"] != "mail [email]" {
		t.Fatal(d)
	}
	if err := Configure(WithRedactHook(func(string) (string, error) { return "", errors.New("no") })); err != nil {
		t.Fatal(err)
	}
	r.reset()
	TrackAction(actx, TrackActionOptions{ActionType: ActionFeedbackGiven, InteractionID: "i", ActionDetails: map[string]any{"feedback_text": "x"}})
	if _, has := r.props(t, "ai_user_action")["action_details"]; has {
		t.Fatal("unmaskable feedback text kept")
	}
	// One rule: redaction off skips the built-in masking and the hook alike.
	if err := Configure(WithRedaction(false)); err != nil {
		t.Fatal(err)
	}
	r.reset()
	TrackAction(actx, TrackActionOptions{ActionType: ActionFeedbackGiven, InteractionID: "i", ActionDetails: map[string]any{"feedback_text": "mail a@b.com"}})
	if d := r.props(t, "ai_user_action")["action_details"].(map[string]any); d["feedback_text"] != "mail a@b.com" {
		t.Fatal(d)
	}
	if err := Configure(WithRedaction(true)); err != nil {
		t.Fatal(err)
	}

	// A non-standard action type warns and is still sent; server feedback off sends nothing.
	s := &sink{}
	TrackAction(ctx, TrackActionOptions{ActionType: "waved", InteractionID: "i", Analytics: s})
	if len(s.names()) != 1 || logs.count("non-standard action_type") != 1 {
		t.Fatalf("non-standard: %v %s", s.names(), logs)
	}
	if err := Configure(WithServerFeedback(false)); err != nil {
		t.Fatal(err)
	}
	TrackAction(ctx, TrackActionOptions{ActionType: ActionShared, InteractionID: "i", Analytics: s})
	if len(s.names()) != 1 {
		t.Fatal("sent with server feedback off")
	}
}

func TestTrackScore(t *testing.T) {
	r := setupAI(t, WithCaptureContent(true))
	logs := captureLogs(t)
	score := Score{EvaluationName: "helpful", ScoreValue: Ptr(1.0), Evaluator: EvaluatorHuman, Explanation: "good"}
	TrackScore(context.Background(), TrackScoreOptions{Score: score, InteractionID: "i"})
	TrackScore(context.Background(), TrackScoreOptions{Score: Score{EvaluationName: "x", Evaluator: EvaluatorHuman}, InteractionID: "i", ConversationID: "c"})
	if len(r.all()) != 0 || logs.count("ai_interaction_scored dropped") != 2 {
		t.Fatalf("dropped: %v %s", r.names(), logs)
	}
	ctx := WithValues(context.Background(), ContextValues{AnonymousID: "anon", ConversationID: "conv", Feature: "f", Surface: SurfaceEmail})
	TrackScore(ctx, TrackScoreOptions{Score: score, InteractionID: "i", Trigger: TriggerUser, AccountID: "acct", OnBehalfOf: "boss"})
	m := r.named("ai_interaction_scored")[0]
	p := m.Properties
	if p["conversation_id"] != "conv" || p["feature"] != "f" || p["surface"] != "email" || p["trigger"] != "user" || p["explanation"] != "good" ||
		p["score_value"] != 1.0 || p["evaluator"] != "human" || m.AnonymousId != "anon" || p["account_id"] != "acct" || p["on_behalf_of"] != "boss" {
		t.Fatalf("score: %+v", m)
	}
	// The active interaction's capture setting wins.
	r.reset()
	actx, _ := StartInteraction(ctx, InteractionOptions{CaptureContent: Ptr(false), NoPromptEvent: true})
	TrackScore(actx, TrackScoreOptions{Score: score, InteractionID: "i", Trigger: "bogus"})
	if p := r.props(t, "ai_interaction_scored"); p["explanation"] != nil || p["trigger"] != nil {
		t.Fatal(p)
	}
}
