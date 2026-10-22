//go:build !race

package rudderstackai

// raceSlowdown stretches time budgets under the race detector; 1 without it.
const raceSlowdown = 1
