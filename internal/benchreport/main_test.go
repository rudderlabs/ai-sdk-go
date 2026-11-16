package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sample = `goos: linux
cpu: AMD EPYC 7763 64-Core Processor
BenchmarkOverhead/interaction/manual/sdk-4                	   10000	     30000 ns/op	     35000 cpu-ns/op	        70.00 gc/10k-op	     28000 p50-ns	     90000 p99-ns	   18000 B/op	     175 allocs/op
BenchmarkOverhead/interaction/openai-stream/baseline-4    	    1000	    500000 ns/op	    600000 cpu-ns/op	      3000 gc/10k-op	    480000 p50-ns	   1000000 p99-ns	  716000 B/op	    5263 allocs/op
BenchmarkOverhead/interaction/openai-stream/sdk-4         	    1000	    800000 ns/op	    900000 cpu-ns/op	      4500 gc/10k-op	    780000 p50-ns	   1500000 p99-ns	 1000000 B/op	   10100 allocs/op
PASS
`

func TestResults(t *testing.T) {
	order, sides, cpu, err := parse(strings.NewReader(sample))
	if err != nil {
		t.Fatal(err)
	}
	if cpu != "AMD EPYC 7763 64-Core Processor" {
		t.Errorf("cpu %q", cpu)
	}
	rs := results(order, sides)
	if len(rs) != 2 || rs[0].ID != "interaction/manual" || rs[0].BaselineP50Us != nil || rs[0].OverheadP50Us != 28 {
		t.Fatalf("manual: %+v", rs[0])
	}
	stream := rs[1]
	if *stream.BaselineP50Us != 480 || stream.OverheadP50Us != 300 || stream.OverheadCPUUs != 300 ||
		stream.PerUnit.OverheadUs != 3 || stream.ExtraAllocs != 4837 || stream.ExtraGCPer10k != 1500 {
		t.Fatalf("stream: %+v %+v", stream, stream.PerUnit)
	}
	if got := table(rs, cpu); !strings.Contains(got, "| interaction/openai-stream | 480 | 780 | 1500 | 300 | 300 | 3 per chunk | 1500 | 284000 | 4837 |") {
		t.Errorf("table:\n%s", got)
	}
}

func TestMedianOfRuns(t *testing.T) {
	in := `BenchmarkOverhead/interaction/manual/sdk-4  10  1 ns/op  1 cpu-ns/op  1 gc/10k-op  10000 p50-ns  1 p99-ns  1 B/op  100 allocs/op
BenchmarkOverhead/interaction/manual/sdk-4  10  1 ns/op  1 cpu-ns/op  1 gc/10k-op  30000 p50-ns  1 p99-ns  1 B/op  300 allocs/op
BenchmarkOverhead/interaction/manual/sdk-4  10  1 ns/op  1 cpu-ns/op  1 gc/10k-op  20000 p50-ns  1 p99-ns  1 B/op  200 allocs/op
`
	order, sides, _, err := parse(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if rs := results(order, sides); len(rs) != 1 || rs[0].SdkP50Us != 20 || rs[0].ExtraAllocs != 200 {
		t.Fatalf("median of runs: %+v", rs)
	}
}

func TestBudget(t *testing.T) {
	dir := t.TempDir()
	budget := filepath.Join(dir, "budget.json")
	write := func(b map[string]float64) {
		raw, _ := json.Marshal(b)
		if err := os.WriteFile(budget, raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out, summary := filepath.Join(dir, "go.json"), filepath.Join(dir, "summary.md")
	write(map[string]float64{"interaction/openai-stream": 5000, "unmeasured": 1})
	var stdout bytes.Buffer
	if err := run(strings.NewReader(sample), &stdout, out, budget, summary); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(out); err != nil || !strings.Contains(string(raw), `"extraAllocsPerOp": 4837`) {
		t.Errorf("json: %s %v", raw, err)
	}
	if raw, err := os.ReadFile(summary); err != nil || !strings.Contains(string(raw), "### Go SDK overhead") {
		t.Errorf("summary: %s %v", raw, err)
	}
	write(map[string]float64{"interaction/openai-stream": 4000})
	if err := run(strings.NewReader(sample), &stdout, "", budget, ""); err == nil || !strings.Contains(err.Error(), "4837 extra allocs/op, budget 4000") {
		t.Errorf("over budget: %v", err)
	}
	if err := run(strings.NewReader("PASS\n"), &stdout, "", "", ""); err == nil {
		t.Error("empty input passed")
	}
	if err := run(strings.NewReader(sample), &stdout, "", filepath.Join(dir, "missing.json"), ""); err == nil {
		t.Error("missing budget passed")
	}
}
