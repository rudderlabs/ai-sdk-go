module github.com/rudderlabs/ai-sdk-go/rudderbedrock

go 1.26.10

require (
	github.com/aws/aws-sdk-go-v2 v1.47.1
	github.com/aws/aws-sdk-go-v2/service/bedrockruntime v1.63.1
	github.com/aws/smithy-go v1.28.1
	github.com/rudderlabs/ai-sdk-go v0.3.0
	github.com/rudderlabs/analytics-go/v4 v4.4.0
)

require (
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.7.20 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.5.4 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.8.4 // indirect
	github.com/dlclark/regexp2 v1.12.0 // indirect
	github.com/google/jsonschema-go v0.4.3 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/grafana/jsonparser v0.0.0-20250908162026-5c2524e07b4c // indirect
	github.com/modelcontextprotocol/go-sdk v1.8.0 // indirect
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.3 // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/backo-go v1.1.0 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	golang.org/x/oauth2 v0.36.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	golang.org/x/time v0.15.0 // indirect
)

// Developed against the core in this repository; consumers resolve the required release.
replace github.com/rudderlabs/ai-sdk-go => ../
