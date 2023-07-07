package main

import (
	"fmt"
	"os"
	"runtime/pprof"

	"go.keploy.io/server/cmd"
)



func main() {
	// Start profiling
	f, err := os.Create("myprogram.prof")
	if err != nil {

		fmt.Println(err)
		return

	}
	pprof.StartCPUProfile(f)
	defer pprof.StopCPUProfile()

	cmd.Execute()
}

// package main

// import (
// 	"path/filepath"
// 	"runtime"
// 	"strconv"
// 	"strings"

// 	"github.com/sirupsen/logrus"
// )

// func main() {
// 	logger := logrus.New()

// 	// Set the formatter
// 	logger.SetFormatter(&logrus.JSONFormatter{
// 		// DisableColors: false,
// 		// ForceColors:   true,
// 	})

// 	// Add the Hook to include the code path
// 	// logger.AddHook(NewCodePathHook())

// 	// Example usage
// 	logger.Info("This is an informational log.")
// 	logger.Warn("This is a warning log.")
// 	logger.Error("This is an error log.")
// }

// // CodePathHook is a Logrus Hook implementation that adds the code path to the log entry.
// type CodePathHook struct{}

// // Levels returns the log levels at which this hook should be triggered.
// func (h *CodePathHook) Levels() []logrus.Level {
// 	return logrus.AllLevels
// }

// // Fire is called when a log event is fired, and it adds the code path to the log entry.
// func (h *CodePathHook) Fire(entry *logrus.Entry) error {
// 	// Get the caller's file path and line number
// 	_, file, line, ok := runtime.Caller(0) // Adjust the call stack depth as needed
// 	if ok {
// 		// Extract the directory name and file name
// 		dir := filepath.Dir(file)
// 		filename := filepath.Base(file)

// 		// Convert the line number to string
// 		lineStr := strconv.Itoa(line)

// 		// Append the directory and filename to the log entry's message
// 		entry.Message = strings.Join([]string{entry.Message, dir, filename, lineStr}, " ")
// 	}

// 	return nil
// }

// // NewCodePathHook creates a new instance of CodePathHook.
// func NewCodePathHook() logrus.Hook {
// 	return &CodePathHook{}
// }
