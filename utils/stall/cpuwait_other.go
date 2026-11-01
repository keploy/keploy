//go:build !linux

package stall

import "time"

// threadWaits: no per-thread run delay to read outside Linux.
type threadWaits struct{}

func (*threadWaits) longest() time.Duration { return 0 }
