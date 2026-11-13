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
	"fmt"
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
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
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
	details   rudderstackai.CallDetails
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

// classifyError preserves HTTP auth, rate and timeout errors before mapping AWS exception names.
func classifyError(err error) (string, bool) {
	var resp *smithyhttp.ResponseError
	if errors.As(err, &resp) {
		if t := rudderstackai.ErrorTypeForStatus(resp.HTTPStatusCode()); t != "" && t != "invalid_request" {
			return t, true
		}
	}
	var api smithy.APIError
	if errors.As(err, &api) {
		n := strings.ToLower(api.ErrorCode())
		switch {
		case strings.Contains(n, "throttling"):
			return "rate_limit", true
		case strings.Contains(n, "accessdenied"):
			return "auth_error", true
		case strings.Contains(n, "validationexception"):
			if kit.ContextLimit("", api.ErrorMessage()) {
				if resp != nil {
					return kit.HTTPErrorType(resp.HTTPStatusCode(), map[string]any{"message": api.ErrorMessage()}), true
				}
				return "context_limit", true
			}
			return "invalid_request", true
		}
	}
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

// toolOutcomeValue is a tool result as the tool rules and tool_breakdown see it: a single json
// block is its value, a single text block parsed JSON or text. Unchanged by per-call content.
func toolOutcomeValue(content []types.ToolResultContentBlock) any {
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

// toolResultValue is a tool result as sent: a single json block is its value, text only is one
// string, anything else the list of parts (media as blobs, never bytes).
func toolResultValue(content []types.ToolResultContentBlock) any {
	if len(content) == 1 {
		if j, ok := content[0].(*types.ToolResultContentBlockMemberJson); ok {
			return docValue(j.Value)
		}
	}
	var parts []rudderstackai.GenAIPart
	for _, block := range content {
		switch b := block.(type) {
		case *types.ToolResultContentBlockMemberJson:
			parts = append(parts, rudderstackai.GenAIPart{"type": "json", "content": docValue(b.Value)})
		case *types.ToolResultContentBlockMemberText:
			parts = append(parts, kit.TextPart(b.Value)...)
		case *types.ToolResultContentBlockMemberImage:
			parts = append(parts, imagePart(b.Value)...)
		case *types.ToolResultContentBlockMemberVideo:
			parts = append(parts, videoPart(b.Value)...)
		case *types.ToolResultContentBlockMemberDocument:
			parts = append(parts, documentPart(b.Value)...)
		default:
			parts = append(parts, kit.OtherPart(otherName(block))...)
		}
	}
	return kit.ResponseOf(parts)
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
			kit.RecordToolResult(i, *r.Value.ToolUseId, rudderstackai.ToolResult{Error: &failed, Result: toolOutcomeValue(r.Value.Content)}, len(r.Value.Content) > 0)
		}
	}
}

// otherName is the Converse JSON key of a block the SDK does not map.
func otherName(block any) string {
	t := fmt.Sprintf("%T", block)
	t = t[strings.LastIndex(t, "Member")+len("Member"):]
	if t == "" {
		return ""
	}
	return strings.ToLower(t[:1]) + t[1:]
}

// mediaPart maps an image, video or document block: an S3 location is a URI, inline bytes a bare blob.
func mediaPart(modality string, s3 *types.S3Location, mime string) []rudderstackai.GenAIPart {
	if s3 != nil {
		return kit.URIPart(modality, aws.ToString(s3.Uri), mime)
	}
	return []rudderstackai.GenAIPart{kit.BlobPart(modality, mime)}
}

func imagePart(b types.ImageBlock) []rudderstackai.GenAIPart {
	var s3 *types.S3Location
	if src, ok := b.Source.(*types.ImageSourceMemberS3Location); ok {
		s3 = &src.Value
	}
	mime := ""
	if b.Format != "" {
		mime = "image/" + string(b.Format)
	}
	return mediaPart("image", s3, mime)
}

func videoPart(b types.VideoBlock) []rudderstackai.GenAIPart {
	var s3 *types.S3Location
	if src, ok := b.Source.(*types.VideoSourceMemberS3Location); ok {
		s3 = &src.Value
	}
	return mediaPart("video", s3, "")
}

func documentPart(b types.DocumentBlock) []rudderstackai.GenAIPart {
	var s3 *types.S3Location
	if src, ok := b.Source.(*types.DocumentSourceMemberS3Location); ok {
		s3 = &src.Value
	}
	return mediaPart("document", s3, "")
}

