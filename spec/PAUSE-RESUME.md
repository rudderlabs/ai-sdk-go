# Paused runs and task starts

Since v0.4. These additions are declared in the existing `ai-events-v0.3.schema.json` and `ai-client-events-v0.2.schema.json` pending the final version layer. SDK `SPEC_VERSION` remains `0.3.0`; existing captures remain valid.

A run has one interaction ID and one prompt event. Every pause sends `ai_response_received` with `response_status: paused` and a `pause_reason`. Resuming keeps the IDs and starts a new segment; it sends neither another prompt nor another task-started event. A final end is idempotent. `end` with an explicit or derived paused status behaves like `pause`; a repeated pause before a resume sends nothing. A model or tool call recorded while paused resumes the interaction automatically.

Each response contains only that segment's model/tool counts, tokens, costs, usage and tool breakdown, latency and first-token timing. Summing responses counts each call once. Only the final response adds `pause_count` (positive integer) and `total_paused_ms` (nonnegative integer). Paused time runs from each pause to its resume, or final end while paused, and is excluded from segment latency. A client may report a rendered pause with the same ID and `response_status: paused`; counters and usage remain server-owned.

`pause` returns a JSON snapshot (`PausedInteraction` in Node and Go, a dict in Python), or no snapshot after a final end. Store it alongside the application's job state and pass it as the `resume` option of the next interaction/task. This supports worker restarts and waits lasting days. Each SDK uses the same snake_case keys:

| Required key | Meaning |
| --- | --- |
| `interaction_id`, `conversation_id` | Nonempty run IDs, reused on every segment |
| `trigger` | `user` or `system` |
| `pause_count` | Positive integer, including this pause |
| `total_paused_ms` | Nonnegative integer, waits before this pause |
| `paused_at` | Nonnegative integer, Unix milliseconds at the current pause |

Optional nonempty strings: `user_id`, `anonymous_id`, `feature`, `surface`, `account_id`, `on_behalf_of`, `trigger_source`, `task_name`, `trigger_event_id`, `parent_interaction_id`, `intent`, `trace_id`. Snapshot IDs, identity and run fields take precedence over arguments and request context, including fields absent from the snapshot. No prompt/response content, previous calls, clients or process objects are serialized. Configure the resumed worker's analytics, prices and privacy policies separately. Unusable snapshots warn and start a new interaction.

For an in-process continuation call `resume()` on the paused interaction. Paused interactions are removed from the open-interaction registry, so TTL and shutdown do not finalize them. Returning normally from an interaction/task leaves a paused run paused; an exception still sends a final error. An abandoned paused run retains its last paused event.

Pause timestamps use the wall clock so cross-process waits can be measured; negative elapsed time after clock skew is clamped to zero. Snapshots do not coordinate workers: persist them reliably and serialize approval deliveries per job. Replaying a snapshot in two workers can produce duplicate segments; transport delivery and application job state remain the app's responsibility.

`ai_task_started` is optional and off by default (`emitTaskStarted`, `emit_task_started`, `WithTaskStartedEvents`). It is sent before a new task's work and contains `conversation_id`, `interaction_id`, `task_name`, and supplied `trigger_source` and `parent_interaction_id`, plus the run's background fields. Resumes and ordinary interactions send none. A task start without a final response identifies an in-flight or abandoned run; its most recent paused response identifies a wait.

`fixtures/conformance-v0.3.json` covers repeated pauses, explicit and implicit resumes, persisted snapshots, no-ops, final ends while paused and chained task starts. Approval-loop examples in each SDK demonstrate three worker invocations separated by two approvals.
