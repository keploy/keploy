package settings

import (
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

var (
	Emoji                 = "\U0001F430" + " Keploy:"
	realTimeOffset uint64 = 0
)

// InitRealTimeOffset calculates the offset between the real clock and the monotonic clock used in the BPF.
func InitRealTimeOffset() error {
	var monotonicTime, realTime unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &monotonicTime); err != nil {
		return fmt.Errorf("%s failed getting monotonic clock due to: %v", Emoji, err)
	}
	if err := unix.ClockGettime(unix.CLOCK_REALTIME, &realTime); err != nil {
		return fmt.Errorf("%s failed getting real clock time due to: %v", Emoji, err)
	}
	realTimeOffset = uint64(time.Second)*(uint64(realTime.Sec)-uint64(monotonicTime.Sec)) + uint64(realTime.Nsec) - uint64(monotonicTime.Nsec)
	// realTimeCopy := time.Unix(int64(realTimeOffset/1e9), int64(realTimeOffset%1e9))
	// log.Debug(fmt.Sprintf("%s real time offset is: %v", Emoji, realTimeCopy))
	return nil
}

// GetRealTimeOffset is a getter for the real-time-offset.
func GetRealTimeOffset() uint64 {
	return realTimeOffset
}
