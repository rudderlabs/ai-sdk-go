# Privacy

## Content capture

### Serializer behavior

Captured content honours `MarshalJSON` and `MarshalText` with `encoding/json` precedence, with spans on or off. Each value is captured once per report; events, spans, sizes and rule-returned properties reuse that output. Sizes count JSON serializer output, including HTML escapes. In-process resume reuses captured content; JSON restore currently drops deferred call-event content. A later report captures the value again. Native raw bytes are stripped. Ordinary serializer text, including base64, follows masking and capture controls.

A serializer error, panic, invalid JSON or re-entry produces JSON `null`; its fields are never walked and the event still arrives. Top-level fields that forbid null are omitted. Span context is captured at span start, even with content capture off; admitted manual attributes are captured at End. Disabled content attributes run no serializers. OTel attributes retain byte stripping. See [observed MCP results](mcp.md#observed-result-capture).

App callbacks, rules, classifiers, request controls and return values keep their inputs. Serializers an app serializer calls itself are outside the SDK's count. Go by-value receivers without stable identity run once per occurrence; named maps and slices reuse serialized output.

Content capture is off by default: no prompt, answer, feedback or failure text is sent. `ContentCapture` turns kinds on one at a time; a nil key takes `Default`, then the level above:

```go
err := rudderstackai.Configure(rudderstackai.WithContentCapture(rudderstackai.ContentCapture{
	Default: rudderstackai.Ptr(false), CallReason: rudderstackai.Ptr(true), ErrorMessage: rudderstackai.Ptr(true),
}))
ctx, i := rudderstackai.StartInteraction(ctx, rudderstackai.InteractionOptions{
	ContentCapture: &rudderstackai.ContentCapture{LLMCalls: rudderstackai.Ptr(false)}, // the rest follows Configure
})
```

| Field              | Sends                                                                                                       |
| ------------------ | ----------------------------------------------------------------------------------------------------------- |
| `Default`          | Every key not set at this level (not `IntentSummary` or `Spans.HTTP`).                                      |
| `AnswerText`       | `prompt_text` and `response_text`.                                                                          |
| `LLMCalls`         | `input_messages`, `input_truncated`, `output_text` and `output_tool_calls` (with `WithLLMCallEvents`).      |
| `Spans`            | Span content: `&SpanCapture{Default, Messages, ToolCalls, HTTP}`, see below.                                |
| `FeedbackText`     | `feedback_text` (`TrackAction`, MCP `submit_feedback`) and `capability_detail`.                             |
| `ScoreExplanation` | Score `explanation`.                                                                                        |
| `IntentSummary`    | `intent_summary`, only when set: false drops it; nil keeps the classifier's own `IntentSummary` option.      |
| `CallReason`       | MCP `call_reason`, and its field in `request_context`.                                                      |
| `ErrorMessage`     | MCP `error_message`.                                                                                        |

`Spans` covers the [tracing exporter](tracing.md) and SDK spans:
- `Messages` is prompt, completion, instruction, retrieval and error text, `gen_ai.*.message` and `gen_ai.choice` events and the status message.
- `ToolCalls` is tool arguments and results, and all other content of a tool-execution span (`gen_ai.operation.name` `execute_tool`, OpenInference `TOOL`, Traceloop or LangSmith `tool`, or Vercel `ai.toolCall`) except error text.
- Nil kinds take `Spans.Default`, then `Default`.
- `HTTP: Ptr(true)` (off unless set) keeps `url.path` and `url.full` / `http.url` / `http.target` without the query string, masked. `url.query`, the user agent, client addresses and headers are never sent.

Levels, most general first: `Configure`, then the interaction or task, MCP wrapper, or tracing exporter. Interactions and tasks resolve policy at start; wrappers and exporters read `Configure` per request or export. At each level, `CaptureContent` sets `Default`; `ContentCapture` keys override it. Existing bool settings keep their behavior. Content-named custom properties follow that field's key. Masking and length caps always apply. MCP wrappers' default `CaptureContent: false` follows the global setting. To disable one server, use `ContentCapture: &rudderstackai.ContentCapture{Default: rudderstackai.Ptr(false)}`.

## Privacy and masking

Sent by default: IDs, models, token counts, latency, cost, status, tool names. Never sent by default: prompt, answer and feedback text, score explanations, tool arguments and results, MCP resource contents and elicitation answers.

### Custom properties

Custom response extras, declared tool properties and action details drop content-named keys when their kind of capture is off: `prompt_text`, `response_text`, `feedback_text`, `intent_summary`, `explanation`, `call_reason`, `capability_detail`, `error_message`, and `input_messages`, `output_text` and `output_tool_calls` (`LLMCalls`). The check recognizes camelCase, PascalCase, uppercase, hyphens and spaces, and warns once per original key. Unrelated names such as `promptLength` and `cardKinds` are kept. SDK-owned property names are reserved under those same spellings even with capture on. Use the dedicated prompt/response fields for those properties.

Captured text and the classifier's `intent_summary` are masked first:

| Pattern                                                                                                                                                                                  | Becomes    |
| ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------- |
| Email addresses                                                                                                                                                                          | `[email]`  |
| Phone numbers: `+` and 9-15 digits, or 10-12 digits in common groupings                                                                                                                  | `[phone]`  |
| Payment card numbers that pass the Luhn check                                                                                                                                            | `[card]`   |
| IPv4 and IPv6 addresses                                                                                                                                                                  | `[ip]`     |
| API keys and tokens (`sk-`, Stripe `sk_live_` / `rk_live_`, Google `AIza`, Slack, GitHub, AWS access keys, JWTs, `Bearer`), passwords in connection URLs, values of `api_key=`, `token=`, `password=` and similar | `[secret]` |
| US Social Security numbers                                                                                                                                                               | `[ssn]`    |
| IBANs (checksum-validated)                                                                                                                                                               | `[iban]`   |

```go
err := rudderstackai.Configure(
	rudderstackai.WithCaptureContent(true),
	rudderstackai.WithRedactHook(func(text string) (string, error) {
		return honorifics.ReplaceAllString(text, "[name]"), nil // runs after the built-in patterns
	}),
)
```

- Patterns catch fixed shapes only: names, postal addresses and free-form IDs are not caught, and some order numbers look like phone numbers. Use the hook, or leave capture off.
- Masking sees through zero-width and other invisible format characters and through fullwidth and other compatibility forms (`４１５-５５５-０１００`, `jane.doe＠example.com`); the mask covers the value's own characters, and text that is not masked is sent exactly as written. Compatibility forms come from golang.org/x/text's Unicode tables, so a character newer than those tables is not folded.
- Fail closed: a hook that returns an error or panics drops the field. Text over 32,000 code points is withheld rather than masked.
- `WithRedaction(false)` sends captured text unmasked and does not run the hook; MCP `Options.DisableRedaction` does the same for `Redact`, but agent-written MCP text (`call_reason`, `capability_detail`, `feedback_text`, `error_message`) that still matches a built-in rule is withheld. The patterns are defined in [`spec/fixtures/redaction-v0.1.json`](../spec/fixtures/redaction-v0.1.json).
- `MaskText(text)` applies the same patterns to text you send yourself, such as free text a [declared tool event](events.md#declared-tool-events) rule's `Properties` returns: the SDK sends those as returned. It ignores `WithRedaction` and the hooks; `ok` is false for text over 32,000 code points, which you should not send.

MCP agent-written text follows its [content controls](mcp.md#options). Span export follows [Privacy and size](tracing.md#privacy-and-size).

Each masking pattern has a 100 ms budget. On timeout, text is sent as `[redaction timed out]`, never unmasked (`redaction_timeout`).
