// Package ruddergenai tracks Google Gen AI SDK (google.golang.org/genai) calls, on the Gemini API
// or Vertex AI, as RudderStack AI interaction events.
//
// The genai client has no middleware hook, so tracking sits in its HTTP client: pass
// [HTTPClient] (or a client using [Transport]) as genai.ClientConfig.HTTPClient.
//
//	client, err := genai.NewClient(ctx, &genai.ClientConfig{
//		APIKey:     os.Getenv("GEMINI_API_KEY"),
//		Backend:    genai.BackendGeminiAPI,
//		HTTPClient: ruddergenai.HTTPClient(nil),
//	})
//	err = rudderstackai.RunInteraction(ctx, rudderstackai.InteractionOptions{UserID: "u1"},
//		func(ctx context.Context, i *rudderstackai.Interaction) error {
//			_, err := client.Models.GenerateContent(ctx, "gemini-2.5-flash", genai.Text("Hi"), nil)
//			return err
//		})
//
// Models.GenerateContent and Models.GenerateContentStream are tracked (chats included, as they
// call the same endpoints); every other request passes through untracked. A call outside an
// interaction is reported as its own one-call interaction.
//
// Vertex AI with Google Cloud credentials: genai skips its own credential setup when given an
// HTTP client, so wrap an authenticated one, e.g.
// ruddergenai.HTTPClient(google.DefaultClient(ctx, "https://www.googleapis.com/auth/cloud-platform")).
//
// Tokens follow Google's usageMetadata: input is promptTokenCount (which includes
// cachedContentTokenCount) plus toolUsePromptTokenCount, output is candidatesTokenCount plus
// thoughtsTokenCount, and thoughts are also reasoning tokens, so input + output equals Google's
// totalTokenCount and nothing is priced twice.
package ruddergenai

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	analytics "github.com/rudderlabs/analytics-go/v4"
	"google.golang.org/genai"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/kit"
)

const integration = "google-genai"

// Provider labels.
const (
	providerGemini = "google"
	providerVertex = "google.vertex"
)

// codeExecution is Gemini's hosted code execution tool: each executableCode part is one call.
const codeExecution = "code_execution"

// Option configures tracking.
type Option func(*options)

type options struct {
	provider  string
	analytics analytics.Client
}

// WithProvider sets the provider label sent as provider. Default: google for the Gemini API,
// google.vertex for Vertex AI (detected from each request's URL).
func WithProvider(provider string) Option { return func(o *options) { o.provider = provider } }

// WithAnalytics sets the RudderStack client for calls made outside an interaction or context
// that names one.
func WithAnalytics(client analytics.Client) Option {
	return func(o *options) { o.analytics = client }
}

func init() { kit.RegisterErrorClassifier(classifyError) }

// Transport returns an http.RoundTripper that tracks the genai requests sent through base (nil:
// http.DefaultTransport). Requests and responses reach the SDK unchanged.
func Transport(base http.RoundTripper, opts ...Option) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	t := &transport{base: base}
	for _, opt := range opts {
		opt(&t.o)
	}
	return t
}

// HTTPClient returns a copy of base (nil: a zero http.Client) whose transport is wrapped with
// [Transport], for genai.ClientConfig.HTTPClient.
func HTTPClient(base *http.Client, opts ...Option) *http.Client {
	c := &http.Client{}
	if base != nil {
		*c = *base
	}
	c.Transport = Transport(c.Transport, opts...)
	return c
}

type transport struct {
	base http.RoundTripper
	o    options
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	method := methodOf(req.URL.Path)
	if method == "" {
		return t.base.RoundTrip(req)
	}
	provider := t.o.provider
	if provider == "" {
		provider = providerOf(req)
	}
	model := modelOf(req.URL.Path)
	spec := &kit.Spec{
		Provider:    provider,
		Integration: integration,
		Analytics:   t.o.analytics,
		IsStream:    func(*http.Request, map[string]any) bool { return method == "streamGenerateContent" },
		OnRequest: func(c *kit.Call) {
			c.Model = model
			recordFunctionResponses(c.Interaction, c.Request["contents"])
		},
		OnResponse: func(c *kit.Call, body map[string]any, _ *http.Response) bool {
			recordResponse(c, body)
			return true
		},
		NewStream: func(c *kit.Call, _ *http.Response) kit.StreamHandler { return newStream(c) },
		ErrorType: errorType,
	}
	return spec.Intercept(req, t.base.RoundTrip)
}