// blockParts maps one Converse content block to GenAI parts, or returns a tool result to send as
// its own tool message.
func blockParts(block types.ContentBlock) (parts []rudderstackai.GenAIPart, response rudderstackai.GenAIPart) {
	switch b := block.(type) {
	case *types.ContentBlockMemberText:
		return kit.TextPart(b.Value), nil
	case *types.ContentBlockMemberToolUse:
		return kit.ToolCallPart(aws.ToString(b.Value.ToolUseId), aws.ToString(b.Value.Name), docValue(b.Value.Input)), nil
	case *types.ContentBlockMemberToolResult:
		var result any
		if len(b.Value.Content) > 0 {
			result = toolResultValue(b.Value.Content)
		}
		return nil, kit.ToolResponsePart(aws.ToString(b.Value.ToolUseId), result)
	case *types.ContentBlockMemberReasoningContent:
		if t, ok := b.Value.(*types.ReasoningContentBlockMemberReasoningText); ok {
			return kit.ReasoningPart(aws.ToString(t.Value.Text)), nil
		}
		return kit.OtherPart("reasoningContent"), nil
	case *types.ContentBlockMemberImage:
		return imagePart(b.Value), nil
	case *types.ContentBlockMemberVideo:
		return videoPart(b.Value), nil
	case *types.ContentBlockMemberDocument:
		return documentPart(b.Value), nil
	}
	return kit.OtherPart(otherName(block)), nil
}

// inputMessages is the conversation Converse was sent: system blocks first, then every message,
// with each tool result as its own tool message, in order.
func inputMessages(system []types.SystemContentBlock, messages []types.Message) []rudderstackai.GenAIMessage {
	var all []rudderstackai.GenAIMessage
	if system != nil {
		var parts []rudderstackai.GenAIPart
		for _, b := range system {
			if t, ok := b.(*types.SystemContentBlockMemberText); ok {
				parts = append(parts, kit.TextPart(t.Value)...)
			} else {
				parts = append(parts, kit.OtherPart(otherName(b))...)
			}
		}
		all = append(all, kit.Message("system", parts...))
	}
	for _, m := range messages {
		role := "user"
		if m.Role == types.ConversationRoleAssistant {
			role = "assistant"
		}
		var current []rudderstackai.GenAIPart
		flush := func() {
			if len(current) > 0 {
				all = append(all, kit.Message(role, current...))
			}
			current = nil
		}
		for _, block := range m.Content {
			parts, response := blockParts(block)
			if response != nil {
				flush()
				all = append(all, kit.Message("tool", response))
			} else {
				current = append(current, parts...)
			}
		}
		flush()
	}
	return kit.Messages(all...)
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
	defer func() {
		if r := recover(); r != nil {
			hook.Recovered(r, "safe")
		}
	}()
	fn()
}

type streamKey struct{}

// tracker sees each operation's typed input and output.
type tracker struct{ cfg *config }

func (*tracker) ID() string { return trackerID }

func (t *tracker) HandleInitialize(ctx context.Context, in middleware.InitializeInput, next middleware.InitializeHandler) (out middleware.InitializeOutput, md middleware.Metadata, err error) {
	// A panic in tracking is contained: before the call it runs untracked, after it the caller
	// gets the call's own result. A panic in the next handler keeps unwinding.
	callCtx := ctx
	var sent, inNext bool
	defer func() {
		if inNext {
			return
		}
		r := recover()
		if r == nil {
			return
		}
		hook.Recovered(r, "rudderbedrock.HandleInitialize")
		if !sent {
			out, md, err = next.HandleInitialize(callCtx, in)
		}
	}()
	return t.handle(ctx, in, func(ctx context.Context) (middleware.InitializeOutput, middleware.Metadata, error) {
		sent, inNext = true, true
		out, md, err = next.HandleInitialize(ctx, in)
		inNext = false
		return out, md, err
	})
}

func (t *tracker) handle(ctx context.Context, in middleware.InitializeInput, call func(context.Context) (middleware.InitializeOutput, middleware.Metadata, error)) (middleware.InitializeOutput, middleware.Metadata, error) {
	var model *string
	var messages []types.Message
	var system []types.SystemContentBlock
	stream := false
	switch p := in.Parameters.(type) {
	case *bedrockruntime.ConverseInput:
		model, messages, system = p.ModelId, p.Messages, p.System
	case *bedrockruntime.ConverseStreamInput:
		model, messages, system, stream = p.ModelId, p.Messages, p.System, true
	default:
		return call(ctx)
	}
	if kit.Untracked(ctx) {
		return call(ctx)
	}
	started := time.Now()
	i, ephemeral := kit.ForCall(ctx, integration, t.cfg.analytics)
	if i == nil {
		return call(ctx)
	}
	ctx, span := hook.Tracing.BeginWrapper(ctx, i, started)
	c := &kit.Call{Span: span, Details: rudderstackai.CallDetailsFromContext(rudderstackai.WithCallDetails(ctx, t.cfg.details)), Interaction: i, Ephemeral: ephemeral, Provider: provider, Integration: integration, Model: aws.ToString(model), StartedAt: started}
	hook.Tracing.WrapperMetadata(c.Span, c.Provider, c.Model)
	safe(func() {
		if c.CapturesContent() {
			c.InputMessages = inputMessages(system, messages)
		}
		recordToolResults(i, messages)
		kit.KeepMessages(i, plainMessages(messages))
	})
	var sc *streamCall
	if stream {
		sc = &streamCall{call: c, ctx: ctx}
		ctx = context.WithValue(ctx, streamKey{}, sc)
	}
	defer func() {
		if value := recover(); value != nil {
			if c.Span != nil {
				safe(func() { c.EndedAt = time.Now(); failed(c, sc, kit.PanicErrorType(value)) })
			}
			panic(value)
		}
	}()
	out, md, err := call(ctx)
	if err != nil {
		safe(func() {
			t := kit.ErrorTypeOfTransport(err)
			kit.NoteCallError(i, err, t)
			failed(c, sc, t)
		})
		return out, md, err
	}
	if o, ok := out.Result.(*bedrockruntime.ConverseOutput); ok {
		id, _ := awsmiddleware.GetRequestIDMetadata(md)
		safe(func() {
			defer func() {
				if r := recover(); r != nil {
					hook.Recovered(r, "rudderbedrock.extract")
					hook.Tracing.FinishRaw(c.Span, c.Provider, c.Model, 0, time.Now(), errors.New("response extraction failed"))
				}
			}()
			recordResponse(c, o, id)
		})
	}
	if stream && c.Span != nil {
		o, ok := out.Result.(*bedrockruntime.ConverseStreamOutput)
		if !ok || o == nil || o.GetStream() == nil {
			safe(func() {
				hook.Tracing.FinishRaw(c.Span, c.Provider, c.Model, 0, time.Now(), errors.New("response has no stream"))
			})
		}
	}
	return out, md, err
}

