// Package rudderbedrock tracks Amazon Bedrock Runtime Converse and ConverseStream calls made with
// the AWS SDK for Go v2 as RudderStack AI events.
//
// Attach it to the client (or to one call) with [WithTracking]:
//
//	client := bedrockruntime.NewFromConfig(cfg, rudderbedrock.WithTracking())
//	err := rudderstackai.RunInteraction(ctx, rudderstackai.InteractionOptions{UserID: "user_1"},
//		func(ctx context.Context, _ *rudderstackai.Interaction) error {
//			_, err := client.Converse(ctx, &bedrockruntime.ConverseInput{
//				ModelId:  aws.String("us.anthropic.claude-sonnet-4-5-20250929-v1:0"),
//				Messages: []types.Message{{Role: types.ConversationRoleUser, Content: []types.ContentBlock{&types.ContentBlockMemberText{Value: "Hi"}}}},
//			})
//			return err
//		})
//
// A call outside an interaction opens one of its own, ended when the call (or its stream) ends.
// Other operations pass through untracked.
package rudderbedrock

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	analytics "github.com/rudderlabs/analytics-go/v4"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/kit"
)

const (
	provider    = "aws.bedrock"
	integration = "bedrock"
	trackerID   = "RudderStackAITracking"
	streamID    = "RudderStackAIStream"
)

// Option configures [WithTracking].
type Option func(*config)

type config struct {
	analytics analytics.Client
}

// WithAnalytics sets the RudderStack client for calls when no interaction or context names one.
func WithAnalytics(client analytics.Client) Option {
	return func(c *config) { c.analytics = client }
}

// WithTracking returns a client option that tracks Converse and ConverseStream calls: pass it to
// bedrockruntime.New / NewFromConfig, or to a single call. Applying it twice tracks a call once.
func WithTracking(opts ...Option) func(*bedrockruntime.Options) {
	c := &config{}
	for _, o := range opts {
		o(c)
	}
	return func(o *bedrockruntime.Options) {
		// Streamed events are seen as the SDK decodes them: its event stream reader cannot be
		// swapped after the SDK has started watching it.
		if _, ok := o.Protocol.(*eventProtocol); !ok && o.Protocol != nil {
			o.Protocol = &eventProtocol{ClientProtocol: o.Protocol}
		}
		o.APIOptions = append(o.APIOptions, func(s *middleware.Stack) error {
			if _, ok := s.Initialize.Get(trackerID); ok {
				return nil
			}
			// First, so validation errors are recorded like any failed call.
			if err := s.Initialize.Add(&tracker{cfg: c}, middleware.Before); err != nil {
				return err
			}
			// Last, so the body is wrapped before the SDK starts decoding the stream.
			return s.Deserialize.Add(streamWrapper{}, middleware.After)
		})
	}
}

func init() { kit.RegisterErrorClassifier(classifyError) }

// classifyError maps AWS errors: the exception name first, then the HTTP status.
func classifyError(err error) (string, bool) {
	var api smithy.APIError
	if errors.As(err, &api) {
		n := strings.ToLower(api.ErrorCode())
		switch {
		case strings.Contains(n, "throttling"):
			return "rate_limit", true
		case strings.Contains(n, "accessdenied"):
			return "auth_error", true
		case strings.Contains(n, "validationexception"):
			return "invalid_request", true
		}
	}
	var resp *smithyhttp.ResponseError
	if errors.As(err, &resp) {
		if t := rudderstackai.ErrorTypeForStatus(resp.HTTPStatusCode()); t != "" {
			return t, true
		}
	}
	return "", false
}

var finishReasons = map[types.StopReason]string{
	"end_turn":             "stop",
	"stop_sequence":        "stop",
	"tool_use":             "tool_calls",
	"max_tokens":           "length",
	"guardrail_intervened": "content_filter",
	"content_filtered":     "content_filter",
}

// finishReason is a Converse stopReason as a spec finish_reason; unknown values pass through.
func finishReason(r types.StopReason) string {
	if f, ok := finishReasons[r]; ok {
		return f
	}
	return string(r)
}

