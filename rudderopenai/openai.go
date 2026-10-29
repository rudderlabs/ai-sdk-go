// Package rudderopenai tracks github.com/openai/openai-go/v3 calls as RudderStack AI events:
// Chat Completions and the Responses API, streamed or not, through the client's middleware.
//
// Calls inside an interaction ([rudderstackai.RunInteraction]) report into it; a call outside one
// emits its own ai_response_received. Tracking never changes what the app sends or receives,
// except that a streamed chat request asks for usage (stream_options.include_usage) and the usage
// chunk that adds is hidden from the app.
//
//	client := openai.NewClient(rudderopenai.WithTracking())
//	err := rudderstackai.RunInteraction(ctx, rudderstackai.InteractionOptions{UserID: "u1"},
//		func(ctx context.Context, _ *rudderstackai.Interaction) error {
//			_, err := client.Chat.Completions.New(ctx, params)
//			return err
//		})
//
// For an OpenAI-compatible gateway, name it: rudderopenai.WithTracking(rudderopenai.WithProvider("openrouter")).
package rudderopenai

import (
	"errors"
	"net/http"
	"strings"
	"sync"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/ssestream"
	analytics "github.com/rudderlabs/analytics-go/v4"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/kit"
)

const integration = "openai"

type config struct {
	details   rudderstackai.CallDetails
	provider  string
	analytics analytics.Client
	// streamUsage: nil asks with a fallback, true always asks, false never does.
	streamUsage *bool
}

// Option configures [Middleware] and [WithTracking].
type Option func(*config)

// WithProvider sets the provider label sent as provider. Defaults to openai; set it for
// OpenAI-compatible endpoints: openrouter, litellm, gateway, azure (azure.ai.openai), vllm, ...
func WithProvider(provider string) Option {
	return func(c *config) {
		if provider != "" {
			c.provider = provider
		}
	}
}

// WithAnalytics sets the RudderStack client for calls made outside an interaction when the
// configured global client should not be used.
func WithAnalytics(client analytics.Client) Option {
	return func(c *config) { c.analytics = client }
}

// WithStreamUsage sets whether streamed Chat Completions ask for usage
// (stream_options.include_usage), so tokens and cost are known. Without it, usage is asked on
// every host and a request the host rejects (HTTP 400 or 422 naming the parameter, or with no
// error code) is sent once more without it; once a host names it, this middleware stops adding it
// there. true always adds it (no resend); false never does, and streams without usage are
// recorded with unknown usage, unpriced.
func WithStreamUsage(on bool) Option {
	return func(c *config) { c.streamUsage = &on }
}

// WithTracking returns a client or request option that tracks the client's Chat Completions and
// Responses API calls. It is option.WithMiddleware(Middleware(opts...)).
func WithTracking(opts ...Option) option.RequestOption {
	return option.WithMiddleware(Middleware(opts...))
}

// Middleware returns an openai-go middleware that tracks Chat Completions (POST
// .../chat/completions) and Responses API (POST .../responses) calls; other requests pass through.
func Middleware(opts ...Option) option.Middleware {
	c := config{provider: "openai"}
	for _, o := range opts {
		o(&c)
	}
	chat := chatSpec(c, &rejectedHosts{hosts: map[string]bool{}})
	responses := responsesSpec(c)
	return func(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
		path := strings.TrimSuffix(req.URL.Path, "/")
		switch {
		case strings.HasSuffix(path, "/chat/completions"):
			return chat.Intercept(req, next)
		case strings.HasSuffix(path, "/responses"):
			return responses.Intercept(req, next)
		}
		return next(req)
	}
}

// rejectedHosts remembers hosts that named stream_options in a rejection, so this middleware stops
// adding it there instead of paying a failed request each time.
type rejectedHosts struct {
	mu    sync.Mutex
	hosts map[string]bool
}

func (r *rejectedHosts) has(host string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hosts[host]
}

func (r *rejectedHosts) add(host string) {
	r.mu.Lock()
	r.hosts[host] = true
	r.mu.Unlock()
}

// gateway is what a LiteLLM proxy reports in its response headers.
type gateway struct {
	cost *float64
	// model is the deployment that served the call; the body's model is the alias asked for.
	model string
}

func gatewayHeaders(resp *http.Response) gateway {
	var g gateway
	if resp == nil {
		return g
	}
	if c, ok := reportedCost(resp.Header.Get("x-litellm-response-cost")); ok {
		g.cost = c
	}
	g.model = strings.TrimSpace(resp.Header.Get("x-litellm-model-name"))
	return g
}

// streamErrorType maps a streamed error event (a chunk with a top-level error) like Node's
// APIError: a quota or rate-limit code is rate_limit, anything else api_error.
func streamErrorType(data map[string]any) string {
	if e := kit.Map(data["error"]); e != nil && kit.Map(e["error"]) != nil {
		data = e
	}
	return kit.HTTPErrorType(0, data)
}

func init() {
	// Errors the app gets back from openai-go map like the failed calls recorded for them.
	kit.RegisterErrorClassifier(func(err error) (string, bool) {
		var apiErr *openai.Error
		if errors.As(err, &apiErr) {
			return kit.HTTPErrorType(apiErr.StatusCode, map[string]any{"error": map[string]any{"code": apiErr.Code, "type": apiErr.Type}}), true
		}
		var streamErr *ssestream.StreamError
		if errors.As(err, &streamErr) {
			data, ok := decode(streamErr.Event.Data)
			if !ok {
				return "api_error", true
			}
			return streamErrorType(data), true
		}
		return "", false
	})
}

// WithCallPurpose labels the purpose of model calls through this integration.
func WithCallPurpose(purpose string) Option {
	return func(c *config) { c.details.CallPurpose = purpose }
}

// WithFallbackUsed records whether the app selected a fallback provider or model.
func WithFallbackUsed(used bool) Option { return func(c *config) { c.details.FallbackUsed = &used } }

// WithBYOK records whether the call used the end customer's own provider key.
func WithBYOK(byok bool) Option { return func(c *config) { c.details.BYOK = &byok } }
