//go:build load

package conformance

// Sustained load: concurrent interactions (wrapped OpenAI calls, tool calls, streams, provider
// errors) and MCP tools/call requests, each from a new interaction, through the real analytics-go
// client to a local data plane. Phase 1 has a healthy data plane, phase 2 one that answers HTTP
// 500. The same workload runs in the Node.js and Python SDKs.
//
//	go test -tags load -run TestLoad -v ./internal/conformance
//
// LOAD_SECONDS (phase 1; phase 2 runs half as long), LOAD_RATE (interactions per second, each
// followed by one MCP call) and LOAD_WORKERS scale it. In phase 2 every event must be delivered
// or lost after analytics-go's retries; analytics-go never refuses one.

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	analytics "github.com/rudderlabs/analytics-go/v4"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/rudderopenai"
)

func envOr(name string, def float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(name), 64); err == nil && v > 0 {
		return v
	}
	return def
}

// countingPlane counts events by name, and answers ended by Shutdown; failing makes it answer
// every batch with HTTP 500.
type countingPlane struct {
	mu       sync.Mutex
	counts   map[string]int
	shutdown int
	rejected int
	failing  atomic.Bool
}

func (p *countingPlane) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if p.failing.Load() {
		p.mu.Lock()
		p.rejected++
		p.mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if r.Header.Get("Content-Encoding") == "gzip" {
		if zr, err := gzip.NewReader(bytes.NewReader(body)); err == nil {
			body, _ = io.ReadAll(zr)
		}
	}
	var batch struct {
		Batch []struct {
			Event      string `json:"event"`
			Properties struct {
				EndReason string `json:"end_reason"`
			} `json:"properties"`
		} `json:"batch"`
	}
	_ = json.Unmarshal(body, &batch)
	p.mu.Lock()
	for _, m := range batch.Batch {
		p.counts[m.Event]++
		if m.Properties.EndReason == "shutdown" {
			p.shutdown++
		}
	}
	p.mu.Unlock()
	_, _ = w.Write([]byte("{}"))
}

func (p *countingPlane) count(name string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.counts[name]
}

// fakeOpenAI answers in process: a tool call while tools are offered and no tool result is in the
// conversation yet, else text; streamed when asked; HTTP 429 every 50th request.
type fakeOpenAI struct{ requests atomic.Int64 }

const loadModel = "gpt-4.1-mini-2025-04-14"

func (f *fakeOpenAI) RoundTrip(req *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	_ = req.Body.Close()
	reply := func(status int, contentType, s string) (*http.Response, error) {
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(bytes.NewBufferString(s))}, nil
	}
	if f.requests.Add(1)%50 == 0 {
		return reply(429, "application/json", `{"error":{"message":"rate limited","type":"rate_limit_error"}}`)
	}
	var p struct {
		Stream   bool  `json:"stream"`
		Tools    []any `json:"tools"`
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(body, &p)
	head := `{"id":"chatcmpl-load","created":1760400000,"model":"` + loadModel + `",`
	if p.Stream {
		var b bytes.Buffer
		b.WriteString("data: " + head + `"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}` + "\n\n")
		for range 20 {
			b.WriteString("data: " + head + `"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"token "}}]}` + "\n\n")
		}
		b.WriteString("data: " + head + `"object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n")
		b.WriteString("data: " + head + `"object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":12,"completion_tokens":20,"total_tokens":32}}` + "\n\n")
		b.WriteString("data: [DONE]\n\n")
		return reply(200, "text/event-stream", b.String())
	}
	toolResult := false
	for _, m := range p.Messages {
		toolResult = toolResult || m.Role == "tool"
	}
	if len(p.Tools) > 0 && !toolResult {
		// Its own response ID: the SDK counts two responses with one ID as one call.
		return reply(200, "application/json", strings.Replace(head, "chatcmpl-load", "chatcmpl-load-tool", 1)+
			`"object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant",`+
			`"content":null,"tool_calls":[{"id":"call_load","type":"function","function":{"name":"lookup_order","arguments":"{}"}}]},`+
			`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":20,"completion_tokens":8,"total_tokens":28}}`)
	}
	return reply(200, "application/json", head+`"object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant",`+
		`"content":"Your order ships tomorrow."},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":5,"total_tokens":17}}`)
}

