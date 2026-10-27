//go:build race

package replayer_test

// raceEnabled: under -race a recorded set's 10k mock replay, whose every call
// reads the whole set, takes most of go test's ten minutes; it tests no
// concurrency.
const raceEnabled = true
