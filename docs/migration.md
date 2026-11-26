# Migration

## Checking a migration

`aicheck` validates recorded RudderStack events during a migration. Its separate module adds no app dependency. Record once per switch setting while exercising the app. Replace the analytics-go client with `ruddertest.Recorder` where the app builds it, before `rudderstackai.Configure` and switch reads. This captures legacy calls and switch-off runs; configuring the recorder after startup misses them:

```go
client, err := ruddertest.NewRecorder("events.jsonl") // github.com/rudderlabs/ai-sdk-go/ruddertest, instead of analytics.NewWithConfig(...)
```

The recorder writes to the current folder. Record outside the repo, such as a temp folder, or add the capture file to `.gitignore` to avoid committing it.

```bash
go install github.com/rudderlabs/ai-sdk-go/cmd/aicheck@v0.8.0
aicheck events.jsonl --expect-legacy chatTurnCompleted \
  --feature support_bot --surface web --user-id demo_user_1 --expect-joins
aicheck legacy-off.jsonl --absent chatTurnCompleted
aicheck ai-off.jsonl --no-ai-events --expect-legacy chatTurnCompleted
aicheck what-changes ai-off.jsonl events.jsonl  # redacted before/after diff for the PR
```

The check validates every event against the spec this version emits (and client copies against the client-event schema), and checks that legacy events keep sending (`--expect-legacy`) or stop when their switch is off (`--absent`, counted by provenance: events without an AI SDK's `context.wrapperLibrary`, so an AI event with the same name does not count), that no AI event is sent with the AI switch off (`--no-ai-events`).

It checks client/server joins in both directions (`--expect-joins`, feedback copies per action type included, honouring `--client-opt-out client_prompt|client_response|client_feedback|server_feedback`, `--feedback-endpoint` and `--client-surface` for surfaces with a browser client).

It checks that app-caught failures are not reported as success (`--failed-interaction <id>`), plus identity (`--user-id`, `--expect-session`, and `--mcp-user-id` or `--mcp-anonymous` for MCP servers), content capture and MCP flags (`aicheck --help`).

Exit code 0 passes, 1 fails, 2 is bad usage or an unreadable file.

## Upgrading to AI events spec v0.4

Events carry `specVersion` `0.4.0`. The server schema and conformance fixture are now `ai-events-v0.4.schema.json` and `conformance-v0.4.json`. The client schema remains `ai-client-events-v0.2.schema.json`, following the v0.3 release precedent. The migration checker accepts both 0.3.0 and 0.4.0 captures.

- [One-shot and client events](client-events.md).
- Server fields: [response fields](events.md#ai_response_received), [credits](configuration.md#pricing), [call metadata](integrations.md#how-integrations-work), [agent versions and variants](configuration.md#interactionoptions), [MCP results](mcp.md#events), [tool events](events.md#declared-tool-events) and [error types](events.md#error-types).
- [Sampling](configuration.md#sampling).
- [Pause/resume](configuration.md#pauses-and-resumes) and [`ai_task_started`](events.md#ai_task_started).

## Upgrading to 0.3.0

No API was removed.

- RudderStack client: [minimum version](integrations.md#compatibility) and [custom-client flushing](configuration.md#flush-and-shutdown).
- **`app_error`**: Update queries that count `api_error` as all failures. Provider classification and `AcceptOutput`: [Status](events.md#status).
- **Usage on failed streams**: A Gemini stream that fails mid-way is now a failed call, no longer a success. Retained usage and pricing: [Streaming and cancellation](integrations.md#streaming-and-cancellation) and [Cost](events.md#cost).
