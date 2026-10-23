# Changelog

## [0.2.0](https://github.com/rudderlabs/ai-sdk-go/compare/ruddergenai/v0.1.0...ruddergenai/v0.2.0) (2026-10-23)


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

* **anthropic,mcp:** send requested_model and pin declared MCP events ([#10](https://github.com/rudderlabs/ai-sdk-go/issues/10)) ([2538907](https://github.com/rudderlabs/ai-sdk-go/commit/2538907b23555abb84ea2983813b6fc9c15ce051))
* apply the cross-SDK parity round-up fixes ([#9](https://github.com/rudderlabs/ai-sdk-go/issues/9)) ([87a76fd](https://github.com/rudderlabs/ai-sdk-go/commit/87a76fd3252458d503398b56f383ed5ad8861dfc))
