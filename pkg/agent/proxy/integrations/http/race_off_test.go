//go:build !race

package http

// raceOn: this test binary is built with -race (race_on_test.go).
const raceOn = false
