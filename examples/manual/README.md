# Manual example

An agent turn reported by hand, with no provider integration: `StartInteraction`, `RecordCall` for each model call, `RecordToolCalls` and `RecordToolResult` for the tool, `End`. `Configure` declares a `ticket_created` event for the `create_ticket` tool (`WithEventRules`), turns on `WithLLMCallEvents` and `WithToolBreakdown`, sets a classifier with a custom `Classify` function and prices the model.

```sh
# End to end against a local mock data plane, no credentials needed:
go run ./examples/manual --check

# The same check, with the events sent to your data plane (skipped without both variables):
RUDDERSTACK_WRITE_KEY=... RUDDERSTACK_DATA_PLANE_URL=... go run ./examples/manual --live

# Once; without the RudderStack variables the events go to a local data plane and are printed:
go run ./examples/manual
```

`--check` must produce, all tagged `manual` and for `user_42`, with the interaction and conversation IDs:

- `ai_user_prompt_created`;
- `ai_llm_response_received` per call, with tokens, `finish_reason` and `cost_usd`;
- `ticket_created` with `tool_name`, `tool_call_id`, `tool_status: success`, `tool_duration_ms` and the rule's `priority` and `ticket_id`;
- `ai_response_received`: `llm_call_count: 2`, `tool_call_count: 1`, summed tokens and cost, a `tool_breakdown` row with the measured duration;
- `ai_interaction_classified` from the custom classifier: `intent`, `outcome`, `intent_summary`, `classifier: custom`;
- every event valid against the spec schema.