// methodOf returns the tracked method a request path calls (generateContent or
// streamGenerateContent), or "".
func methodOf(path string) string {
	_, method, ok := strings.Cut(path[strings.LastIndex(path, "/")+1:], ":")
	if ok && (method == "generateContent" || method == "streamGenerateContent") {
		return method
	}
	return ""
}

// modelOf returns the model a path names: the ID after models/, or the last resource name (a
// tuned model endpoint).
func modelOf(path string) string {
	name, _, _ := strings.Cut(path, ":")
	if i := strings.LastIndex(name, "/models/"); i >= 0 {
		return name[i+len("/models/"):]
	}
	return name[strings.LastIndex(name, "/")+1:]
}

// providerOf tells Vertex AI from the Gemini API by the request URL.
func providerOf(req *http.Request) string {
	host := req.URL.Hostname()
	if strings.Contains(req.URL.Path, "/publishers/google/models/") || strings.HasSuffix(host, "aiplatform.googleapis.com") {
		return providerVertex
	}
	return providerGemini
}

// usage is Google's usageMetadata in spec terms.
type usage struct {
	input, output, cached int
	reasoning             *int
}

func usageOf(v any) *usage {
	m := kit.Map(v)
	if m == nil {
		return nil
	}
	u := &usage{
		input:  kit.Int(m["promptTokenCount"]) + kit.Int(m["toolUsePromptTokenCount"]),
		output: kit.Int(m["candidatesTokenCount"]) + kit.Int(m["thoughtsTokenCount"]),
		cached: kit.Int(m["cachedContentTokenCount"]),
	}
	u.reasoning = kit.IntPtr(m["thoughtsTokenCount"])
	return u
}

// responseParts returns the parts of every candidate.
func responseParts(body map[string]any) []map[string]any {
	var out []map[string]any
	for _, c := range kit.List(body["candidates"]) {
		for _, p := range kit.List(kit.Map(kit.Map(c)["content"])["parts"]) {
			if part := kit.Map(p); part != nil {
				out = append(out, part)
			}
		}
	}
	return out
}

func hasCall(parts []map[string]any) bool {
	for _, p := range parts {
		if p["functionCall"] != nil {
			return true
		}
	}
	return false
}

var finishReasons = map[string]string{
	"STOP":                     "stop",
	"MAX_TOKENS":               "length",
	"SAFETY":                   "content_filter",
	"RECITATION":               "content_filter",
	"BLOCKLIST":                "content_filter",
	"PROHIBITED_CONTENT":       "content_filter",
	"SPII":                     "content_filter",
	"IMAGE_SAFETY":             "content_filter",
	"IMAGE_PROHIBITED_CONTENT": "content_filter",
	"IMAGE_RECITATION":         "content_filter",
}

// finishReasonOf normalises Gemini's finishReason; unknown values pass through. Gemini says STOP
// for a function call too, and a blocked prompt has no candidates, only a blockReason.
func finishReasonOf(body map[string]any, calledTool bool) string {
	candidates := kit.List(body["candidates"])
	raw := ""
	if len(candidates) > 0 {
		raw = kit.Str(kit.Map(candidates[0])["finishReason"])
	}
	if raw == "" {
		if kit.Str(kit.Map(body["promptFeedback"])["blockReason"]) != "" {
			return "content_filter"
		}
		return ""
	}
	if raw == "STOP" && calledTool {
		return "tool_calls"
	}
	if r, ok := finishReasons[raw]; ok {
		return r
	}
	return raw
}

