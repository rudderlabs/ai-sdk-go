# Client events

See the [shared client specification](../spec/CLIENT-EVENTS.md) for web and mobile SDKs, one-shot interactions, suggestions, feedback, client engagement and declared tool events since v0.4.

For a feature without a chat thread, set `OneShot: true` when starting the interaction or task. Return the interaction ID to the client and use it as both `interaction_id` and `conversation_id`. Conversation ID precedence and the chat default: [InteractionOptions](configuration.md#interactionoptions).

Export destination rules: [`ai_user_action`](events.md#ai_user_action).

A browser/client response copy may also report `incomplete` when it renders a partial terminal answer from a run that stopped at a cap. The client schema remains v0.2.
