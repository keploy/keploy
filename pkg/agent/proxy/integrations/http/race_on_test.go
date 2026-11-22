//go:build race

package http

// raceOn: this test binary is built with -race, under which sync.Pool drops
// what it is given at random, so a count of allocations varies run to run.
const raceOn = true
