# Changelog

## Unreleased

- SDK-7218: MCP events send `mcp_session_id` only when the server transport issued the ID (whatever its shape: go-sdk and mcp-go IDs are not UUIDs) or, when it issued none, as on stateless HTTP, when the client's `Mcp-Session-Id` header is a UUID or 32 hex digits; any other header is not sent, so a client can no longer put a name or email there. A rejected header still scopes `call_index`, `ms_since_previous_call` and `retry_count` in memory. With a custom mcp-go `WithSessionIdManager`, later requests send the ID only when it is a UUID or 32 hex digits.

- SDK-6943: MCP events drop a client-supplied `_meta` `interaction_id` or `trace_id` that is not a UUID or 32 hex digits (the server mints its own `interaction_id`; send a UUID or 32-hex ID to join a client's interaction to the server's events), mask `client_name` and `client_title` with the built-in rules, send `caller_model` as `unknown` when the built-in rules would change it, withhold `client_name`, `client_title`, `error_message`, `call_reason`, `capability_detail` and `feedback_text` when their cut leaves a pattern, withhold `call_reason`, `capability_detail` and `feedback_text` that still match a built-in rule even with redaction off, and omit a `client_version` that does not match the version pattern or that the built-in masking would change (an IP, SSN, card number or phone). A declared-event rule can no longer set `client_name` or `client_version` in any spelling; only the request's own are sent.

- SDK-6798: send `call_index` on every `ai_llm_response_received` and, with `WithLLMCallEvents` and content capture, `input_messages`, `input_truncated`, `output_text` and `output_tool_calls` from the OpenAI, Anthropic, Google GenAI and Bedrock modules, `rudderotel` product events and `RecordCall` (`LLMCall.InputMessages`, `OutputToolCalls`). Content is masked like `prompt_text` and trimmed to the 30,000-byte event cap. Pause snapshots carry an optional `call_sequence`.

- SDK-6791: keep MCP tool, prompt, capability and argument names tied to the server's current registry (mcp-go task and session tools included), allow only `YYYY-MM-DD` protocol versions, encode request IDs uniformly (decimal integers, hashed strings, `unknown` otherwise), clean declared tool event properties and client labels, and key per-caller state, pending elicitations (one-hour expiry) and client windows by raw account, user and session or interaction in both Go MCP wrappers.

- Release AI events spec v0.4.0 with the v0.4 server schema and conformance fixture; the migration checker accepts 0.3.0 and 0.4.0 captures. The client schema remains v0.2.

- SDK-5991: support repeated pauses and persisted resumes on top of the client, server-field and sampling stack. Preserve sampling decisions, server scope, confidence and pending call credits across JSON restore; count compactions per response segment.
- SDK-5992: add per-event sampling with `WithSampleRates`, a stable interaction ID draw and `sample_rate` on kept events.
- SDK-5990: add server fields, credit pricing, declined answers, denied tools, confidence, versions, per-call detail, compactions and app-sent limit events.
- SDK-5989: add `OneShot: true` on interactions and tasks, using the interaction ID as conversation ID when no thread exists. Existing chat defaults retain a separate generated ID.
- SDK-5989: extend the shared client schemas, documentation and checker fixtures for web/mobile prompts, actionable suggestions, scoped feedback, exports, conversation/agent controls, limits and declared client tool events.

## [0.15.0](https://github.com/rudderlabs/ai-sdk-go/compare/v0.10.0...v0.15.0) (2026-11-18)


### Features

