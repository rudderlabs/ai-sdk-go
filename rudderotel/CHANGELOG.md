# Changelog

## [0.3.0](https://github.com/rudderlabs/ai-sdk-go/compare/rudderotel/v0.2.0...rudderotel/v0.3.0) (2026-11-13)


### Features

* add per-call input and output text to ai_llm_response_received ([#65](https://github.com/rudderlabs/ai-sdk-go/issues/65)) ([0294667](https://github.com/rudderlabs/ai-sdk-go/commit/029466731c2bfe5e1da6d75a7e34d8f8ffce2f23))
* emit wrapper and manual spans with WithEmitSpans ([#72](https://github.com/rudderlabs/ai-sdk-go/issues/72)) ([9b6e3c7](https://github.com/rudderlabs/ai-sdk-go/commit/9b6e3c785d0db8e651cd6a63a9173433e55ddb5d))

## [0.2.0](https://github.com/rudderlabs/ai-sdk-go/compare/rudderotel/v0.1.1...rudderotel/v0.2.0) (2026-11-12)


### Features

* **mcp:** add OAuth client and remembered client name to MCP events ([#53](https://github.com/rudderlabs/ai-sdk-go/issues/53)) ([dbe17bc](https://github.com/rudderlabs/ai-sdk-go/commit/dbe17bc8153c4ad10c97a12f5871bdc1bf5f2cc7))
* **rudderotel:** consume OpenTelemetry spans for analytics ([#57](https://github.com/rudderlabs/ai-sdk-go/issues/57)) ([8c835ff](https://github.com/rudderlabs/ai-sdk-go/commit/8c835ffdacd0129cc22bd72f06996de7e26d8682))


### Bug Fixes

* **rudderotel:** mask strings inside structured content attributes ([#63](https://github.com/rudderlabs/ai-sdk-go/issues/63)) ([db256b1](https://github.com/rudderlabs/ai-sdk-go/commit/db256b1f1b6aececd87701df9491212ff914c147))


### Miscellaneous

* **release:** bump integration core requirements with each core release ([#60](https://github.com/rudderlabs/ai-sdk-go/issues/60)) ([e5045b8](https://github.com/rudderlabs/ai-sdk-go/commit/e5045b898d09d8ab068d28331e786245adf320ec))
* **rudderotel:** cover error text and attribute locations in tracing conformance ([#62](https://github.com/rudderlabs/ai-sdk-go/issues/62)) ([546dff0](https://github.com/rudderlabs/ai-sdk-go/commit/546dff02e69ecb1d91b58b2f3a5da43fb6948c0d))

## [0.1.1](https://github.com/rudderlabs/ai-sdk-go/compare/rudderotel/v0.1.0...rudderotel/v0.1.1) (2026-10-23)


### Bug Fixes

* price India-region Bedrock models and align MCP event_name fallback ([#14](https://github.com/rudderlabs/ai-sdk-go/issues/14)) ([3594546](https://github.com/rudderlabs/ai-sdk-go/commit/3594546f84858cedb4031c163625a4327bdfb608))


### Miscellaneous

* raise minimum Go version to 1.26.9 ([#12](https://github.com/rudderlabs/ai-sdk-go/issues/12)) ([ebe549b](https://github.com/rudderlabs/ai-sdk-go/commit/ebe549b50605da1e7896f2303a3c223c45ecd516))

## 0.1.0 (2026-10-19)


### Features

* add AI interaction events and provider integrations ([#2](https://github.com/rudderlabs/ai-sdk-go/issues/2)) ([ecbd5c6](https://github.com/rudderlabs/ai-sdk-go/commit/ecbd5c660272e4fef84f7ece64406f40d1877749))
