//go:build !linux

package utils

// platformProgressSeen: a stage's progress cannot be seen here.
const platformProgressSeen = false

// platformWorkProbe: a stage of teardown work's progress cannot be seen here,
// so it counts as making progress throughout.
func platformWorkProbe() workProbe { return nil }

// ProcessProgress cannot see a process's progress here: it reports progress
// at every call, as a stage whose progress cannot be seen counts as making it.
func ProcessProgress(int) (moved func() bool) { return func() bool { return true } }
