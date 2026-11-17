# Claude Managed Agents example

One Claude Managed Agents session turn on `claude-haiku-5-5`: the agent runs `echo hi` with the built-in bash tool, calls a custom `get_weather` tool the app answers, then replies. `rudderanthropic.TrackSessionEvents` reads the session's event stream and reports the turn as one answer. `RegisterPrice` sets cache read and cache write rates.

```sh
# End to end against a local mock data plane and a mock LLM server, no credentials needed:
go run ./examples/managed-agents --check

# The same check, with the events sent to your data plane (skipped without both variables):
RUDDERSTACK_WRITE_KEY=... RUDDERSTACK_DATA_PLANE_URL=... go run ./examples/managed-agents --live

# Once for real (needs `ANTHROPIC_API_KEY`); without the RudderStack variables the events go to a local data plane and are printed:
go run ./examples/managed-agents
```

A real run creates an environment and an agent; set `MANAGED_ENVIRONMENT_ID` and `MANAGED_AGENT_ID` to reuse ones you created before.

`--check` replays `spec/fixtures/cassettes/claude-managed-agents-tools.json` (recorded live, shared with the Node.js and Python SDKs) and must produce:

- `ai_user_prompt_created` and `ai_response_received` tagged `claude-managed-agents`, for `user_42`, with the session ID as `conversation_id`;
- `ai_response_received`: `llm_call_count: 2`, `tool_call_count: 2`, input tokens as the total (6 uncached + 4283 cache read + 4590 cache write), and `total_cost_usd` priced at each rate;
- every event valid against the spec schema.
