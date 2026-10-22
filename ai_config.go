package rudderstackai

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"time"

	analytics "github.com/rudderlabs/analytics-go/v4"
)

// config is the process-wide configuration of the AI interaction events.
type config struct {
	analytics           analytics.Client
	captureContent      bool
	emitLLMCalls        bool
	toolBreakdown       bool
	identityFromCookies bool
	redact              bool
	redactHook          func(string) (string, error)
	toolRules           map[string]ToolEventRule
	classifier          *classifierConfig
	azureDeployments    map[string]string
	serverFeedback      bool
	evaluator           EvaluateFunc
	traceIDFrom         func(context.Context) string
	onError             func(error, ErrorInfo)
	limits              Limits
}

func defaultConfig() config {
	return config{identityFromCookies: true, redact: true, serverFeedback: true, limits: defaultLimits}
}

var global = struct {
	mu     sync.RWMutex
	cfg    config
	prices map[string]price
	warned map[string]time.Time
}{cfg: defaultConfig(), prices: map[string]price{}, warned: map[string]time.Time{}}

// cfg returns a snapshot of the configuration.
func cfg() config {
	global.mu.RLock()
	defer global.mu.RUnlock()
	return global.cfg
}

// ConfigOption changes one setting of [Configure].
type ConfigOption func(*config, *pending) error

// pending holds what Configure applies after every option validated.
type pending struct {
	prices          map[string]ModelPrice
	rearmClassifier bool
}

// Configure sets process-wide defaults for the AI interaction events. Only the settings passed
// change. An invalid option returns an error and changes nothing.
//
//	err := rudderstackai.Configure(rudderstackai.WithAnalytics(client))
func Configure(options ...ConfigOption) error {
	global.mu.RLock()
	next := global.cfg
	global.mu.RUnlock()
	todo := &pending{}
	for _, o := range options {
		if o == nil {
			continue
		}
		if err := o(&next, todo); err != nil {
			return err
		}
	}
	entries := map[string]price{}
	for model, p := range todo.prices {
		key, entry, err := priceEntry(model, p)
		if err != nil {
			return err
		}
		entries[key] = entry
	}
	global.mu.Lock()
	defer global.mu.Unlock()
	if !sameClient(next.analytics, global.cfg.analytics) {
		delete(global.warned, "no-client")
		delete(global.warned, "track-failed")
	}
	if todo.rearmClassifier {
		delete(global.warned, "error:"+KindUserCallback)
	}
	global.cfg = next
	for k, v := range entries {
		global.prices[k] = v
	}
	return nil
}

// WithAnalytics sets the RudderStack client used when no interaction or context names one.
// Nil clears it.
func WithAnalytics(client analytics.Client) ConfigOption {
	return func(c *config, _ *pending) error {
		c.analytics = client
		return nil
	}
}

// WithCaptureContent sends prompt_text, response_text and feedback_text (masked). Off by
// default: treat these as personal data.
func WithCaptureContent(on bool) ConfigOption {
	return func(c *config, _ *pending) error {
		c.captureContent = on
		return nil
	}
}

// WithLLMCallEvents also sends one ai_llm_response_received per model call. Off by default.
func WithLLMCallEvents(on bool) ConfigOption {
	return func(c *config, _ *pending) error {
		c.emitLLMCalls = on
		return nil
	}
}

// WithToolBreakdown adds tool_breakdown (calls, failures and durations per agent, MCP server,
// tool and parent tool) to ai_response_received. Tool names only, never inputs or outputs. Off by
// default.
func WithToolBreakdown(on bool) ConfigOption {
	return func(c *config, _ *pending) error {
		c.toolBreakdown = on
		return nil
	}
}

// WithIdentityFromCookies reads identity from the RudderStack JavaScript SDK cookies of a request
// (rl_anonymous_id, rl_user_id, rl_session). On by default.
func WithIdentityFromCookies(on bool) ConfigOption {
	return func(c *config, _ *pending) error {
		c.identityFromCookies = on
		return nil
	}
}

// WithRedaction turns redaction of opt-in text (prompt_text, response_text, feedback_text,
// intent_summary, explanation) on or off: the built-in masking and the [WithRedactHook] hook. On
// by default: emails, phone numbers, payment cards, IP addresses, IDs and API keys or tokens become
// [email], [phone], [card], [ip], [ssn], [iban] and [secret]. Names and addresses are not caught.
func WithRedaction(on bool) ConfigOption {
	return func(c *config, _ *pending) error {
		c.redact = on
		return nil
	}
}

// WithRedactHook runs your own masking after the built-in patterns, while redaction is on. If it
// fails or panics, the text field is dropped. Nil removes it.
func WithRedactHook(hook func(text string) (string, error)) ConfigOption {
	return func(c *config, _ *pending) error {
		c.redactHook = hook
		return nil
	}
}

// WithEventRules declares customer-named tool events: each call of a listed tool the model makes
// sends one track event with the rule's name, plus tool_name, tool_call_id, tool_status and
// tool_duration_ms and the interaction's IDs. Replaces earlier rules. Names starting with "ai_"
// or "mcp_" are rejected.
func WithEventRules(rules EventRules) ConfigOption {
	return func(c *config, _ *pending) error {
		out := map[string]ToolEventRule{}
		for tool, rule := range rules.Tools {
			if rule.Event == "" {
				return fmt.Errorf("rudderstackai: EventRules.Tools[%q]: event must be a non-empty string", tool)
			}
			if strings.HasPrefix(rule.Event, "ai_") || strings.HasPrefix(rule.Event, "mcp_") {
				return fmt.Errorf("rudderstackai: EventRules.Tools[%q]: event names starting with ai_ or mcp_ are reserved (%s)", tool, rule.Event)
			}
			out[tool] = rule
		}
		c.toolRules = out
		return nil
	}
}

