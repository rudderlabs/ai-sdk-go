# Changelog

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
