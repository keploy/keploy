package conn

import (
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

var (
	realTimeOffset uint64 = 0
)

// InitRealTimeOffset calculates the offset between the real clock and the monotonic clock used in the BPF.
func initRealTimeOffset() error {
	var monotonicTime, realTime unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &monotonicTime); err != nil {
		return fmt.Errorf("failed getting monotonic clock due to: %v", err)
	}
	if err := unix.ClockGettime(unix.CLOCK_REALTIME, &realTime); err != nil {
		return fmt.Errorf("failed getting real clock time due to: %v", err)
	}
	realTimeOffset = uint64(time.Second)*(uint64(realTime.Sec)-uint64(monotonicTime.Sec)) + uint64(realTime.Nsec) - uint64(monotonicTime.Nsec)
	// realTimeCopy := time.Unix(int64(realTimeOffset/1e9), int64(realTimeOffset%1e9))
	// log.Debug(fmt.Sprintf("%s real time offset is: %v", Emoji, realTimeCopy))
	return nil
}

// GetRealTimeOffset is a getter for the real-time-offset.
func getRealTimeOffset() uint64 {
	return realTimeOffset
}

// convertUnixNanoToTime takes a Unix timestamp in nanoseconds as a uint64 and returns the corresponding time.Time
func convertUnixNanoToTime(unixNano uint64) time.Time {
	// Unix time is the number of seconds since January 1, 1970 UTC,
	// so convert nanoseconds to seconds for time.Unix function
	seconds := int64(unixNano / uint64(time.Second))
	nanoRemainder := int64(unixNano % uint64(time.Second))
	return time.Unix(seconds, nanoRemainder)
}

//// LogAny appends input of any type to a logs.txt file in the current directory
//func LogAny(value string) error {
//
//	logMessage := value
//
//	// Add a timestamp to the log message
//	timestamp := time.Now().Format("2006-01-02 15:04:05")
//	logLine := fmt.Sprintf("%s - %s\n", timestamp, logMessage)
//
//	// Open logs.txt in append mode, create it if it doesn't exist
//	file, err := os.OpenFile("logs.txt", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
//	if err != nil {
//		return err
//	}
//	defer file.Close()
//
//	// Write the log line to the file
//	_, err = file.WriteString(logLine)
//	if err != nil {
//		return err
//	}
//
//	return nil
//}
