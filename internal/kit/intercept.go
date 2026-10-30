package kit

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	analytics "github.com/rudderlabs/analytics-go/v4"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

// Call is one intercepted model request.
type Call struct {
	Details      rudderstackai.CallDetails
	FirstTokenAt time.Time
	Interaction  *rudderstackai.Interaction
	// Ephemeral: the integration opened the interaction for this call and ends it.
	Ephemeral   bool
	Provider    string
	Integration string
	// Model is the requested model.
	Model     string
	StartedAt time.Time
	EndedAt   time.Time
	// Request is the decoded request body.
	Request map[string]any
	// HTTPRequest is the request as sent.
	HTTPRequest *http.Request
}

// StreamHandler sees each server-sent event of a streamed response and how the stream ended.
type StreamHandler interface {
	// OnEvent gets each event's name (may be empty) and data.
	OnEvent(event string, data []byte)
	// Finish is called once when the stream ends; errorType is set when it failed.
	Finish(end End, errorType string)
	// Complete reports whether the provider's terminal event arrived, so closing the stream
	// afterwards completes the call rather than cancels it.
	Complete() bool
}

// Complete reports whether the provider's terminal event arrived.
func (o *Observer) Complete() bool { return o.Done }

// EventFilter is an optional StreamHandler extension: Hide, asked after OnEvent, drops that event
// from what the SDK reads (a usage-only chunk the integration asked for, not the caller).
type EventFilter interface {
	Hide(event string, data []byte) bool
}

// LineHandler is an optional StreamHandler extension: OnLine gets each line that is not an SSE
// field or comment (Gemini sends a mid-stream error as a bare JSON line).
type LineHandler interface {
	OnLine(line []byte)
}

// Prepared is a streamed request changed before it is sent (stream_options.include_usage).
type Prepared struct {
	Body map[string]any
	// Names of the parameters added: a 400 or 422 that names one (or carries no error code) is
	// sent once more as the caller wrote it. Empty: never resent.
	Names []string
	// Accepted runs when the resend worked after an error naming one of Names.
	Accepted func()
}

// Spec describes one provider API for [Intercept].
type Spec struct {
	Details     rudderstackai.CallDetails
	Provider    string
	Integration string
	Analytics   analytics.Client
	// IsStream reports whether the request streams.
	IsStream func(req *http.Request, body map[string]any) bool
	// Untracked passes a request through untracked (a background response that is only queued).
	Untracked func(req *http.Request, body map[string]any) bool
	// OnRequest sees the request before it is sent (tool results of earlier calls).
	OnRequest func(c *Call)
	// PrepareStream may change a streamed request.
	PrepareStream func(c *Call) *Prepared
	// OnResponse records a successful non-streamed response; false when it was not a finished
	// call (nothing recorded).
	OnResponse func(c *Call, body map[string]any, resp *http.Response) bool
	// NewStream returns the handler of a streamed response.
	NewStream func(c *Call, resp *http.Response) StreamHandler
	// ErrorType maps a failed response; nil uses the HTTP status.
	ErrorType func(status int, body map[string]any) string
}

type trackedKey struct{ integration string }

