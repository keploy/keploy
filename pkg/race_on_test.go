//go:build race

package pkg

// raceOn: this test binary is built with -race, under which sync.Pool drops
// what it is given at random, so what a call allocates varies run to run.
const raceOn = true
