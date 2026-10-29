# AI events spec changelog

Changes to [`ai-events-v0.4.schema.json`](ai-events-v0.4.schema.json), shared byte for byte by the Node.js, Python and Go SDKs.

## 0.4.0 — 2026-10-29

- **SDK-5989 — one-shot and client events.** Explicit one-shot interactions use the interaction ID as the required conversation ID when no thread exists. Chat defaults retain a separate conversation ID. Web and mobile client events cover prompt metadata, actionable suggestions and reversion, scoped feedback and preset reasons, snake_case export destinations, conversation pin/share, agent and mode selection, assistant entry/close, limits and declared client tool outcomes.
- **SDK-5990 — server fields.** Answers support `blocked` with `block_reason`, app-set confidence, credits, agent versions, variants and compaction counts. Declared tools support denied outcomes, MCP server names and result sizes. Per-call events include measured timing, credits, purpose, fallback and BYOK flags; usage entries group by purpose. Provider context-window errors use `context_limit`; classifier outcomes include `unsupported`. App-sent `ai_limit_reached` describes customer usage limits.
- **SDK-5992 — sampling.** Optional per-event rates stamp kept server events with `sample_rate`, including `ai_limit_reached`. The shared SHA-256 interaction draw keeps decisions consistent across events and segments. Answers remain unsampled by default; the shared sampling fixture fixes cross-SDK decisions.
- **SDK-5991 — pause/resume.** Each paused or final answer segment shares the interaction ID. Paused segments carry `pause_reason`; final segments carry `pause_count` and `total_paused_ms`. JSON snapshots preserve scope, deduplication and pending usage across restarts. Optional `ai_task_started` reports new background runs once and is not repeated on resume.

The server schema and conformance fixture are v0.4; SDKs emit specVersion 0.4.0. The migration checker accepts both 0.3.0 and 0.4.0 captures. The client schema remains v0.2, matching the v0.3 release precedent. “Since v0.4” annotations remain: the v0.3 release did not remove version annotations.
