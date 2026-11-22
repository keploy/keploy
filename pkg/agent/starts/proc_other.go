//go:build !linux && !darwin

package starts

import "time"

type sysProc struct{}

func (sysProc) Parent(uint32) (uint32, bool) { return 0, false }

func (sysProc) Birth(uint32) (time.Time, bool) { return time.Time{}, false }

func (sysProc) Program(uint32) string { return "" }

var Default = New(sysProc{}, 0)