// Intercept runs one HTTP exchange through the provider's middleware chain and records it. The
// request and response reach the SDK unchanged, except a streamed request may gain
// stream_options. Never fails the request because of tracking.
func (s *Spec) Intercept(req *http.Request, send func(*http.Request) (*http.Response, error)) (resp *http.Response, err error) {
	// A panic in the SDK's own steps is contained: before the request went out it is sent
	// untracked, after it the caller gets the provider's response or error as it came. A panic
	// in the next handler is the caller's own and keeps unwinding.
	in := req
	var sent, inNext bool
	var got *http.Response
	var gotErr error
	next := func(r *http.Request) (*http.Response, error) {
		sent, inNext = true, true
		got, gotErr = send(r)
		inNext = false
		return got, gotErr
	}
	defer func() {
		if inNext {
			return
		}
		r := recover()
		if r == nil {
			return
		}
		hook.Recovered(r, "kit.Intercept")
		if sent {
			resp, err = got, gotErr
			return
		}
		resp, err = send(in)
	}()
	ctx := req.Context()
	// Seen by an outer copy of this middleware (a client wrapped twice), or the SDK's own call.
	if ctx.Value(trackedKey{s.Integration}) != nil || Untracked(ctx) || req.Method != http.MethodPost {
		return next(req)
	}
	started := time.Now()
	body, raw := readBody(req)
	if s.Untracked != nil && s.Untracked(req, body) {
		return next(req)
	}
	stream := s.IsStream != nil && s.IsStream(req, body)
	n, numbered := retryNumber(req)
	var key attemptKey
	var prev *attempt
	if numbered {
		key = keyFor(ctx, s, req, raw)
		prev = resume(key, n)
	}
	if prev != nil {
		started = prev.call.StartedAt
	}
	var i *rudderstackai.Interaction
	var ephemeral bool
	if prev != nil && prev.call.Ephemeral {
		// The retry reports into the interaction its first attempt opened.
		i, ephemeral, started = prev.call.Interaction, true, prev.call.StartedAt
	} else {
		i, ephemeral = ForCall(ctx, s.Integration, s.Analytics)
	}
	if i == nil {
		return next(req)
	}
	req = req.WithContext(context.WithValue(ctx, trackedKey{s.Integration}, true))
	model, _ := body["model"].(string)
	details := rudderstackai.CallDetailsFromContext(rudderstackai.WithCallDetails(ctx, s.Details))
	c := &Call{Details: details, Interaction: i, Ephemeral: ephemeral, Provider: s.Provider, Integration: s.Integration, Model: model, StartedAt: started, Request: body, HTTPRequest: req}
	safe(func() {
		if s.OnRequest != nil {
			s.OnRequest(c)
		}
		if msgs, ok := body["messages"]; ok {
			KeepMessages(i, msgs)
		} else if in, ok := body["input"]; ok {
			KeepMessages(i, in)
		} else if in, ok := body["contents"]; ok {
			KeepMessages(i, in)
		}
	})
	var prepared *Prepared
	if stream && s.PrepareStream != nil {
		safe(func() { prepared = s.PrepareStream(c) })
	}
	original := req
	if prepared != nil {
		if b, err := json.Marshal(prepared.Body); err == nil {
			req = withBody(req, b)
		} else {
			prepared = nil
		}
	}
	resp, err = next(req)
	if err == nil && prepared != nil && len(prepared.Names) > 0 && refusedParams(resp, prepared.Names) != "" {
		why := refusedParams(resp, prepared.Names)
		_ = resp.Body.Close()
		resp, err = next(original)
		if err == nil && resp.StatusCode < 400 && why == "named" && prepared.Accepted != nil {
			safe(prepared.Accepted)
		}
	}
	if err != nil {
		c.EndedAt = time.Now()
		t := ErrorTypeOfTransport(err)
		safe(func() { NoteCallError(i, err, t) })
		// A connection error carries no body to tell a retry by: inside an interaction it waits
		// for the retry, the next call or the end; a call outside one is recorded now.
		if numbered && !ephemeral && req.Context().Err() == nil {
			safe((&attempt{spec: s, call: c, errorType: t, key: key}).hold)
			return resp, err
		}
		s.failed(c, t)
		return resp, err
	}
	if resp.StatusCode >= 400 {
		raw := peekBody(resp)
		c.EndedAt = time.Now() // after the body read, as for successful responses
		var decoded map[string]any
		_ = json.Unmarshal(raw, &decoded)
		t := ""
		if s.ErrorType != nil {
			safe(func() { t = s.ErrorType(resp.StatusCode, decoded) })
		}
		if t == "" {
			t = HTTPErrorType(resp.StatusCode, decoded)
		}
		if numbered {
			// Recorded once the SDK shows whether it retries (see retry.go).
			a := &attempt{spec: s, call: c, errorType: t, key: key}
			safe(a.hold)
			resp.Body = &probeBody{ReadCloser: resp.Body, a: a}
			return resp, nil
		}
		s.failed(c, t)
		return resp, nil
	}
	if stream && s.NewStream != nil && isEventStream(resp) {
		var h StreamHandler
		safe(func() { h = s.NewStream(c, resp) })
		if h != nil {
			body := &sseBody{src: resp.Body, h: h, i: i, ctx: req.Context(), reader: bufio.NewReader(resp.Body)}
			body.filter, _ = h.(EventFilter)
			resp.Body = body
			return resp, nil
		}
	}
	reply := peekBody(resp)
	safe(func() {
		var decoded map[string]any
		if json.Unmarshal(reply, &decoded) != nil {
			return
		}
		recorded := s.OnResponse != nil && s.OnResponse(c, decoded, resp)
		if ephemeral && recorded {
			EndEphemeral(i, "")
		}
	})
	return resp, nil
}

