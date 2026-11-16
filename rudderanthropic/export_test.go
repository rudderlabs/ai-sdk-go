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
