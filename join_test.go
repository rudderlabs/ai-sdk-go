package rudderstackai

import (
	"context"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	analytics "github.com/rudderlabs/analytics-go/v4"
)

type joinStep struct {
	Kind        string     `json:"kind"`
	Label       string     `json:"label"`
	Interaction string     `json:"interaction"`
	Parent      string     `json:"parent"`
	Children    []joinStep `json:"children"`
}

type joinScenario struct {
	Name      string     `json:"name"`
	Requires  string     `json:"requires"`
	EmitSpans bool       `json:"emit_spans"`
	Steps     []joinStep `json:"steps"`
	Expected  struct {
		Answers      map[string]int `json:"answers"`
		OtherAnswers int            `json:"other_answers"`
		Warnings     int            `json:"warnings"`
		Spans        []struct {
			Label       string  `json:"label"`
			Parent      *string `json:"parent"`
			Interaction string  `json:"interaction"`
		} `json:"spans"`
	} `json:"expected"`
}

// wrapperCall reports one call the way a provider wrapper does: the interaction comes from ctx,
// or an ephemeral one is opened and ended for it.
func wrapperCall(ctx context.Context, model string) {
	i, ephemeral := forCall(ctx, IntegrationOpenAI, nil)
	if i == nil {
		return
	}
	_, s := beginWrapperSpan(ctx, i, time.Now())
	i.recordWrapperCall(LLMCall{Provider: "openai", Model: model, ResponseID: "r-" + model, InputTokens: 1, OutputTokens: 1}, s, 200)
	if ephemeral {
		i.End(EndOptions{})
	}
}

func rearmJoinWarnings() {
	global.mu.Lock()
	delete(global.warned, "join:unknown")
	delete(global.warned, "join:parent")
	global.mu.Unlock()
}

func TestSharedExplicitContextFixture(t *testing.T) {
	var fixture struct {
		Scenarios []joinScenario `json:"scenarios"`
	}
	loadFixture(t, "spec/fixtures/explicit-context-v0.1.json", &fixture)
	for _, sc := range fixture.Scenarios {
		t.Run(sc.Name, func(t *testing.T) {
			if sc.Requires == "integration" {
				t.Skip("platform limit: Go has no framework integrations")
			}
			r := setupAI(t, WithEmitSpans(sc.EmitSpans))
			rearmJoinWarnings()
			logs := captureLogs(t)
			interactions := map[string]string{}
			spans := map[string]string{}
			var run func(ctx context.Context, steps []joinStep)
			run = func(ctx context.Context, steps []joinStep) {
				for _, st := range steps {
					switch st.Kind {
					case "interaction":
						ctx, i := StartInteraction(ctx, InteractionOptions{UserID: st.Label, Classify: Ptr(false)})
						interactions[st.Label] = i.ID()
						run(ctx, st.Children)
						i.End(EndOptions{})
					case "lost":
						run(context.Background(), st.Children)
					case "join":
						id, ok := interactions[st.Interaction]
						if !ok {
							id = "unknown-id"
						}
						parent := spans[st.Parent]
						if st.Parent == "invalid" {
							parent = "xyz"
						}
						run(JoinInteraction(ctx, id, JoinOptions{ParentSpanID: parent}), st.Children)
					case "manual":
						ctx, s := StartSpan(ctx, SpanOptions{Name: st.Label, Operation: "execute_tool"})
						spans[st.Label] = s.SpanID
						run(ctx, st.Children)
						s.End(nil)
					case "wrapper":
						wrapperCall(ctx, st.Label)
					}
				}
			}
			run(context.Background(), sc.Steps)
			waitBackgroundWork(10 * time.Second)

			labels := map[string]string{}
			for label, id := range interactions {
				labels[id] = label
			}
			counts := map[string]int{}
			for label := range sc.Expected.Answers {
				counts[label] = 0
			}
			other := 0
			for _, e := range r.named("ai_response_received") {
				label, known := labels[e.Properties["interaction_id"].(string)]
				if !known {
					other++
					continue
				}
				if _, want := counts[label]; want {
					counts[label] += e.Properties["llm_call_count"].(int)
				}
			}
			for label, n := range sc.Expected.Answers {
				if counts[label] != n {
					t.Errorf("answers of %s: %d calls, want %d", label, counts[label], n)
				}
			}
			if other != sc.Expected.OtherAnswers {
				t.Errorf("other answers %d, want %d", other, sc.Expected.OtherAnswers)
			}
			if n := logs.count("JoinInteraction"); n != sc.Expected.Warnings {
				t.Errorf("warnings %d, want %d: %s", n, sc.Expected.Warnings, logs.String())
			}
			exported := r.named("ai_otel_span")
			if len(exported) != len(sc.Expected.Spans) {
				t.Fatalf("exported %d spans, want %d", len(exported), len(sc.Expected.Spans))
			}
			byID := map[string]string{}
			label := func(name any) string { return strings.TrimPrefix(name.(string), "chat ") }
			for _, e := range exported {
				byID[e.Properties["span_id"].(string)] = label(e.Properties["span_name"])
			}
			first := ""
			for _, st := range sc.Steps {
				if st.Kind == "interaction" {
					first = st.Label
					break
				}
			}
			for _, want := range sc.Expected.Spans {
				found := false
				for _, e := range exported {
					if label(e.Properties["span_name"]) != want.Label {
						continue
					}
					found = true
					parent, _ := e.Properties["parent_span_id"].(string)
					got := byID[parent]
					if parent != "" && got == "" {
						got = "?"
					}
					if (want.Parent == nil && parent != "") || (want.Parent != nil && got != *want.Parent) {
						t.Errorf("%s: parent %q, want %v", want.Label, got, want.Parent)
					}
					owner := first
					if want.Interaction != "" {
						owner = want.Interaction
					}
					if labels[e.Properties["interaction_id"].(string)] != owner {
						t.Errorf("%s belongs to %v, want %s", want.Label, e.Properties["interaction_id"], owner)
					}
				}
				if !found {
					t.Errorf("span %s not exported", want.Label)
				}
			}
		})
	}
}