// tokens reads Converse usage in spec terms: AWS inputTokens excludes cache reads and writes.
func tokens(u *types.TokenUsage) (input, output, cached, written int) {
	cached = int(aws.ToInt32(u.CacheReadInputTokens))
	written = int(aws.ToInt32(u.CacheWriteInputTokens))
	return int(aws.ToInt32(u.InputTokens)) + cached + written, int(aws.ToInt32(u.OutputTokens)), cached, written
}

// docValue is a smithy document as plain JSON values.
func docValue(d document.Interface) any {
	if d == nil {
		return nil
	}
	b, err := d.MarshalSmithyDocument()
	if err != nil {
		return nil
	}
	var v any
	if json.Unmarshal(b, &v) != nil {
		return nil
	}
	return v
}

// toolResultValue: a single json block is its value, a single text block parsed JSON or text.
func toolResultValue(content []types.ToolResultContentBlock) any {
	if len(content) == 1 {
		switch b := content[0].(type) {
		case *types.ToolResultContentBlockMemberJson:
			return docValue(b.Value)
		case *types.ToolResultContentBlockMemberText:
			return kit.ParseToolValue(b.Value)
		}
	}
	out := make([]any, 0, len(content))
	for _, block := range content {
		switch b := block.(type) {
		case *types.ToolResultContentBlockMemberJson:
			out = append(out, map[string]any{"json": docValue(b.Value)})
		case *types.ToolResultContentBlockMemberText:
			out = append(out, map[string]any{"text": b.Value})
		}
	}
	return out
}

// recordToolResults settles earlier tool calls from the toolResult blocks sent back.
func recordToolResults(i *rudderstackai.Interaction, messages []types.Message) {
	for _, m := range messages {
		for _, block := range m.Content {
			r, ok := block.(*types.ContentBlockMemberToolResult)
			if !ok || aws.ToString(r.Value.ToolUseId) == "" {
				continue
			}
			failed := r.Value.Status == types.ToolResultStatusError
			i.RecordToolResult(*r.Value.ToolUseId, rudderstackai.ToolResult{Error: &failed, Result: toolResultValue(r.Value.Content)})
		}
	}
}

// plainMessages is the request's text as the classifier reads it ({role, content: [{text}]}).
func plainMessages(messages []types.Message) []any {
	out := make([]any, 0, len(messages))
	for _, m := range messages {
		var content []any
		for _, block := range m.Content {
			if t, ok := block.(*types.ContentBlockMemberText); ok {
				content = append(content, map[string]any{"text": t.Value})
			}
		}
		out = append(out, map[string]any{"role": string(m.Role), "content": content})
	}
	return out
}

func safe(fn func()) {
	defer func() { _ = recover() }()
	fn()
}

type streamKey struct{}

// tracker sees each operation's typed input and output.
type tracker struct{ cfg *config }

func (*tracker) ID() string { return trackerID }

func (t *tracker) HandleInitialize(ctx context.Context, in middleware.InitializeInput, next middleware.InitializeHandler) (middleware.InitializeOutput, middleware.Metadata, error) {
	var model *string
	var messages []types.Message
	stream := false
	switch p := in.Parameters.(type) {
	case *bedrockruntime.ConverseInput:
		model, messages = p.ModelId, p.Messages
	case *bedrockruntime.ConverseStreamInput:
		model, messages, stream = p.ModelId, p.Messages, true
	default:
		return next.HandleInitialize(ctx, in)
	}
	if kit.Untracked(ctx) {
		return next.HandleInitialize(ctx, in)
	}
	started := time.Now()
	i, ephemeral := kit.ForCall(ctx, integration, t.cfg.analytics)
	if i == nil {
		return next.HandleInitialize(ctx, in)
	}
	c := &kit.Call{Interaction: i, Ephemeral: ephemeral, Provider: provider, Integration: integration, Model: aws.ToString(model), StartedAt: started}
	safe(func() {
		recordToolResults(i, messages)
		kit.KeepMessages(i, plainMessages(messages))
	})
	var sc *streamCall
	if stream {
		sc = &streamCall{call: c, ctx: ctx}
		ctx = context.WithValue(ctx, streamKey{}, sc)
	}
	out, md, err := next.HandleInitialize(ctx, in)
	if err != nil {
		safe(func() { failed(c, sc, kit.ErrorTypeOfTransport(err)) })
		return out, md, err
	}
	if o, ok := out.Result.(*bedrockruntime.ConverseOutput); ok {
		id, _ := awsmiddleware.GetRequestIDMetadata(md)
		safe(func() { recordResponse(c, o, id) })
	}
	return out, md, err
}

