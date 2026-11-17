package rudderotel

// liveRegistrations is the number of open spans the shared dedup state holds.
func liveRegistrations() int {
	otelSpans.mu.Lock()
	defer otelSpans.mu.Unlock()
	return len(otelSpans.open)
}
