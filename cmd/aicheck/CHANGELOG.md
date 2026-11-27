# Changelog

## [0.8.1](https://github.com/rudderlabs/ai-sdk-go/compare/cmd/aicheck/v0.8.0...cmd/aicheck/v0.8.1) (2026-11-27)


### Bug Fixes

* **mcp:** drop non-UUID IDs, mask client info, check versions [SDK-6943] ([#109](https://github.com/rudderlabs/ai-sdk-go/issues/109)) ([8e669c6](https://github.com/rudderlabs/ai-sdk-go/commit/8e669c65da9433177e0e4fa52b5db90d79028d2f))
* **mcp:** send client session IDs only when ID-shaped [SDK-7218] ([#121](https://github.com/rudderlabs/ai-sdk-go/issues/121)) ([436c18c](https://github.com/rudderlabs/ai-sdk-go/commit/436c18cab3fccb6c75758bcd4091cad585fa736e))


### Miscellaneous

* **deps:** bump golang.org/x/text from 0.42.0 to 0.44.0 in the gomod-non-major group across 1 directory ([#126](https://github.com/rudderlabs/ai-sdk-go/issues/126)) ([a87341c](https://github.com/rudderlabs/ai-sdk-go/commit/a87341c08745e1550838cb6af341be59ed37ab7d))
* drop tests and rows the shared fixtures already run [SDK-7032] ([#108](https://github.com/rudderlabs/ai-sdk-go/issues/108)) ([6122dbd](https://github.com/rudderlabs/ai-sdk-go/commit/6122dbd5cbf2ece46d980774fb5c03653ed6f3b7))
* remove unused code and simplify docs [SDK-7231] ([#114](https://github.com/rudderlabs/ai-sdk-go/issues/114)) ([f3864d5](https://github.com/rudderlabs/ai-sdk-go/commit/f3864d5b205640236615fd36b646debe4843493a))

## [0.8.0](https://github.com/rudderlabs/ai-sdk-go/compare/cmd/aicheck/v0.7.0...cmd/aicheck/v0.8.0) (2026-11-18)


### Features

* **mcp:** record the calling model on mcp_tool_called ([#87](https://github.com/rudderlabs/ai-sdk-go/issues/87)) ([4e8b59a](https://github.com/rudderlabs/ai-sdk-go/commit/4e8b59a151694ac1d8bd0730b87bcf1b82e7bec4))


### Bug Fixes

* **mcp:** read _meta.traceparent by the W3C rule ([#95](https://github.com/rudderlabs/ai-sdk-go/issues/95)) ([ed6d00d](https://github.com/rudderlabs/ai-sdk-go/commit/ed6d00d694e9f1daf4efb01879afd07d369ff47d))

## [0.7.0](https://github.com/rudderlabs/ai-sdk-go/compare/cmd/aicheck/v0.6.0...cmd/aicheck/v0.7.0) (2026-11-16)


### Features

* accept a per-feature content capture policy ([#83](https://github.com/rudderlabs/ai-sdk-go/issues/83)) ([511489d](https://github.com/rudderlabs/ai-sdk-go/commit/511489d3752301ecca2c5059d112d99159239b31))
* **mcp:** add the error_message spec and guards ahead of capture ([#74](https://github.com/rudderlabs/ai-sdk-go/issues/74)) ([0c9d637](https://github.com/rudderlabs/ai-sdk-go/commit/0c9d63744f82cd3d758878a93378f0990bd6cfbd))
* **mcp:** name HTTP clients from the User-Agent when no clientInfo is found ([#73](https://github.com/rudderlabs/ai-sdk-go/issues/73)) ([4e85149](https://github.com/rudderlabs/ai-sdk-go/commit/4e8514913078481d7c600f28e019888c68d9296f))

## [0.6.0](https://github.com/rudderlabs/ai-sdk-go/compare/cmd/aicheck/v0.5.0...cmd/aicheck/v0.6.0) (2026-11-12)


### Features

* add TrackClassification for app-sent classifications ([#58](https://github.com/rudderlabs/ai-sdk-go/issues/58)) ([6af69de](https://github.com/rudderlabs/ai-sdk-go/commit/6af69de122b9064760a6ec82817a10fe758293a4))
* **mcp:** add OAuth client and remembered client name to MCP events ([#53](https://github.com/rudderlabs/ai-sdk-go/issues/53)) ([dbe17bc](https://github.com/rudderlabs/ai-sdk-go/commit/dbe17bc8153c4ad10c97a12f5871bdc1bf5f2cc7))
* **mcp:** add opt-in submit_feedback tool and deprecate request_capability ([#64](https://github.com/rudderlabs/ai-sdk-go/issues/64)) ([79ac495](https://github.com/rudderlabs/ai-sdk-go/commit/79ac495dda7937fa9228cbe8961348b06010a73e))
* **spec:** add per-call content and wrapper span contract ([#56](https://github.com/rudderlabs/ai-sdk-go/issues/56)) ([48bbbf2](https://github.com/rudderlabs/ai-sdk-go/commit/48bbbf2b7bd4a4ca2b7c5ac146080a98d6ca516f))


### Bug Fixes

* **mcp:** keep client-controlled text out of MCP events ([#59](https://github.com/rudderlabs/ai-sdk-go/issues/59)) ([41e40a3](https://github.com/rudderlabs/ai-sdk-go/commit/41e40a3cfee0a9931bb1aaa6d5085b81d16b3371))

## [0.5.0](https://github.com/rudderlabs/ai-sdk-go/compare/cmd/aicheck/v0.4.0...cmd/aicheck/v0.5.0) (2026-11-05)


### ⚠ BREAKING CHANGES

* fixes from the production audit of AI events ([#47](https://github.com/rudderlabs/ai-sdk-go/issues/47))

### Bug Fixes

* fixes from the production audit of AI events ([#47](https://github.com/rudderlabs/ai-sdk-go/issues/47)) ([de9ad71](https://github.com/rudderlabs/ai-sdk-go/commit/de9ad71e6531d0c3c6bbb4cfbc79c46e3411d582))

## [0.4.0](https://github.com/rudderlabs/ai-sdk-go/compare/cmd/aicheck/v0.3.0...cmd/aicheck/v0.4.0) (2026-11-05)


### ⚠ BREAKING CHANGES

* **mcp:** turn request_context off by default ([#43](https://github.com/rudderlabs/ai-sdk-go/issues/43))

### Features

* **mcp:** turn request_context off by default ([#43](https://github.com/rudderlabs/ai-sdk-go/issues/43)) ([4f2f6d4](https://github.com/rudderlabs/ai-sdk-go/commit/4f2f6d4d27fedffd2b716e0655ee6926b76a523f))

## [0.3.0](https://github.com/rudderlabs/ai-sdk-go/compare/cmd/aicheck/v0.2.0...cmd/aicheck/v0.3.0) (2026-11-04)


### Features

* **mcp:** fill event gaps on stateless servers ([#40](https://github.com/rudderlabs/ai-sdk-go/issues/40)) ([9b94bba](https://github.com/rudderlabs/ai-sdk-go/commit/9b94bba535a24c5253b73f643255acdfce3e070e))

## [0.2.0](https://github.com/rudderlabs/ai-sdk-go/compare/cmd/aicheck/v0.1.0...cmd/aicheck/v0.2.0) (2026-10-30)


### Features

* add one-shot interactions and v0.4 client events ([#30](https://github.com/rudderlabs/ai-sdk-go/issues/30)) ([f1e7308](https://github.com/rudderlabs/ai-sdk-go/commit/f1e7308cfdb8788dd89d6675a5079878e7cf3527))
* add per-event sampling with sample_rate ([#32](https://github.com/rudderlabs/ai-sdk-go/issues/32)) ([b775943](https://github.com/rudderlabs/ai-sdk-go/commit/b7759438f43f2419d9b1e193e95bd95d07b9a3e0))
* add v0.4 server fields from the customer audit ([#31](https://github.com/rudderlabs/ai-sdk-go/issues/31)) ([b5d1544](https://github.com/rudderlabs/ai-sdk-go/commit/b5d1544ba1505bc9cbd0181aaa986399ae5fabb4))
* **spec:** release AI events spec v0.4.0 ([#34](https://github.com/rudderlabs/ai-sdk-go/issues/34)) ([d0e49b3](https://github.com/rudderlabs/ai-sdk-go/commit/d0e49b38e1697563c32050a74754d59da5ec3fab))
* support runs that pause and resume more than once ([#33](https://github.com/rudderlabs/ai-sdk-go/issues/33)) ([6501ff4](https://github.com/rudderlabs/ai-sdk-go/commit/6501ff471cd8b9515d74d74cb5c5b748180e9388))

## 0.1.0 (2026-10-27)


### Features

* add the aicheck migration check and a recording client ([#23](https://github.com/rudderlabs/ai-sdk-go/issues/23)) ([ff14ec6](https://github.com/rudderlabs/ai-sdk-go/commit/ff14ec6fe3115eb34d04eec7ffa6a048ceb9e698))


### Bug Fixes

* argument_keys set diff, stream usage, app_error, analytics-go 4.4 ([#26](https://github.com/rudderlabs/ai-sdk-go/issues/26)) ([ff4e7a4](https://github.com/rudderlabs/ai-sdk-go/commit/ff4e7a431bba72ccb6be192eb0c002c498b485d5))
