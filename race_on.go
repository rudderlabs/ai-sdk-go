//go:build race

package rudderstackai

// raceSlowdown stretches time budgets under the race detector, which runs regexp2 about ten times
// slower; builds without -race are unaffected.
const raceSlowdown = 10
