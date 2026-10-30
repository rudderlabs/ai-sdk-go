# Changelog

## [0.3.0](https://github.com/rudderlabs/ai-sdk-go/compare/rudderopenai/v0.2.1...rudderopenai/v0.3.0) (2026-10-30)


### Features

* add v0.4 server fields from the customer audit ([#31](https://github.com/rudderlabs/ai-sdk-go/issues/31)) ([b5d1544](https://github.com/rudderlabs/ai-sdk-go/commit/b5d1544ba1505bc9cbd0181aaa986399ae5fabb4))

## [0.2.1](https://github.com/rudderlabs/ai-sdk-go/compare/rudderopenai/v0.2.0...rudderopenai/v0.2.1) (2026-10-27)


### Bug Fixes

* argument_keys set diff, stream usage, app_error, analytics-go 4.4 ([#26](https://github.com/rudderlabs/ai-sdk-go/issues/26)) ([ff4e7a4](https://github.com/rudderlabs/ai-sdk-go/commit/ff4e7a431bba72ccb6be192eb0c002c498b485d5))

## [0.2.0](https://github.com/rudderlabs/ai-sdk-go/compare/rudderopenai/v0.1.0...rudderopenai/v0.2.0) (2026-10-23)


### ⚠ BREAKING CHANGES

* add Flush(ctx) and Shutdown(ctx) with one shared deadline ([#18](https://github.com/rudderlabs/ai-sdk-go/issues/18))

### Features

* add Flush(ctx) and Shutdown(ctx) with one shared deadline ([#18](https://github.com/rudderlabs/ai-sdk-go/issues/18)) ([270a398](https://github.com/rudderlabs/ai-sdk-go/commit/270a3989b9b441777f8c1f7da3fde16aa3ed1434))


### Bug Fixes

* make the integrations releasable with core v0.2.0 ([#20](https://github.com/rudderlabs/ai-sdk-go/issues/20)) ([76cb575](https://github.com/rudderlabs/ai-sdk-go/commit/76cb57528eb130b78ae3bcc9372db066c0dec438))
* price India-region Bedrock models and align MCP event_name fallback ([#14](https://github.com/rudderlabs/ai-sdk-go/issues/14)) ([3594546](https://github.com/rudderlabs/ai-sdk-go/commit/3594546f84858cedb4031c163625a4327bdfb608))


### Miscellaneous

* raise minimum Go version to 1.26.9 ([#12](https://github.com/rudderlabs/ai-sdk-go/issues/12)) ([ebe549b](https://github.com/rudderlabs/ai-sdk-go/commit/ebe549b50605da1e7896f2303a3c223c45ecd516))

## 0.1.0 (2026-10-19)


### ⚠ BREAKING CHANGES

* apply the cross-SDK parity round-up fixes ([#9](https://github.com/rudderlabs/ai-sdk-go/issues/9))

### Features

* add AI interaction events and provider integrations ([#2](https://github.com/rudderlabs/ai-sdk-go/issues/2)) ([ecbd5c6](https://github.com/rudderlabs/ai-sdk-go/commit/ecbd5c660272e4fef84f7ece64406f40d1877749))


### Bug Fixes

* apply the cross-SDK parity round-up fixes ([#9](https://github.com/rudderlabs/ai-sdk-go/issues/9)) ([87a76fd](https://github.com/rudderlabs/ai-sdk-go/commit/87a76fd3252458d503398b56f383ed5ad8861dfc))