* **mcp:** name Claude-User, Codex, Copilot CLI and Devin from the User-Agent ([#90](https://github.com/rudderlabs/ai-sdk-go/issues/90)) ([fc1e4a7](https://github.com/rudderlabs/ai-sdk-go/commit/fc1e4a7f078e56b6504f64e09127b2f6560af252))
* **mcp:** record the calling model on mcp_tool_called ([#87](https://github.com/rudderlabs/ai-sdk-go/issues/87)) ([4e8b59a](https://github.com/rudderlabs/ai-sdk-go/commit/4e8b59a151694ac1d8bd0730b87bcf1b82e7bec4))
* send one span per call with EmitSpans and the span processor ([#89](https://github.com/rudderlabs/ai-sdk-go/issues/89)) ([cb8abde](https://github.com/rudderlabs/ai-sdk-go/commit/cb8abde8f94ee771ba94063a750e9b59b944c59a))


### Bug Fixes

* **core:** honour marshalers when capturing content ([#104](https://github.com/rudderlabs/ai-sdk-go/issues/104)) ([b83922e](https://github.com/rudderlabs/ai-sdk-go/commit/b83922ee180d48fd2542d0d2612b6ad792c4a595))
* correct pause, overflow, twin and snapshot call accounting ([d0013cd](https://github.com/rudderlabs/ai-sdk-go/commit/d0013cd1dc1fb375efdf8b7ddba499ed533402ef))
* count a tool a pause waits on once, in the resumed segment ([#96](https://github.com/rudderlabs/ai-sdk-go/issues/96)) ([b6a46f2](https://github.com/rudderlabs/ai-sdk-go/commit/b6a46f2861912ef93a4e800235fa221275dec56b))
* **mcp:** read _meta.traceparent by the W3C rule ([#95](https://github.com/rudderlabs/ai-sdk-go/issues/95)) ([ed6d00d](https://github.com/rudderlabs/ai-sdk-go/commit/ed6d00d694e9f1daf4efb01879afd07d369ff47d))
* **otel:** keep a product span's owner from span start ([#103](https://github.com/rudderlabs/ai-sdk-go/issues/103)) ([549da20](https://github.com/rudderlabs/ai-sdk-go/commit/549da20c2e5c6ea546d159ac34b767e4b0b169e4))
* **rudderotel:** don't bill or mark successful a caller-cancelled Anthropic request or stream ([d0013cd](https://github.com/rudderlabs/ai-sdk-go/commit/d0013cd1dc1fb375efdf8b7ddba499ed533402ef))


### Miscellaneous

* add overhead benchmarks, a load test and fuzz tests ([#94](https://github.com/rudderlabs/ai-sdk-go/issues/94)) ([678a66c](https://github.com/rudderlabs/ai-sdk-go/commit/678a66c82f72fdc1936e613715b40b752f61a6f2))
* **deps:** bump step-security/harden-runner from 2.22.3 to 2.22.4 in the github-actions-version group across 1 directory ([#68](https://github.com/rudderlabs/ai-sdk-go/issues/68)) ([0730d0d](https://github.com/rudderlabs/ai-sdk-go/commit/0730d0d0c8777736dbab8e77cfb7e57f5d0c75a2))
* **mcp:** check error_message is omitted when the response write fails ([#100](https://github.com/rudderlabs/ai-sdk-go/issues/100)) ([15ece67](https://github.com/rudderlabs/ai-sdk-go/commit/15ece671c347c995404af9186c957363f2242947))
* **spec:** copy the Google ADK AgentTool cassette and changelog entry ([#93](https://github.com/rudderlabs/ai-sdk-go/issues/93)) ([4f56171](https://github.com/rudderlabs/ai-sdk-go/commit/4f561715aee98f13105cf3539b35fefda6991ef6))

## [0.10.0](https://github.com/rudderlabs/ai-sdk-go/compare/v0.9.0...v0.10.0) (2026-11-16)


### Features

* accept a per-feature content capture policy ([#83](https://github.com/rudderlabs/ai-sdk-go/issues/83)) ([511489d](https://github.com/rudderlabs/ai-sdk-go/commit/511489d3752301ecca2c5059d112d99159239b31))
* **mcp:** add the error_message spec and guards ahead of capture ([#74](https://github.com/rudderlabs/ai-sdk-go/issues/74)) ([0c9d637](https://github.com/rudderlabs/ai-sdk-go/commit/0c9d63744f82cd3d758878a93378f0990bd6cfbd))
* **mcp:** name HTTP clients from the User-Agent when no clientInfo is found ([#73](https://github.com/rudderlabs/ai-sdk-go/issues/73)) ([4e85149](https://github.com/rudderlabs/ai-sdk-go/commit/4e8514913078481d7c600f28e019888c68d9296f))
* **mcp:** send error_message from the observed final response ([#80](https://github.com/rudderlabs/ai-sdk-go/issues/80)) ([f0b260d](https://github.com/rudderlabs/ai-sdk-go/commit/f0b260d3a94df8273eeee1814d90d6b49e84f16d))


### Bug Fixes

* count a forced schema tool with no result as structured output ([#85](https://github.com/rudderlabs/ai-sdk-go/issues/85)) ([f6c062a](https://github.com/rudderlabs/ai-sdk-go/commit/f6c062a091778364f31973732f54734ec2dbcfc1))
* count a model request once when spans nest or LlamaIndex adds a prep span ([#82](https://github.com/rudderlabs/ai-sdk-go/issues/82)) ([50bdf88](https://github.com/rudderlabs/ai-sdk-go/commit/50bdf880309a22a9e0315652a78a36b965a4e090))
* map cache writes, provider IDs, finish reasons, Traceloop tools and Mirascope usage ([#81](https://github.com/rudderlabs/ai-sdk-go/issues/81)) ([a7e2cee](https://github.com/rudderlabs/ai-sdk-go/commit/a7e2cee7d7be564c6773f52449f34986b023d974))
* treat Pydantic AI, Mirascope and Eve message keys as span content ([#79](https://github.com/rudderlabs/ai-sdk-go/issues/79)) ([f82840f](https://github.com/rudderlabs/ai-sdk-go/commit/f82840feed63a74be753ae2370ac454455f22344))


### Miscellaneous

* cover goroutine span parents and integration-span platform limits ([#75](https://github.com/rudderlabs/ai-sdk-go/issues/75)) ([e925996](https://github.com/rudderlabs/ai-sdk-go/commit/e9259964d6eee0a866a61ca9be09fda403f919dc))
* sync spec/ with the Node.js capture policy fixtures ([#84](https://github.com/rudderlabs/ai-sdk-go/issues/84)) ([89607ed](https://github.com/rudderlabs/ai-sdk-go/commit/89607edde5cae28adc5851e3a7c169521d66c8ba))


### Documentation

* **mcp:** add auth provider recipe for identifying users ([#76](https://github.com/rudderlabs/ai-sdk-go/issues/76)) ([b886c39](https://github.com/rudderlabs/ai-sdk-go/commit/b886c39edbf50458cd8a3f8f922ee1f4aacf65da))
* note that Mastra is Node.js only ([#78](https://github.com/rudderlabs/ai-sdk-go/issues/78)) ([fa5d306](https://github.com/rudderlabs/ai-sdk-go/commit/fa5d306acc79ad494e2b9093c3e9703977447d1e))

## [0.9.0](https://github.com/rudderlabs/ai-sdk-go/compare/v0.8.0...v0.9.0) (2026-11-13)


### Features

* add AcceptOutput so an accepted token cap does not decide the outcome ([#66](https://github.com/rudderlabs/ai-sdk-go/issues/66)) ([2e25ad9](https://github.com/rudderlabs/ai-sdk-go/commit/2e25ad9a2f11e742fb177a06a327793609678864))
* add per-call input and output text to ai_llm_response_received ([#65](https://github.com/rudderlabs/ai-sdk-go/issues/65)) ([0294667](https://github.com/rudderlabs/ai-sdk-go/commit/029466731c2bfe5e1da6d75a7e34d8f8ffce2f23))
* emit wrapper and manual spans with WithEmitSpans ([#72](https://github.com/rudderlabs/ai-sdk-go/issues/72)) ([9b6e3c7](https://github.com/rudderlabs/ai-sdk-go/commit/9b6e3c785d0db8e651cd6a63a9173433e55ddb5d))
* **mcp:** add ClientInfoFallback, resolved after remembered clients ([#70](https://github.com/rudderlabs/ai-sdk-go/issues/70)) ([a5132d5](https://github.com/rudderlabs/ai-sdk-go/commit/a5132d5eea5b3df5a73120600f5c30b664dbf5fa))


### Bug Fixes

* **mcp:** trim intents and capability labels like Node and Python ([#71](https://github.com/rudderlabs/ai-sdk-go/issues/71)) ([2e7415c](https://github.com/rudderlabs/ai-sdk-go/commit/2e7415c4ad93b35d62b1dc4e64d272578a85ca81))

## [0.8.0](https://github.com/rudderlabs/ai-sdk-go/compare/v0.7.0...v0.8.0) (2026-11-12)


### Features

* add TrackClassification for app-sent classifications ([#58](https://github.com/rudderlabs/ai-sdk-go/issues/58)) ([6af69de](https://github.com/rudderlabs/ai-sdk-go/commit/6af69de122b9064760a6ec82817a10fe758293a4))
* **mcp:** add OAuth client and remembered client name to MCP events ([#53](https://github.com/rudderlabs/ai-sdk-go/issues/53)) ([dbe17bc](https://github.com/rudderlabs/ai-sdk-go/commit/dbe17bc8153c4ad10c97a12f5871bdc1bf5f2cc7))
* **mcp:** add opt-in submit_feedback tool and deprecate request_capability ([#64](https://github.com/rudderlabs/ai-sdk-go/issues/64)) ([79ac495](https://github.com/rudderlabs/ai-sdk-go/commit/79ac495dda7937fa9228cbe8961348b06010a73e))
* **rudderotel:** consume OpenTelemetry spans for analytics ([#57](https://github.com/rudderlabs/ai-sdk-go/issues/57)) ([8c835ff](https://github.com/rudderlabs/ai-sdk-go/commit/8c835ffdacd0129cc22bd72f06996de7e26d8682))
* **spec:** add per-call content and wrapper span contract ([#56](https://github.com/rudderlabs/ai-sdk-go/issues/56)) ([48bbbf2](https://github.com/rudderlabs/ai-sdk-go/commit/48bbbf2b7bd4a4ca2b7c5ac146080a98d6ca516f))


### Bug Fixes

* **mcp:** give the request_capability tool a title and full annotations ([#51](https://github.com/rudderlabs/ai-sdk-go/issues/51)) ([846118d](https://github.com/rudderlabs/ai-sdk-go/commit/846118d7ac05e45dfe45756facaeebe8c1df7747))
* **mcp:** keep client-controlled text out of MCP events ([#59](https://github.com/rudderlabs/ai-sdk-go/issues/59)) ([41e40a3](https://github.com/rudderlabs/ai-sdk-go/commit/41e40a3cfee0a9931bb1aaa6d5085b81d16b3371))
* **rudderotel:** mask strings inside structured content attributes ([#63](https://github.com/rudderlabs/ai-sdk-go/issues/63)) ([db256b1](https://github.com/rudderlabs/ai-sdk-go/commit/db256b1f1b6aececd87701df9491212ff914c147))


### Miscellaneous

* **deps:** bump step-security/harden-runner from 2.22.2 to 2.22.3 in the github-actions-version group ([#49](https://github.com/rudderlabs/ai-sdk-go/issues/49)) ([b5ee6c7](https://github.com/rudderlabs/ai-sdk-go/commit/b5ee6c761dfa7cce675672c0c6108ad3b71b7d0a))
* **release:** bump core requirement of examples and conformance modules ([#61](https://github.com/rudderlabs/ai-sdk-go/issues/61)) ([abbb286](https://github.com/rudderlabs/ai-sdk-go/commit/abbb28636715ca77ab9cf50c94ee4c4181feb944))
* **release:** bump integration core requirements with each core release ([#60](https://github.com/rudderlabs/ai-sdk-go/issues/60)) ([e5045b8](https://github.com/rudderlabs/ai-sdk-go/commit/e5045b898d09d8ab068d28331e786245adf320ec))
* **rudderotel:** cover error text and attribute locations in tracing conformance ([#62](https://github.com/rudderlabs/ai-sdk-go/issues/62)) ([546dff0](https://github.com/rudderlabs/ai-sdk-go/commit/546dff02e69ecb1d91b58b2f3a5da43fb6948c0d))

## [0.7.0](https://github.com/rudderlabs/ai-sdk-go/compare/v0.6.0...v0.7.0) (2026-11-05)


### ⚠ BREAKING CHANGES

* fixes from the production audit of AI events ([#47](https://github.com/rudderlabs/ai-sdk-go/issues/47))

### Bug Fixes

* fixes from the production audit of AI events ([#47](https://github.com/rudderlabs/ai-sdk-go/issues/47)) ([de9ad71](https://github.com/rudderlabs/ai-sdk-go/commit/de9ad71e6531d0c3c6bbb4cfbc79c46e3411d582))

## [0.6.0](https://github.com/rudderlabs/ai-sdk-go/compare/v0.5.1...v0.6.0) (2026-11-05)


### ⚠ BREAKING CHANGES

* **mcp:** turn request_context off by default ([#43](https://github.com/rudderlabs/ai-sdk-go/issues/43))

### Features

* export MaskText for free text sent in tool event properties ([#46](https://github.com/rudderlabs/ai-sdk-go/issues/46)) ([f765dcf](https://github.com/rudderlabs/ai-sdk-go/commit/f765dcfd43371198ee1bf51e926b94729d488373))
* **mcp:** turn request_context off by default ([#43](https://github.com/rudderlabs/ai-sdk-go/issues/43)) ([4f2f6d4](https://github.com/rudderlabs/ai-sdk-go/commit/4f2f6d4d27fedffd2b716e0655ee6926b76a523f))


### Bug Fixes

* **deps:** require analytics-go v4.5.1 ([#45](https://github.com/rudderlabs/ai-sdk-go/issues/45)) ([b61362f](https://github.com/rudderlabs/ai-sdk-go/commit/b61362fe3ebe5a5d70b0bc441b5797172bd92ef7))

## [0.5.1](https://github.com/rudderlabs/ai-sdk-go/compare/v0.5.0...v0.5.1) (2026-11-04)


### Bug Fixes

* **deps:** require analytics-go v4.5.0 ([#41](https://github.com/rudderlabs/ai-sdk-go/issues/41)) ([d127413](https://github.com/rudderlabs/ai-sdk-go/commit/d1274137c03d7fce50d96325faf74c716becde8e))

## [0.5.0](https://github.com/rudderlabs/ai-sdk-go/compare/v0.4.0...v0.5.0) (2026-11-04)


### Features

* **catalog:** cover every SDK property and reuse shared properties ([#38](https://github.com/rudderlabs/ai-sdk-go/issues/38)) ([7531e7f](https://github.com/rudderlabs/ai-sdk-go/commit/7531e7fe8e9aac531a99b25e7c27067e098e4e9e))
* **mcp:** fill event gaps on stateless servers ([#40](https://github.com/rudderlabs/ai-sdk-go/issues/40)) ([9b94bba](https://github.com/rudderlabs/ai-sdk-go/commit/9b94bba535a24c5253b73f643255acdfce3e070e))


### Bug Fixes

* keep provider failures in RunTask and add bounded CloseClient ([#37](https://github.com/rudderlabs/ai-sdk-go/issues/37)) ([26c61dc](https://github.com/rudderlabs/ai-sdk-go/commit/26c61dcf4c31e4e010325bc5ba4ec0c3cec8bb5f))

## [0.4.0](https://github.com/rudderlabs/ai-sdk-go/compare/v0.3.0...v0.4.0) (2026-10-30)


### Features

* add one-shot interactions and v0.4 client events ([#30](https://github.com/rudderlabs/ai-sdk-go/issues/30)) ([f1e7308](https://github.com/rudderlabs/ai-sdk-go/commit/f1e7308cfdb8788dd89d6675a5079878e7cf3527))
* add per-event sampling with sample_rate ([#32](https://github.com/rudderlabs/ai-sdk-go/issues/32)) ([b775943](https://github.com/rudderlabs/ai-sdk-go/commit/b7759438f43f2419d9b1e193e95bd95d07b9a3e0))
* add v0.4 server fields from the customer audit ([#31](https://github.com/rudderlabs/ai-sdk-go/issues/31)) ([b5d1544](https://github.com/rudderlabs/ai-sdk-go/commit/b5d1544ba1505bc9cbd0181aaa986399ae5fabb4))
* **spec:** release AI events spec v0.4.0 ([#34](https://github.com/rudderlabs/ai-sdk-go/issues/34)) ([d0e49b3](https://github.com/rudderlabs/ai-sdk-go/commit/d0e49b38e1697563c32050a74754d59da5ec3fab))
* support runs that pause and resume more than once ([#33](https://github.com/rudderlabs/ai-sdk-go/issues/33)) ([6501ff4](https://github.com/rudderlabs/ai-sdk-go/commit/6501ff471cd8b9515d74d74cb5c5b748180e9388))


### Miscellaneous

* move the PR title check to check-pr-title v2.0.3 ([#28](https://github.com/rudderlabs/ai-sdk-go/issues/28)) ([f760b15](https://github.com/rudderlabs/ai-sdk-go/commit/f760b151791ee6f1a7b9062b1243fda19678f960))

## [0.3.0](https://github.com/rudderlabs/ai-sdk-go/compare/v0.2.0...v0.3.0) (2026-10-27)


### Features

* add the aicheck migration check and a recording client ([#23](https://github.com/rudderlabs/ai-sdk-go/issues/23)) ([ff14ec6](https://github.com/rudderlabs/ai-sdk-go/commit/ff14ec6fe3115eb34d04eec7ffa6a048ceb9e698))


### Bug Fixes

* argument_keys set diff, stream usage, app_error, analytics-go 4.4 ([#26](https://github.com/rudderlabs/ai-sdk-go/issues/26)) ([ff4e7a4](https://github.com/rudderlabs/ai-sdk-go/commit/ff4e7a431bba72ccb6be192eb0c002c498b485d5))
* **mcp:** read no _meta from typed-nil params ([#21](https://github.com/rudderlabs/ai-sdk-go/issues/21)) ([f490406](https://github.com/rudderlabs/ai-sdk-go/commit/f490406029e137502612d6bd7a990c2e0dc01ed6))
* pass the RudderStack client's flush errors to OnError ([#24](https://github.com/rudderlabs/ai-sdk-go/issues/24)) ([8f559d6](https://github.com/rudderlabs/ai-sdk-go/commit/8f559d6fda0effb1da2b84ba25eaa0cf676abf39))


### Miscellaneous

* **deps:** bump harden-runner from 2.22.1 to 2.22.2 ([#25](https://github.com/rudderlabs/ai-sdk-go/issues/25)) ([7763d69](https://github.com/rudderlabs/ai-sdk-go/commit/7763d698d27dbfb3519f92322d1fce9f0d31f655))

## [0.2.0](https://github.com/rudderlabs/ai-sdk-go/compare/v0.1.0...v0.2.0) (2026-10-23)


### ⚠ BREAKING CHANGES

* add Flush(ctx) and Shutdown(ctx) with one shared deadline ([#18](https://github.com/rudderlabs/ai-sdk-go/issues/18))

### Features

* add Flush(ctx) and Shutdown(ctx) with one shared deadline ([#18](https://github.com/rudderlabs/ai-sdk-go/issues/18)) ([270a398](https://github.com/rudderlabs/ai-sdk-go/commit/270a3989b9b441777f8c1f7da3fde16aa3ed1434))


### Bug Fixes

* align requested_model and name-list order with Node.js and Python ([#15](https://github.com/rudderlabs/ai-sdk-go/issues/15)) ([65b0c2b](https://github.com/rudderlabs/ai-sdk-go/commit/65b0c2b2cb23c1c03a8eaaf7516ac97344778f7e))
* bound SDK memory with WithLimits and send events outside locks ([#17](https://github.com/rudderlabs/ai-sdk-go/issues/17)) ([65a403a](https://github.com/rudderlabs/ai-sdk-go/commit/65a403ab0893ec6a6f31a96a39d9d09fcfdc1475))
* contain every SDK panic and report it through WithOnError ([#16](https://github.com/rudderlabs/ai-sdk-go/issues/16)) ([decd518](https://github.com/rudderlabs/ai-sdk-go/commit/decd51881b1e630bc807163a0b98ed0c10382e6b))
* make the integrations releasable with core v0.2.0 ([#20](https://github.com/rudderlabs/ai-sdk-go/issues/20)) ([76cb575](https://github.com/rudderlabs/ai-sdk-go/commit/76cb57528eb130b78ae3bcc9372db066c0dec438))
* price India-region Bedrock models and align MCP event_name fallback ([#14](https://github.com/rudderlabs/ai-sdk-go/issues/14)) ([3594546](https://github.com/rudderlabs/ai-sdk-go/commit/3594546f84858cedb4031c163625a4327bdfb608))


### Miscellaneous

* drop the 0.1.0 release-as pins ([#11](https://github.com/rudderlabs/ai-sdk-go/issues/11)) ([f5697f7](https://github.com/rudderlabs/ai-sdk-go/commit/f5697f76338592f7c31cdd78d9fe15b252b4f9b5))
* raise minimum Go version to 1.26.9 ([#12](https://github.com/rudderlabs/ai-sdk-go/issues/12)) ([ebe549b](https://github.com/rudderlabs/ai-sdk-go/commit/ebe549b50605da1e7896f2303a3c223c45ecd516))

## 0.1.0 (2026-10-19)


### ⚠ BREAKING CHANGES

* apply the cross-SDK parity round-up fixes ([#9](https://github.com/rudderlabs/ai-sdk-go/issues/9))

### Features

* add AI interaction events and provider integrations ([#2](https://github.com/rudderlabs/ai-sdk-go/issues/2)) ([ecbd5c6](https://github.com/rudderlabs/ai-sdk-go/commit/ecbd5c660272e4fef84f7ece64406f40d1877749))
* **mcp:** add MCP server events for go-sdk and mcp-go servers ([#1](https://github.com/rudderlabs/ai-sdk-go/issues/1)) ([8d5d9ea](https://github.com/rudderlabs/ai-sdk-go/commit/8d5d9eab1ce98011c98bb91cf139151f606ee467))


### Bug Fixes

* **anthropic,mcp:** send requested_model and pin declared MCP events ([#10](https://github.com/rudderlabs/ai-sdk-go/issues/10)) ([2538907](https://github.com/rudderlabs/ai-sdk-go/commit/2538907b23555abb84ea2983813b6fc9c15ce051))
* apply the cross-SDK parity round-up fixes ([#9](https://github.com/rudderlabs/ai-sdk-go/issues/9)) ([87a76fd](https://github.com/rudderlabs/ai-sdk-go/commit/87a76fd3252458d503398b56f383ed5ad8861dfc))


### Miscellaneous

* **deps:** bump actions/create-github-app-token from 3.2.0 to 3.3.0 ([#4](https://github.com/rudderlabs/ai-sdk-go/issues/4)) ([74a5efa](https://github.com/rudderlabs/ai-sdk-go/commit/74a5efa905e0459c15678509a388c5e79b287b5d))
* initialize repository ([b6e01c2](https://github.com/rudderlabs/ai-sdk-go/commit/b6e01c2a33d8e7c1467c695d694d1af9535c421d))
* **spec:** sync tracing fixture and FIXTURE-FORMAT from ai-sdk-node ([#8](https://github.com/rudderlabs/ai-sdk-go/issues/8)) ([9fe6cd8](https://github.com/rudderlabs/ai-sdk-go/commit/9fe6cd89e34c61a6bebacec7b6c9ad62e0261e9d))
