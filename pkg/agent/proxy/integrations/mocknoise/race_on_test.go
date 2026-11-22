//go:build race

package mocknoise

// raceDetector is whether the tests run under the race detector, whose
// sync.Pool drops pooled items at random, so a pooled regexp machine is
// allocated again: allocation bounds do not hold there.
const raceDetector = true
