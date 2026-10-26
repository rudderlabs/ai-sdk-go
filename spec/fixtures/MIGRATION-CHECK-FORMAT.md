# Migration check fixture format (`migration-check-v0.1.json`)

Shared, byte for byte, by the Node.js (`npx @rudderstack/ai check`), Python (`python -m rudderstack.ai.check`) and Go (`aicheck`) migration checkers. Each SDK runs every case and compares its exit code and stdout with the expected ones, so the three print the same lines for the same capture.

```json
{
  "fixture_version": 1,
  "spec_version": "0.3.0",
  "description": "...",
  "cases": [
    { "name": "...", "command": "check" | "what-changes", "captures": ["pass"], "args": ["--expect-joins"], "exit_code": 0, "stdout": "..." | null }
  ]
}
```

A driver runs the checker in process with the arguments `[what-changes]? <captures...> <args...>`: `what-changes` first for that command (Node.js also puts `check` first for the other), then each capture as the path `migration-check/<capture>.jsonl` next to this file (a capture without a file, such as `does-not-exist`, is a missing file), then `args` unchanged. It compares the exit code, and stdout exactly unless `stdout` is `null` (`--help`, whose usage names the SDK's own command). stderr is not compared. Every checker uses its bundled schemas: `spec/ai-events-v0.3.schema.json` and `spec/ai-client-events-v0.2.schema.json`.

The captures come from the `rudder-ai-events-verify` skill's tested fixtures and from real recordings of the three SDKs (`go_recorder`, `node_recorder`, `python_recorder`); `formatting`, `mcp`, `duplicates`, `wc_edge_*` and the error captures cover the output rules below. The Python checker generates `stdout` and `exit_code`; regenerate them after an intended change and review the diff.

## Rules every checker follows

### Input

- A capture is UTF-8 JSONL. Lines split on `\r\n`, `\r` or `\n`; a line holding only spaces, tabs, `\f` or `\v` is skipped. Any other line must be one JSON object (no `NaN` or `Infinity`), else exit 2 with `error: <path>:<line>: not valid JSON` or `... not a JSON object` on stderr. An unreadable file: `error: <path>: cannot read file`.
- Numbers keep their JSON kind: a literal without `.`, `e` or `E` is an integer (exact; over 4,300 digits is not valid JSON, as in Python), any other is a 64-bit float. Object keys keep their order; a repeated key keeps its first position and its last value.
- A track call is a message whose `type` is `track` or absent and whose `event` is a non-empty string. A field that should be an object (`properties`, `context`, `context.wrapperLibrary`, `action_details`) and is not reads as an empty object. `interaction_id`, `conversation_id` and `parent_interaction_id` join only when they are non-empty strings. `argument_keys` counts only as a list.
- "Set" means truthy as in Python: not `null`, `false`, `0`, `0.0`, `""`, `[]` or `{}`.

### Arguments

`check <events.jsonl> [flags]` and `what-changes <before.jsonl> <after.jsonl>`. Flags take their value as the next argument or after `=`; options and the positional may come in any order; `--` ends the options. `--help` or `-h` anywhere prints the usage and exits 0. Exit 2 (usage on stderr) for an unknown flag, a missing value, a value on a boolean flag, a wrong positional count, or a `--client-opt-out` other than `client_prompt`, `client_response`, `client_feedback` and `server_feedback`. `--spec` and `--client-schema` replace the bundled schemas; one that is unreadable, not JSON, not a valid draft 2020-12 schema (an invalid `pattern`, say), or not an object with a string `$id` and an object `$defs` exits 2 with `error: <path>: cannot read schema`. A schema without `$schema` is read as draft 2020-12. List flags (`--expect-legacy`, `--absent`, `--feature`, `--surface`, `--client-surface`, `--client-opt-out`, `--feedback-endpoint`, `--failed-interaction`, `--mcp-server`, `--mcp-intent`) repeat once per value.

### Printed values

Values in `check` lines are printed as Python's `repr` (`str` where the line interpolates a plain value: IDs, task names): `'text'` (double quotes when the text holds `'` and no `"`; `\\`, the quote, `\t`, `\n`, `\r` escaped; other non-printable characters, meaning not a letter, mark, number, punctuation, symbol or the ASCII space, as `\xhh`, `\uhhhh` or `\Uhhhhhhhh`), `None`, `True`, `False`, integers in full, floats as the shortest round-trip digits (`0.5`, `2.0`, `1e-07`, `1e+16`: exponent form below 1e-4 or from 1e16, two exponent digits at least), `[a, b]`, `{'k': v}` in key order, `('a', None)`.

`what-changes` prints JSON as Python's `json.dumps(..., sort_keys=True)`: keys sorted by code point, `", "` and `": "` separators (`indent=2` for the before/after pairs, with `[]` and `{}` for empty containers), floats as above but `Infinity`/`NaN` for non-finite, and every character outside printable ASCII escaped (`\n`, `\"`, `\\`, `\t`, `\r`, `\b`, `\f`, else `\u00e9`; above U+FFFF as a lowercase surrogate pair). Type names: `str`, `int`, `float`, `bool`, `NoneType`, `dict`, `list`. Field paths join keys with `.`; a `\` or `.` inside a key is escaped as `\\` or `\.`. Sorting is by code point everywhere.

### Schema errors

Each failed keyword prints one line, `#<n> <event> (spec): <location>: <message>` (or `(client schema)`). Lines are distinct and sorted per event and schema, never echo the captured value, and are worded by the checker, not its validator. Only leaf errors count: `allOf`, `$ref`, `properties`, `items`, `dependentSchemas` and `if`/`then`/`else` report their failing children; `anyOf`, `oneOf` and `not` are one error each. `<location>` is `properties` followed by `.key` or `[index]` per step.

| Keyword | Message |
| --- | --- |
| `required` | one line per missing property, in schema order: `missing required 'name'` |
| `additionalProperties` (false) | one line per property in neither `properties` nor `patternProperties`: `unexpected 'name'` |
| `type` | `not of type integer` (several: `string or null`, in schema order) |
| `enum`, `const` | `not one of [...]`, `not 'value'` (schema value as `repr`) |
| `minLength`, `maxLength`, `minimum`, `maximum` | `shorter than 1`, `longer than 1`, `less than 0`, `greater than 0` (lengths in code points) |
| `pattern` | `does not match '<pattern>'` |
| `uniqueItems` | `items not unique` |
| `not`, `anyOf`, `oneOf` | `matches a schema it must not`, `matches none of anyOf`, `does not match exactly one of oneOf` |
| false schema, other | `not allowed`, `fails <keyword>` |
