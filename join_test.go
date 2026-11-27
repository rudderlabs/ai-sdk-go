package rudderstackai

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

// restoreFromJSON pauses i and restores it in this process from its snapshot's JSON, as a worker
// that stored the snapshot would.
func restoreFromJSON(ctx context.Context, t *testing.T, i *Interaction, o InteractionOptions) *Interaction {
	t.Helper()
	snapshot, ok := i.Pause(PauseOptions{})
	if !ok {
		t.Fatal("pause failed")
	}
	var stored PausedInteraction
	if err := json.Unmarshal([]byte(mustJSON(snapshot)), &stored); err != nil {
		t.Fatal(err)
	}
	o.Resume = &stored
	_, restored := StartInteraction(ctx, o)
	return restored
}

// A join carries the session and message context the interaction resolved, whether its options
// (cookies, a request, Context) or the context supplied them, to helpers, child interactions and
// a call after it ended.
func TestJoinInteractionCarriesTheResolvedSessionAndContext(t *testing.T) {
	const session = int64(1700000000000)
	cookies := map[string]string{"rl_session": `{"id":1700000000000}`}
	header := "rl_session=%7B%22id%22%3A1700000000000%7D"
	sources := map[string]func(context.Context, *InteractionOptions) context.Context{
		"options cookie values": func(ctx context.Context, o *InteractionOptions) context.Context {
			o.CookieValues = cookies
			return ctx
		},
		"options cookie header": func(ctx context.Context, o *InteractionOptions) context.Context {
			o.Cookies = header
			return ctx
		},
		"options request": func(ctx context.Context, o *InteractionOptions) context.Context {
			o.Request = httptest.NewRequest(http.MethodGet, "/", nil)
			o.Request.Header.Set("Cookie", header)
			return ctx
		},
		"options session ID": func(ctx context.Context, o *InteractionOptions) context.Context {
			o.Context = map[string]any{"sessionId": session}
			return ctx
		},
		"options context": func(ctx context.Context, o *InteractionOptions) context.Context {
			o.Context = map[string]any{"app": "owner"}
			return ctx
		},
		"context cookies": func(ctx context.Context, _ *InteractionOptions) context.Context {
			return WithValues(ctx, ContextValues{CookieValues: cookies})
		},
		"context session ID": func(ctx context.Context, _ *InteractionOptions) context.Context {
			return WithValues(ctx, ContextValues{Context: map[string]any{"sessionId": session}})
		},
	}
	for name, source := range sources {
		for _, phase := range []string{"open", "resumed", "restored"} {
			t.Run(name+"/"+phase, func(t *testing.T) {
				r := setupAI(t)
				o := InteractionOptions{UserID: "owner", ConversationID: "conv", Classify: Ptr(false)}
				started := source(context.Background(), &o)
				_, target := StartInteraction(started, o)
				switch phase {
				case "resumed":
					target.Pause(PauseOptions{})
					target.Resume()
				case "restored":
					target = restoreFromJSON(started, t, target, o)
				}
				from := len(r.all())
				// The worker's own context carries another request's message context.
				caller := WithValues(context.Background(), ContextValues{UserID: "caller", Context: map[string]any{"app": "caller"}})
				joined := JoinInteraction(caller, target.ID(), JoinOptions{})
				TrackAction(joined, TrackActionOptions{ActionType: ActionAccepted, InteractionID: target.ID()})
				TrackScore(joined, TrackScoreOptions{InteractionID: target.ID(), Score: Score{EvaluationName: "quality", ScoreValue: Ptr(1.0), Evaluator: "human"}})
				TrackClassification(joined, TrackClassificationOptions{InteractionID: target.ID(), Intent: "help", Outcome: "resolved"})
				TrackLimitReached(joined, TrackLimitReachedOptions{LimitType: "daily"})
				_, child := StartInteraction(joined, InteractionOptions{Classify: Ptr(false)})
				child.End(EndOptions{})
				target.End(EndOptions{})
				wrapperCall(joined, "after-end")
				events := r.all()[from:]
				var own map[string]any
				for _, e := range events {
					if e.Event == "ai_response_received" && e.Properties["interaction_id"] == target.ID() {
						own = e.Context.Extra
					}
				}
				wantSession, wantApp := own["sessionId"], own["app"]
				if phase != "restored" {
					// Restoring reads no cookies: only Context survives it, as the target's own events show.
					wantSession, wantApp = any(session), nil
					if name == "options context" {
						wantSession, wantApp = nil, "owner"
					}
					if own["sessionId"] != wantSession || own["app"] != wantApp {
						t.Fatalf("the target's own answer carries %v/%v, want %v/%v", own["sessionId"], own["app"], wantSession, wantApp)
					}
				}
				seen := map[string]bool{}
				for _, e := range events {
					seen[e.Event] = true
					if got := e.Context.Extra; got["sessionId"] != wantSession || got["app"] != wantApp {
						t.Errorf("%s of %v: session %v, app %v; want the target's %v, %v", e.Event, e.Properties["interaction_id"], got["sessionId"], got["app"], wantSession, wantApp)
					}
				}
				for _, want := range []string{"ai_user_action", "ai_interaction_scored", "ai_interaction_classified", "ai_limit_reached", "ai_user_prompt_created", "ai_response_received"} {
					if !seen[want] {
						t.Errorf("no %s sent", want)
					}
				}
				if answers := r.named("ai_response_received"); answers[len(answers)-1].Properties["llm_call_count"] != 1 {
					t.Error("the call after the end was not a separate answer")
				}
			})
		}
	}
}