// failed records a call that raised; a stream already under way fails with its tools seen.
func failed(c *kit.Call, sc *streamCall, errorType string) {
	if sc != nil && sc.end(kit.Errored, errorType) {
		return
	}
	c.Interaction.RecordCall(rudderstackai.LLMCall{Provider: c.Provider, Model: c.Model, ErrorType: errorType, Integration: c.Integration, StartedAt: c.StartedAt})
	if c.Ephemeral {
		kit.EndEphemeral(c.Interaction, errorType)
	}
}

func recordResponse(c *kit.Call, out *bedrockruntime.ConverseOutput, responseID string) {
	call := rudderstackai.LLMCall{
		Provider: c.Provider, Integration: c.Integration, Model: c.Model, FinishReason: finishReason(out.StopReason),
		ResponseID: responseID, StartedAt: c.StartedAt, UsageUnknown: out.Usage == nil,
	}
	if out.Usage != nil {
		call.InputTokens, call.OutputTokens, call.CachedInputTokens, call.CacheWriteTokens = tokens(out.Usage)
	}
	var tools []rudderstackai.ToolCall
	if m, ok := out.Output.(*types.ConverseOutputMemberMessage); ok {
		var text strings.Builder
		for _, block := range m.Value.Content {
			switch b := block.(type) {
			case *types.ContentBlockMemberText:
				text.WriteString(b.Value)
			case *types.ContentBlockMemberToolUse:
				tools = append(tools, rudderstackai.ToolCall{ID: aws.ToString(b.Value.ToolUseId), Name: aws.ToString(b.Value.Name), Args: docValue(b.Value.Input)})
			}
		}
		call.OutputText = text.String()
	}
	if c.Interaction.RecordCall(call) && len(tools) > 0 {
		c.Interaction.RecordToolCalls(tools...)
	}
	if c.Ephemeral {
		kit.EndEphemeral(c.Interaction, "")
	}
}

// streamCall is one ConverseStream call: its events arrive on the SDK's decoding goroutine, an
// early Close on the app's.
type streamCall struct {
	call *kit.Call
	ctx  context.Context

	mu    sync.Mutex
	obs   *kit.Observer
	ended bool
	// pending are decoded events the app may not have received yet: the SDK decodes up to
	// readAhead events ahead (two unbuffered channels), so an event counts once two more decoded.
	pending []smithy.Deserializable
}

const readAhead = 2

// start begins observing a streamed response, before the SDK reads its first event.
func (s *streamCall) start(resp *smithyhttp.Response) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := kit.NewObserver(s.call)
	// Only messageStop completes the answer; a stream that ends without it was cut off.
	o.Incomplete = true
	for _, h := range []string{"X-Amzn-Requestid", "X-Amz-Requestid"} {
		if id := resp.Header.Get(h); id != "" {
			o.ResponseID = id
			break
		}
	}
	s.obs = o
	resp.Body = &trackedBody{ReadCloser: resp.Body, s: s}
}

// end records the stream once; false when it was never observed.
func (s *streamCall) end(how kit.End, errorType string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.obs == nil {
		return false
	}
	s.endLocked(how, errorType)
	return true
}

func (s *streamCall) endLocked(how kit.End, errorType string) {
	if s.ended {
		return
	}
	s.ended = true
	safe(func() { s.obs.Finish(how, errorType) })
}

