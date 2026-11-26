# Client events

See the [shared client specification](../spec/CLIENT-EVENTS.md) for web and mobile SDKs, one-shot interactions, suggestions, feedback, client engagement and declared tool events since v0.4.

For features without a chat thread, use [one-shot interactions](configuration.md#interactionoptions). Return the interaction ID to the client as both `interaction_id` and `conversation_id`.

Export destination rules: [`ai_user_action`](events.md#ai_user_action).

A browser/client response copy may also report `incomplete` when it renders a partial terminal answer from a run that stopped at a cap. The client schema remains v0.2.
