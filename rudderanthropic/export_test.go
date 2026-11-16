package rudderanthropic

// ResetManagedAgents forgets every session's read events, threads and paused turns (tests only).
func ResetManagedAgents() {
	sessionStates.Lock()
	sessionStates.m, sessionStates.order = map[string]*sessionState{}, nil
	sessionStates.Unlock()
	pausedTurns.Lock()
	pausedTurns.m, pausedTurns.order = map[string]*pausedTurn{}, nil
	pausedTurns.Unlock()
}

// PanickingOption is a session option that panics, like a getter that throws (tests only).
func PanickingOption() SessionOption { return func(*sessionConfig) { panic("option failed") } }
