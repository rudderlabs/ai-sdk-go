# OpenAI example

A support answer from a streamed Chat Completions call (`gpt-4.1-mini`), tracked by `rudderopenai.WithTracking()` inside `rudderstackai.RunInteraction`, with a registered price, then the user's thumbs-up sent with `TrackAction`.

```sh
# End to end against a local mock data plane and a mock LLM server, no credentials needed:
go run ./examples/openai --check

# The same check, with the events sent to your data plane (skipped without both variables):
RUDDERSTACK_WRITE_KEY=... RUDDERSTACK_DATA_PLANE_URL=... go run ./examples/openai --live

# Once for real (needs `OPENAI_API_KEY`); without the RudderStack variables the events go to a local data plane and are printed:
go run ./examples/openai
```

`--check` replays `spec/fixtures/cassettes/examples/node-openai.json` (the cassette has no tool call) and must produce:

- `ai_user_prompt_created` and `ai_response_received` tagged `openai` in `context.wrapperLibrary`, for `user_42`, with the interaction and conversation IDs;
- `ai_response_received`: `response_status: success`, `primary_model` as served (`gpt-4.1-mini-2025-04-14`), `finish_reason: stop`, 29 input and 11 output tokens, `total_cost_usd` from the registered price, `time_to_first_token_ms`;
- `ai_user_action` (`feedback_given`, thumbs up) with the same interaction and conversation IDs, tagged `manual`;
- every event valid against the spec schema.
