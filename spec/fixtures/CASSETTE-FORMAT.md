# Provider cassettes and field-by-field event checks

Shared, byte for byte, by the Node.js (`@rudderstack/ai`) and Python (`rudderstack-ai`) SDKs. A cassette holds the HTTP traffic of one run between an app and a provider API. Both SDKs replay the same files through their mock LLM server, so the real provider clients parse them, and derive the expected event values from them. `cassette-conformance-v0.1.json` holds test vectors for every rule below; each SDK runs all of them.

## Layout

`spec/fixtures/cassettes/<name>.json`. Example recordings are named after the example they record: `examples/node-<example>.json` (Node.js `examples/<example>`) and `examples/python-<example>.json` (Python `examples/<example>`). A live run writes its recording under the same name, so promoting it is a copy.

```json
{
  "synthetic": true,
  "description": "...",
  "sources": ["https://..."],
  "interactions": [
    {
      "request": { "method": "POST", "path": "/v1/chat/completions" },
      "response": { "status": 200, "headers": { "content-type": "application/json" }, "body": {} }
    }
  ]
}
```

- `synthetic: true`: written from the provider's documented response examples (`sources`), not recorded. A recording has `synthetic: false`, `recorded_at` and no `sources`.
- `request`: `method` and `path` only (no query, headers or body), so credentials sent by the client are never stored.
- `response`: `status`, `headers` and either `body` (parsed JSON, or a string when the body is not JSON) or `events`. `events` holds a streamed body: `[{ "event"?: string, "payload": any }]`. For `text/event-stream` each entry is one server-sent event (`event:` line when present, `payload` from the `data:` line, parsed JSON or a string such as `[DONE]`). For `application/vnd.amazon.eventstream` each entry is one AWS event-stream message (`:event-type` header as `event`, JSON payload). Bedrock's InvokeModel stream sends `chunk` messages whose payload is `{ "bytes": <base64> }`, the base64 of one Anthropic stream event (JSON).

## Recording

A recording proxy listens on `127.0.0.1` and forwards each request, unchanged, to the upstream base URL (the provider, Ollama or LiteLLM), with `accept-encoding: identity`. It streams the upstream response back as it arrives and records it: a `text/event-stream` body as `events` (one per server-sent event), an `application/vnd.amazon.eventstream` body as `events` (one per message), any other body as `body`. A client that leaves early (an aborted stream) does not cut the recording, which holds the whole upstream response, and the recording is read once every forwarded request has finished. A JSON body override (used for Ollama: `reasoning_effort: "none"` on `/chat/completions`, `reasoning: { "effort": "none" }` on `/responses`, so qwen3 answers without a long thinking phase on CPU runners) applies only to requests whose path ends with the configured suffix and never to the recording. Before writing, the recording is scrubbed.

### Scrubbing

1. `request` keeps only `method` and `path`, without the query string.
2. `response.headers` keeps only `content-type`, `x-litellm-model-name` and `x-litellm-response-cost` (lower-cased names).
3. In every body and payload, recursively: an object key whose normalised form (lower case, `-` → `_`) is one of `api_key`, `apikey`, `x_api_key`, `authorization`, `proxy_authorization`, `access_token`, `refresh_token`, `secret`, `client_secret`, `organization`, `organization_id`, `org_id`, `openai_organization`, `project`, `project_id`, `openai_project`, `request_id`, `x_request_id` has its value replaced by `"<redacted>"`. No other key is touched (token counts stay).
4. In every string value, each match of `sk-[A-Za-z0-9_-]{16,}`, `org-[A-Za-z0-9]{16,}`, `proj_[A-Za-z0-9]{16,}` or `req_[A-Za-z0-9]{16,}`, and each occurrence of the value of `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN`, `GOOGLE_API_KEY`, `GEMINI_API_KEY`, `AZURE_OPENAI_API_KEY`, `AWS_BEARER_TOKEN_BEDROCK` or `ANTHROPIC_VERTEX_ACCESS_TOKEN` when it is set and at least 8 characters long, is replaced by `<redacted>`.

