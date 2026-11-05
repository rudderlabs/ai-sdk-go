# AI events catalog

These `rudder/v1` resources cover the server AI events v0.4 and client events
v0.2 schemas, including MCP events, lifecycle reasons and client engagement.
Import all four YAML files together. Node.js, Python and Go SDK repositories
ship identical copies of every file in this directory; copy changes verbatim.

SDK guards compare warehouse snake_case names after lowercasing, collapsing non-alphanumeric separator runs and trimming underscores; original custom names and warning keys remain unchanged. Model fields use lowercase canonical Claude identities for emission, grouping and deduplication; repeatedly remove cloud profiles, `anthropic.`, Bedrock versions, Vertex revisions and eight-digit dates, retaining `-latest` and other aliases. Non-Claude IDs remain byte-unchanged; pricing uses raw IDs. See [the spec changelog](../CHANGELOG.md) and [shared audit fixtures](../fixtures/audit-normalisation-v0.1.json) for exact rules and examples.

## Using with an existing catalog

Reuse an existing property by reference when its name matches a schema wire key.
Keep one definition per wire key: never rename `surface` to `ai_surface`, prefix
`trigger`, or rename nested fields. Common collisions include `surface`,
`trigger`, `intent`, `feature`, `source`, `model`, `provider`, `outcome`,
`confidence`, `error_type`, `tool_status`, `account_id`, `country` and `destination`.

Before importing, merge matching definitions, then replace every matching
`#property:<id>` reference in the tracking plan and custom types with the existing
property's catalog reference. The existing catalog ID may differ, but its `name`
must remain the wire key. Merge descriptions and permitted types/values; do not
create a second property with the same name. For example, reuse
`#property:product_surface` if that existing property's name is `surface`.

Shared definitions allow the union of values permitted across both schemas.
An open string in any event makes the shared property an open string. The
schemas remain authoritative for restrictions specific to an event:

| Property | Schema values and merge guidance |
| --- | --- |
| `surface` | Where the user met the AI: `web`, `mobile`, `slack`, `teams`, `email`, `chatgpt_app`, `claude_app`, `ide`, `api`, `other`. Wider than a typical web/mobile enum; narrower than an unrestricted app string. Merge values for the app; SDK events must use this subset. |
| `trigger` | AI interactions use only `user` or `system`. Client `ai_feedback_requested` accepts any nonempty string, such as `after_answer`, `after_n_answers`, `on_close` or `manual`; examples are not an enum. Reuse one open string. |
| `intent`, `feature`, `outcome` | App-defined strings, not enums. Remove any shared restriction that would reject valid SDK values; keep app-specific validation in its schema. |
| `source` | Shared enum `client`, `server`; client events require exactly `client`. |
| `tool_status` | Shared enum `success`, `error`, `denied`, `unknown`. MCP request events allow only `success`, `error`; MCP/client declared tools additionally allow `unknown`. |
| `error_type` | Open on AI answers; MCP error types are `rate_limit`, `timeout`, `cancelled`, `denied`, `auth_error`, `invalid_request`, `connection_error`, `api_error`. |
| `response_status` | Shared server/client enum includes `incomplete` for a partial answer that ended before completion. |
| `limit_type` | Open on server events; client copies use only `rate`, `quota`, `credits`, `plan`. |
| `model`, `provider` | Shared string/null types; nullable breakdown entries reuse these same definitions. |
| `feedback_value` | The server schema imposes no type restriction: all JSON types are permitted. |

The catalog format does not support per-event enum overrides on property refs.
Keep narrower event restrictions, conditional requirements and client prohibitions
in the JSON schemas. The tracking plan deliberately allows custom properties.
Nested `UsageEntry`, `ToolEntry`, `ToolErrorCounts` and `ActionDetails` custom types
reference the existing property definitions; their field names stay wire keys.

## Customer-named tool events

`declared_tool_event` and `mcp_declared_tool_event` are template catalog IDs,
not SDK-emitted event names. Their example names are `tool_executed` and
`external_tool_executed`. Replace each event name with the customer's configured name
(or duplicate the template with new catalog IDs for multiple tools). Keep all
property references. Actual declared event names must not start with `ai_` or
`mcp_`.

## Coverage check

The repository's test suite reads both JSON schemas and all catalog YAML files.
It resolves local JSON pointers and `allOf`, recursively checks each event's
property references (including array objects), and compares shared types/enums
with their union across the schemas. It also checks array item types/enums and
custom type references. Required flags match the intersection of unconditional
requirements across server and client copies, including nested fields and
requirements inherited through `$ref` and `allOf`. Conditional requirements stay
in the schemas, including the first client prompt exception. The Go, vitest and
pytest tests use the same traversal.
Conditional validation and numeric/string bounds remain in the JSON schemas.

## Warehouse column types

The tracking plan validates event properties; it does not configure warehouse
column types. Configure those separately in the destination.
