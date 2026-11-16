// Command benchreport turns the BenchmarkOverhead output of `go test -bench` into the overhead
// table the Node.js and Python SDKs print: for each scenario, the SDK's p50 and p99, and what it
// adds over the same call without the SDK. With -budget, it fails when a scenario's extra
// allocations per op exceed the ceiling in that file.
//
//	go test -run '^$' -bench Overhead -benchmem -count 5 . ./rudderopenai > go.txt
//	go run ./internal/benchreport -budget internal/benchreport/alloc_budget.json < go.txt
//
// With -count, each metric is the median of the runs, which alternate baseline and sdk.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// side is one sub-benchmark: the scenario with the SDK ("sdk") or without it ("baseline").
type side struct {
	ns, cpu, gc, p50, p99, bytes, allocs float64
}

// Result is one scenario, in microseconds, bytes and counts per op.
type Result struct {
	ID              string   `json:"id"`
	BaselineP50Us   *float64 `json:"baselineP50Us"`
	SdkP50Us        float64  `json:"sdkP50Us"`
	SdkP99Us        float64  `json:"sdkP99Us"`
	OverheadP50Us   float64  `json:"overheadP50Us"`
	OverheadCPUUs   float64  `json:"overheadCpuUs"`
	PerUnit         *Unit    `json:"perUnit"`
	ExtraGCPer10k   float64  `json:"extraGcPer10k"`
	ExtraBytesPerOp float64  `json:"extraBytesPerOp"`
	ExtraAllocs     float64  `json:"extraAllocsPerOp"`
}

// Unit divides a scenario's overhead into a per-unit figure.
type Unit struct {
	Name       string  `json:"name"`
	OverheadUs float64 `json:"overheadUs"`
}

// units are the streamed scenarios' chunk counts, as the benchmarks send them.
var units = map[string]int{"interaction/openai-stream": 100, "interaction/openai-stream+spans": 100}

var line = regexp.MustCompile(`^BenchmarkOverhead/(.+)/(baseline|sdk)(?:-\d+)?\s+\d+\s+(.*)$`)

// parse reads benchmark output: the scenarios in first-seen order, and the cpu line.
func parse(r io.Reader) (order []string, sides map[string]map[string]side, cpu string, err error) {
	runs := map[string]map[string][]side{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		text := sc.Text()
		if c, ok := strings.CutPrefix(text, "cpu: "); ok && cpu == "" {
			cpu = c
		}
		m := line.FindStringSubmatch(text)
		if m == nil {
			continue
		}
		fields := strings.Fields(m[3])
		var s side
		for i := 0; i+1 < len(fields); i += 2 {
			v, perr := strconv.ParseFloat(fields[i], 64)
			if perr != nil {
				return nil, nil, "", fmt.Errorf("%s: %w", text, perr)
			}
			switch fields[i+1] {
			case "ns/op":
				s.ns = v
			case "cpu-ns/op":
				s.cpu = v
			case "gc/10k-op":
				s.gc = v
			case "p50-ns":
				s.p50 = v
			case "p99-ns":
				s.p99 = v
			case "B/op":
				s.bytes = v
			case "allocs/op":
				s.allocs = v
			}
		}
		if runs[m[1]] == nil {
			runs[m[1]] = map[string][]side{}
			order = append(order, m[1])
		}
		runs[m[1]][m[2]] = append(runs[m[1]][m[2]], s)
	}
	sides = map[string]map[string]side{}
	for id, bySide := range runs {
		sides[id] = map[string]side{}
		for name, list := range bySide {
			sides[id][name] = medianSide(list)
		}
	}
	return order, sides, cpu, sc.Err()
}

// medianSide is the median of each metric over the runs of one side (go test -count n runs the
// sub-benchmarks n times, alternating baseline and sdk).
func medianSide(list []side) side {
	med := func(get func(side) float64) float64 {
		vs := make([]float64, len(list))
		for i, s := range list {
			vs[i] = get(s)
		}
		sort.Float64s(vs)
		return vs[len(vs)/2]
	}
	return side{
		ns:     med(func(s side) float64 { return s.ns }),
		cpu:    med(func(s side) float64 { return s.cpu }),
		gc:     med(func(s side) float64 { return s.gc }),
		p50:    med(func(s side) float64 { return s.p50 }),
		p99:    med(func(s side) float64 { return s.p99 }),
		bytes:  med(func(s side) float64 { return s.bytes }),
		allocs: med(func(s side) float64 { return s.allocs }),
	}
}

func round(v float64, digits int) float64 {
	p, _ := strconv.ParseFloat(strconv.FormatFloat(v, 'f', digits, 64), 64)
	return p
}