// A join sends through the client the interaction resolved: its own, or the one a wrapper call
// adopted, never the caller's or the global one.
func TestJoinInteractionUsesTheInteractionsClient(t *testing.T) {
	for _, how := range []string{"options", "context", "adopted"} {
		for _, phase := range []string{"open", "resumed"} {
			t.Run(how+"/"+phase, func(t *testing.T) {
				global := setupAI(t)
				owner, caller := &sink{}, &sink{}
				ctx := context.Background()
				o := InteractionOptions{UserID: "owner", Classify: Ptr(false)}
				switch how {
				case "options":
					o.Analytics = owner
				case "context":
					ctx = WithValues(ctx, ContextValues{Analytics: owner})
				}
				ctx, target := StartInteraction(ctx, o)
				if how == "adopted" {
					forCall(ctx, IntegrationOpenAI, owner)
				}
				if phase == "resumed" {
					target.Pause(PauseOptions{})
					target.Resume()
				}
				before := len(owner.names())
				joined := JoinInteraction(WithValues(context.Background(), ContextValues{Analytics: caller}), target.ID(), JoinOptions{})
				TrackAction(joined, TrackActionOptions{ActionType: ActionAccepted, InteractionID: target.ID()})
				_, child := StartInteraction(joined, InteractionOptions{Classify: Ptr(false)})
				child.End(EndOptions{})
				got := owner.names()[before:]
				if !slices.Contains(got, "ai_user_action") || !slices.Contains(got, "ai_response_received") {
					t.Errorf("the owner's client got %v", got)
				}
				if n := len(caller.names()); n != 0 {
					t.Errorf("the caller's client got %d events", n)
				}
				if n := len(global.all()); n != 0 {
					t.Errorf("the global client got %d events", n)
				}
				target.End(EndOptions{})
			})
		}
	}
}

// A child interaction started with capture off inside a join keeps its calls, and their content,
// under its own policy.
func TestJoinInteractionChildKeepsItsCapturePolicy(t *testing.T) {
	for _, spans := range []bool{false, true} {
		for _, start := range []string{"StartInteraction", "RunInteraction"} {
			t.Run(fmt.Sprintf("%s spans=%v", start, spans), func(t *testing.T) {
				r := setupAI(t, WithEmitSpans(spans), WithLLMCallEvents(true), WithCaptureContent(true))
				_, target := StartInteraction(context.Background(), InteractionOptions{UserID: "target", Classify: Ptr(false)})
				joined := JoinInteraction(context.Background(), target.ID(), JoinOptions{})
				o := InteractionOptions{UserID: "child", CaptureContent: Ptr(false), Classify: Ptr(false)}
				var child *Interaction
				call := func(ctx context.Context) {
					i, _ := forCall(ctx, IntegrationOpenAI, nil)
					_, s := beginWrapperSpan(ctx, i, time.Now())
					i.recordWrapperCall(LLMCall{Provider: "openai", Model: "m", ResponseID: "r1", InputTokens: 1, OutputTokens: 1,
						InputMessages: []GenAIMessage{textMessage("user", "private-input")}, OutputText: "private-output"}, s, 200)
				}
				if start == "RunInteraction" {
					_ = RunInteraction(joined, o, func(ctx context.Context, c *Interaction) error {
						child = c
						call(ctx)
						return nil
					})
				} else {
					var ctx context.Context
					ctx, child = StartInteraction(joined, o)
					call(ctx)
					child.End(EndOptions{})
				}
				target.End(EndOptions{})
				waitBackgroundWork(10 * time.Second)
				for _, e := range r.all() {
					if b, _ := json.Marshal(e); strings.Contains(string(b), "private-") {
						t.Errorf("%s sent the child's content", e.Event)
					}
				}
				for _, e := range r.named("ai_response_received") {
					want := 0
					if e.Properties["interaction_id"] == child.ID() {
						want = 1
					}
					if e.Properties["llm_call_count"] != want {
						t.Errorf("%v counted %v calls, want %d", e.Properties["interaction_id"], e.Properties["llm_call_count"], want)
					}
				}
			})
		}
	}
}

