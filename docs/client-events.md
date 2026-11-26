# Client events

See the [shared client specification](../spec/CLIENT-EVENTS.md) for web and mobile SDKs, one-shot interactions, suggestions, feedback, client engagement and declared tool events since v0.4.

For a feature without a chat thread, set [`OneShot: true`](configuration.md#interactionoptions) when starting the interaction or task. Return `in.ID()` as `interaction_id` and `in.ConversationID()` as `conversation_id`. An explicit or inherited conversation ID takes precedence (the context's for an interaction, a chained parent's for a task); the existing chat default still generates a separate conversation ID. The IDs match only for one-shot runs without an explicit or inherited conversation ID.

Export destination rules: [`ai_user_action`](events.md#ai_user_action).

A browser/client response copy may also report `incomplete` when it renders a partial terminal answer from a run that stopped at a cap. The client schema remains v0.2.
