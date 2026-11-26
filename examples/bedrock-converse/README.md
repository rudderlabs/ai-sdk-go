# Bedrock Converse example

A weather answer from Amazon Bedrock Runtime through a cross-region inference profile: `Converse` calls a `get_weather` tool, the app runs it, and `ConverseStream` streams the answer to the tool result. The client is `bedrockruntime.NewFromConfig(cfg, rudderbedrock.WithTracking())`, inside `rudderstackai.RunInteraction`. A price registered for `claude-haiku-4-5` prices the profile ID.

See [setup](../README.md#setup).

```sh
go run ./examples/bedrock-converse --check
RUDDERSTACK_WRITE_KEY=... RUDDERSTACK_DATA_PLANE_URL=... go run ./examples/bedrock-converse --live

# Needs AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY and AWS_REGION.
# An app would use config.LoadDefaultConfig:
go run ./examples/bedrock-converse
```

`--check` replays `spec/fixtures/cassettes/bedrock-converse.json` with static credentials and must produce:

- `ai_user_prompt_created` and `ai_response_received` tagged `bedrock`, for `user_42`, with the interaction and conversation IDs;
- `ai_response_received`: `primary_provider: aws.bedrock`, `primary_model: claude-haiku-4-5` (the canonical family name of the profile ID), `llm_call_count: 2`, `tool_call_count: 1`, input tokens including cache reads and writes (AWS reports them apart), `total_cached_input_tokens`, `total_cache_write_tokens`, `total_cost_usd`;
