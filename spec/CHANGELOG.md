# AI events spec changelog

Changes to [`ai-events-v0.3.schema.json`](ai-events-v0.3.schema.json), shared byte for byte by the Node.js, Python and Go SDKs.

## Unreleased — sampling (SDK-5992)

- Add optional `sample_rate` on sampled server events, including `ai_limit_reached`, and declare it in the tracking plan. The shared sampling fixture fixes the interaction hash and keep/drop decision across SDKs.

## Unreleased — since v0.4 (SDK-5990)

Server fields customers already send by hand:

- **Declined and denied.** `response_status: blocked` with `block_reason` (`guardrail`, `policy`, `unsupported`, `skipped`, `other`); `tool_status: denied` on declared tool events; `unsupported` in the default outcomes.
- **Confidence.** App-set `confidence` (`low`, `medium`, `high`) on `ai_response_received`. A hand-off to a person is `outcome: escalated` plus the hand-off tool's declared tool event.
- **Credits.** `total_credits` on `ai_response_received` and `credits` on `usage_breakdown` entries and `ai_llm_response_received`, from the app's credit pricing. New app-sent event `ai_limit_reached` (`limit_type`, with the scope fields).
- **Per-call detail** on `ai_llm_response_received`: `latency_ms`, `time_to_first_token_ms`, and app-set `call_purpose`, `fallback_used`, `byok`. `usage_breakdown` entries carry `call_purpose` and are grouped by it.
- **Versions.** `agent_version` and `variant` in the shared scoping fields.
- **MCP client calls.** `mcp_server` and `result_size_bytes` on declared tool events.
- **Compaction.** `compaction_count` on `ai_response_received`; `context_limit` in the `error_type` values.

These additions remain in the v0.3 server and v0.2 client schemas. SDK specVersion stays 0.3.0 until the final stacked layer.

## Additions since v0.4 — SDK-5989

- One-shot interactions use the interaction ID as the required conversation ID with explicit server opt-in; chat defaults and existing 0.3.0 captures remain compatible.
- Client events cover web and mobile SDKs (JavaScript, iOS, Android, React Native, Flutter).
- Add actionable suggestion exposure and reversion with item counts; scoped feedback, preset reasons, dismissal and request triggers; exports with snake_case destination labels.
- Add conversation pin/share, agent/mode selection, assistant entry/close methods, prompt input/context metadata, client limits and declared client tool outcomes.
- Extend conformance and migration fixtures. Existing schema filenames, schema IDs and SPEC_VERSION remain unchanged; a later stacked layer performs the version rename.
