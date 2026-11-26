//go:build unix

package testkit

import (
	"context"
	"runtime"
	"slices"
	"syscall"
	"testing"
	"time"
)

// Measure reports latency percentiles, CPU time and GC alongside benchmark allocations.
func Measure(b *testing.B, op func(context.Context) error) {
	ctx := context.Background()
	b.ReportAllocs()
	samples := make([]time.Duration, b.N)
	var ru0, ru1 syscall.Rusage
	var ms0, ms1 runtime.MemStats
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru0)
	runtime.ReadMemStats(&ms0)
	b.ResetTimer()
	for i := range b.N {
		start := time.Now()
		if err := op(ctx); err != nil {
			b.Fatal(err)
		}
		samples[i] = time.Since(start)
	}
	b.StopTimer()
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru1)
	runtime.ReadMemStats(&ms1)
	slices.Sort(samples)
	cpu := func(ru syscall.Rusage) time.Duration {
		return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
	}
	b.ReportMetric(float64(samples[len(samples)/2]), "p50-ns")
	b.ReportMetric(float64(samples[min(len(samples)-1, len(samples)*99/100)]), "p99-ns")
	b.ReportMetric(float64(cpu(ru1)-cpu(ru0))/float64(b.N), "cpu-ns/op")
	b.ReportMetric(float64(ms1.NumGC-ms0.NumGC)*10_000/float64(b.N), "gc/10k-op")
}
