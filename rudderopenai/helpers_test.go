package rudderopenai_test

import "github.com/openai/openai-go/v3/option"

// body supplies the same raw JSON body to regular and fuzzed requests.
func body(s string) option.RequestOption {
	return option.WithRequestBody("application/json", []byte(s))
}