## Replay

Replaying a cassette queues each interaction on a model route as an exact reply of the mock LLM server, in order, with its status and kept headers: `body` as JSON, `events` as server-sent events (`event:` line when present; a string payload such as `[DONE]` sent as is), or, with content type `application/vnd.amazon.eventstream`, as AWS event-stream messages (headers `:event-type` from `event`, `:content-type: application/json`, `:message-type: event`). The real provider clients and frameworks parse them. After the run, every queued reply must have been served.

## Provider facts

`provider_facts(cassette)` turns each interaction on a model route into one call fact; other interactions (`/embeddings`, `/messages/count_tokens`, `/models`, ...) are ignored. A route is the path's ending: `/chat/completions` → `openai.chat` (Azure OpenAI's `/openai/deployments/<deployment>/chat/completions` too), `/responses` → `openai.responses`, `/messages` → `anthropic.messages`. Anthropic models on Amazon Bedrock (`/model/<model>/invoke`, `/model/<model>/invoke-with-response-stream`) and Google Vertex AI (`/publishers/anthropic/models/<model>:rawPredict`, `:streamRawPredict`, except the `count-tokens` model) are `anthropic.messages` too: same bodies and stream events; a Bedrock `chunk` message is read as the Anthropic event in its `bytes`. `:generateContent` → `google.generate`, `:streamGenerateContent` → `google.stream` (the Gemini API's `models/<model>:...`; the client's `?alt=sse` and any `key` query parameter are not part of the stored path).

```json
{ "api": "openai.chat", "status": "success", "model": "gpt-4.1-mini-2025-04-14", "finish_reason": "stop",
  "usage": { "input": 0, "output": 0, "cached": 0, "cache_write": 0, "reasoning": 0 }, "tools": ["lookup_order"] }
```

