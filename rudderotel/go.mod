module github.com/rudderlabs/ai-sdk-go/rudderotel

go 1.26.10

require go.opentelemetry.io/otel/trace v1.47.0

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	go.opentelemetry.io/otel v1.47.0 // indirect
)

// Developed against the core in this repository; consumers resolve the required release.
replace github.com/rudderlabs/ai-sdk-go => ../
