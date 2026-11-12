//go:build !race

package proxy

// raceOn: this test binary is built with -race (race_on_test.go).
const raceOn = false
