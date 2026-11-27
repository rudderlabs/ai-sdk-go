# Changelog

## [0.6.0](https://github.com/rudderlabs/ai-sdk-go/compare/rudderotel/v0.5.0...rudderotel/v0.6.0) (2026-11-27)


### Features

* add JoinInteraction for work that loses the SDK's context ([#98](https://github.com/rudderlabs/ai-sdk-go/issues/98)) ([b3daf39](https://github.com/rudderlabs/ai-sdk-go/commit/b3daf392e4d6cf0e310ed67319324748b841a838))


### Bug Fixes

* count tool outputs with successful false as failed calls [SDK-6934] ([#110](https://github.com/rudderlabs/ai-sdk-go/issues/110)) ([272b550](https://github.com/rudderlabs/ai-sdk-go/commit/272b55064b6c4504c0f37212c77e7d1049b79d06))
* **tracing:** drop a detached span whose owner is ambiguous [SDK-7017] ([#128](https://github.com/rudderlabs/ai-sdk-go/issues/128)) ([c5451cd](https://github.com/rudderlabs/ai-sdk-go/commit/c5451cd471d14611c58188a5f64b892eea21e257))


### Miscellaneous

* **deps:** bump golang.org/x/text from 0.42.0 to 0.44.0 in the gomod-non-major group across 1 directory ([#126](https://github.com/rudderlabs/ai-sdk-go/issues/126)) ([a87341c](https://github.com/rudderlabs/ai-sdk-go/commit/a87341c08745e1550838cb6af341be59ed37ab7d))
* **emit:** pin that a full delivery queue blocks rather than drops [SDK-7215] ([#120](https://github.com/rudderlabs/ai-sdk-go/issues/120)) ([6b3ba59](https://github.com/rudderlabs/ai-sdk-go/commit/6b3ba592791b2183b569970bdb966b1c316dfc86))
* remove unused code and simplify docs [SDK-7231] ([#114](https://github.com/rudderlabs/ai-sdk-go/issues/114)) ([f3864d5](https://github.com/rudderlabs/ai-sdk-go/commit/f3864d5b205640236615fd36b646debe4843493a))

## [0.5.0](https://github.com/rudderlabs/ai-sdk-go/compare/rudderotel/v0.4.0...rudderotel/v0.5.0) (2026-11-18)


### Features

* send one span per call with EmitSpans and the span processor ([#89](https://github.com/rudderlabs/ai-sdk-go/issues/89)) ([cb8abde](https://github.com/rudderlabs/ai-sdk-go/commit/cb8abde8f94ee771ba94063a750e9b59b944c59a))


### Bug Fixes

* **core:** honour marshalers when capturing content ([#104](https://github.com/rudderlabs/ai-sdk-go/issues/104)) ([b83922e](https://github.com/rudderlabs/ai-sdk-go/commit/b83922ee180d48fd2542d0d2612b6ad792c4a595))
* correct pause, overflow, twin and snapshot call accounting ([d0013cd](https://github.com/rudderlabs/ai-sdk-go/commit/d0013cd1dc1fb375efdf8b7ddba499ed533402ef))
* **otel:** keep a product span's owner from span start ([#103](https://github.com/rudderlabs/ai-sdk-go/issues/103)) ([549da20](https://github.com/rudderlabs/ai-sdk-go/commit/549da20c2e5c6ea546d159ac34b767e4b0b169e4))
* **rudderotel:** don't bill or mark successful a caller-cancelled Anthropic request or stream ([d0013cd](https://github.com/rudderlabs/ai-sdk-go/commit/d0013cd1dc1fb375efdf8b7ddba499ed533402ef))

## [0.4.0](https://github.com/rudderlabs/ai-sdk-go/compare/rudderotel/v0.3.0...rudderotel/v0.4.0) (2026-11-16)


### Features

* accept a per-feature content capture policy ([#83](https://github.com/rudderlabs/ai-sdk-go/issues/83)) ([511489d](https://github.com/rudderlabs/ai-sdk-go/commit/511489d3752301ecca2c5059d112d99159239b31))


### Bug Fixes

* count a forced schema tool with no result as structured output ([#85](https://github.com/rudderlabs/ai-sdk-go/issues/85)) ([f6c062a](https://github.com/rudderlabs/ai-sdk-go/commit/f6c062a091778364f31973732f54734ec2dbcfc1))
* count a model request once when spans nest or LlamaIndex adds a prep span ([#82](https://github.com/rudderlabs/ai-sdk-go/issues/82)) ([50bdf88](https://github.com/rudderlabs/ai-sdk-go/commit/50bdf880309a22a9e0315652a78a36b965a4e090))
* map cache writes, provider IDs, finish reasons, Traceloop tools and Mirascope usage ([#81](https://github.com/rudderlabs/ai-sdk-go/issues/81)) ([a7e2cee](https://github.com/rudderlabs/ai-sdk-go/commit/a7e2cee7d7be564c6773f52449f34986b023d974))

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
