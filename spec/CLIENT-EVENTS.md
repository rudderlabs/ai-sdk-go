# Client events — additions since v0.4

A client is a web or mobile app using a RudderStack SDK: JavaScript, iOS, Android, React Native or Flutter. All client events carry `source: "client"`. Validate them against [ai-client-events-v0.2.schema.json](ai-client-events-v0.2.schema.json); client copies of prompt, response, action and declared tool events also validate against [ai-events-v0.4.schema.json](ai-events-v0.4.schema.json). SDK events emit `context.wrapperLibrary.specVersion: "0.4.0"`. The migration checker also accepts existing 0.3.0 captures.

## One-shot interactions

A Generate button, suggested fix or prefilled form can produce one answer without a chat thread. Set the required `conversation_id` to `interaction_id` on every event for that interaction, on both client and server. Generate or return one interaction ID and reuse it across copies and actions. Server options are `oneShot: true` (Node.js), `one_shot=True` (Python), and `OneShot: true` (Go), including background tasks. Without an explicit or context conversation ID, these options use the interaction ID. Explicit/context IDs and parent task conversations retain precedence.

The options default to false: existing chat callers that omit a conversation ID keep a separate generated conversation ID. This avoids silently changing their joins or conversation counts. Existing captures may omit `conversation_id` on the first client prompt of a chat thread the server has not yet created; one-shot prompts always set both IDs.

## Fields and events

Every addition below is since v0.4. Names and kinds are app-defined categories, excluding names of user content or URLs.

| Event | Additions |
| --- | --- |
| `ai_user_prompt_created` | `input_method` (e.g. text, voice, button), `agent_name`, `mode`, nonnegative integer `context_item_count`, unique string array `context_item_types` (context excluding files) |
| `ai_assistant_opened` | `entry_point` (e.g. launcher, shortcut, deep_link) |
| `ai_assistant_closed` | `close_method` (e.g. close_button, navigation), alongside existing `open_duration_ms` |
| `ai_agent_selected` | `agent_name` or `mode`, optionally `from_agent_name` / `from_mode` |
| `ai_suggestion_shown` | Both IDs, `suggestion_kind` and optional nonnegative integer `item_count` |
| `ai_feedback_requested` | Both IDs, `trigger` describing why the app asked (e.g. after_answer, on_close, manual) |
| `ai_conversation_pinned`, `ai_conversation_shared` | `conversation_id`; omit conversation titles and share links |
| `ai_limit_reached` | `limit_type`: rate, quota, credits or plan; IDs when known |
| Customer-named declared tool event | Both IDs, `tool_name`, `tool_status` (success, error, unknown), optional `tool_call_id`, `tool_duration_ms`, `agent_name`; `source: "client"` for tools the client runs |

`ai_suggestion_shown` applies only to actionable AI output shown inline or in an assistant: fixes, autofill, proposed action items or starter prompts. Rendering an ordinary chat answer does not send it. Use it as the denominator of suggestion acceptance. `accepted`, `edited`, `rejected` and new `reverted` describe that lifecycle; reverted means undoing an accepted suggestion. `action_details.items_total` and `items_accepted` are nonnegative integer counts of actionable items offered and kept.

On `ai_user_action`, new `feedback_dismissed` means the user closed a request without answering. `action_details.feedback_reasons` is a string array of preset reasons. `action_details.feedback_scope` is interaction (the default), conversation or feature. Scoped feedback still carries both IDs: mint an interaction ID for the feedback and use it as the conversation ID too when no thread exists. A conversation/feature feedback action does not require an answer for that newly minted ID. `ai_feedback_requested` is the denominator of feedback response rate.

New `exported` uses `action_details.destination`, a snake_case label such as `download`, `dashboard`, `agent` or `ide`. Labels have 1-64 ASCII characters: a lowercase letter first, then lowercase letters, digits or underscores. Client-declared tool events report execution outcomes; send no raw arguments or results. Prompt and feedback text follow the application's content-capture and masking policy.
