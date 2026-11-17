package rudderotel

// liveRegistrations is the number of open spans the shared dedup state holds.
func liveRegistrations() int {
	otelSpans.mu.Lock()
	defer otelSpans.mu.Unlock()
	return len(otelSpans.open)
}

// closedEntry reports whether the shared dedup state still holds span k's closed entry.
func closedEntry(k spanKey) bool {
	otelSpans.mu.Lock()
	defer otelSpans.mu.Unlock()
	return otelSpans.ended[k] != nil
}