// failed records a call that raised; a stream already under way fails with its tools seen.
func failed(c *kit.Call, sc *streamCall, errorType string) {
	if sc != nil && sc.end(kit.Errored, errorType) {
		return
	}
	c.RecordCall(rudderstackai.LLMCall{Provider: c.Provider, Model: c.Model, ErrorType: errorType, Integration: c.Integration, UsageUnknown: c.Span != nil})
	if c.Ephemeral {
		kit.EndEphemeral(c.Interaction, errorType)
	}
}

func recordResponse(c *kit.Call, out *bedrockruntime.ConverseOutput, responseID string) {
	call := rudderstackai.LLMCall{
		Provider: c.Provider, Integration: c.Integration, Model: c.Model, FinishReason: finishReason(out.StopReason),
		ResponseID: responseID, UsageUnknown: out.Usage == nil,
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
	if c.CapturesContent() {
		call.OutputToolCalls = kit.ToolCalls(tools)
	}
	if c.RecordCall(call) && len(tools) > 0 {
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

	mu               sync.Mutex
	obs              *kit.Observer
	ended            bool
	stopCancellation func() bool
	// pending are decoded events the app may not have received yet: the SDK decodes up to
	// readAhead events ahead (two unbuffered channels), so an event counts once two more decoded.
	pending []smithy.Deserializable
}

const readAhead = 2

// start begins observing a streamed response, before the SDK reads its first event.
func (s *streamCall) start(resp *smithyhttp.Response) {
	defer func() {
		if r := recover(); r != nil {
			hook.Recovered(r, "rudderbedrock.stream.setup")
			safe(func() {
				hook.Tracing.FinishRaw(s.call.Span, s.call.Provider, s.call.Model, 0, time.Now(), errors.New("stream observer setup failed"))
			})
		}
	}()
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
	if s.call.Span != nil {
		s.stopCancellation = context.AfterFunc(s.ctx, func() {
			safe(func() { s.end(kit.Errored, kit.ErrorTypeOfTransport(s.ctx.Err())) })
		})
	}
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
	if s.stopCancellation != nil {
		s.stopCancellation()
	}
	if s.call.Span != nil && s.obs.Complete() {
		how, errorType = kit.Completed, ""
	}
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
		kit.NoteCallError(s.call.Interaction, err, t)
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
	safe(func() {
		b.s.mu.Lock()
		defer b.s.mu.Unlock()
		if !b.s.ended {
			how := kit.Cancelled
			if b.s.obs.Done {
				how = kit.Completed
			}
			b.s.endLocked(how, "")
		}
	})
	return b.ReadCloser.Close()
}

// eventProtocol reports the events decoded from a tracked body.
type eventProtocol struct {
	smithyhttp.ClientProtocol
}

func (p *eventProtocol) DeserializeEventMessage(schema *smithy.Schema, registry *smithy.TypeRegistry, r io.Reader) (smithy.Deserializable, error) {
	event, err := p.ClientProtocol.DeserializeEventMessage(schema, registry, r)
	// This runs on the AWS SDK's decoding goroutine, where a panic would end the process.
	if b, ok := r.(*trackedBody); ok {
		safe(func() { b.s.observe(event, err) })
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

// WithCallPurpose labels the purpose of model calls through this integration.
func WithCallPurpose(purpose string) Option {
	return func(c *config) { c.details.CallPurpose = purpose }
}

// WithFallbackUsed records whether the app selected a fallback provider or model.
func WithFallbackUsed(used bool) Option { return func(c *config) { c.details.FallbackUsed = &used } }

// WithBYOK records whether the call used the end customer's own provider key.
func WithBYOK(byok bool) Option { return func(c *config) { c.details.BYOK = &byok } }
