package rudderotel

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
	"weak"

	analytics "github.com/rudderlabs/analytics-go/v4"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
)

// SDK-7017: tenants with their own clients share the interaction, user, anonymous and
// conversation IDs. A detached span (no interaction in its context) naming that ID must never
// reach a tenant whose work it is not; dropping it is fine.

const sdk7017ID = "sdk7017-shared"

type sdk7017Mode struct{ simple, emit bool }

// Both processor kinds and both span settings, paired to keep the matrix small.
var sdk7017Modes = []sdk7017Mode{{simple: true}, {emit: true}}

func (m sdk7017Mode) String() string { return fmt.Sprintf("simple=%v/emit=%v", m.simple, m.emit) }

func sdk7017Setup(t *testing.T, m sdk7017Mode) (*sdktrace.TracerProvider, *SpanProcessor) {
	t.Helper()
	provider, p, _ := testProvider(t, Options{Simple: m.simple, ProductEvents: true, CaptureContent: rudderstackai.Ptr(true)})
	configureSpans(t, rudderstackai.WithEmitSpans(m.emit), rudderstackai.WithCaptureContent(true), rudderstackai.WithLLMCallEvents(true),
		rudderstackai.WithEventRules(rudderstackai.EventRules{Tools: map[string]rudderstackai.ToolEventRule{"lookup": {
			Event: "lookup_done",
			Properties: func(args map[string]any, result any) map[string]any {
				return map[string]any{"args": args, "result": result}
			},
		}}}))
	return provider, p
}

func sdk7017Options(client analytics.Client) rudderstackai.InteractionOptions {
	return rudderstackai.InteractionOptions{InteractionID: sdk7017ID, UserID: "shared-user", AnonymousID: "shared-anonymous", ConversationID: "shared-conversation", Analytics: client, Classify: rudderstackai.Ptr(false)}
}

func sdk7017Start(t *testing.T, client analytics.Client) *rudderstackai.Interaction {
	t.Helper()
	_, i := rudderstackai.StartInteraction(context.Background(), sdk7017Options(client))
	t.Cleanup(func() { i.End(rudderstackai.EndOptions{}) })
	return i
}

func sdk7017Snapshot(t *testing.T, i *rudderstackai.Interaction, viaJSON bool) rudderstackai.PausedInteraction {
	t.Helper()
	snapshot, ok := i.Pause(rudderstackai.PauseOptions{})
	if !ok {
		t.Fatal("pause failed")
	}
	if viaJSON {
		data, err := json.Marshal(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		snapshot = rudderstackai.PausedInteraction{}
		if err := json.Unmarshal(data, &snapshot); err != nil {
			t.Fatal(err)
		}
	}
	return snapshot
}

func sdk7017Restore(t *testing.T, snapshot rudderstackai.PausedInteraction, client analytics.Client) *rudderstackai.Interaction {
	t.Helper()
	_, r := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{Resume: &snapshot, Analytics: client, Classify: rudderstackai.Ptr(false)})
	t.Cleanup(func() { r.End(rudderstackai.EndOptions{}) })
	return r
}

// sdk7017Work is label's private model call and tool call, without the interaction ID.
func sdk7017Work(label string) [][]attribute.KeyValue {
	return [][]attribute.KeyValue{
		{attribute.String("gen_ai.operation.name", "chat"), attribute.String("gen_ai.provider.name", "openai"), attribute.String("gen_ai.request.model", "mock"),
			attribute.Int("gen_ai.usage.input_tokens", 7), attribute.Int("gen_ai.usage.output_tokens", 3),
			attribute.String("gen_ai.prompt.0.content", label+"_INPUT"), attribute.String("gen_ai.completion.0.content", label+"_OUTPUT")},
		{attribute.String("gen_ai.operation.name", "execute_tool"), attribute.String("gen_ai.tool.name", "lookup"), attribute.String("gen_ai.tool.call.id", label+"_CALL"),
			attribute.String("gen_ai.tool.call.arguments", `{"secret":"`+label+`_ARGS"}`), attribute.String("gen_ai.tool.call.result", `{"secret":"`+label+`_RESULT"}`)},
	}
}