type quietLogger struct{}

func (quietLogger) Logf(string, ...any)   {}
func (quietLogger) Errorf(string, ...any) {}

// lostCounter counts, by event name, the messages analytics-go gave up on after its retries.
type lostCounter struct {
	mu     sync.Mutex
	counts map[string]int
}

func (l *lostCounter) Success(analytics.Message) {}

func (l *lostCounter) Failure(m analytics.Message, _ error) {
	name := ""
	switch t := m.(type) {
	case analytics.Track:
		name = t.Event
	case *analytics.Track:
		name = t.Event
	}
	l.mu.Lock()
	l.counts[name]++
	l.mu.Unlock()
}

func (l *lostCounter) count(name string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.counts[name]
}

func (l *lostCounter) reset() {
	l.mu.Lock()
	clear(l.counts)
	l.mu.Unlock()
}

func heapAfterGC() uint64 {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc
}

type lookupArgs struct {
	Order string `json:"order"`
}

func TestLoad(t *testing.T) {
	seconds, workers, rate := envOr("LOAD_SECONDS", 30), int(envOr("LOAD_WORKERS", 200)), envOr("LOAD_RATE", 500)
	const maxMapEntries = 2_000 // lowered from 10,000 so a short run fills the MCP maps and evicts
	const server = "load-mcp"
	plane := &countingPlane{counts: map[string]int{}}
	lost := &lostCounter{counts: map[string]int{}}
	dp := httptest.NewServer(plane)
	defer dp.Close()
	client, err := analytics.NewWithConfig("LOAD_WRITE_KEY", analytics.Config{
		DataPlaneUrl: dp.URL, BatchSize: 200, Interval: time.Second, Logger: quietLogger{}, Callback: lost,
		RetryAfter: func(int) time.Duration { return 50 * time.Millisecond },
	})
	if err != nil {
		t.Fatal(err)
	}
	var errMu sync.Mutex
	errs := map[string]int{}
	if err := rudderstackai.Configure(rudderstackai.WithAnalytics(client), rudderstackai.WithCaptureContent(false),
		rudderstackai.WithLimits(rudderstackai.Limits{MaxMapEntries: maxMapEntries}),
		rudderstackai.WithOnError(func(_ error, info rudderstackai.ErrorInfo) {
			errMu.Lock()
			errs[string(info.Kind)]++
			errMu.Unlock()
		})); err != nil {
		t.Fatal(err)
	}
	llm := openai.NewClient(option.WithBaseURL("https://load.invalid/v1"), option.WithAPIKey("load"), option.WithMaxRetries(0),
		option.WithHTTPClient(&http.Client{Transport: &fakeOpenAI{}}), rudderopenai.WithTracking())
	tools := []openai.ChatCompletionToolUnionParam{openai.ChatCompletionFunctionTool(openai.FunctionDefinitionParam{Name: "lookup_order"})}

	// One MCP server per connection, all with one name: their state is shared, as when a server is
	// rebuilt per request.
	ctx := context.Background()
	sessions := make([]*mcp.ClientSession, workers)
	// Released however the test ends, so a failed check or worker leaves nothing running.
	t.Cleanup(func() {
		for _, cs := range sessions {
			if cs != nil {
				_ = cs.Close()
			}
		}
		_ = client.Close()
	})
	for w := range sessions {
		s := mcp.NewServer(&mcp.Implementation{Name: server, Version: "1.0.0"}, nil)
		mcp.AddTool(s, &mcp.Tool{Name: "lookup_order"}, func(_ context.Context, _ *mcp.CallToolRequest, in lookupArgs) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: in.Order + ": shipped"}}}, nil, nil
		})
		if err := rudderstackai.WrapMCPServer(s, rudderstackai.Options{Analytics: client}); err != nil {
			t.Fatal(err)
		}
		ct, st := mcp.NewInMemoryTransports()
		if _, err := s.Connect(ctx, st, nil); err != nil {
			t.Fatal(err)
		}
		if sessions[w], err = mcp.NewClient(&mcp.Implementation{Name: "load-client", Version: "1.0.0"}, nil).Connect(ctx, ct, nil); err != nil {
			t.Fatal(err)
		}
	}

	var seq, interactions, mcpCalls atomic.Int64
	turn := func(ctx context.Context, n int64) string {
		var id string
		_ = rudderstackai.RunInteraction(ctx, rudderstackai.InteractionOptions{
			UserID: fmt.Sprintf("user-%d", n%50_000), ConversationID: fmt.Sprintf("c-%d", n%5_000),
		}, func(ctx context.Context, in *rudderstackai.Interaction) error {
			id = in.ID()
			user := []openai.ChatCompletionMessageParamUnion{openai.UserMessage("Where is my order?")}
			if n%10 == 0 {
				s := llm.Chat.Completions.NewStreaming(ctx, openai.ChatCompletionNewParams{Model: "gpt-4.1-mini", Messages: user})
				for s.Next() {
					_ = s.Current()
				}
				return s.Err()
			}
			first, err := llm.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{Model: "gpt-4.1-mini", Messages: user, Tools: tools})
			if err != nil {
				return err // every 50th provider request fails with 429
			}
			msgs := append(user, first.Choices[0].Message.ToParam())
			for _, c := range first.Choices[0].Message.ToolCalls {
				msgs = append(msgs, openai.ToolMessage("shipped", c.ID))
			}
			_, err = llm.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{Model: "gpt-4.1-mini", Messages: msgs, Tools: tools})
			return err
		})
		return id
	}

	phase := func(seconds float64, warmup func()) float64 {
		start := time.Now()
		until := start.Add(time.Duration(seconds * float64(time.Second)))
		// Workers claim a slot only when starting, spacing starts by one interval without catching
		// up after a stall. After sleeping, they recheck the schedule and phase deadline.
		var slotMu sync.Mutex
		next := start
		interval := time.Duration(float64(time.Second) / rate)
		var warmed sync.Once
		var failed atomic.Bool // the first failure stops every worker at its next slot
		var wg sync.WaitGroup
		for _, cs := range sessions {
			wg.Go(func() {
				for {
					slotMu.Lock()
					now := time.Now()
					if failed.Load() || !now.Before(until) {
						slotMu.Unlock()
						return
					}
					if now.Before(next) {
						wait := next.Sub(now)
						slotMu.Unlock()
						time.Sleep(wait)
						continue
					}
					next = now.Add(interval)
					slotMu.Unlock()
					n := seq.Add(1)
					id := turn(ctx, n)
					interactions.Add(1)
					meta := mcp.Meta{}
					if id != "" {
						meta["com.rudderstack/interaction_id"] = id
					}
					if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "lookup_order", Arguments: map[string]any{"order": fmt.Sprintf("A-%d", n%100)}, Meta: meta}); err != nil {
						t.Error(err)
						failed.Store(true)
						return
					}
					mcpCalls.Add(1)
					if warmup != nil && time.Since(start) > time.Duration(seconds*float64(time.Second))/4 {
						warmed.Do(warmup)
					}
				}
			})
		}
		wg.Wait()
		return time.Since(start).Seconds()
	}

	var warmHeap uint64
	// Phase 1 needs no pacing wait: Enqueue never refuses an event; it waits while the queue is full.
	phase1 := phase(seconds, func() { warmHeap = heapAfterGC() })
	sent := [3]int64{interactions.Load(), interactions.Load(), mcpCalls.Load()}
	if !rudderstackai.Flush(ctx) {
		t.Error("flush after phase 1 timed out")
	}
	delivered := [3]int64{
		int64(plane.count("ai_user_prompt_created")), int64(plane.count("ai_response_received")), int64(plane.count("mcp_tool_called")),
	}

	lost.reset()
	plane.failing.Store(true)
	phase2 := phase(seconds/2, nil)
	plane.failing.Store(false)
	recovered := rudderstackai.Flush(ctx)
	// Counted before Shutdown, which sends the events of interactions still open.
	p2 := interactions.Load() - sent[0]
	sent2 := map[string]int64{"ai_user_prompt_created": p2, "ai_response_received": p2, "mcp_tool_called": mcpCalls.Load() - sent[2]}
	deliveredBefore := map[string]int64{"ai_user_prompt_created": delivered[0], "ai_response_received": delivered[1], "mcp_tool_called": delivered[2]}
	accounted2 := map[string]int64{}
	for name := range sent2 {
		accounted2[name] = int64(plane.count(name)) - deliveredBefore[name] + int64(lost.count(name))
	}
	// Shutdown ends every interaction still open as incomplete with end_reason shutdown: none may be.
	rudderstackai.Shutdown(ctx)
	for _, cs := range sessions {
		_ = cs.Close()
	}
	endHeap := heapAfterGC()

	summary := fmt.Sprintf("### Go load test (%s, %d workers, %.0f interactions/s target)\n\n"+
		"| Phase | Seconds | Interactions | MCP calls | Interactions/s | Data plane |\n| --- | --- | --- | --- | --- | --- |\n"+
		"| 1 healthy | %.0f | %d | %d | %.0f | %d prompts, %d answers, %d tool calls delivered |\n"+
		"| 2 HTTP 500 | %.0f | %d | %d | %.0f | %d batches rejected; %d events lost after retries |\n\n"+
		"Heap after GC: %.1f MB after warm-up, %.1f MB at the end. Contained errors: %v.\n",
		runtime.Version(), workers, rate, phase1, sent[0], sent[2], float64(sent[0])/phase1, delivered[0], delivered[1], delivered[2],
		phase2, p2, mcpCalls.Load()-sent[1], float64(p2)/phase2, plane.rejected, lost.count("ai_user_prompt_created")+lost.count("ai_response_received")+lost.count("mcp_tool_called"), float64(warmHeap)/1e6, float64(endHeap)/1e6, errs)
	fmt.Println(summary)
	if path := os.Getenv("GITHUB_STEP_SUMMARY"); path != "" {
		if f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			_, _ = f.WriteString(summary + "\n")
			_ = f.Close()
		}
	}

	if delivered != sent {
		t.Errorf("phase 1 delivered %v (prompts, answers, tool calls), want %v", delivered, sent)
	}
	if plane.rejected == 0 {
		t.Error("phase 2 never reached a failing data plane")
	}
	for name, want := range sent2 {
		if accounted2[name] != want {
			t.Errorf("phase 2 %s: delivered + lost = %d, sent %d", name, accounted2[name], want)
		}
	}
	if plane.shutdown > 0 {
		t.Errorf("%d interactions were still open at the end", plane.shutdown)
	}
	if !recovered {
		t.Error("the flush after the data plane recovered timed out")
	}
	if errs["internal"] > 0 {
		t.Errorf("%d SDK steps failed", errs["internal"])
	}
	if mcpCalls.Load() > maxMapEntries && errs["limit_reached"] == 0 {
		t.Error("the MCP maps never reached their cap: the run did not exercise eviction")
	}
	if float64(endHeap) > float64(warmHeap)*1.25+8e6 {
		t.Errorf("heap grew from %d to %d bytes", warmHeap, endHeap)
	}
}
