package conn

import (
	"fmt"
	"go.keploy.io/server/v2/config"
	proxyHttp "go.keploy.io/server/v2/pkg/core/proxy/integrations/http"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

var (
	realTimeOffset uint64
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

func isFiltered(logger *zap.Logger, req *http.Request, opts models.IncomingOptions) bool {
	destPort, err := strconv.Atoi(strings.Split(req.Host, ":")[1])
	if err != nil {
		utils.LogError(logger, err, "failed to obtain destination port from request")
		return false
	}
	var bypassRules []config.BypassRule

	for _, filter := range opts.Filters {
		bypassRules = append(bypassRules, filter.BypassRule)
	}

	// Host, Path and Port matching
	headerOpts := models.OutgoingOptions{
		Rules:          bypassRules,
		MongoPassword:  "",
		SQLDelay:       0,
		FallBackOnMiss: false,
	}
	passThrough := proxyHttp.IsPassThrough(logger, req, uint(destPort), headerOpts)

	for _, filter := range opts.Filters {
		if filter.URLMethods != nil && len(filter.URLMethods) != 0 {
			urlMethodMatch := false
			for _, method := range filter.URLMethods {
				if method == req.Method {
					urlMethodMatch = true
					break
				}
			}
			passThrough = urlMethodMatch
			if !passThrough {
				continue
			}
		}
		if filter.Headers != nil && len(filter.Headers) != 0 {
			headerMatch := false
			for filterHeaderKey, filterHeaderValue := range filter.Headers {
				regex, err := regexp.Compile(filterHeaderValue)
				if err != nil {
					utils.LogError(logger, err, "failed to compile the header regex")
					continue
				}
				if req.Header.Get(filterHeaderKey) != "" {
					for _, value := range req.Header.Values(filterHeaderKey) {
						headerMatch = regex.MatchString(value)
						if headerMatch {
							break
						}
					}
				}
				passThrough = headerMatch
				if passThrough {
					break
				}
			}
		}
	}

	return passThrough
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
