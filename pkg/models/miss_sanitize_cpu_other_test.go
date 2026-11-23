//go:build !linux && !darwin

package models

import "time"

// threadCPU reads no thread clock here: Windows counts a thread's CPU time
// in clock ticks of about 15 ms, too coarse for a call's share.
func threadCPU() (time.Duration, bool) {
	return 0, false
}
