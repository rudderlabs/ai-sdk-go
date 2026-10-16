# Background agent example

A scheduled job with no user in the loop. `rudderstackai.RunTask` drafts a digest with a streamed Anthropic call (`rudderanthropic`), with `TriggerSource: schedule`, `AccountID` and `OnBehalfOf`; a review sub-task started inside it chains to it; `TrackScore` sends a code check of the result.

```sh
# End to end against a local mock data plane and a mock LLM server, no credentials needed:
go run ./examples/background-agent --check

# The same check, with the events sent to your data plane (skipped without both variables):
RUDDERSTACK_WRITE_KEY=... RUDDERSTACK_DATA_PLANE_URL=... go run ./examples/background-agent --live

# Once for real (needs `ANTHROPIC_API_KEY`); without the RudderStack variables the events go to a local data plane and are printed:
go run ./examples/background-agent
```

`--check` replays `spec/fixtures/cassettes/examples/node-anthropic.json` once per model call and must produce:

- no `ai_user_prompt_created` (a task has no prompt event);
- two `ai_response_received` tagged `anthropic`, for the anonymous system user (`anonymousId: ai_system`), `trigger: system`, with `account_id`, `on_behalf_of`, the same `conversation_id`, and each one's own tokens and cost: the task's (`task_name: nightly-digest`, `trigger_source: schedule`) and the sub-task's (`task_name: digest-review`, `trigger_source: agent`, `parent_interaction_id` = the task's `interaction_id`);
- `ai_interaction_scored` for the task: `evaluation_name`, `score_value`, `evaluator: code`, `trigger: system`;
- every event valid against the spec schema.
