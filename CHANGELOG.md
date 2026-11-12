# Changelog

## Unreleased

- SDK-6798: send `call_index` on every `ai_llm_response_received` and, with `WithLLMCallEvents` and content capture, `input_messages`, `input_truncated`, `output_text` and `output_tool_calls` from the OpenAI, Anthropic, Google GenAI and Bedrock modules, `rudderotel` product events and `RecordCall` (`LLMCall.InputMessages`, `OutputToolCalls`). Content is masked like `prompt_text` and trimmed to the 30,000-byte event cap. Pause snapshots carry an optional `call_sequence`.

- SDK-6791: keep MCP tool, prompt, capability and argument names tied to the server's current registry (mcp-go task and session tools included), allow only `YYYY-MM-DD` protocol versions, encode request IDs uniformly (decimal integers, hashed strings, `unknown` otherwise), clean declared tool event properties and client labels, and key per-caller state, pending elicitations (one-hour expiry) and client windows by raw account, user and session or interaction in both Go MCP wrappers.

- Release AI events spec v0.4.0 with the v0.4 server schema and conformance fixture; the migration checker accepts 0.3.0 and 0.4.0 captures. The client schema remains v0.2.

- SDK-5991: support repeated pauses and persisted resumes on top of the client, server-field and sampling stack. Preserve sampling decisions, server scope, confidence and pending call credits across JSON restore; count compactions per response segment.
- SDK-5992: add per-event sampling with `WithSampleRates`, a stable interaction ID draw and `sample_rate` on kept events.
- SDK-5990: add server fields, credit pricing, declined answers, denied tools, confidence, versions, per-call detail, compactions and app-sent limit events.
- SDK-5989: add `OneShot: true` on interactions and tasks, using the interaction ID as conversation ID when no thread exists. Existing chat defaults retain a separate generated ID.
- SDK-5989: extend the shared client schemas, documentation and checker fixtures for web/mobile prompts, actionable suggestions, scoped feedback, exports, conversation/agent controls, limits and declared client tool events.

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
