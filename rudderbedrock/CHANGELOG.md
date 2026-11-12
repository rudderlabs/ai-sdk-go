# Changelog

## [0.5.0](https://github.com/rudderlabs/ai-sdk-go/compare/rudderbedrock/v0.4.0...rudderbedrock/v0.5.0) (2026-11-12)


### Features

* **mcp:** add OAuth client and remembered client name to MCP events ([#53](https://github.com/rudderlabs/ai-sdk-go/issues/53)) ([dbe17bc](https://github.com/rudderlabs/ai-sdk-go/commit/dbe17bc8153c4ad10c97a12f5871bdc1bf5f2cc7))


### Miscellaneous

* **release:** bump integration core requirements with each core release ([#60](https://github.com/rudderlabs/ai-sdk-go/issues/60)) ([e5045b8](https://github.com/rudderlabs/ai-sdk-go/commit/e5045b898d09d8ab068d28331e786245adf320ec))

## [0.4.0](https://github.com/rudderlabs/ai-sdk-go/compare/rudderbedrock/v0.3.2...rudderbedrock/v0.4.0) (2026-11-05)


### ⚠ BREAKING CHANGES

* fixes from the production audit of AI events ([#47](https://github.com/rudderlabs/ai-sdk-go/issues/47))

### Bug Fixes

* fixes from the production audit of AI events ([#47](https://github.com/rudderlabs/ai-sdk-go/issues/47)) ([de9ad71](https://github.com/rudderlabs/ai-sdk-go/commit/de9ad71e6531d0c3c6bbb4cfbc79c46e3411d582))

## [0.3.2](https://github.com/rudderlabs/ai-sdk-go/compare/rudderbedrock/v0.3.1...rudderbedrock/v0.3.2) (2026-11-05)


### Bug Fixes

* **deps:** require analytics-go v4.5.1 ([#45](https://github.com/rudderlabs/ai-sdk-go/issues/45)) ([b61362f](https://github.com/rudderlabs/ai-sdk-go/commit/b61362fe3ebe5a5d70b0bc441b5797172bd92ef7))

## [0.3.1](https://github.com/rudderlabs/ai-sdk-go/compare/rudderbedrock/v0.3.0...rudderbedrock/v0.3.1) (2026-11-04)


### Bug Fixes

* **deps:** require analytics-go v4.5.0 ([#41](https://github.com/rudderlabs/ai-sdk-go/issues/41)) ([d127413](https://github.com/rudderlabs/ai-sdk-go/commit/d1274137c03d7fce50d96325faf74c716becde8e))

## [0.3.0](https://github.com/rudderlabs/ai-sdk-go/compare/rudderbedrock/v0.2.1...rudderbedrock/v0.3.0) (2026-10-30)


### Features

* add v0.4 server fields from the customer audit ([#31](https://github.com/rudderlabs/ai-sdk-go/issues/31)) ([b5d1544](https://github.com/rudderlabs/ai-sdk-go/commit/b5d1544ba1505bc9cbd0181aaa986399ae5fabb4))

## [0.2.1](https://github.com/rudderlabs/ai-sdk-go/compare/rudderbedrock/v0.2.0...rudderbedrock/v0.2.1) (2026-10-27)


### Bug Fixes

* argument_keys set diff, stream usage, app_error, analytics-go 4.4 ([#26](https://github.com/rudderlabs/ai-sdk-go/issues/26)) ([ff4e7a4](https://github.com/rudderlabs/ai-sdk-go/commit/ff4e7a431bba72ccb6be192eb0c002c498b485d5))

## [0.2.0](https://github.com/rudderlabs/ai-sdk-go/compare/rudderbedrock/v0.1.0...rudderbedrock/v0.2.0) (2026-10-23)


### ⚠ BREAKING CHANGES

* add Flush(ctx) and Shutdown(ctx) with one shared deadline ([#18](https://github.com/rudderlabs/ai-sdk-go/issues/18))

### Features

* add Flush(ctx) and Shutdown(ctx) with one shared deadline ([#18](https://github.com/rudderlabs/ai-sdk-go/issues/18)) ([270a398](https://github.com/rudderlabs/ai-sdk-go/commit/270a3989b9b441777f8c1f7da3fde16aa3ed1434))


### Bug Fixes

* contain every SDK panic and report it through WithOnError ([#16](https://github.com/rudderlabs/ai-sdk-go/issues/16)) ([decd518](https://github.com/rudderlabs/ai-sdk-go/commit/decd51881b1e630bc807163a0b98ed0c10382e6b))
* make the integrations releasable with core v0.2.0 ([#20](https://github.com/rudderlabs/ai-sdk-go/issues/20)) ([76cb575](https://github.com/rudderlabs/ai-sdk-go/commit/76cb57528eb130b78ae3bcc9372db066c0dec438))
* price India-region Bedrock models and align MCP event_name fallback ([#14](https://github.com/rudderlabs/ai-sdk-go/issues/14)) ([3594546](https://github.com/rudderlabs/ai-sdk-go/commit/3594546f84858cedb4031c163625a4327bdfb608))


### Miscellaneous

* raise minimum Go version to 1.26.9 ([#12](https://github.com/rudderlabs/ai-sdk-go/issues/12)) ([ebe549b](https://github.com/rudderlabs/ai-sdk-go/commit/ebe549b50605da1e7896f2303a3c223c45ecd516))

## 0.1.0 (2026-10-19)


### Features

* add AI interaction events and provider integrations ([#2](https://github.com/rudderlabs/ai-sdk-go/issues/2)) ([ecbd5c6](https://github.com/rudderlabs/ai-sdk-go/commit/ecbd5c660272e4fef84f7ece64406f40d1877749))