// textOf returns the answer text, without thought summaries.
func textOf(parts []map[string]any) string {
	var b strings.Builder
	for _, p := range parts {
		if t, ok := p["text"].(string); ok && p["thought"] != true {
			b.WriteString(t)
		}
	}
	return b.String()
}

func recordResponse(c *kit.Call, body map[string]any) {
	parts := responseParts(body)
	u := usageOf(body["usageMetadata"])
	model := kit.Str(body["modelVersion"])
	if model == "" {
		model = c.Model
	}
	call := rudderstackai.LLMCall{
		Integration: integration, Provider: c.Provider, Model: model, RequestedModel: c.Model,
		ResponseID: kit.Str(body["responseId"]), FinishReason: finishReasonOf(body, hasCall(parts)),
		StartedAt: c.StartedAt, OutputText: textOf(parts), UsageUnknown: u == nil,
	}
	if u != nil {
		call.InputTokens, call.OutputTokens, call.ReasoningTokens, call.CachedInputTokens = u.input, u.output, u.reasoning, u.cached
	}
	if !c.Interaction.RecordCall(call) {
		return
	}
	var tools []rudderstackai.ToolCall
	for _, p := range parts {
		if fc := kit.Map(p["functionCall"]); fc != nil {
			tools = append(tools, rudderstackai.ToolCall{ID: kit.Str(fc["id"]), Name: kit.Str(fc["name"]), Args: fc["args"]})
		} else if p["executableCode"] != nil {
			tools = append(tools, rudderstackai.ToolCall{Name: codeExecution})
		}
	}
	if len(tools) > 0 {
		c.Interaction.RecordToolCalls(tools...)
	}
}

// recordFunctionResponses settles the tool calls a request answers. Google's convention for a
// failed call is an error key in the response.
func recordFunctionResponses(i *rudderstackai.Interaction, contents any) {
	for _, p := range requestParts(contents) {
		fr := kit.Map(p["functionResponse"])
		if fr == nil {
			continue
		}
		response := kit.Map(fr["response"])
		failed := response["error"] != nil
		var result any = response
		switch {
		case failed:
			result = response["error"]
		case response["output"] != nil:
			result = response["output"]
		}
		i.RecordToolResult(kit.Str(fr["id"]), rudderstackai.ToolResult{Error: &failed, Result: result})
	}
}

// requestParts returns the parts of request contents: a content, a part, or a list of those.
func requestParts(v any) []map[string]any {
	if list, ok := v.([]any); ok {
		var out []map[string]any
		for _, x := range list {
			out = append(out, requestParts(x)...)
		}
		return out
	}
	m := kit.Map(v)
	if m == nil {
		return nil
	}
	parts, ok := m["parts"].([]any)
	if !ok {
		return []map[string]any{m}
	}
	var out []map[string]any
	for _, p := range parts {
		if part := kit.Map(p); part != nil {
			out = append(out, part)
		}
	}
	return out
}

// Google RPC status names, for errors whose HTTP status names no error type.
var rpcErrorTypes = map[string]string{
	"RESOURCE_EXHAUSTED": "rate_limit",
	"DEADLINE_EXCEEDED":  "timeout",
	"UNAUTHENTICATED":    "auth_error",
	"PERMISSION_DENIED":  "auth_error",
	"INVALID_ARGUMENT":   "invalid_request",
	"CANCELLED":          "cancelled",
}

func errorType(status int, body map[string]any) string {
	if t := rudderstackai.ErrorTypeForStatus(status); t != "" {
		return t
	}
	return rpcErrorTypes[kit.Str(kit.Map(body["error"])["status"])]
}

// classifyError maps a genai.APIError (value or pointer) by its HTTP status, then its RPC status.
func classifyError(err error) (string, bool) {
	var e genai.APIError
	var p *genai.APIError
	switch {
	case errors.As(err, &e):
	case errors.As(err, &p) && p != nil:
		e = *p
	default:
		return "", false
	}
	if t := rudderstackai.ErrorTypeForStatus(e.Code); t != "" {
		return t, true
	}
	if t := rpcErrorTypes[e.Status]; t != "" {
		return t, true
	}
	return "api_error", true
}