func TestJoinInteractionFromAGoroutine(t *testing.T) {
	r := setupAI(t, WithEmitSpans(true))
	ctx, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false)})
	_, parent := StartSpan(ctx, SpanOptions{Name: "fan-out", Operation: "invoke_agent"})
	var wg sync.WaitGroup
	for n := range 3 {
		wg.Go(func() {
			// A worker that only got the IDs, on a fresh context.
			joined := JoinInteraction(context.Background(), i.ID(), JoinOptions{ParentSpanID: parent.SpanID})
			wrapperCall(joined, "worker-"+strconv.Itoa(n))
		})
	}
	wg.Wait()
	parent.End(nil)
	i.End(EndOptions{})
	answers := r.named("ai_response_received")
	if len(answers) != 1 || answers[0].Properties["llm_call_count"] != 3 {
		t.Fatalf("want one answer with 3 calls, got %d answers", len(answers))
	}
	workers := 0
	for _, e := range r.named("ai_otel_span") {
		if !strings.HasPrefix(e.Properties["span_name"].(string), "chat worker-") {
			continue
		}
		workers++
		if e.Properties["parent_span_id"] != parent.SpanID {
			t.Errorf("worker span parent %v, want %s", e.Properties["parent_span_id"], parent.SpanID)
		}
	}
	if workers != 3 {
		t.Errorf("want 3 worker spans, got %d", workers)
	}
}

func TestJoinInteractionEdgeCases(t *testing.T) {
	setupAI(t, WithEmitSpans(true))
	rearmJoinWarnings()
	logs := captureLogs(t)
	//nolint:staticcheck // a nil context is tolerated
	if got := JoinInteraction(nil, "unknown-id", JoinOptions{}); got == nil {
		t.Fatal("nil context must become context.Background()")
	}
	ctx, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false)})
	defer i.End(EndOptions{})
	for _, bad := range []string{"0000000000000000", "ABCDEF0123456789", "abc"} {
		rearmJoinWarnings()
		joined := JoinInteraction(context.Background(), i.ID(), JoinOptions{ParentSpanID: bad})
		if FromContext(joined) != i || activeSDKSpan(joined) != nil {
			t.Errorf("%q: want the interaction without a parent", bad)
		}
	}
	if FromContext(JoinInteraction(ctx, "unknown-id", JoinOptions{})) != i {
		t.Error("an unknown ID must leave the context unchanged")
	}
	// The unknown ID warns twice: the loop re-armed it.
	if n := logs.count("JoinInteraction"); n != 5 {
		t.Errorf("want 5 warnings, got %d: %s", n, logs.String())
	}
}