func (s *Spec) failed(c *Call, errorType string) {
	safe(func() {
		c.RecordCall(rudderstackai.LLMCall{Provider: c.Provider, Model: c.Model, ErrorType: errorType, Integration: c.Integration})
		if c.Ephemeral {
			EndEphemeral(c.Interaction, errorType)
		}
	})
}

// ErrorTypeOfTransport maps a transport error (no HTTP response).
func ErrorTypeOfTransport(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	return rudderstackai.ErrorTypeOf(err)
}

// Provider error codes of quota and rate limits.
var rateLimitCodes = map[string]bool{"insufficient_quota": true, "rate_limit_exceeded": true}

// HTTPErrorType maps a failed HTTP response: the status, else a rate-limit error code, else
// api_error.
func HTTPErrorType(status int, body map[string]any) string {
	e := Map(body["error"])
	if e == nil {
		e = body
	}
	code, message := Str(e["code"]), strings.ToLower(Str(e["message"]))
	if (status == 400 || status == 422 || status == 0) && ContextLimit(code, message) {
		return "context_limit"
	}
	if t := rudderstackai.ErrorTypeForStatus(status); t != "" {
		return t
	}
	if e := Map(body["error"]); e != nil && (rateLimitCodes[Str(e["code"])] || rateLimitCodes[Str(e["type"])]) {
		return "rate_limit"
	}
	return "api_error"
}

func isEventStream(resp *http.Response) bool {
	return strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")
}

// refusedParams: "named" for an HTTP 400/422 naming a parameter we added, "unclear" for one
// without an error code, else "".
func refusedParams(resp *http.Response, names []string) string {
	if resp.StatusCode != 400 && resp.StatusCode != 422 {
		return ""
	}
	raw := peekBody(resp)
	for _, n := range names {
		if bytes.Contains(raw, []byte(n)) {
			return "named"
		}
	}
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	if e := Map(decoded["error"]); e == nil || e["code"] == nil {
		return "unclear"
	}
	return ""
}

// readBody decodes a JSON request body and restores it for the SDK; raw is the body as sent.
func readBody(req *http.Request) (body map[string]any, raw []byte) {
	if req.Body == nil {
		return map[string]any{}, nil
	}
	raw, _ = io.ReadAll(req.Body)
	_ = req.Body.Close()
	req.Body = io.NopCloser(bytes.NewReader(raw))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(raw)), nil }
	if json.Unmarshal(raw, &body) != nil || body == nil {
		return map[string]any{}, raw
	}
	return body, raw
}

func withBody(req *http.Request, b []byte) *http.Request {
	r := req.Clone(req.Context())
	r.Body = io.NopCloser(bytes.NewReader(b))
	r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }
	r.ContentLength = int64(len(b))
	return r
}

