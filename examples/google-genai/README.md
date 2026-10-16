# Google Gen AI example

A weather answer on the Gemini API (`gemini-3.5-flash-lite`): `generateContent` calls a `get_weather` tool, the app runs it, and `generateContentStream` streams the answer to the function response. Tracking sits in the HTTP client (`ruddergenai.HTTPClient(nil)` in `genai.ClientConfig`), inside `rudderstackai.RunInteraction` with `ToolBreakdown` on.

```sh
# End to end against a local mock data plane and a mock LLM server, no credentials needed:
go run ./examples/google-genai --check

# The same check, with the events sent to your data plane (skipped without both variables):
RUDDERSTACK_WRITE_KEY=... RUDDERSTACK_DATA_PLANE_URL=... go run ./examples/google-genai --live

# Once for real (needs `GEMINI_API_KEY` or `GOOGLE_API_KEY`); without the RudderStack variables the events go to a local data plane and are printed:
go run ./examples/google-genai
```

`--check` replays `spec/fixtures/cassettes/google-genai.json` and must produce:

- `ai_user_prompt_created` and `ai_response_received` tagged `google-genai`, for `user_42`, with the interaction and conversation IDs;
- `ai_response_received`: `llm_call_count: 2`, `tool_call_count: 1`, tokens summed over both calls (the stream's last running total), `finish_reason: stop`, `total_cost_usd`, and a `tool_breakdown` row for `get_weather`;
- every event valid against the spec schema.
