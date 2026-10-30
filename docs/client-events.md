# Client events

See the [shared client specification](../spec/CLIENT-EVENTS.md) for web and mobile SDKs, one-shot interactions, suggestions, feedback, client engagement and declared tool events since v0.4.

For a feature without a chat thread, set `OneShot: true` when starting the interaction or task. Return the interaction ID to the client and use it as both `interaction_id` and `conversation_id`. Explicit or context conversation IDs take precedence; the existing chat default still generates a separate conversation ID.

`exported` uses `action_details.destination`, a snake_case label such as `download`, `dashboard`, `agent` or `ide` (1-64 ASCII characters: a lowercase letter first, then lowercase letters, digits or underscores).
