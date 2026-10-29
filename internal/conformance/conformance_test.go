// Package conformance runs the shared cross-SDK fixture spec/fixtures/conformance-v0.3.json
// (format: spec/fixtures/FIXTURE-FORMAT.md) against this SDK and its integrations.
package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	analytics "github.com/rudderlabs/analytics-go/v4"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
	"github.com/rudderlabs/ai-sdk-go/internal/testkit"
)

type obj = map[string]any

type scenario struct {
	Name         string           `json:"name"`
	Description  string           `json:"description"`
	Config       obj              `json:"config"`
	Replies      map[string][]obj `json:"replies"`
	Steps        []obj            `json:"steps"`
	Expected     []obj            `json:"expected"`
	ExactNumbers bool             `json:"exact_numbers"`
}

// apiDriver runs one `call` step of an API. Integration test files register theirs in init.
type apiDriver func(ctx context.Context, r *run, step obj) error

var apis = map[string]apiDriver{}

// gaps names scenarios a Go integration cannot reproduce, with the reason. Integration test files
// add theirs in init; keep it short and explain each one.
var gaps = map[string]string{}

// Ops and APIs that belong to Node.js and Python integrations with no Go counterpart.
var platformOps = map[string]string{
	"langchain_tool":             "LangChain is a Node.js and Python framework",
	"langchain_invoke":           "LangChain is a Node.js and Python framework",
	"langchain_stream":           "LangChain is a Node.js and Python framework",
	"langchain_retrieve":         "LangChain is a Node.js and Python framework",
	"langchain_multi_prompt_run": "LangChain is a Node.js and Python framework",
	"langchain_late_callbacks":   "LangChain is a Node.js and Python framework",
	"openai_agents_run":          "the OpenAI Agents SDK has no Go version",
	"claude_messages":            "the Claude Agent SDK has no Go version",
}

// recorder is an in-memory RudderStack client.
type recorder struct {
	mu   sync.Mutex
	msgs []analytics.Track
}

func (r *recorder) Enqueue(m analytics.Message) error {
	t, ok := m.(analytics.Track)
	if !ok {
		return fmt.Errorf("unexpected %T", m)
	}
	r.mu.Lock()
	r.msgs = append(r.msgs, t)
	r.mu.Unlock()
	return nil
}

func (r *recorder) Close() error { return nil }

func (r *recorder) all() []analytics.Track {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]analytics.Track(nil), r.msgs...)
}

// run is one scenario's state.
type run struct {
	t    *testing.T
	llm  *testkit.LLM
	last *rudderstackai.Interaction
	cur  *rudderstackai.Interaction
}

var llm *testkit.LLM

func TestMain(m *testing.M) {
	llm = testkit.StartLLM()
	code := m.Run()
	llm.Close()
	os.Exit(code)
}