// sdk7017Detached starts and ends label's model and tool spans with no interaction in context,
// naming the shared ID.
func sdk7017Detached(provider *sdktrace.TracerProvider, label string) {
	for _, attrs := range sdk7017Work(label) {
		_, s := provider.Tracer("sdk7017").Start(context.Background(), label, trace.WithAttributes(append(attrs, attribute.String(InteractionIDAttribute, sdk7017ID))...))
		s.End()
	}
}

func sdk7017Flush(t *testing.T, p *SpanProcessor) {
	t.Helper()
	if err := p.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	rudderstackai.Flush(context.Background())
}

// sdk7017RequireNone fails when a tenant that did no work of its own got label's content, a call
// event or call accounting.
func sdk7017RequireNone(t *testing.T, tenant string, events []object, label string) {
	t.Helper()
	for _, e := range events {
		if strings.Contains(render(e), label) {
			t.Errorf("%s received %s content: %s", tenant, label, render(e))
			continue
		}
		switch e["event"] {
		case "ai_llm_response_received", "lookup_done":
			t.Errorf("%s received a foreign call event: %s", tenant, render(e))
		case "ai_response_received":
			if props := e["properties"].(object); props["llm_call_count"] != float64(0) || props["tool_call_count"] != float64(0) {
				t.Errorf("%s received foreign call accounting: %s", tenant, render(e))
			}
		}
	}
}

// sdk7017RequireOwn fails unless the tenant got label's model call and tool call once each, with content.
func sdk7017RequireOwn(t *testing.T, tenant string, events []object, label string) {
	t.Helper()
	calls, tools := 0, 0
	for _, e := range events {
		switch e["event"] {
		case "ai_llm_response_received":
			calls++
			if !strings.Contains(render(e), label+"_INPUT") || !strings.Contains(render(e), label+"_OUTPUT") {
				t.Errorf("%s model call lost content: %s", tenant, render(e))
			}
		case "lookup_done":
			tools++
			if !strings.Contains(render(e), label+"_ARGS") || !strings.Contains(render(e), label+"_RESULT") {
				t.Errorf("%s tool call lost content: %s", tenant, render(e))
			}
		}
	}
	if calls != 1 || tools != 1 {
		t.Errorf("%s model calls=%d tool calls=%d, want 1 each", tenant, calls, tools)
	}
}

// S1 (Go path 1): A and B open, A pauses, A's snapshot is restored and the restored run ends. A's
// later detached work must not reach B. The restore never takes over B's registration.
func TestSDK7017RestoreBesideOpenTwin(t *testing.T) {
	for _, m := range sdk7017Modes {
		for _, bFirst := range []bool{false, true} {
			for _, bState := range []string{"open", "paused", "paused-resumed"} {
				for _, viaJSON := range []bool{false, true} {
					t.Run(fmt.Sprintf("%v/b-first=%v/b=%s/json=%v", m, bFirst, bState, viaJSON), func(t *testing.T) {
						provider, p := sdk7017Setup(t, m)
						clientA, _ := recorder(t)
						clientB, eventsB := recorder(t)
						var a, b *rudderstackai.Interaction
						var bctx context.Context
						startB := func() {
							bctx, b = rudderstackai.StartInteraction(context.Background(), sdk7017Options(clientB))
							t.Cleanup(func() { b.End(rudderstackai.EndOptions{}) })
						}
						if bFirst {
							startB()
						}
						a = sdk7017Start(t, clientA)
						if !bFirst {
							startB()
						}
						snapshot := sdk7017Snapshot(t, a, viaJSON)
						if bState != "open" {
							sdk7017Snapshot(t, b, false)
						}
						if bState == "paused-resumed" {
							b.Resume()
						}
						sdk7017Restore(t, snapshot, clientA).End(rudderstackai.EndOptions{})
						if rudderstackai.FromContext(bctx) != b || b.Paused() != (bState == "paused") {
							t.Fatal("the restore of A's snapshot took over B")
						}
						sdk7017Detached(provider, "A_PRIVATE")
						b.End(rudderstackai.EndOptions{})
						a.End(rudderstackai.EndOptions{})
						sdk7017Flush(t, p)
						sdk7017RequireNone(t, "B", eventsB(), "A_PRIVATE")
					})
				}
			}
		}
	}
}