// WithClassifier turns on intent classification: after each successful answer of an interaction
// you open, your classifier labels it with an intent and an outcome, sent as
// ai_interaction_classified. Nothing waits for it. Nil turns it off.
func WithClassifier(options *ClassifierOptions) ConfigOption {
	return func(c *config, p *pending) error {
		if options == nil {
			c.classifier = nil
			return nil
		}
		cc, err := newClassifierConfig(*options)
		if err != nil {
			return err
		}
		c.classifier = cc
		p.rearmClassifier = true
		return nil
	}
}

// WithEvaluator scores each successful answer after it is out, sent as ai_interaction_scored,
// one event per score. Nothing waits for it; its own model calls are never tracked. Nil turns it
// off.
func WithEvaluator(evaluate EvaluateFunc) ConfigOption {
	return func(c *config, _ *pending) error {
		c.evaluator = evaluate
		return nil
	}
}

// WithServerFeedback sends ai_user_action from [TrackAction]. On by default; turn it off when the
// browser sends its own copy and you want no server copy.
func WithServerFeedback(on bool) ConfigOption {
	return func(c *config, _ *pending) error {
		c.serverFeedback = on
		return nil
	}
}

// WithTraceIDFrom sets where an interaction without a TraceID finds its trace_id: fn reads it
// from the context at the start of the interaction and at each integration call until one is
// found. The rudderotel module provides one for the active OpenTelemetry span. Nil removes it.
func WithTraceIDFrom(fn func(ctx context.Context) string) ConfigOption {
	return func(c *config, _ *pending) error {
		c.traceIDFrom = fn
		return nil
	}
}

// WithPrices registers model prices, merged into the registered ones (as [RegisterPrice] for
// each).
func WithPrices(prices map[string]ModelPrice) ConfigOption {
	return func(_ *config, p *pending) error {
		if p.prices == nil {
			p.prices = map[string]ModelPrice{}
		}
		for k, v := range prices {
			p.prices[k] = v
		}
		return nil
	}
}

// WithAzureDeploymentModels maps Azure OpenAI deployment names to base models, used only to price
// calls with provider azure.ai.openai; events keep the deployment name. Replaces the previous
// mapping; nil clears it.
func WithAzureDeploymentModels(deployments map[string]string) ConfigOption {
	return func(c *config, _ *pending) error {
		out := map[string]string{}
		for d, m := range deployments {
			if d == "" || m == "" {
				return errors.New("rudderstackai: azure deployment models must map non-empty deployment names to models")
			}
			out[strings.ToLower(d)] = strings.ToLower(m)
		}
		c.azureDeployments = out
		return nil
	}
}

// resetForTest restores the defaults, empties the price table and forgets clients.
func resetForTest() {
	global.mu.Lock()
	global.cfg = defaultConfig()
	global.prices = map[string]price{}
	global.warned = map[string]time.Time{}
	global.mu.Unlock()
	clients.mu.Lock()
	clients.m.Clear()
	clients.order = nil
	clients.mu.Unlock()
	resetOpenForTest()
	reported.mu.Lock()
	reported.last = map[string]time.Time{}
	reported.mu.Unlock()
}

// sameClient compares clients without panicking on uncomparable implementations.
func sameClient(a, b analytics.Client) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	ta, tb := reflect.TypeOf(a), reflect.TypeOf(b)
	return ta == tb && ta.Comparable() && a == b
}

// clients are the clients the SDK sent through, for [Flush]: at most MaxMapEntries, oldest
// forgotten first. A client seen before costs one lock-free lookup.
var clients = struct {
	m     sync.Map // analytics.Client -> struct{}
	mu    sync.Mutex
	order []analytics.Client
}{}

// rememberClient keeps a client the SDK sent through, for [Flush].
func rememberClient(c analytics.Client) {
	if c == nil || !reflect.TypeOf(c).Comparable() {
		return
	}
	if _, ok := clients.m.Load(c); ok {
		return
	}
	limit := cfg().limits.MaxMapEntries
	clients.mu.Lock()
	if _, loaded := clients.m.LoadOrStore(c, struct{}{}); loaded {
		clients.mu.Unlock()
		return
	}
	clients.order = append(clients.order, c)
	full := len(clients.order) > limit
	if full {
		clients.m.Delete(clients.order[0])
		clients.order = clients.order[1:]
	}
	clients.mu.Unlock()
	if full {
		limitReached("MaxMapEntries (clients)")
	}
}

// rememberedClients are the clients rememberClient kept.
func rememberedClients() []analytics.Client {
	clients.mu.Lock()
	defer clients.mu.Unlock()
	return append([]analytics.Client(nil), clients.order...)
}

// warnOnce warns once per key, or again after every (zero: never again); otherwise logs at debug.
func warnOnce(key, msg string, every time.Duration) {
	global.mu.Lock()
	last, seen := global.warned[key]
	now := time.Now()
	again := !seen || (every > 0 && now.Sub(last) >= every)
	if again {
		global.warned[key] = now
	}
	global.mu.Unlock()
	if again {
		slog.Warn(logPrefix + msg)
		return
	}
	debugf(msg)
}

func warnf(format string, args ...any) { slog.Warn(logPrefix + fmt.Sprintf(format, args...)) }