func loadFixture(t *testing.T) []scenario {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(testkit.RepoRoot(), "spec/fixtures/conformance-v0.3.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Scenarios []scenario `json:"scenarios"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return f.Scenarios
}

// unsupported names why a scenario cannot run in Go, or "".
func unsupported(steps []obj) string {
	for _, s := range steps {
		op := str(s["op"])
		if why, ok := platformOps[op]; ok {
			return op + ": " + why
		}
		if op == "call" {
			if _, ok := apis[str(s["api"])]; !ok {
				return "api " + str(s["api"]) + " has no Go driver"
			}
		}
		for _, k := range []string{"steps", "during"} {
			if nested, ok := s[k].([]any); ok {
				if why := unsupported(objs(nested)); why != "" {
					return why
				}
			}
		}
	}
	return ""
}

func TestConformance(t *testing.T) {
	scenarios := loadFixture(t)
	skipped := map[string][]string{}
	passed := 0
	for _, sc := range scenarios {
		why := unsupported(sc.Steps)
		if g, ok := gaps[sc.Name]; ok && why == "" {
			why = g
		}
		if why != "" {
			skipped[why] = append(skipped[why], sc.Name)
			continue
		}
		ok := t.Run(sc.Name, func(t *testing.T) { runScenario(t, sc) })
		if ok {
			passed++
		}
	}
	reasons := make([]string, 0, len(skipped))
	for why := range skipped {
		reasons = append(reasons, why)
	}
	sort.Strings(reasons)
	total := 0
	for _, why := range reasons {
		total += len(skipped[why])
		t.Logf("skipped %d (%s): %s", len(skipped[why]), why, strings.Join(skipped[why], ", "))
	}
	t.Logf("conformance: %d run (%d passed), %d skipped, of %d", len(scenarios)-total, passed, total, len(scenarios))
}

func configure(t *testing.T, c obj, rec *recorder) {
	t.Helper()
	hook.Reset()
	opts := []rudderstackai.ConfigOption{
		rudderstackai.WithAnalytics(rec),
		rudderstackai.WithCaptureContent(boolOr(c["capture_content"], false)),
		rudderstackai.WithLLMCallEvents(boolOr(c["emit_llm_calls"], false)),
		rudderstackai.WithToolBreakdown(boolOr(c["tool_breakdown"], false)),
		rudderstackai.WithIdentityFromCookies(boolOr(c["identity_from_cookies"], true)),
		rudderstackai.WithRedaction(boolOr(c["redact"], true)),
		rudderstackai.WithServerFeedback(boolOr(c["server_feedback"], true)),
	}
	if az, ok := c["azure_deployment_models"].(obj); ok {
		m := map[string]string{}
		for k, v := range az {
			m[k] = str(v)
		}
		opts = append(opts, rudderstackai.WithAzureDeploymentModels(m))
	}
	if rules, ok := c["event_rules"].(obj); ok {
		opts = append(opts, rudderstackai.WithEventRules(eventRules(rules)))
	}
	if cl, ok := c["classifier"].(obj); ok {
		opts = append(opts, rudderstackai.WithClassifier(classifier(cl)))
	}
	if ev, ok := c["evaluator"]; ok {
		opts = append(opts, rudderstackai.WithEvaluator(evaluator(ev)))
	}
	if spec, ok := c["credits"]; ok {
		opts = append(opts, rudderstackai.WithCredits(func(call rudderstackai.CreditCall) (*float64, error) {
			if spec == "throw" {
				return nil, errRule
			}
			return fptr(obj0(obj0(spec)["by_model"])[call.Model]), nil
		}))
	}
	if err := rudderstackai.Configure(opts...); err != nil {
		t.Fatal(err)
	}
}

var errRule = errors.New("rule failed")

func eventRules(rules obj) rudderstackai.EventRules {
	out := rudderstackai.EventRules{Tools: map[string]rudderstackai.ToolEventRule{}}
	for name, v := range obj(rules["tools"].(obj)) {
		if s, ok := v.(string); ok {
			out.Tools[name] = rudderstackai.ToolEventRule{Event: s}
			continue
		}
		spec := v.(obj)
		rule := rudderstackai.ToolEventRule{Event: str(spec["event"])}
		if spec["properties"] == "throw" {
			rule.Properties = func(map[string]any, any) map[string]any { panic(errRule) }
		} else if keys, ok := spec["properties_args"].([]any); ok {
			rule.Properties = func(args map[string]any, _ any) map[string]any {
				p := map[string]any{}
				for _, k := range keys {
					if v, ok := args[str(k)]; ok {
						p[str(k)] = v
					}
				}
				return p
			}
		}
		switch spec["when"] {
		case "throw":
			rule.When = func(map[string]any, any) bool { panic(errRule) }
		case "never":
			rule.When = func(map[string]any, any) bool { return false }
		}
		out.Tools[name] = rule
	}
	return out
}

func classifier(c obj) *rudderstackai.ClassifierOptions {
	o := &rudderstackai.ClassifierOptions{Intents: strs(c["intents"]), Outcomes: strs(c["outcomes"])}
	delay := time.Duration(num(c["delay_ms"])) * time.Millisecond
	result := c["classify"]
	o.Classify = func(ctx context.Context, _ rudderstackai.ClassifyTurn) (rudderstackai.ClassifyResult, error) {
		if delay > 0 {
			time.Sleep(delay)
		}
		if result == "throw" {
			return rudderstackai.ClassifyResult{}, errRule
		}
		r := result.(obj)
		return rudderstackai.ClassifyResult{Intent: str(r["intent"]), Outcome: str(r["outcome"]), IntentSummary: str(r["intent_summary"])}, nil
	}
	return o
}

func evaluator(spec any) rudderstackai.EvaluateFunc {
	return func(context.Context, rudderstackai.EvaluationTurn) ([]rudderstackai.Score, error) {
		if spec == "throw" {
			return nil, errRule
		}
		var out []rudderstackai.Score
		for _, s := range objs(spec.(obj)["scores"].([]any)) {
			sc := rudderstackai.Score{EvaluationName: str(s["evaluation_name"]), ScoreLabel: str(s["score_label"]), Evaluator: str(s["evaluator"]), Explanation: str(s["explanation"])}
			if v, ok := s["score_value"].(float64); ok {
				sc.ScoreValue = &v
			}
			if b, ok := s["sampled"].(bool); ok {
				sc.Sampled = &b
			}
			out = append(out, sc)
		}
		return out, nil
	}
}

func runScenario(t *testing.T, sc scenario) {
	rec := &recorder{}
	configure(t, sc.Config, rec)
	llm.Reset()
	for route, replies := range sc.Replies {
		for _, rp := range replies {
			raw, _ := json.Marshal(rp)
			var reply testkit.Reply
			if err := json.Unmarshal(raw, &reply); err != nil {
				t.Fatal(err)
			}
			llm.Script(route, reply)
		}
	}
	r := &run{t: t, llm: llm}
	if err := r.steps(context.Background(), sc.Steps); err != nil {
		t.Fatalf("steps: %v", err)
	}
	rudderstackai.Flush(context.Background())
	actual := normalise(rec.all())
	for _, m := range rec.all() {
		props := map[string]any(m.Properties)
		if err := testkit.ValidateEvent(m.Event, jsonRoundTrip(props).(obj)); err != nil {
			t.Errorf("schema: %v", err)
		}
	}
	if diffs := differences(jsonRoundTrip(sc.Expected), jsonRoundTrip(actual), sc.ExactNumbers, ""); len(diffs) > 0 {
		got, _ := json.MarshalIndent(actual, "", "  ")
		t.Errorf("%s\n%s\nactual:\n%s", sc.Description, strings.Join(diffs, "\n"), got)
	}
}

func (r *run) steps(ctx context.Context, steps []obj) error {
	for _, s := range steps {
		if err := r.step(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

var errExpected = errors.New("expected the step to fail, but it completed")

func expectError(s obj, err error) error {
	if s["expect_error"] == true {
		if err == nil {
			return errExpected
		}
		return nil
	}
	return err
}

func (r *run) step(ctx context.Context, s obj) error {
	switch op := str(s["op"]); op {
	case "call":
		if details, ok := s["call_options"].(obj); ok {
			ctx = rudderstackai.WithCallDetails(ctx, rudderstackai.CallDetails{CallPurpose: str(details["call_purpose"]), FallbackUsed: boolPtr(details["fallback_used"]), BYOK: boolPtr(details["byok"])})
		}
		driver := apis[str(s["api"])]
		return expectError(s, driver(ctx, r, s))
	case "interaction":
		err := rudderstackai.RunInteraction(ctx, interactionOptions(obj0(s["options"])), func(ctx context.Context, i *rudderstackai.Interaction) error {
			return r.inside(ctx, i, objs0(s["steps"]))
		})
		rudderstackai.Flush(context.Background())
		return expectError(s, err)
	case "task":
		err := rudderstackai.RunTask(ctx, str(s["task_name"]), taskOptions(obj0(s["options"])), func(ctx context.Context, i *rudderstackai.Interaction) error {
			return r.inside(ctx, i, objs0(s["steps"]))
		})
		rudderstackai.Flush(context.Background())
		return expectError(s, err)
	case "context":
		v := obj0(s["values"])
		cv := rudderstackai.ContextValues{
			UserID: str(v["user_id"]), AnonymousID: str(v["anonymous_id"]), ConversationID: str(v["conversation_id"]),
			Feature: str(v["feature"]), Surface: rudderstackai.Surface(str(v["surface"])), AccountID: str(v["account_id"]),
			OnBehalfOf: str(v["on_behalf_of"]), AgentVersion: str(v["agent_version"]), Variant: str(v["variant"]), Context: obj0(v["context"]),
		}
		cv.Cookies, cv.CookieValues = cookies(v["cookies"])
		err := r.steps(rudderstackai.WithValues(ctx, cv), objs0(s["steps"]))
		if r.cur == nil {
			rudderstackai.Flush(context.Background())
		}
		return err
	case "raise":
		if retry, ok := s["retry_error"].(obj); ok {
			return fmt.Errorf("%s: %w", str(s["message"]), statusError(num(retry["status"])))
		}
		return errors.New(str(s["message"]))
	case "set_intent":
		r.cur.SetIntent(str(s["value"]))
	case "set_confidence":
		r.cur.SetConfidence(rudderstackai.Confidence(str(s["value"])))
	case "record_compaction":
		if n, ok := s["count"].(float64); ok {
			r.cur.RecordCompaction(int(n))
		} else {
			r.cur.RecordCompaction()
		}
	case "set_outcome":
		r.cur.SetOutcome(str(s["value"]))
	case "end":
		r.cur.End(rudderstackai.EndOptions{
			ResponseText: str(s["response_text"]), ExtraProperties: obj0(s["extra_properties"]),
			Status: rudderstackai.ResponseStatus(str(s["status"])), ErrorType: str(s["error_type"]),
			BlockReason: rudderstackai.BlockReason(str(s["block_reason"])), PauseReason: rudderstackai.PauseReason(str(s["pause_reason"])), ClientDisconnected: s["client_disconnected"] == true,
		})
	case "record_call":
		r.cur.RecordCall(llmCall(s))
	case "record_tool_call":
		if calls, ok := s["calls"].([]any); ok {
			var tc []rudderstackai.ToolCall
			for _, c := range objs(calls) {
				tc = append(tc, toolCall(c))
			}
			r.cur.RecordToolCalls(tc...)
		} else {
			n := 1
			if c, ok := s["count"].(float64); ok {
				n = int(c)
			}
			r.cur.RecordToolCallCount(n)
		}
	case "record_tool_result":
		c := toolCall(obj0(s["result"]))
		r.cur.RecordToolResult(str(s["id"]), rudderstackai.ToolResult{Error: c.Error, ErrorType: c.ErrorType, Duration: c.Duration, Result: c.Result})
	case "track_action":
		o := obj0(s["options"])
		id := str(o["interaction_id"])
		if id == "$last" && r.last != nil {
			id = r.last.ID()
		}
		rudderstackai.TrackAction(ctx, rudderstackai.TrackActionOptions{
			ActionType: rudderstackai.ActionType(str(o["action_type"])), InteractionID: id,
			ConversationID: str(o["conversation_id"]), UserID: str(o["user_id"]), AnonymousID: str(o["anonymous_id"]),
			ActionDetails: obj0(o["action_details"]), AccountID: str(o["account_id"]), OnBehalfOf: str(o["on_behalf_of"]), AgentVersion: str(o["agent_version"]), Variant: str(o["variant"]),
			Surface: rudderstackai.Surface(str(o["surface"])),
		})
	case "track_limit_reached":
		o := obj0(s["options"])
		id := str(o["interaction_id"])
		if id == "$last" && r.last != nil {
			id = r.last.ID()
		}
		rudderstackai.TrackLimitReached(ctx, rudderstackai.TrackLimitReachedOptions{
			LimitType: str(o["limit_type"]), Feature: str(o["feature"]), InteractionID: id,
			ConversationID: str(o["conversation_id"]), UserID: str(o["user_id"]), AnonymousID: str(o["anonymous_id"]),
			AccountID: str(o["account_id"]), OnBehalfOf: str(o["on_behalf_of"]), AgentVersion: str(o["agent_version"]), Variant: str(o["variant"]),
			Surface: rudderstackai.Surface(str(o["surface"])),
		})
	case "track_score":
		o := obj0(s["options"])
		id, conv := str(o["interaction_id"]), str(o["conversation_id"])
		if id == "$last" && r.last != nil {
			id = r.last.ID()
		}
		if conv == "$last" && r.last != nil {
			conv = r.last.ConversationID()
		}
		score := rudderstackai.Score{EvaluationName: str(o["evaluation_name"]), ScoreLabel: str(o["score_label"]), Evaluator: str(o["evaluator"]), Explanation: str(o["explanation"])}
		if v, ok := o["score_value"].(float64); ok {
			score.ScoreValue = &v
		}
		if b, ok := o["sampled"].(bool); ok {
			score.Sampled = &b
		}
		rudderstackai.TrackScore(ctx, rudderstackai.TrackScoreOptions{
			Score: score, InteractionID: id, ConversationID: conv, UserID: str(o["user_id"]), AnonymousID: str(o["anonymous_id"]),
			Feature: str(o["feature"]), Surface: rudderstackai.Surface(str(o["surface"])), Trigger: rudderstackai.Trigger(str(o["trigger"])),
			AccountID: str(o["account_id"]), OnBehalfOf: str(o["on_behalf_of"]), AgentVersion: str(o["agent_version"]), Variant: str(o["variant"]),
		})
	case "register_price":
		if err := rudderstackai.RegisterPrice(str(s["model"]), modelPrice(s)); err != nil {
			return err
		}
	case "configure_prices":
		prices := map[string]rudderstackai.ModelPrice{}
		for m, p := range obj0(s["prices"]) {
			prices[m] = modelPrice(p.(obj))
		}
		return rudderstackai.Configure(rudderstackai.WithPrices(prices))
	case "clear_prices":
		rudderstackai.ClearPrices()
	case "shutdown":
		rudderstackai.Shutdown(context.Background())
	default:
		return fmt.Errorf("unknown op %s", op)
	}
	return nil
}

func (r *run) inside(ctx context.Context, i *rudderstackai.Interaction, steps []obj) error {
	outer := r.cur
	r.cur, r.last = i, i
	defer func() { r.cur = outer }()
	return r.steps(ctx, steps)
}

// statusError is a provider error a retry library gave up on.
type statusError int

func (e statusError) Error() string       { return fmt.Sprintf("provider error (HTTP %d)", int(e)) }
func (e statusError) HTTPStatusCode() int { return int(e) }

func cookies(v any) (string, map[string]string) {
	switch c := v.(type) {
	case string:
		return c, nil
	case obj:
		m := map[string]string{}
		for k, val := range c {
			m[k] = str(val)
		}
		return "", m
	}
	return "", nil
}

func boolPtr(v any) *bool {
	if b, ok := v.(bool); ok {
		return &b
	}
	return nil
}

func interactionOptions(o obj) rudderstackai.InteractionOptions {
	out := rudderstackai.InteractionOptions{
		OneShot: o["one_shot"] == true, InteractionID: str(o["interaction_id"]),
		UserID: str(o["user_id"]), AnonymousID: str(o["anonymous_id"]), ConversationID: str(o["conversation_id"]),
		Feature: str(o["feature"]), Intent: str(o["intent"]), TraceID: str(o["trace_id"]), InputMethod: str(o["input_method"]),
		PromptText: str(o["prompt_text"]), Trigger: rudderstackai.Trigger(str(o["trigger"])), AccountID: str(o["account_id"]),
		OnBehalfOf: str(o["on_behalf_of"]), AgentVersion: str(o["agent_version"]), Variant: str(o["variant"]), TriggerSource: str(o["trigger_source"]), TaskName: str(o["task_name"]),
		ParentInteractionID: str(o["parent_interaction_id"]), ToolBreakdown: boolPtr(o["tool_breakdown"]),
		IdentityFromCookies: boolPtr(o["identity_from_cookies"]), Classify: boolPtr(o["classify"]),
		Surface: rudderstackai.Surface(str(o["surface"])), TriggerEventID: str(o["trigger_event_id"]),
		AttachmentTypes: strs(o["attachment_types"]),
	}
	if n, ok := o["attachment_count"].(float64); ok {
		out.AttachmentCount = rudderstackai.Ptr(int(n))
	}
	out.Cookies, out.CookieValues = cookies(o["cookies"])
	return out
}

func taskOptions(o obj) rudderstackai.TaskOptions {
	out := rudderstackai.TaskOptions{
		OneShot: o["one_shot"] == true, InteractionID: str(o["interaction_id"]),
		UserID: str(o["user_id"]), AnonymousID: str(o["anonymous_id"]), ConversationID: str(o["conversation_id"]), Feature: str(o["feature"]), TraceID: str(o["trace_id"]),
		AccountID: str(o["account_id"]), OnBehalfOf: str(o["on_behalf_of"]), AgentVersion: str(o["agent_version"]), Variant: str(o["variant"]), TriggerSource: str(o["trigger_source"]),
		ParentInteractionID: str(o["parent_interaction_id"]), ToolBreakdown: boolPtr(o["tool_breakdown"]),
		Classify: boolPtr(o["classify"]), Surface: rudderstackai.Surface(str(o["surface"])), TriggerEventID: str(o["trigger_event_id"]),
	}
	if v, has := o["parent_interaction_id"]; has && v == nil {
		out.NoParent = true
	}
	return out
}

func intPtr(v any) *int {
	if f, ok := v.(float64); ok {
		n := int(f)
		return &n
	}
	return nil
}

func llmCall(s obj) rudderstackai.LLMCall {
	c := rudderstackai.LLMCall{
		Credits: fptr(s["credits"]), CallPurpose: str(s["call_purpose"]), FallbackUsed: boolPtr(s["fallback_used"]), BYOK: boolPtr(s["byok"]),
		Provider: str(s["provider"]), Model: str(s["model"]), RequestedModel: str(s["requested_model"]),
		InputTokens: int(num(s["input_tokens"])), OutputTokens: int(num(s["output_tokens"])), ReasoningTokens: intPtr(s["reasoning_tokens"]),
		CachedInputTokens: int(num(s["cached_input_tokens"])), CacheWriteTokens: int(num(s["cache_write_tokens"])),
		CacheWrite1hTokens: int(num(s["cache_write_1h_tokens"])), ServiceTier: str(s["service_tier"]),
		CallCount: int(num(s["call_count"])), ErrorCount: intPtr(s["error_count"]), AgentName: str(s["agent_name"]),
		ResponseID: str(s["response_id"]), FinishReason: str(s["finish_reason"]), ErrorType: str(s["error_type"]),
		UsageUnknown: s["usage_known"] == false,
	}
	if timing, ok := s["timing"].(obj); ok {
		c.EndedAt = time.Now()
		c.StartedAt = c.EndedAt.Add(-time.Duration(num(timing["duration_ms"]) * float64(time.Millisecond)))
		if ms, ok := timing["first_token_ms"].(float64); ok {
			c.FirstTokenAt = c.StartedAt.Add(time.Duration(ms * float64(time.Millisecond)))
		}
	}
	if v, ok := s["cost_usd"].(float64); ok {
		c.CostUSD = &v
	}
	if m, ok := s["server_tool_calls"].(obj); ok {
		c.ServerToolCalls = map[string]int{}
		for k, v := range m {
			c.ServerToolCalls[k] = int(num(v))
		}
	}
	return c
}

func toolCall(c obj) rudderstackai.ToolCall {
	tc := rudderstackai.ToolCall{
		ID: str(c["id"]), Name: str(c["name"]), MCPServer: str(c["mcp_server"]), ParentTool: str(c["parent_tool"]),
		AgentName: str(c["agent_name"]), Error: boolPtr(c["error"]), ErrorType: str(c["error_type"]), Args: c["args"], Result: c["result"],
	}
	if ms, ok := c["duration_ms"].(float64); ok {
		d := time.Duration(ms * float64(time.Millisecond))
		tc.Duration = &d
	}
	return tc
}

func fptr(v any) *float64 {
	if f, ok := v.(float64); ok {
		return &f
	}
	return nil
}

func tokenPrice(p obj) rudderstackai.TokenPrice {
	return rudderstackai.TokenPrice{
		InputPerMtok: num(p["input_per_mtok"]), OutputPerMtok: num(p["output_per_mtok"]),
		CachedInputPerMtok: fptr(p["cached_input_per_mtok"]), CacheWritePerMtok: fptr(p["cache_write_per_mtok"]),
		CacheWrite1hPerMtok: fptr(p["cache_write_1h_per_mtok"]),
	}
}

func modelPrice(p obj) rudderstackai.ModelPrice {
	out := rudderstackai.ModelPrice{TokenPrice: tokenPrice(p), Provider: str(p["provider"])}
	if m, ok := p["tier_multipliers"].(obj); ok {
		out.TierMultipliers = map[string]float64{}
		for k, v := range m {
			out.TierMultipliers[k] = num(v)
		}
	}
	if lc, ok := p["long_context"].(obj); ok {
		out.LongContext = &rudderstackai.LongContextPrice{TokenPrice: tokenPrice(lc), ThresholdTokens: int(num(lc["threshold_tokens"]))}
	}
	if m, ok := p["server_tool_per_call"].(obj); ok {
		out.ServerToolPerCall = map[string]float64{}
		for k, v := range m {
			out.ServerToolPerCall[k] = num(v)
		}
	}
	return out
}

// ----------------------------------------------------------------- normalisation and comparison

var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func normalise(msgs []analytics.Track) []obj {
	ids := map[string]string{}
	counters := map[string]int{}
	symbol := func(v any, kind string) any {
		s, ok := v.(string)
		if !ok || !uuidV4.MatchString(s) {
			return v
		}
		// a one-shot interaction's conversation_id is its interaction_id
		if v, ok := ids["int:"+s]; ok && kind == "conv" {
			return v
		}
		key := kind + ":" + s
		if _, ok := ids[key]; !ok {
			counters[kind]++
			ids[key] = fmt.Sprintf("$%s%d", kind, counters[kind])
		}
		return ids[key]
	}
	measured := func(v any) any {
		if f, ok := v.(float64); ok && f >= 0 {
			return "<number>"
		}
		return v
	}
	out := make([]obj, 0, len(msgs))
	for _, m := range msgs {
		e := obj{"event": m.Event}
		if m.UserId != "" {
			e["userId"] = m.UserId
		}
		if m.AnonymousId != "" {
			e["anonymousId"] = m.AnonymousId
		}
		props := jsonRoundTrip(map[string]any(m.Properties)).(obj)
		if value, has := props["interaction_id"]; has {
			props["interaction_id"] = symbol(value, "int")
		}
		if v, ok := props["parent_interaction_id"]; ok {
			props["parent_interaction_id"] = symbol(v, "int")
		}
		if value, has := props["conversation_id"]; has {
			props["conversation_id"] = symbol(value, "conv")
		}
		for _, k := range []string{"latency_ms", "time_to_first_token_ms", "tool_duration_ms", "classifier_duration_ms"} {
			if v, ok := props[k]; ok {
				props[k] = measured(v)
			}
		}
		if tb, ok := props["tool_breakdown"].([]any); ok {
			for _, entry := range objs(tb) {
				if _, ok := entry["duration_ms"]; ok {
					entry["duration_ms"] = measured(entry["duration_ms"])
					entry["max_duration_ms"] = measured(entry["max_duration_ms"])
				}
			}
		}
		e["properties"] = props
		ctx := obj{}
		if m.Context != nil {
			for k, v := range m.Context.Extra {
				ctx[k] = v
			}
		}
		ctx = jsonRoundTrip(ctx).(obj)
		if wl, ok := ctx["wrapperLibrary"].(obj); ok {
			delete(wl, "name")
			delete(wl, "version")
		}
		e["context"] = ctx
		out = append(out, e)
	}
	return out
}

func differences(expected, actual any, exact bool, path string) []string {
	if en, ok := expected.(float64); ok {
		if an, ok := actual.(float64); ok {
			tol := 0.0
			if !exact {
				tol = 1e-9 * math.Max(1, math.Abs(en))
			}
			if math.Abs(en-an) <= tol {
				return nil
			}
			return []string{fmt.Sprintf("%s: expected %v, got %v", path, en, an)}
		}
	}
	if ea, ok := expected.([]any); ok {
		if aa, ok := actual.([]any); ok {
			if len(ea) != len(aa) {
				return []string{fmt.Sprintf("%s: expected %d items, got %d", path, len(ea), len(aa))}
			}
			var out []string
			for n := range ea {
				out = append(out, differences(ea[n], aa[n], exact, fmt.Sprintf("%s[%d]", path, n))...)
			}
			return out
		}
	}
	if eo, ok := expected.(obj); ok {
		if ao, ok := actual.(obj); ok {
			keys := map[string]bool{}
			for k := range eo {
				keys[k] = true
			}
			for k := range ao {
				keys[k] = true
			}
			sorted := make([]string, 0, len(keys))
			for k := range keys {
				sorted = append(sorted, k)
			}
			sort.Strings(sorted)
			var out []string
			for _, k := range sorted {
				p := k
				if path != "" {
					p = path + "." + k
				}
				ev, eok := eo[k]
				av, aok := ao[k]
				if eok != aok {
					out = append(out, fmt.Sprintf("%s: expected %s, got %s", p, js(ev, eok), js(av, aok)))
					continue
				}
				out = append(out, differences(ev, av, exact, p)...)
			}
			return out
		}
	}
	e, _ := json.Marshal(expected)
	a, _ := json.Marshal(actual)
	if string(e) != string(a) {
		return []string{fmt.Sprintf("%s: expected %s, got %s", path, e, a)}
	}
	return nil
}

func js(v any, present bool) string {
	if !present {
		return "(absent)"
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// ----------------------------------------------------------------- JSON helpers

func jsonRoundTrip(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		panic(err)
	}
	return out
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func num(v any) float64 {
	f, _ := v.(float64)
	return f
}

func boolOr(v any, fallback bool) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return fallback
}

func obj0(v any) obj {
	o, _ := v.(obj)
	return o
}

func objs(list []any) []obj {
	out := make([]obj, 0, len(list))
	for _, v := range list {
		if o, ok := v.(obj); ok {
			out = append(out, o)
		}
	}
	return out
}

func objs0(v any) []obj {
	l, _ := v.([]any)
	return objs(l)
}

func strs(v any) []string {
	l, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(l))
	for _, x := range l {
		out = append(out, str(x))
	}
	return out
}