func TestJoinInteractionRestoresTheInteractionScope(t *testing.T) {
	for _, after := range []string{"open", "ended"} {
		t.Run("interaction "+after, func(t *testing.T) {
			r := setupAI(t)
			started := WithValues(context.Background(), ContextValues{Feature: "support", Context: map[string]any{"app": "joined"}})
			_, i := StartInteraction(started, InteractionOptions{UserID: "u", ConversationID: "conv", AccountID: "acct", Variant: "v", AgentVersion: "2", Classify: Ptr(false)})
			// The worker's own context carries another request's values.
			caller := WithValues(context.Background(), ContextValues{UserID: "caller", ConversationID: "caller-conv", AccountID: "other", Variant: "w", Feature: "other"})
			joined := JoinInteraction(caller, i.ID(), JoinOptions{})
			if after == "ended" {
				i.End(EndOptions{})
			}
			TrackAction(joined, TrackActionOptions{InteractionID: i.ID(), ActionType: ActionFeedbackGiven})
			action := r.named("ai_user_action")
			if len(action) != 1 {
				t.Fatalf("want one action, got %d", len(action))
			}
			p := action[0].Properties
			if action[0].UserId != "u" || p["conversation_id"] != "conv" || p["account_id"] != "acct" {
				t.Errorf("action identity %q/%v/%v, want the interaction's", action[0].UserId, p["conversation_id"], p["account_id"])
			}
			// A child interaction started in the joined scope inherits it too.
			_, child := StartInteraction(joined, InteractionOptions{Classify: Ptr(false)})
			child.mu.Lock()
			got := []string{child.userID, child.conversationID, child.accountID, child.variant, child.agentVersion, child.feature}
			child.mu.Unlock()
			child.End(EndOptions{})
			if want := []string{"u", "conv", "acct", "v", "2", "support"}; !slices.Equal(got, want) {
				t.Errorf("child scope %v, want %v", got, want)
			}
			if after == "open" {
				i.End(EndOptions{})
			}
		})
	}
}

func TestJoinInteractionNewestOwnsAReusedID(t *testing.T) {
	setupAI(t)
	_, older := StartInteraction(context.Background(), InteractionOptions{InteractionID: "dup", UserID: "a", Classify: Ptr(false)})
	defer older.End(EndOptions{})
	_, newer := StartInteraction(context.Background(), InteractionOptions{InteractionID: "dup", UserID: "b", Classify: Ptr(false)})
	defer newer.End(EndOptions{})
	if got := FromContext(JoinInteraction(context.Background(), "dup", JoinOptions{})); got != newer {
		t.Error("the older interaction owned the reused ID")
	}
}

func TestJoinInteractionNeverJoinsAPausingInteraction(t *testing.T) {
	r := setupAI(t)
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false)})
	var during context.Context
	r.onEnqueue = func(m analytics.Message) {
		if track, ok := m.(analytics.Track); ok && track.Event == "ai_response_received" && during == nil {
			during = JoinInteraction(context.Background(), i.ID(), JoinOptions{})
		}
	}
	i.Pause(PauseOptions{})
	if during == nil || FromContext(during) == i {
		t.Error("a pausing interaction was joinable")
	}
	if FromContext(JoinInteraction(context.Background(), i.ID(), JoinOptions{})) == i {
		t.Error("a paused interaction was joinable")
	}
}

// gateHandler blocks the JoinInteraction parent warning until release closes, as a slow
// slog.Handler would.
type gateHandler struct{ entered, release chan struct{} }

func (h *gateHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *gateHandler) Handle(_ context.Context, r slog.Record) error {
	if strings.Contains(r.Message, "JoinInteraction: ParentSpanID") {
		close(h.entered)
		<-h.release
	}
	return nil
}
func (h *gateHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *gateHandler) WithGroup(string) slog.Handler      { return h }

// The warning logger is app code: a target paused, ended or replaced under its ID while it runs
// is not joined, and the caller's context comes back.
func TestJoinInteractionRechecksTheTargetAfterTheWarning(t *testing.T) {
	for _, change := range []string{"pause", "end", "replace"} {
		t.Run(change, func(t *testing.T) {
			setupAI(t, WithEmitSpans(true))
			rearmJoinWarnings()
			gate := &gateHandler{entered: make(chan struct{}), release: make(chan struct{})}
			prev := slog.Default()
			slog.SetDefault(slog.New(gate))
			t.Cleanup(func() { slog.SetDefault(prev) })
			caller, original := StartInteraction(context.Background(), InteractionOptions{UserID: "original", Classify: Ptr(false)})
			defer original.End(EndOptions{})
			_, target := StartInteraction(context.Background(), InteractionOptions{UserID: "joined", Classify: Ptr(false)})
			defer target.End(EndOptions{})
			out := make(chan context.Context, 1)
			go func() { out <- JoinInteraction(caller, target.ID(), JoinOptions{ParentSpanID: "invalid"}) }()
			<-gate.entered
			switch change {
			case "pause":
				if _, ok := target.Pause(PauseOptions{}); !ok {
					t.Fatal("pause failed")
				}
			case "end":
				target.End(EndOptions{})
			case "replace":
				_, newer := StartInteraction(context.Background(), InteractionOptions{InteractionID: target.ID(), UserID: "newer", Classify: Ptr(false)})
				defer newer.End(EndOptions{})
			}
			close(gate.release)
			if got := FromContext(<-out); got != original {
				t.Errorf("joined %p after the target changed, want the caller's %p", got, original)
			}
		})
	}
}