// stream observes a streamGenerateContent response. usageMetadata is a running total for the
// request: the last value wins, never a sum.
type stream struct {
	*kit.Observer
	n          int
	open       string
	args       map[string]map[string]any
	calledTool bool
}

func newStream(c *kit.Call) *stream {
	return &stream{Observer: kit.NewObserver(c), args: map[string]map[string]any{}}
}

func (s *stream) OnEvent(_ string, data []byte) {
	var chunk map[string]any
	if json.Unmarshal(data, &chunk) != nil || chunk == nil {
		return
	}
	if m := kit.Str(chunk["modelVersion"]); m != "" {
		s.Model = m
	}
	if id := kit.Str(chunk["responseId"]); id != "" {
		s.ResponseID = id
	}
	if u := usageOf(chunk["usageMetadata"]); u != nil {
		s.InputTokens, s.OutputTokens, s.ReasoningTokens, s.CachedInputTokens = &u.input, &u.output, u.reasoning, u.cached
	}
	parts := responseParts(chunk)
	for _, p := range parts {
		text, _ := p["text"].(string)
		fc := kit.Map(p["functionCall"])
		if text != "" || fc != nil {
			s.FirstToken()
		}
		if p["thought"] != true {
			s.NoteText(text)
		}
		switch {
		case fc != nil:
			s.noteCall(fc)
		case p["executableCode"] != nil:
			s.NoteTool(s.nextKey(), "", codeExecution, nil, "")
		}
	}
	if hasCall(parts) {
		s.calledTool = true
	}
	if r := finishReasonOf(chunk, s.calledTool); r != "" {
		s.FinishReason = r
		s.Done = true
	}
}

func (s *stream) nextKey() string {
	s.n++
	return strconv.Itoa(s.n)
}

// noteCall notes a functionCall part. Vertex AI can stream one call's arguments over several
// parts (willContinue); the parts after the first carry no name and continue it.
func (s *stream) noteCall(fc map[string]any) {
	name := kit.Str(fc["name"])
	key := s.open
	if key == "" || name != "" {
		key = s.nextKey()
	}
	args := fc["args"]
	if partials := kit.List(fc["partialArgs"]); args == nil && len(partials) > 0 {
		merged := s.args[key]
		if merged == nil {
			merged = map[string]any{}
			s.args[key] = merged
		}
		for _, p := range partials {
			applyPartialArg(merged, kit.Map(p))
		}
		args = merged
	}
	s.NoteTool(key, kit.Str(fc["id"]), name, args, "")
	s.open = ""
	if fc["willContinue"] == true {
		s.open = key
	}
}

// applyPartialArg sets a streamed argument at a $.a.b path; strings that continue are appended.
func applyPartialArg(args, partial map[string]any) {
	path := kit.Str(partial["jsonPath"])
	if !strings.HasPrefix(path, "$.") {
		return
	}
	keys := strings.Split(path[2:], ".")
	for _, k := range keys {
		if k == "" {
			return
		}
	}
	var value any
	switch {
	case partial["stringValue"] != nil:
		value = partial["stringValue"]
	case partial["numberValue"] != nil:
		value = partial["numberValue"]
	case partial["boolValue"] != nil:
		value = partial["boolValue"]
	case hasKey(partial, "nullValue"):
		value = nil
	default:
		return
	}
	target := args
	for _, k := range keys[:len(keys)-1] {
		next := kit.Map(target[k])
		if next == nil {
			next = map[string]any{}
			target[k] = next
		}
		target = next
	}
	leaf := keys[len(keys)-1]
	if add, ok := value.(string); ok {
		if cur, ok := target[leaf].(string); ok {
			value = cur + add
		}
	}
	target[leaf] = value
}

func hasKey(m map[string]any, k string) bool {
	_, ok := m[k]
	return ok
}