// S2 (Go path 2): A pauses, its snapshot is restored, the restored run (or a second restore of it)
// ends, and the app still holds A. A same-ID B, started after or before the restore, must not
// receive A's detached work.
func TestSDK7017HeldOriginalAfterRestore(t *testing.T) {
	for _, m := range sdk7017Modes {
		for _, bBefore := range []bool{false, true} {
			for _, viaJSON := range []bool{false, true} {
				for _, chain := range []int{1, 2} {
					t.Run(fmt.Sprintf("%v/b-before-restore=%v/json=%v/restores=%d", m, bBefore, viaJSON, chain), func(t *testing.T) {
						provider, p := sdk7017Setup(t, m)
						clientA, _ := recorder(t)
						clientB, eventsB := recorder(t)
						a := sdk7017Start(t, clientA)
						var b *rudderstackai.Interaction
						if bBefore {
							b = sdk7017Start(t, clientB)
						}
						// The restored run pauses and is restored again: both earlier segments stay held.
						held := []*rudderstackai.Interaction{a}
						segment := a
						for range chain {
							segment = sdk7017Restore(t, sdk7017Snapshot(t, segment, viaJSON), clientA)
							held = append(held, segment)
						}
						segment.End(rudderstackai.EndOptions{})
						if !bBefore {
							b = sdk7017Start(t, clientB)
						}
						sdk7017Detached(provider, "A_PRIVATE")
						b.End(rudderstackai.EndOptions{})
						sdk7017Flush(t, p)
						sdk7017RequireNone(t, "B", eventsB(), "A_PRIVATE")
						runtime.KeepAlive(held)
					})
				}
			}
		}
	}
}

// sdk7017StartLate starts label's model and tool spans with no interaction in context and no ID;
// with lateKind they carry no attributes until setID, so they are not AI spans at start either.
func sdk7017StartLate(provider *sdktrace.TracerProvider, label string, lateKind bool) (spans []trace.Span, setID func()) {
	work := sdk7017Work(label)
	for _, attrs := range work {
		start := attrs
		if lateKind {
			start = nil
		}
		_, s := provider.Tracer("sdk7017").Start(context.Background(), label, trace.WithAttributes(start...))
		spans = append(spans, s)
	}
	return spans, func() {
		for n, s := range spans {
			if lateKind {
				s.SetAttributes(work[n]...)
			}
			s.SetAttributes(attribute.String(InteractionIDAttribute, sdk7017ID))
		}
	}
}

// S3: a detached span with no ID at start gets one through SetAttributes while A (and B) are live.
// one-ends-new-C, both-end-new-C: they end and C starts with the same IDs before the span ends;
// C must not receive it. shared-one-left: B ends; the ID was shared when set, so A must not either.
func TestSDK7017LateIDNeverReachesLaterTenant(t *testing.T) {
	for _, m := range sdk7017Modes {
		for _, shape := range []string{"one-ends-new-C", "both-end-new-C", "shared-one-left"} {
			for _, lateKind := range []bool{false, true} {
				t.Run(fmt.Sprintf("%v/%s/late-kind=%v", m, shape, lateKind), func(t *testing.T) {
					provider, p := sdk7017Setup(t, m)
					clients := make([]analytics.Client, 3)
					events := make([]func() []object, 3)
					for n := range clients {
						clients[n], events[n] = recorder(t)
					}
					a := sdk7017Start(t, clients[0])
					var b *rudderstackai.Interaction
					if shape != "one-ends-new-C" {
						b = sdk7017Start(t, clients[1])
					}
					spans, setID := sdk7017StartLate(provider, "A_PRIVATE", lateKind)
					setID()
					if b != nil {
						b.End(rudderstackai.EndOptions{})
					}
					if shape == "shared-one-left" {
						for _, s := range spans {
							s.End()
						}
						a.End(rudderstackai.EndOptions{})
						sdk7017Flush(t, p)
						sdk7017RequireNone(t, "A", events[0](), "A_PRIVATE")
						sdk7017RequireNone(t, "B", events[1](), "A_PRIVATE")
						return
					}
					a.End(rudderstackai.EndOptions{})
					c := sdk7017Start(t, clients[2])
					for _, s := range spans {
						s.End()
					}
					c.End(rudderstackai.EndOptions{})
					sdk7017Flush(t, p)
					sdk7017RequireNone(t, "C", events[2](), "A_PRIVATE")
				})
			}
		}
	}
}