Missing numbers are 0. On `openai.*` routes, a response's `x-litellm-model-name` header (the LiteLLM proxy's served deployment) replaces `model`. `status` is `error` when the HTTP status is 400 or more, or a streamed `error` event (Anthropic) or a payload with an `error` object (OpenAI and Gemini, including the `response` of a Responses stream's final event) appears; `model`, `finish_reason`, `usage` and `tools` then hold what was seen before the failure. `tools` lists the name of every tool call the response makes, in order.

| `api` | `model` | `finish_reason` | `usage` | `tools` |
| --- | --- | --- | --- | --- |
| `openai.chat` | `model` | `choices[0].finish_reason` | `input` `prompt_tokens`, `output` `completion_tokens`, `cached` `prompt_tokens_details.cached_tokens`, `reasoning` `completion_tokens_details.reasoning_tokens` | `choices[0].message.tool_calls[].function.name` |
| `openai.chat`, streamed | first non-empty `model` (Azure OpenAI's first chunk, `prompt_filter_results`, has `""`) | last non-null `choices[0].finish_reason` | the last non-null `usage`, as above | each `delta.tool_calls[]` entry with a `function.name` |
| `openai.responses` | `model` | `tool_calls` when `output` has a `function_call` item, else `stop` for `status: completed`, `length` for `status: incomplete` with `incomplete_details.reason: max_output_tokens`, otherwise `status` | `input` `input_tokens`, `output` `output_tokens`, `cached` `input_tokens_details.cached_tokens`, `reasoning` `output_tokens_details.reasoning_tokens` | `output[]` items of type `function_call`: `name` |
| `openai.responses`, streamed | as above, from the `response.completed` (or `response.incomplete` / `response.failed`) event's `response` | | | |
| `anthropic.messages` | `model` | `stop_reason` | `input` `input_tokens + cache_read_input_tokens + cache_creation_input_tokens`, `output` `output_tokens`, `cached` `cache_read_input_tokens`, `cache_write` `cache_creation_input_tokens`, `reasoning` `output_tokens_details.thinking_tokens` | `content[]` blocks of type `tool_use`: `name` |
| `anthropic.messages`, streamed | `message_start.message.model` | `message_delta.delta.stop_reason` | `message_start.message.usage`, each field overridden by a later `message_delta.usage` field that is present | `content_block_start` blocks of type `tool_use` |
| `google.generate` | `modelVersion` | `candidates[0].finishReason` (Gemini reports `STOP` for a function call too) | from `usageMetadata`: `input` `promptTokenCount + toolUsePromptTokenCount` (`promptTokenCount` includes the cached tokens), `output` `candidatesTokenCount + thoughtsTokenCount` (thinking is billed as output), `cached` `cachedContentTokenCount`, `reasoning` `thoughtsTokenCount` | `candidates[0].content.parts[]` with a `functionCall`: `functionCall.name` |
| `google.stream` | first payload's `modelVersion` | last non-null `candidates[0].finishReason` | the last `usageMetadata` (a running total), as above | every chunk's `functionCall` parts, in order |

### Expected interaction

`expected_interactions(calls, response_events)` splits the calls among the run's `ai_response_received` events in emission order: each takes as many calls as its `llm_call_count`. Every call must be taken (none left over, none missing). For each event, the expected values are:

- `status`: `success` when the last call succeeded (a retry that succeeded is a success), else `error`; an interaction that took no call is a `success` with `model` and `finish_reason` null.
- `model`, `finish_reason`: from the last successful call (`null` when none succeeded).
- `llm_call_count`: the number of calls; `usage`: each field summed over the calls.
- `tools`: `{ <name>: <count> }` over the calls' `tools`.

`provider`, `identity`, `conversation_id`, `cost_usd`, `prompt` and `response` come from the test, not the cassette.

An answer that spans processes (a LangGraph run paused at `interrupt()`, then resumed in a new process with the same `interaction_id`) has one cassette holding every process's calls in order. The calls are split among all processes' `ai_response_received` events in emission order, as above, and each process's events are checked on their own (`assertCassetteRuns` / `assert_cassette_runs`); the test sets `status` (`paused`) for the first.

## `assert_ai_events(events, expected, options?)`

`events` are the events of one interaction (`ai_user_prompt_created`, `ai_response_received`, `ai_llm_call`, `ai_tool_call`, `ai_user_action`...). `expected` has the keys above; any key may be absent, which skips its check. Checks run in this order and the first failure raises an assertion error whose message starts with the property name shown in bold.

1. **ai_response_received**: exactly one such event.
2. **userId** / **anonymousId**: each event except `ai_user_action` carries `expected.identity.user_id` / `anonymous_id` (`null` means the field is absent).
3. **interaction_id** / **conversation_id**: every event carries the response event's non-empty value (checked whatever `expected` holds); `conversation_id` also equals `expected.conversation_id`.
4. **response_status**, **primary_provider**, **primary_model** equal `status`, `provider`, `model`. **finish_reason** equals `finish_reason` after both are lower-cased with `-` replaced by `_`.
5. **llm_call_count** equals `llm_call_count`.
6. **total_input_tokens**, **total_output_tokens**, **total_cached_input_tokens** equal `usage.input`, `usage.output`, `usage.cached`; **total_tokens** equals `usage.input + usage.output`. **total_reasoning_tokens** equals `usage.reasoning` and **total_cache_write_tokens** equals `usage.cache_write` (absent counts as 0), each only while the `ai_response_received` properties in `spec/ai-events-v0.4.schema.json` define it. `options.optional_fields`, when given, replaces the schema lookup (the test vectors always give it).
7. **total_cost_usd**: with `cost_usd` a number, within `1e-9` of it; with `cost_usd: null`, absent.
8. **usage_breakdown**, when `llm_call_count` is given and above 0: present; the rows' `call_count` sum to `llm_call_count`; with `usage` given, their `input_tokens`, `output_tokens` and `cached_input_tokens` sum to `usage.input`, `usage.output` and `usage.cached`, and their `reasoning_tokens` / `cache_write_tokens` (absent counts as 0) to `usage.reasoning` / `usage.cache_write` under the same rule as the totals; each row's `total_tokens` is `input_tokens + output_tokens`.
9. **tool_call_count** equals the sum of `tools`; **tool_breakdown**, when present, has one row per tool name with `call_count` equal to its count. A row with `mcp_server` whose `tool_name` is not in `tools` stands for `mcp__<mcp_server>__<tool_name>` (the SDKs report an MCP tool by its own name and server).
10. **prompt_text** / **response_text** equal `prompt` / `response` (on the prompt and response events).

Local-model and live runs never assert model text: their expected values come from the recorded responses, so the checks compare the events with what the provider actually returned.

## Mock LLM server edge modes

Both mock LLM servers (Node.js `testkit/src/mock-llm-server.ts`, Python `tests/mock_server.py`) answer these the same way, for tests that need a reply no cassette holds:

| Trigger | Routes | Reply |
| --- | --- | --- |
| model `mock-unauthorized` | all | `401`. OpenAI routes: `{ "error": { "message": "Incorrect API key provided (mock)", "type": "invalid_request_error", "code": "invalid_api_key" } }`. Anthropic: `{ "type": "error", "error": { "type": "authentication_error", "message": "invalid x-api-key (mock)" } }`. |
| model `mock-forbidden` | all | `403`. OpenAI routes: `{ "error": { "message": "Forbidden (mock)", "type": "invalid_request_error", "code": "permission_denied" } }`. Anthropic: `{ "type": "error", "error": { "type": "permission_error", "message": "Forbidden (mock)" } }`. |
| any `429` or `529` reply | all | header `retry-after: 1`. |
| model `mock-stream-error`, `stream: true` | `/v1/messages` | `message_start`, `ping`, then `event: error` with `{ "type": "error", "error": { "type": "overloaded_error", "message": "Overloaded (mock)" } }`, and the stream ends. |
| request `thinking: { "type": "enabled", ... }` | `/v1/messages` | a `thinking` block (`thinking`: `"I should answer concisely."`, `signature`: `"mock-signature"`) before the other content. Streamed: `content_block_start` (empty thinking), one `thinking_delta`, one `signature_delta`, `content_block_stop`. |
| every Anthropic stream | `/v1/messages` | one `ping` right after `message_start`. |
| every OpenAI usage | chat, responses | `completion_tokens_details.reasoning_tokens` / `output_tokens_details.reasoning_tokens` is `4` (part of the output tokens). |

## Test vectors (`cassette-conformance-v0.1.json`)

- `scrub[]`: `scrub(input, env)` deep-equals `output`; `env` stands in for the process environment.
- `facts[]`: `provider_facts(cassette)` deep-equals `calls`. `cassette` is a path under `spec/fixtures/cassettes/` or an inline cassette.
- `expected_interactions[]`: `expected_interactions(provider_facts(cassette), responses)`, where `responses` are `ai_response_received` events whose only property is `llm_call_count` (from `llm_call_counts`), deep-equals `expected`, or fails with a message starting with `error`.
- `assert.cases[]`: start from copies of `base_events` and `base_expected`. `expected` is merged over `base_expected` key by key (`expected_replace` replaces it). Then, on the events, `set` assigns each dotted path (the first segment is the event index, numeric segments index arrays), `delete` removes each path, `duplicate` appends a copy of each listed event, and `remove` drops the listed events (indices refer to the list before `duplicate`). `assert_ai_events(events, expected, { optional_fields })`, with `optional_fields` from the case (default `[]`), passes when `error` is `null` and otherwise fails with a message starting with `error`.
