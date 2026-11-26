# Migration

## Checking a migration

Moving AI events your app already sends through RudderStack to this SDK? `aicheck` checks a recording of what the app sends; it is its own module, so the app gains no dependency. Record the events while you drive the app, once per switch setting. In the test or harness, make `ruddertest.Recorder` the app's own analytics-go client where the app builds it, before the app calls `rudderstackai.Configure` and reads its switches, so legacy calls and switch-off runs are recorded too (configuring a recorder after start misses them):

```go
client, err := ruddertest.NewRecorder("events.jsonl") // github.com/rudderlabs/ai-sdk-go/ruddertest, instead of analytics.NewWithConfig(...)
```

The recorder creates the file in the current folder, where it's easy to commit by accident: record outside the repo (for example in a temp folder) or add the capture file to `.gitignore`.

```bash
go install github.com/rudderlabs/ai-sdk-go/cmd/aicheck@v0.8.0
aicheck events.jsonl --expect-legacy chatTurnCompleted \
  --feature support_bot --surface web --user-id demo_user_1 --expect-joins
aicheck legacy-off.jsonl --absent chatTurnCompleted
aicheck ai-off.jsonl --no-ai-events --expect-legacy chatTurnCompleted
aicheck what-changes ai-off.jsonl events.jsonl  # redacted before/after diff for the PR
```

The check validates every event against the spec this version emits (and client copies against the client-event schema), and checks that legacy events keep sending (`--expect-legacy`) or stop when their switch is off (`--absent`, counted by provenance: events without an AI SDK's `context.wrapperLibrary`, so an AI event with the same name does not count), that no AI event is sent with the AI switch off (`--no-ai-events`), client/server joins in both directions (`--expect-joins`, feedback copies per action type included, honouring `--client-opt-out client_prompt|client_response|client_feedback|server_feedback`, `--feedback-endpoint` and `--client-surface` for surfaces with a browser client), that app-caught failures are not reported as success (`--failed-interaction <id>`), plus identity (`--user-id`, `--expect-session`, and `--mcp-user-id` or `--mcp-anonymous` for MCP servers), content capture and MCP flags (`aicheck --help`). Exit code 0 passes, 1 fails, 2 is bad usage or an unreadable file.

## Upgrading to AI events spec v0.4

Events carry `specVersion` `0.4.0`. The server schema and conformance fixture are now `ai-events-v0.4.schema.json` and `conformance-v0.4.json`. The client schema remains `ai-client-events-v0.2.schema.json`, following the v0.3 release precedent. The migration checker accepts both 0.3.0 and 0.4.0 captures.

- **One-shot and client events**: opt into `OneShot` for interactions without a thread. Shared client schemas cover prompts, suggestions, feedback, exports, conversation and agent controls, limits and declared tools on web and mobile.
- **Server fields**: blocked answers, denied tools, confidence, credits, per-call timing and purpose, fallback and BYOK flags, agent versions, variants, MCP result sizes, compaction and `context_limit` errors.
- **Sampling**: `WithSampleRates` controls each event; kept events carry `sample_rate`. Events of one interaction share a stable draw. Answers are unsampled by default.
- **Pause/resume**: paused and final answer segments share an interaction ID, including after JSON restore. Final answers carry pause counts and total paused time. Optional `ai_task_started` fires once per new background run.

## Upgrading to 0.3.0

No API was removed.

- **RudderStack client**: every module requires `github.com/rudderlabs/analytics-go/v4` v4.5.1, so `Flush` and `Shutdown` send its queue. A custom client without `Flush(ctx)` or `Flush()` is still reported once as `client_outdated`.
- **`app_error`**: an answer that fails in your own code after a successful model call has `error_type: app_error`, not `api_error`. An error from a model call, or one that carries a provider signal (HTTP status, provider error code or provider error type), keeps its type. A generic `EndOptions{Status: StatusError, ErrorType: "app_error"}` also preserves the latest call's provider classification; a successful retry clears it. Call `in.AcceptOutput()` when you keep output that stopped at the token cap, so a later failure in your code is not reported as `length`. Update queries that count `api_error` as all failures.
- **Usage on failed streams**: a streamed call that fails partway keeps the tokens it already reported and still counts in `llm_call_count` (unpriced). A Gemini stream that fails mid-way is now a failed call, no longer a success.