func results(order []string, sides map[string]map[string]side) []Result {
	out := make([]Result, 0, len(order))
	for _, id := range order {
		sdk, ok := sides[id]["sdk"]
		if !ok {
			continue
		}
		base, hasBase := sides[id]["baseline"]
		r := Result{
			ID:              id,
			SdkP50Us:        round(sdk.p50/1000, 1),
			SdkP99Us:        round(sdk.p99/1000, 1),
			OverheadP50Us:   round((sdk.p50-base.p50)/1000, 1),
			OverheadCPUUs:   round((sdk.cpu-base.cpu)/1000, 1),
			ExtraGCPer10k:   round(sdk.gc-base.gc, 1),
			ExtraBytesPerOp: sdk.bytes - base.bytes,
			ExtraAllocs:     sdk.allocs - base.allocs,
		}
		if hasBase {
			b := round(base.p50/1000, 1)
			r.BaselineP50Us = &b
		}
		if n := units[id]; n > 0 {
			r.PerUnit = &Unit{Name: "chunk", OverheadUs: round((sdk.p50-base.p50)/1000/float64(n), 3)}
		}
		out = append(out, r)
	}
	return out
}

func table(rs []Result, cpu string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "### Go SDK overhead (%s, %s/%s, %s)\n\n", runtime.Version(), runtime.GOOS, runtime.GOARCH, cpu)
	b.WriteString("| Scenario | Baseline p50 µs | With SDK p50 µs | With SDK p99 µs | Overhead p50 µs | Overhead CPU µs | Per unit µs | Extra GC per 10k ops | Extra B/op | Extra allocs/op |\n")
	b.WriteString("| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |\n")
	for _, r := range rs {
		base, unit := "–", "–"
		if r.BaselineP50Us != nil {
			base = strconv.FormatFloat(*r.BaselineP50Us, 'f', -1, 64)
		}
		if r.PerUnit != nil {
			unit = fmt.Sprintf("%v per %s", r.PerUnit.OverheadUs, r.PerUnit.Name)
		}
		fmt.Fprintf(&b, "| %s | %s | %v | %v | %v | %v | %s | %v | %.0f | %.0f |\n", r.ID, base, r.SdkP50Us, r.SdkP99Us,
			r.OverheadP50Us, r.OverheadCPUUs, unit, r.ExtraGCPer10k, r.ExtraBytesPerOp, r.ExtraAllocs)
	}
	return b.String()
}

// overBudget lists the scenarios whose extra allocations per op exceed their ceiling. A scenario
// without a ceiling is not checked; a ceiling without a measured scenario is ignored.
func overBudget(rs []Result, budget map[string]float64) []string {
	var over []string
	for _, r := range rs {
		if ceiling, ok := budget[r.ID]; ok && r.ExtraAllocs > ceiling {
			over = append(over, fmt.Sprintf("%s: %v extra allocs/op, budget %v", r.ID, r.ExtraAllocs, ceiling))
		}
	}
	sort.Strings(over)
	return over
}

func run(in io.Reader, stdout io.Writer, out, budgetPath, summary string) error {
	order, sides, cpu, err := parse(in)
	if err != nil {
		return err
	}
	rs := results(order, sides)
	if len(rs) == 0 {
		return fmt.Errorf("no BenchmarkOverhead results in the input")
	}
	if out != "" {
		doc, _ := json.MarshalIndent(map[string]any{
			"runtime": runtime.Version(), "platform": runtime.GOOS + "/" + runtime.GOARCH, "cpu": cpu, "results": rs,
		}, "", "  ")
		if err := os.WriteFile(out, append(doc, '\n'), 0o644); err != nil {
			return err
		}
	}
	t := table(rs, cpu)
	_, _ = io.WriteString(stdout, t)
	if summary != "" {
		f, err := os.OpenFile(summary, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		_, _ = io.WriteString(f, t+"\n")
		_ = f.Close()
	}
	if budgetPath == "" {
		return nil
	}
	raw, err := os.ReadFile(budgetPath)
	if err != nil {
		return err
	}
	var budget map[string]float64
	if err := json.Unmarshal(raw, &budget); err != nil {
		return fmt.Errorf("%s: %w", budgetPath, err)
	}
	if over := overBudget(rs, budget); len(over) > 0 {
		return fmt.Errorf("extra allocations over %s:\n  %s", budgetPath, strings.Join(over, "\n  "))
	}
	return nil
}

func main() {
	out := flag.String("out", "", "write the results as JSON to this file")
	budget := flag.String("budget", "", "fail when extra allocs/op exceed the ceilings in this JSON file")
	flag.Parse()
	if err := run(os.Stdin, os.Stdout, *out, *budget, os.Getenv("GITHUB_STEP_SUMMARY")); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