// peekBody reads a response body and puts it back for the SDK.
func peekBody(resp *http.Response) []byte {
	if resp.Body == nil {
		return nil
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	return raw
}

func safe(fn func()) {
	defer func() {
		if r := recover(); r != nil {
			hook.Recovered(r, "safe")
		}
	}()
	fn()
}

// sseBody parses server-sent events as the SDK reads the body, and tells the handler how the
// stream ended: EOF completes it, an early Close cancels it (unless the terminal event arrived),
// a read error fails it.
type sseBody struct {
	src    io.ReadCloser
	reader *bufio.Reader
	h      StreamHandler
	i      *rudderstackai.Interaction
	ctx    context.Context
	buf    bytes.Buffer
	event  string
	data   []string
	once   sync.Once
	mu     sync.Mutex
	// With a filter, an event's lines are held until its blank line decides whether it is shown.
	filter EventFilter
	held   bytes.Buffer
	hide   bool
	// broken: parsing panicked; the rest of the stream passes through unobserved.
	broken bool
}

func (b *sseBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for b.buf.Len() == 0 {
		line, err := b.reader.ReadBytes('\n')
		if len(line) > 0 {
			b.takeSafely(line)
		}
		if err != nil {
			b.buf.Write(b.held.Bytes())
			b.held.Reset()
			if b.buf.Len() == 0 {
				b.end(err)
				return 0, err
			}
			defer b.end(err)
			break
		}
	}
	return b.buf.Read(p)
}

// takeSafely takes a line; a panic in the SDK's parsing stops observing the stream, and the app
// still gets every byte.
func (b *sseBody) takeSafely(line []byte) {
	if b.broken {
		b.buf.Write(line)
		return
	}
	bufLen, heldLen := b.buf.Len(), b.held.Len()
	defer func() {
		if r := recover(); r != nil {
			hook.Recovered(r, "kit.sseBody")
			b.broken = true
			b.buf.Truncate(bufLen)
			b.held.Truncate(heldLen)
			b.buf.Write(b.held.Bytes())
			b.held.Reset()
			b.buf.Write(line)
		}
	}()
	b.take(line)
}

func (b *sseBody) take(line []byte) {
	if b.filter == nil {
		b.buf.Write(line)
		b.parse(line)
		return
	}
	b.held.Write(line)
	b.parse(line)
	if len(bytes.TrimRight(line, "\r\n")) > 0 {
		return
	}
	if !b.hide {
		b.buf.Write(b.held.Bytes())
	}
	b.held.Reset()
	b.hide = false
}

func (b *sseBody) parse(line []byte) {
	s := strings.TrimRight(string(line), "\r\n")
	switch {
	case s == "":
		if len(b.data) > 0 || b.event != "" {
			event, data := b.event, strings.Join(b.data, "\n")
			b.event, b.data = "", nil
			safe(func() { b.h.OnEvent(event, []byte(data)) })
			if b.filter != nil {
				safe(func() { b.hide = b.filter.Hide(event, []byte(data)) })
			}
		}
	case strings.HasPrefix(s, ":"):
	case strings.HasPrefix(s, "event:"):
		b.event = strings.TrimSpace(strings.TrimPrefix(s, "event:"))
	case strings.HasPrefix(s, "data:"):
		d := strings.TrimPrefix(s, "data:")
		b.data = append(b.data, strings.TrimPrefix(d, " "))
	case strings.HasPrefix(s, "id:"), strings.HasPrefix(s, "retry:"):
	default:
		if lh, ok := b.h.(LineHandler); ok {
			safe(func() { lh.OnLine([]byte(s)) })
		}
	}
}

func (b *sseBody) end(err error) {
	b.once.Do(func() {
		if errors.Is(err, io.EOF) {
			// A last event without its blank line still counts.
			if !b.broken {
				safe(func() { b.parse([]byte("\n")) })
			}
			safe(func() { b.h.Finish(Completed, "") })
			return
		}
		t := ErrorTypeOfTransport(err)
		if b.ctx.Err() != nil {
			t = ErrorTypeOfTransport(b.ctx.Err())
		}
		if b.i != nil {
			safe(func() { NoteCallError(b.i, err, t) })
		}
		if t == "cancelled" {
			safe(func() { b.h.Finish(Cancelled, "") })
			return
		}
		safe(func() { b.h.Finish(Errored, t) })
	})
}

func (b *sseBody) Close() error {
	b.once.Do(func() {
		complete := false
		safe(func() { complete = b.h.Complete() })
		if complete {
			safe(func() { b.h.Finish(Completed, "") })
			return
		}
		safe(func() { b.h.Finish(Cancelled, "") })
	})
	return b.src.Close()
}

// RecordCall adds observed boundaries and app metadata before recording a call.
func (c *Call) RecordCall(call rudderstackai.LLMCall) bool {
	call.StartedAt = c.StartedAt
	call.EndedAt = c.EndedAt
	call.FirstTokenAt = c.FirstTokenAt
	call.CallPurpose = c.Details.CallPurpose
	call.FallbackUsed, call.BYOK = c.Details.FallbackUsed, c.Details.BYOK
	return c.Interaction.RecordCall(call)
}

// ContextLimit reports whether a rejected request exceeded the model's context window.
func ContextLimit(code, message string) bool {
	switch strings.ToLower(code) {
	case "context_length_exceeded", "context_window_exceeded", "prompt_too_long":
		return true
	}
	message = strings.ToLower(message)
	for _, part := range []string{"maximum context length", "context window", "prompt is too long", "input is too long", "exceeds the maximum number of tokens"} {
		if strings.Contains(message, part) {
			return true
		}
	}
	return false
}