// observe sees each decoded event, or how decoding stopped.
func (s *streamCall) observe(event smithy.Deserializable, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended || s.obs == nil {
		return
	}
	if err != nil {
		// The app gets every event decoded before the stream stopped.
		for _, e := range s.pending {
			safe(func() { s.onEvent(e) })
		}
		s.pending = nil
	}
	switch {
	case errors.Is(err, io.EOF):
		s.endLocked(kit.Completed, "")
		return
	case err != nil:
		t := kit.ErrorTypeOfTransport(err)
		if s.ctx.Err() != nil {
			t = kit.ErrorTypeOfTransport(s.ctx.Err())
		}
		s.endLocked(kit.Errored, t)
		return
	}
	s.pending = append(s.pending, event)
	for len(s.pending) > readAhead {
		e := s.pending[0]
		s.pending = s.pending[1:]
		safe(func() { s.onEvent(e) })
	}
}

func (s *streamCall) onEvent(event smithy.Deserializable) {
	o := s.obs
	switch e := event.(type) {
	case *types.ContentBlockStartEvent:
		if t, ok := e.Start.(*types.ContentBlockStartMemberToolUse); ok {
			o.NoteTool(blockKey(e.ContentBlockIndex), aws.ToString(t.Value.ToolUseId), aws.ToString(t.Value.Name), nil, "")
		}
	case *types.ContentBlockDeltaEvent:
		o.FirstToken()
		switch d := e.Delta.(type) {
		case *types.ContentBlockDeltaMemberText:
			o.NoteText(d.Value)
		case *types.ContentBlockDeltaMemberToolUse:
			o.NoteToolArgs(blockKey(e.ContentBlockIndex), aws.ToString(d.Value.Input))
		}
	case *types.MessageStopEvent:
		o.FinishReason = finishReason(e.StopReason)
		o.Incomplete, o.Done = false, true
	case *types.ConverseStreamMetadataEvent:
		if e.Usage != nil {
			in, out, cached, written := tokens(e.Usage)
			o.InputTokens, o.OutputTokens, o.CachedInputTokens, o.CacheWriteTokens = &in, &out, cached, written
		}
	}
}

func blockKey(index *int32) string {
	if index == nil {
		return ""
	}
	return strconv.Itoa(int(*index))
}

// trackedBody marks a streamed response body: the protocol reports its events, and closing it
// before the stream ended cancels the call (the app stopped reading).
type trackedBody struct {
	io.ReadCloser
	s *streamCall
}

func (b *trackedBody) Close() error {
	// Ended first, so the decode error the close causes is not taken for a failure.
	b.s.mu.Lock()
	if !b.s.ended {
		how := kit.Cancelled
		if b.s.obs.Done {
			how = kit.Completed
		}
		b.s.endLocked(how, "")
	}
	b.s.mu.Unlock()
	return b.ReadCloser.Close()
}

// eventProtocol reports the events decoded from a tracked body.
type eventProtocol struct {
	smithyhttp.ClientProtocol
}

func (p *eventProtocol) DeserializeEventMessage(schema *smithy.Schema, registry *smithy.TypeRegistry, r io.Reader) (smithy.Deserializable, error) {
	event, err := p.ClientProtocol.DeserializeEventMessage(schema, registry, r)
	if b, ok := r.(*trackedBody); ok {
		b.s.observe(event, err)
	}
	return event, err
}

// streamWrapper wraps a successful ConverseStream response body.
type streamWrapper struct{}

func (streamWrapper) ID() string { return streamID }

func (streamWrapper) HandleDeserialize(ctx context.Context, in middleware.DeserializeInput, next middleware.DeserializeHandler) (middleware.DeserializeOutput, middleware.Metadata, error) {
	out, md, err := next.HandleDeserialize(ctx, in)
	sc, _ := ctx.Value(streamKey{}).(*streamCall)
	resp, ok := out.RawResponse.(*smithyhttp.Response)
	if sc == nil || err != nil || !ok || resp.StatusCode >= 300 || resp.Body == nil {
		return out, md, err
	}
	safe(func() { sc.start(resp) })
	return out, md, err
}
