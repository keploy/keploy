//go:build race

package learn

// raceOn: this test binary is built with -race, under which a call's
// allocation count moves by a few from run to run at one set size, so the
// rows that compare allocations across set sizes compare only without it.
const raceOn = true
