//go:build !linux

package stall

import "time"

// threadWaits: no per-thread run delay or steal to read outside Linux.
type threadWaits struct{}

func (*threadWaits) longest() (time.Duration, bool) { return 0, false }