// The single-tenant cost of the S3 rule: turn A1 ends while the span is open and turn A2 reuses
// its IDs and client before the ID is set. The SDK cannot tell A2 from another tenant: it drops.
func TestSDK7017LateIDDropsAfterSameIDTurnEnds(t *testing.T) {
	for _, m := range sdk7017Modes {
		for _, lateKind := range []bool{false, true} {
			t.Run(fmt.Sprintf("%v/late-kind=%v", m, lateKind), func(t *testing.T) {
				provider, p := sdk7017Setup(t, m)
				clientA, eventsA := recorder(t)
				a1 := sdk7017Start(t, clientA)
				spans, setID := sdk7017StartLate(provider, "A_PRIVATE", lateKind)
				a1.End(rudderstackai.EndOptions{})
				a2 := sdk7017Start(t, clientA)
				setID()
				for _, s := range spans {
					s.End()
				}
				a2.End(rudderstackai.EndOptions{})
				sdk7017Flush(t, p)
				sdk7017RequireNone(t, "A2", eventsA(), "A_PRIVATE")
			})
		}
	}
}

// S3 control: a pause and a token restore are not retirements, and neither is the original
// leaving while its restored run is open. A late-ID span open across them reaches the restored
// run, as on main, whether the app holds the original, drops it (collected) or ends it.
func TestSDK7017LateIDAcrossRestoreKeepsRestoredRun(t *testing.T) {
	for _, m := range sdk7017Modes {
		for _, original := range []string{"held", "collected", "ended"} {
			for _, viaJSON := range []bool{false, true} {
				t.Run(fmt.Sprintf("%v/original=%s/json=%v", m, original, viaJSON), func(t *testing.T) {
					provider, p := sdk7017Setup(t, m)
					clientA, eventsA := recorder(t)
					var spans []trace.Span
					var setID func()
					restored, held, ref := func() (*rudderstackai.Interaction, *rudderstackai.Interaction, weak.Pointer[rudderstackai.Interaction]) {
						_, a := rudderstackai.StartInteraction(context.Background(), sdk7017Options(clientA))
						spans, setID = sdk7017StartLate(provider, "A_PRIVATE", false)
						r := sdk7017Restore(t, sdk7017Snapshot(t, a, viaJSON), clientA)
						switch original {
						case "held":
							t.Cleanup(func() { a.End(rudderstackai.EndOptions{}) })
							return r, a, weak.Make(a)
						case "ended":
							a.End(rudderstackai.EndOptions{})
						}
						return r, nil, weak.Make(a)
					}()
					if original == "collected" {
						deadline := time.Now().Add(5 * time.Second)
						for ref.Value() != nil {
							if time.Now().After(deadline) {
								t.Fatal("dropped original was not collected")
							}
							runtime.GC()
							time.Sleep(time.Millisecond)
						}
					}
					setID()
					for _, s := range spans {
						s.End()
					}
					restored.End(rudderstackai.EndOptions{})
					sdk7017Flush(t, p)
					sdk7017RequireOwn(t, "A", eventsA(), "A_PRIVATE")
					runtime.KeepAlive(held)
				})
			}
		}
	}
}

