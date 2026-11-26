# Performance and limits

The SDK runs in your process for each model call and MCP request. It adds tens of microseconds to model calls that take hundreds of milliseconds to seconds: well under 1%. Content capture adds a few milliseconds per interaction (see below).

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

On CI the MCP round trip's p50 did not change measurably (the go-sdk in-memory round trip alone takes 118 µs and varies more than that), while process CPU, which counts every goroutine, rose by about 131 µs per call; that row therefore has a CPU budget too. Masking captured text is the slowest step.

Extra allocations per call repeat exactly from run to run and are gated in CI: about 300 for an interaction with one wrapped call, 315 for an MCP call, 48 per streamed chunk and 700 with content capture. Each must stay within `internal/benchreport/alloc_budget.json`, set at the measurement plus 25%. Timings are a published target, not a CI gate.

## How it is measured

`make bench` runs root and `rudderopenai` `BenchmarkOverhead` against an in-process OpenAI `http.RoundTripper`, in-memory go-sdk MCP transports and an analytics client that drops events. No network time is counted. Each scenario has `baseline` and `sdk` sub-benchmarks. `BENCH_ROUNDS` (default 5) runs baseline then SDK per scenario to reduce drift. Repeated sub-benchmark names get Go's `#01`, `#02` suffixes; `internal/benchreport` takes each metric's median across rounds and generates the table.

| Scenario                          | What runs                                                                                              |
| --------------------------------- | ------------------------------------------------------------------------------------------------------ |
| `interaction/manual`              | `RunInteraction` with one `RecordCall`: all of it is SDK work, so it has no baseline                  |
| `interaction/openai`              | `RunInteraction` with one wrapped chat completion                                                      |
| `interaction/openai-stream`       | The same, streamed: 100 content chunks                                                                 |
| `interaction/openai+content`      | A 4 KB prompt and a 4 KB answer, `WithCaptureContent` and `WithLLMCallEvents` on, all masked           |
| `interaction/openai+spans`        | `interaction/openai` with `WithEmitSpans`                                                              |
| `interaction/openai-stream+spans` | `interaction/openai-stream` with `WithEmitSpans`                                                       |
| `mcp/tools-call`                  | One `tools/call` through `WrapMCPServer`, over in-memory transports                                    |

Each benchmark reports, beside `ns/op`, `B/op` and `allocs/op`, the p50 and p99 of one call (`p50-ns`, `p99-ns`), process CPU per call (`cpu-ns/op`, from `getrusage`, so it includes other goroutines) and garbage collections per 10,000 calls (`gc/10k-op`). The raw output goes to `bench-results/go.txt`, the table's data to `bench-results/go.json`.

## Load

`make load` runs `TestLoad` in `internal/conformance` and `TestLoadMCPMaps` in the root package (build tag `load`). `TestLoad` runs 200 goroutines at 500 interactions per second. Nine in ten interactions make two wrapped OpenAI calls with a tool call between them, and every tenth makes one streamed call instead; every fiftieth provider request fails with HTTP 429. Each interaction is followed by an MCP `tools/call` carrying the interaction's ID. Events go through analytics-go to a local data plane. A second phase repeats the workload while the data plane answers HTTP 500.

The run fails on missing prompt, answer or tool-call events with a healthy data plane, interactions left open, an `internal` error, MCP maps exceeding their cap or never reaching it, or post-GC heap growth above 25% plus 8 MB from warm-up. The MCP cap is 2,000; `TestLoad` requires eviction only after more than 2,000 MCP calls. It also fails if the second phase rejects no data or recovery flush does not finish. On CI, 30 seconds at 500 interactions/second delivered all 15,000 answers and 15,000 tool calls. Heap fell from 10.3 MB after warm-up to 4.2 MB. PRs run 30 seconds; `main` and nightly jobs run 5 minutes. Override with `LOAD_SECONDS`, `LOAD_RATE` and `LOAD_WORKERS`.

On the laptop, 5,000 interactions per second (about 15,000 events per second) went through without loss. While the data plane is down, analytics-go retries and then drops batches; the SDK's memory stays flat.

## Limits

Memory caps and their effects: [configuration](configuration.md#limits). Product-event trimming: [events](events.md#event-size). MCP state caps: [MCP limits](mcp.md#limits). Span budgets: [tracing](tracing.md#privacy-and-size).

An interaction leaves nothing behind once it ends. MCP keeps each session's or interaction's call position, and one `retry_count` entry per distinct tool and arguments, so a long session can fill a map. Each MCP map holds up to `MaxMapEntries` entries per server and drops the oldest first.

## Fuzz tests

Go's native fuzzing covers the protocol and content parsers. Every `go test` runs the seed inputs; a weekly CI job fuzzes each target for 60 seconds and uploads any failing input.

| Target | Package | Checks |
| --- | --- | --- |
| `FuzzJoinIDs` | root | Any `_meta` JSON gives bounded IDs, and a valid `traceparent` wins over `com.rudderstack/trace_id` |
| `FuzzTraceparent` | root | A `traceparent` whose fields are hex of the right length gives a trace ID exactly when it is valid W3C; any other gives a 32-digit lowercase ID or none |
| `FuzzChatUsage`, `FuzzMessagesUsage`, `FuzzUsage` | `rudderopenai`, `rudderanthropic`, `ruddergenai` | Whatever a response carries as usage, the app gets the provider SDK's result unchanged, and every event validates with non-negative integer token totals |
| `FuzzChatStreamSplit`, `FuzzResponsesStreamSplit`, `FuzzMessagesStreamSplit` | `rudderopenai`, `rudderanthropic` | A recorded stream gives the same chunks and events however its bytes are split |
| `FuzzChatStreamCut`, `FuzzResponsesStreamCut`, `FuzzMessagesStreamCut` | `rudderopenai`, `rudderanthropic` | A cut or corrupted stream reaches the app exactly as the provider SDK alone delivers it, and still ends in one conforming answer |

Run one with `go test -run '^$' -fuzz '^FuzzChatUsage$' -fuzztime 60s ./rudderopenai`. A failing input lands under `testdata/fuzz/`; commit it to keep it as a seed.
