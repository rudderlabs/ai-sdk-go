# Client events

See the [shared client specification](../spec/CLIENT-EVENTS.md) for web and mobile SDKs, one-shot interactions, suggestions, feedback, client engagement and declared tool events since v0.4.

For features without a chat thread, use [one-shot interactions](configuration.md#interactionoptions). Return `in.ID()` as `interaction_id` and `in.ConversationID()` as `conversation_id`. Explicit or context conversation IDs take precedence; the existing chat default still generates a separate conversation ID. The IDs match only for one-shot interactions without an explicit or context conversation ID.

Export destination rules: [`ai_user_action`](events.md#ai_user_action).

A browser/client response copy may also report `incomplete` when it renders a partial terminal answer from a run that stopped at a cap. The client schema remains v0.2.