// A late-ID span is open while A pauses and the app drops A (and, with 2 restores, A's restored
// segment after it pauses too). Collected before the last restore, the run retired: that restore
// is admitted as a new run and the span drops (main delivered it; a documented limit). Collected
// after a token restore, the run went on in its restored segment, which still receives it.
func TestSDK7017LateIDAcrossCollectedPausedRun(t *testing.T) {
	for _, m := range sdk7017Modes {
		for _, collected := range []string{"before-restore", "after-token-restore"} {
			for _, restores := range []int{1, 2} {
				t.Run(fmt.Sprintf("%v/collected=%s/restores=%d", m, collected, restores), func(t *testing.T) {
					provider, p := sdk7017Setup(t, m)
					clientA, eventsA := recorder(t)
					var spans []trace.Span
					var setID func()
					var snapshot rudderstackai.PausedInteraction
					var refs []weak.Pointer[rudderstackai.Interaction]
					restored := func() *rudderstackai.Interaction {
						_, segment := rudderstackai.StartInteraction(context.Background(), sdk7017Options(clientA))
						spans, setID = sdk7017StartLate(provider, "A_PRIVATE", false)
						for n := range restores {
							refs = append(refs, weak.Make(segment))
							snapshot = sdk7017Snapshot(t, segment, true)
							if n == restores-1 && collected == "before-restore" {
								return nil
							}
							// Not sdk7017Restore: its cleanup would keep the segment alive.
							_, segment = rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{Resume: &snapshot, Analytics: clientA, Classify: rudderstackai.Ptr(false)})
						}
						return segment
					}()
					deadline := time.Now().Add(5 * time.Second)
					for _, ref := range refs {
						for ref.Value() != nil {
							if time.Now().After(deadline) {
								t.Fatal("dropped segment was not collected")
							}
							runtime.GC()
							time.Sleep(time.Millisecond)
						}
					}
					if restored == nil {
						restored = sdk7017Restore(t, snapshot, clientA)
					}
					setID()
					for _, s := range spans {
						s.End()
					}
					restored.End(rudderstackai.EndOptions{})
					sdk7017Flush(t, p)
					if collected == "before-restore" {
						sdk7017RequireNone(t, "A", eventsA(), "A_PRIVATE")
					} else {
						sdk7017RequireOwn(t, "A", eventsA(), "A_PRIVATE")
					}
				})
			}
		}
	}
}

// sdk7017Cap allows one open or paused interaction.
func sdk7017Cap(t *testing.T) {
	t.Helper()
	if err := rudderstackai.Configure(rudderstackai.WithLimits(rudderstackai.Limits{MaxOpenInteractions: 1})); err != nil {
		t.Fatal(err)
	}
}

// sdk7017Refused starts B with the shared IDs past the cap: it runs untracked.
func sdk7017Refused(t *testing.T, client analytics.Client) *rudderstackai.Interaction {
	t.Helper()
	_, b := rudderstackai.StartInteraction(context.Background(), sdk7017Options(client))
	if !b.Ended() {
		t.Fatal("B was admitted past MaxOpenInteractions")
	}
	return b
}

// A run refused at MaxOpenInteractions runs untracked and never registers. While the app holds it,
// its detached work naming the shared ID (at start, or set late) must not reach tracked A; nor may
// a late ID once B ended while the span was open (a retirement).
func TestSDK7017RefusedAtCapNeverReachesTrackedTwin(t *testing.T) {
	for _, m := range sdk7017Modes {
		for _, shape := range []string{"start-id", "late-id", "late-id-after-b-ends"} {
			t.Run(fmt.Sprintf("%v/%s", m, shape), func(t *testing.T) {
				provider, p := sdk7017Setup(t, m)
				sdk7017Cap(t)
				clientA, eventsA := recorder(t)
				clientB, eventsB := recorder(t)
				a := sdk7017Start(t, clientA)
				b := sdk7017Refused(t, clientB)
				if shape == "start-id" {
					sdk7017Detached(provider, "B_PRIVATE")
				} else {
					spans, setID := sdk7017StartLate(provider, "B_PRIVATE", false)
					if shape == "late-id-after-b-ends" {
						b.End(rudderstackai.EndOptions{})
					}
					setID()
					for _, s := range spans {
						s.End()
					}
				}
				b.End(rudderstackai.EndOptions{})
				a.End(rudderstackai.EndOptions{})
				sdk7017Flush(t, p)
				sdk7017RequireNone(t, "A", eventsA(), "B_PRIVATE")
				sdk7017RequireNone(t, "B", eventsB(), "B_PRIVATE")
			})
		}
	}
}

// The refused run's lookup entry takes no slot: another run is still refused while A is open, and
// one is admitted once A ends.
func TestSDK7017RefusedRunTakesNoSlot(t *testing.T) {
	sdk7017Setup(t, sdk7017Modes[0])
	sdk7017Cap(t)
	clientA, _ := recorder(t)
	clientB, _ := recorder(t)
	a := sdk7017Start(t, clientA)
	b := sdk7017Refused(t, clientB)
	sdk7017Refused(t, clientB)
	a.End(rudderstackai.EndOptions{})
	if c := sdk7017Start(t, clientB); c.Ended() {
		t.Fatal("a run was refused after A ended")
	}
	runtime.KeepAlive(b)
}

