# Anthropic example

A support answer from a streamed Messages call (`claude-haiku-4-5`, small `max_tokens`) with the store policy as a cached system prompt, tracked by `rudderanthropic.WithTracking()` inside `rudderstackai.RunInteraction`. `RegisterPrice` sets cache read and cache write rates.

```sh
# End to end against a local mock data plane and a mock LLM server, no credentials needed:
go run ./examples/anthropic --check

# The same check, with the events sent to your data plane (skipped without both variables):
RUDDERSTACK_WRITE_KEY=... RUDDERSTACK_DATA_PLANE_URL=... go run ./examples/anthropic --live

# Once for real (needs `ANTHROPIC_API_KEY`); without the RudderStack variables the events go to a local data plane and are printed:
go run ./examples/anthropic
```

`--check` replays `spec/fixtures/cassettes/examples/node-anthropic.json` (a cache read and a cache write) and must produce:

- `ai_user_prompt_created` and `ai_response_received` tagged `anthropic`, for `user_42`, with the interaction and conversation IDs;
- `ai_response_received`: `finish_reason: end_turn`, input tokens as the total (22 uncached + 1024 cache read + 120 cache write), `total_cached_input_tokens`, `total_cache_write_tokens`, and `total_cost_usd` priced at each rate, also in `usage_breakdown`;
- every event valid against the spec schema.
