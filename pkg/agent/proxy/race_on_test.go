//go:build race

package proxy

// raceOn: this test binary is built with -race, under which the random walks
// draw fewer seeds (walkSeeds, sharedHTTPWalkSeeds).
const raceOn = true