// Once the refused run ends, or the app drops it and it is collected, a detached span started
// afterwards reaches tracked A again.
func TestSDK7017RefusedRunLeavesLookupWhenGone(t *testing.T) {
	for _, m := range sdk7017Modes {
		for _, gone := range []string{"ended", "collected"} {
			t.Run(fmt.Sprintf("%v/%s", m, gone), func(t *testing.T) {
				provider, p := sdk7017Setup(t, m)
				sdk7017Cap(t)
				clientA, eventsA := recorder(t)
				clientB, _ := recorder(t)
				a := sdk7017Start(t, clientA)
				ref := func() weak.Pointer[rudderstackai.Interaction] {
					b := sdk7017Refused(t, clientB)
					if gone == "ended" {
						b.End(rudderstackai.EndOptions{})
					}
					return weak.Make(b)
				}()
				if gone == "collected" {
					deadline := time.Now().Add(5 * time.Second)
					for ref.Value() != nil {
						if time.Now().After(deadline) {
							t.Fatal("dropped refused run was not collected")
						}
						runtime.GC()
						time.Sleep(time.Millisecond)
					}
				}
				sdk7017Detached(provider, "A_PRIVATE")
				a.End(rudderstackai.EndOptions{})
				sdk7017Flush(t, p)
				sdk7017RequireOwn(t, "A", eventsA(), "A_PRIVATE")
			})
		}
	}
}

// S3 control: A alone is live from the span's start to its end, so its late ID still reaches A.
func TestSDK7017LateIDKeepsOwnerLiveThroughout(t *testing.T) {
	for _, m := range sdk7017Modes {
		for _, lateKind := range []bool{false, true} {
			t.Run(fmt.Sprintf("%v/late-kind=%v", m, lateKind), func(t *testing.T) {
				provider, p := sdk7017Setup(t, m)
				clientA, eventsA := recorder(t)
				a := sdk7017Start(t, clientA)
				spans, setID := sdk7017StartLate(provider, "A_PRIVATE", lateKind)
				setID()
				for _, s := range spans {
					s.End()
				}
				a.End(rudderstackai.EndOptions{})
				sdk7017Flush(t, p)
				sdk7017RequireOwn(t, "A", eventsA(), "A_PRIVATE")
			})
		}
	}
}

// Single-tenant control: after A's snapshot is restored, a detached span while the restored run
// is open reaches the restored run, whether the app still holds the original or dropped it.
func TestSDK7017SingleTenantRestoreKeepsDetachedSpans(t *testing.T) {
	for _, m := range sdk7017Modes {
		for _, held := range []bool{true, false} {
			for _, viaJSON := range []bool{false, true} {
				t.Run(fmt.Sprintf("%v/original-held=%v/json=%v", m, held, viaJSON), func(t *testing.T) {
					provider, p := sdk7017Setup(t, m)
					clientA, eventsA := recorder(t)
					restored, original, ref := func() (*rudderstackai.Interaction, *rudderstackai.Interaction, weak.Pointer[rudderstackai.Interaction]) {
						_, a := rudderstackai.StartInteraction(context.Background(), sdk7017Options(clientA))
						r := sdk7017Restore(t, sdk7017Snapshot(t, a, viaJSON), clientA)
						if held {
							t.Cleanup(func() { a.End(rudderstackai.EndOptions{}) })
							return r, a, weak.Make(a)
						}
						return r, nil, weak.Make(a)
					}()
					if !held {
						deadline := time.Now().Add(5 * time.Second)
						for ref.Value() != nil {
							if time.Now().After(deadline) {
								t.Fatal("dropped original was not collected")
							}
							runtime.GC()
							time.Sleep(time.Millisecond)
						}
					}
					sdk7017Detached(provider, "A_PRIVATE")
					restored.End(rudderstackai.EndOptions{})
					sdk7017Flush(t, p)
					sdk7017RequireOwn(t, "A", eventsA(), "A_PRIVATE")
					runtime.KeepAlive(original)
				})
			}
		}
	}
}
