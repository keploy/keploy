//go:build !linux

package utils

// platformProgressSeen: a stage's progress cannot be seen here.
const platformProgressSeen = false

// platformWorkProbe: a stage of teardown work's progress cannot be seen here,
// so it counts as making progress throughout.
func platformWorkProbe() workProbe { return nil }