// A context saved before the interaction was restored from JSON in this process follows the run,
// as in Node.js: its call counts in the restored interaction, or once that ended, as a separate
// answer, never in the superseded segment.
func TestInteractionSavedContextFollowsAJSONRestore(t *testing.T) {
	for _, spans := range []bool{false, true} {
		for _, phase := range []string{"ended", "restored", "restored twice", "restored and ended"} {
			t.Run(fmt.Sprintf("%s spans=%v", phase, spans), func(t *testing.T) {
				r := setupAI(t, WithEmitSpans(spans))
				saved, target := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false)})
				var outer *Span
				if spans {
					saved, outer = StartSpan(saved, SpanOptions{Name: "outer", Operation: "invoke_agent"})
				}
				id := target.ID()
				restores := map[string]int{"restored": 1, "restored twice": 3, "restored and ended": 1}[phase]
				for range restores {
					target = restoreFromJSON(context.Background(), t, target, InteractionOptions{Classify: Ptr(false)})
				}
				if restores > 0 && FromContext(saved) != target {
					t.Error("FromContext of the saved context is not the restored interaction")
				}
				separate := phase == "ended" || phase == "restored and ended"
				if separate {
					target.End(EndOptions{})
				}
				wrapperCall(saved, "late")
				target.End(EndOptions{})
				if outer != nil {
					outer.End(nil)
				}
				waitBackgroundWork(10 * time.Second)
				total, final := 0, -1
				for _, e := range r.named("ai_response_received") {
					n := e.Properties["llm_call_count"].(int)
					total += n
					if e.Properties["interaction_id"] == id && e.Properties["response_status"] != "paused" {
						final = n
					}
					if e.Properties["interaction_id"] != id && (!separate || e.Properties["parent_interaction_id"] != id) {
						t.Errorf("unexpected answer %v", e.Properties)
					}
				}
				if total != 1 {
					t.Errorf("the late call counted %d times, want once", total)
				}
				if want := map[bool]int{true: 0, false: 1}[separate]; final != want {
					t.Errorf("the run's final answer counted %d calls, want %d", final, want)
				}
				if n := openCount(); n != 0 {
					t.Errorf("%d interactions left open", n)
				}
				if !spans || separate {
					return
				}
				for _, e := range r.named("ai_otel_span") {
					if e.Properties["span_name"] != "chat late" {
						continue
					}
					if e.Properties["interaction_id"] != id || e.Properties["trace_id"] != outer.TraceID || e.Properties["parent_span_id"] != outer.SpanID {
						t.Errorf("late call span %v, want the run's, under %s", e.Properties, outer.SpanID)
					}
				}
			})
		}
	}
}

// A snapshot without a session restored inside another restored run's scope joins with no session
// of its own: helpers must not send the enclosing run's.
func TestSDK6945JoinRestoredWithoutSessionIgnoresEnclosingScopeSession(t *testing.T) {
	r := setupAI(t)
	snapshotOf := func(o InteractionOptions) PausedInteraction {
		_, i := StartInteraction(context.Background(), o)
		s, ok := i.Pause(PauseOptions{})
		if !ok {
			t.Fatal("pause failed")
		}
		var stored PausedInteraction
		if err := json.Unmarshal([]byte(mustJSON(s)), &stored); err != nil {
			t.Fatal(err)
		}
		return stored
	}
	outer := snapshotOf(InteractionOptions{UserID: "u", Classify: Ptr(false), Context: map[string]any{"app": "outer"}, CookieValues: map[string]string{"rl_session": `{"id":1700000000000}`}})
	inner := snapshotOf(InteractionOptions{UserID: "u", Classify: Ptr(false)})
	if inner.SessionID != 0 {
		t.Fatalf("inner snapshot has session %v", inner.SessionID)
	}
	err := RunInteraction(context.Background(), InteractionOptions{Resume: &outer}, func(ctx context.Context, _ *Interaction) error {
		_, restored := StartInteraction(ctx, InteractionOptions{Resume: &inner, Classify: Ptr(false)})
		from := len(r.all())
		joined := JoinInteraction(ctx, restored.ID(), JoinOptions{})
		TrackAction(joined, TrackActionOptions{ActionType: ActionAccepted, InteractionID: restored.ID()})
		for _, e := range r.all()[from:] {
			if got := e.Context.Extra["sessionId"]; got != nil {
				t.Errorf("%s carries the enclosing run's session %v", e.Event, got)
			}
		}
		restored.End(EndOptions{})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
