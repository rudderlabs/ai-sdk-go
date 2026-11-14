package conformance

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sync/errgroup"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/testkit"
)

func TestGoroutineSpanParents(t *testing.T) {
	selected := map[string]bool{}
	for _, name := range strings.Fields("openai_chat openai_chat_stream anthropic_create anthropic_create_stream google_generate_plain google_generate_stream") {
		selected[name] = true
	}
	for _, sc := range loadFixture(t) {
		if !selected[sc.Name] {
			continue
		}
		delete(selected, sc.Name)
		for _, mode := range []string{"WaitGroup", "errgroup"} {
			t.Run(sc.Name+"/"+mode, func(t *testing.T) {
				rec := &recorder{}
				configure(t, obj{"emit_spans": true, "emit_llm_calls": true}, rec)
				llm.Reset()
				for route, replies := range sc.Replies {
					for range 3 {
						for _, raw := range replies {
							var reply testkit.Reply
							if err := remarshal(raw, &reply); err != nil {
								t.Fatal(err)
							}
							llm.Script(route, reply)
						}
					}
				}
				var call obj
				for _, step := range sc.Steps {
					if step["op"] == "interaction" {
						call = objs(step["steps"].([]any))[0]
					}
				}
				ctx, interaction := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{
					UserID: "goroutine-user", ConversationID: "goroutine-conversation", Classify: rudderstackai.Ptr(false),
				})
				defer interaction.End(rudderstackai.EndOptions{})
				ctx, root := rudderstackai.StartSpan(ctx, rudderstackai.SpanOptions{Name: "agent", Operation: "invoke_agent"})
				defer root.End(nil)
				type toolCall struct {
					span  *rudderstackai.Span
					model string
				}
				tools := make(chan toolCall, 2)
				type link struct {
					span     *rudderstackai.Span
					parentID string
				}
				retrievals := make(chan link, 2)
				ready := sync.WaitGroup{}
				ready.Add(2)
				worker := func(ctx context.Context, n int) error {
					workerCall := maps.Clone(call)
					params := maps.Clone(obj0(call["params"]))
					model := fmt.Sprintf("%s-worker-%d", str(params["model"]), n)
					params["model"] = model
					workerCall["params"] = params
					toolCtx, tool := rudderstackai.StartSpan(ctx, rudderstackai.SpanOptions{Name: fmt.Sprintf("tool-%d", n), Operation: "execute_tool"})
					defer tool.End(nil)
					tools <- toolCall{tool, model}
					ready.Done()
					ready.Wait() // Both tools stay active while their independent descendants start.
					var nested sync.WaitGroup
					nested.Go(func() {
						_, retrieval := rudderstackai.StartSpan(toolCtx, rudderstackai.SpanOptions{Name: "retrieve", Operation: "retrieval"})
						retrievals <- link{retrieval, tool.SpanID}
						retrieval.End(nil)
					})
					nested.Wait()
					return apis[str(workerCall["api"])](toolCtx, &run{t: t, llm: llm}, workerCall)
				}
				if mode == "errgroup" {
					group, groupCtx := errgroup.WithContext(ctx)
					for n := range 2 {
						group.Go(func() error { return worker(groupCtx, n) })
					}
					if err := group.Wait(); err != nil {
						t.Fatal(err)
					}
				} else {
					var group sync.WaitGroup
					errors := make(chan error, 2)
					for n := range 2 {
						group.Go(func() { errors <- worker(ctx, n) })
					}
					group.Wait()
					for range 2 {
						if err := <-errors; err != nil {
							t.Fatal(err)
						}
					}
				}
				// A provider call started in a goroutine can also parent directly to the agent.
				var direct errgroup.Group
				direct.Go(func() error { return apis[str(call["api"])](ctx, &run{t: t, llm: llm}, call) })
				if err := direct.Wait(); err != nil {
					t.Fatal(err)
				}
				root.End(nil)
				interaction.End(rudderstackai.EndOptions{})
				rudderstackai.Flush(context.Background())
				parents := map[string]string{root.SpanID: ""}
				toolIDs := map[string]bool{}
				wrapperParentByModel := map[string]string{str(obj0(call["params"])["model"]): root.SpanID}
				for range 2 {
					tool := <-tools
					parents[tool.span.SpanID] = root.SpanID
					toolIDs[tool.span.SpanID] = true
					wrapperParentByModel[tool.model] = tool.span.SpanID
				}
				retrievalIDs := map[string]string{}
				for range 2 {
					retrieval := <-retrievals
					retrievalIDs[retrieval.span.SpanID] = retrieval.parentID
				}
				seen := map[string]bool{}
				wrapperParents := map[string]int{}
				for _, event := range rec.all() {
					if event.Event != "ai_otel_span" {
						continue
					}
					p := event.Properties
					id, parent := str(p["span_id"]), str(p["parent_span_id"])
					if seen[id] {
						t.Fatalf("span %s emitted twice", id)
					}
					seen[id] = true
					if want, manual := parents[id]; manual {
						if parent != want {
							t.Errorf("manual span parent = %s, want %s", parent, want)
						}
					} else if want, retrieval := retrievalIDs[id]; retrieval {
						if parent != want {
							t.Errorf("nested goroutine lost tool parent: %v", p)
						}
					} else {
						model := str(obj0(p["gen_ai"])["request_model"])
						want, ok := wrapperParentByModel[model]
						if !ok || parent != want {
							t.Errorf("wrapper for model %q parent = %s, want %s", model, parent, want)
						}
						wrapperParents[parent]++
					}
					if p["trace_id"] != root.TraceID || p["interaction_id"] != interaction.ID() || p["conversation_id"] != "goroutine-conversation" || event.UserId != "goroutine-user" {
						t.Errorf("goroutine lost trace or interaction identity: %v", p)
					}
					if err := testkit.ValidateEvent(event.Event, jsonRoundTrip(p).(obj)); err != nil {
						t.Fatal(err)
					}
				}
				if len(seen) != 8 || len(wrapperParents) != 3 || wrapperParents[root.SpanID] != 1 {
					t.Fatalf("spans=%d, wrapper parents=%v; want 8 spans and one wrapper per agent/tool", len(seen), wrapperParents)
				}
				for id := range toolIDs {
					if wrapperParents[id] != 1 {
						t.Errorf("tool %s has %d wrapper children, want 1", id, wrapperParents[id])
					}
				}
			})
		}
	}
	if len(selected) != 0 {
		t.Fatalf("missing provider fixtures: %v", selected)
	}
}
