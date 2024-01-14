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
	// logger.Debug(fmt.Sprintf("%s real time offset is: %v", Emoji, realTimeCopy))
	return nil
}

// GetRealTimeOffset is a getter for the real-time-offset.
func GetRealTimeOffset() uint64 {
	return realTimeOffset
}

// BootTime the System boot time
var BootTime time.Time

func init() {
	var ts unix.Timespec
	err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts)
	now := time.Now()
	if err != nil {
		panic(fmt.Errorf("init boot time error: %v", err))
	}
	bootTimeNano := now.UnixNano() - ts.Nano()
	BootTime = time.Unix(bootTimeNano/1e9, bootTimeNano%1e9)
}

func GetRealTime(bpfTime uint64) time.Time {
	fmt.Printf("BootTimeCopy:%v\n", BootTime)
	timeCopy := time.Unix(BootTime.Unix(), int64(BootTime.Nanosecond()))
	return timeCopy.Add(time.Duration(bpfTime))
}
