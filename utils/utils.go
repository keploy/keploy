package utils

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"time"

	sentry "github.com/getsentry/sentry-go"
)

func attachLogFileToSentry(logFilePath string) {
	file, err := os.Open(logFilePath)
	if err != nil {
		errors.New(fmt.Sprintf("Error opening log file: %s", err.Error()))
		return
	}
	defer file.Close()

	content, _ := ioutil.ReadAll(file)

	sentry.ConfigureScope(func(scope *sentry.Scope) {
		scope.SetExtra("logfile", string(content))
	})
	fmt.Println("Sent the event to sentry.")
	sentry.Flush(time.Second * 5)
}

func HandlePanic() {
	if r := recover(); r != nil {
		attachLogFileToSentry("/tmp/keploy-logs.txt")
		sentry.CaptureException(errors.New(fmt.Sprint(r)))
		sentry.Flush(time.Second * 2)
	}
}
