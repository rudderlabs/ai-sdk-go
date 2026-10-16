# Bedrock Converse example

A weather answer from Amazon Bedrock Runtime through a cross-region inference profile: `Converse` calls a `get_weather` tool, the app runs it, and `ConverseStream` streams the answer to the tool result. The client is `bedrockruntime.NewFromConfig(cfg, rudderbedrock.WithTracking())`, inside `rudderstackai.RunInteraction`. A price registered for `claude-haiku-4-5` prices the profile ID.

```sh
# End to end against a local mock data plane and a mock LLM server, no credentials needed:
go run ./examples/bedrock-converse --check

# The same check, with the events sent to your data plane (skipped without both variables):
RUDDERSTACK_WRITE_KEY=... RUDDERSTACK_DATA_PLANE_URL=... go run ./examples/bedrock-converse --live

# Once for real (needs `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` and `AWS_REGION`; an app would use `config.LoadDefaultConfig`); without the RudderStack variables the events go to a local data plane and are printed:
go run ./examples/bedrock-converse
```

`--check` replays `spec/fixtures/cassettes/bedrock-converse.json` with static credentials and must produce:

- `ai_user_prompt_created` and `ai_response_received` tagged `bedrock`, for `user_42`, with the interaction and conversation IDs;
- `ai_response_received`: `primary_provider: aws.bedrock`, the profile ID as `primary_model`, `llm_call_count: 2`, `tool_call_count: 1`, input tokens including cache reads and writes (AWS reports them apart), `total_cached_input_tokens`, `total_cache_write_tokens`, `total_cost_usd`;
- every event valid against the spec schema.
