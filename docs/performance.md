# Performance and limits

What `ai-sdk-go` costs your app, and the limits that keep it bounded. The SDK does its work in your process, next to each model call and MCP request. A model call takes hundreds of milliseconds to seconds, while the SDK adds tens of microseconds: well under 1% of the call. Content capture costs more, a few milliseconds per interaction (see below).

- [Overhead](#overhead)
- [How it is measured](#how-it-is-measured)
- [Load](#load)
- [Limits](#limits)
- [Fuzz tests](#fuzz-tests)

## Overhead

The overhead is the median (p50) of the same call with and without the SDK. "CI" is a GitHub-hosted `ubuntu-latest` runner (AMD EPYC 9V45, 4 vCPUs, Go 1.26.11); "laptop" is an Apple M4 Pro (Go 1.26.11). Shared runners vary from run to run, so the budget is about twice the CI measurement.

| Work                                                                   | Budget (CI)                 | CI                  | Laptop              |
| ---------------------------------------------------------------------- | --------------------------- | ------------------- | ------------------- |
| Interaction with one wrapped model call                                | ≤ 50 µs                     | 23 µs               | 19 µs               |
| Interaction with one `RecordCall`, no wrapper                          |                             | 12 µs               | 10 µs               |
| MCP `tools/call` through `WrapMCPServer`                               | ≤ 50 µs; ≤ 300 µs CPU       | no change; 131 µs CPU | 11 µs; 39 µs CPU  |
| Each streamed chunk                                                    | ≤ 9 µs                      | 4.5 µs              | 3.9 µs              |
| `WithEmitSpans`, added per model call                                  | ≤ +60 µs                    | +28 µs              | +24 µs              |
| `WithEmitSpans`, added per streamed chunk                              | ≤ +1.5 µs                   | +0.6 µs             | under 1 µs          |
| `WithCaptureContent` with `WithLLMCallEvents`, 8 KB of prompt and answer | ≤ 8 ms                    | 3.7 ms              | 3.3 ms              |

An MCP call's wrapper work runs mostly off the request's path: on CI it leaves the round trip's time unchanged while using about 131 µs of CPU, which is why that row has a CPU budget too. Masking captured text is the slowest step, in the `regexp2` engine; SDK-6932 tracks making it faster, after which the content budget will tighten.

Extra allocations per call repeat exactly from run to run and are gated in CI: about 300 for an interaction with one wrapped call, 315 for an MCP call, 48 per streamed chunk and 700 with content capture. Each must stay within `internal/benchreport/alloc_budget.json`, set at the measurement plus 25%. Timings are a published target, not a CI gate.

## How it is measured

`make bench` runs `BenchmarkOverhead` in the root package and in `rudderopenai`, against an OpenAI endpoint behind an in-process `http.RoundTripper` and an MCP server over the go-sdk's in-memory transports, so no network time is counted, with an analytics client that drops every event, so the numbers are the SDK's own work. Each scenario has a `baseline` sub-benchmark without the SDK and an `sdk` one with it; `internal/benchreport` turns the output into the table above.

| Scenario                          | What runs                                                                                              |
| --------------------------------- | ------------------------------------------------------------------------------------------------------ |
| `interaction/manual`              | `RunInteraction` with one `RecordCall`                                                                 |
| `interaction/openai`              | `RunInteraction` with one wrapped chat completion                                                      |
| `interaction/openai-stream`       | The same, streamed: 100 content chunks                                                                 |
| `interaction/openai+content`      | A 4 KB prompt and a 4 KB answer, `WithCaptureContent` and `WithLLMCallEvents` on, all masked           |
| `interaction/openai+spans`        | `interaction/openai` with `WithEmitSpans`                                                              |
| `interaction/openai-stream+spans` | `interaction/openai-stream` with `WithEmitSpans`                                                       |
| `mcp/tools-call`                  | One `tools/call` through `WrapMCPServer`, over in-memory transports                                    |

Each benchmark reports, beside `ns/op`, `B/op` and `allocs/op`, the p50 and p99 of one call (`p50-ns`, `p99-ns`), process CPU per call (`cpu-ns/op`, from `getrusage`, so it includes other goroutines) and garbage collections per 10,000 calls (`gc/10k-op`). The Node.js and Python SDKs run the same scenario IDs. The raw output goes to `bench-results/go.txt`, the table's data to `bench-results/go.json`.

## Load

`make load` runs `TestLoad` in `internal/conformance` (build tag `load`): 200 goroutines at 500 interactions per second. Each interaction makes two wrapped OpenAI calls with a tool call between them (every tenth one streams, and every fiftieth provider request fails with HTTP 429), followed by an MCP `tools/call` carrying the interaction's ID. Events go through analytics-go to a local data plane. A second phase repeats the workload while the data plane answers HTTP 500.

The run fails if any prompt, answer or tool-call event goes missing while the data plane is healthy, the SDK reports an `internal` error, the MCP maps never reach their cap (lowered to 2,000 so a short run evicts), the heap after garbage collection grows more than 25% (plus 8 MB) from the end of warm-up, the data plane rejected nothing in the second phase, or the flush after it recovers does not finish. On CI, 30 seconds at 500 interactions per second delivered all 15,000 answers and 15,000 tool calls; the heap went from 10.3 MB after warm-up to 4.2 MB at the end. Pull requests run 30 seconds; `main` and a nightly job run 5 minutes. `LOAD_SECONDS`, `LOAD_RATE` and `LOAD_WORKERS` change the run.

On the laptop, 5,000 interactions per second (about 15,000 events per second) went through without loss. While the data plane is down, analytics-go retries and then drops batches; the SDK's memory stays flat.

## Limits

| Limit                                            | Default                 | Set with                                                         |
| ------------------------------------------------ | ----------------------- | ---------------------------------------------------------------- |
| Open interactions                                | 10,000                  | `WithLimits(Limits{MaxOpenInteractions: ...})` ([details](configuration.md#limits)) |
| Interaction lifetime                             | 1 hour                  | `InteractionTTL`                                                 |
| Calls kept one by one per interaction            | 1,000                   | `MaxCallsPerInteraction`                                         |
| Entries per in-memory map (MCP and integrations) | 10,000                  | `MaxMapEntries`                                                  |
| Event properties                                 | 30,000 serialized bytes | fixed ([details](events.md))                                     |

Reaching a limit is reported to `WithOnError` as `limit_reached`; nothing panics into your code. An interaction leaves nothing behind once it ends; MCP keeps a few entries per session or interaction (call positions and argument hashes for `retry_count`), up to `MaxMapEntries` per map.

## Fuzz tests

Go's native fuzzing covers the parsers the Node.js and Python SDKs check with property tests. Every `go test` runs the seed inputs; a weekly CI job fuzzes each target for 60 seconds and uploads any failing input.

| Target | Package | Checks |
| --- | --- | --- |
| `FuzzJoinIDs` | root | Any `_meta` JSON gives bounded IDs, and a valid `traceparent` wins over `com.rudderstack/trace_id` |
| `FuzzTraceparent` | root | A `traceparent` gives a trace ID exactly when it is valid W3C (version 00 with extra fields is left out until SDK-6930) |
| `FuzzChatUsage`, `FuzzMessagesUsage`, `FuzzUsage` | `rudderopenai`, `rudderanthropic`, `ruddergenai` | Whatever a response carries as usage, the app gets the provider SDK's result unchanged, and every event validates with non-negative integer token totals |
| `FuzzChatStreamSplit`, `FuzzResponsesStreamSplit`, `FuzzMessagesStreamSplit` | `rudderopenai`, `rudderanthropic` | A recorded stream gives the same chunks and events however its bytes are split |
| `FuzzChatStreamCut`, `FuzzResponsesStreamCut`, `FuzzMessagesStreamCut` | `rudderopenai`, `rudderanthropic` | A cut or corrupted stream reaches the app exactly as the provider SDK alone delivers it, and still ends in one conforming answer |

Run one with `go test -run '^$' -fuzz '^FuzzChatUsage$' -fuzztime 60s ./rudderopenai`. A failing input lands under `testdata/fuzz/`; commit it to keep it as a seed.
