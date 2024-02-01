package utils

import (
	"bufio"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"runtime/debug"
	"strings"
	"time"

	"github.com/cloudflare/cfssl/log"
	sentry "github.com/getsentry/sentry-go"
	"go.uber.org/zap"
)

var Emoji = "\U0001F430" + " Keploy:"
var ConfigGuide = `
# Example on using tests
#tests: 
#  filters:
#   - path: "/user/app"
#     urlMethods: ["GET"]
#     headers: {
#       "^asdf*": "^test"
#     }
#     host: "dc.services.visualstudio.com"
#Example on using stubs
#stubs: 
#  filters:
#   - path: "/user/app"
#     port: 8080
#   - port: 8081
#   - host: "dc.services.visualstudio.com"
#   - port: 8081
#     host: "dc.services.visualstudio.com"
#     path: "/user/app"
	#
#Example on using globalNoise
#globalNoise: 
#   global:
#     body: {
#        # to ignore some values for a field, 
#        # pass regex patterns to the corresponding array value
#        "url": ["https?://\S+", "http://\S+"],
#     }
#     header: {
#        # to ignore the entire field, pass an empty array
#        "Date": [],
#      }
#    # to ignore fields or the corresponding values for a specific test-set,
#    # pass the test-set-name as a key to the "test-sets" object and
#    # populate the corresponding "body" and "header" objects 
#    test-sets:
#      test-set-1:
#        body: {
#          # ignore all the values for the "url" field
#          "url": []
#        }
#        header: { 
#          # we can also pass the exact value to ignore for a field
#          "User-Agent": ["PostmanRuntime/7.34.0"]
#        }
`

// askForConfirmation asks the user for confirmation. A user must type in "yes" or "no" and
// then press enter. It has fuzzy matching, so "y", "Y", "yes", "YES", and "Yes" all count as
// confirmations. If the input is not recognized, it will ask again. The function does not return
// until it gets a valid response from the user.
func AskForConfirmation(s string) (bool, error) {
	reader := bufio.NewReader(os.Stdin)

	for {
		fmt.Printf("%s [y/n]: ", s)

		response, err := reader.ReadString('\n')
		if err != nil {
			return false, err
		}

		response = strings.ToLower(strings.TrimSpace(response))

		if response == "y" || response == "yes" {
			return true, nil
		} else if response == "n" || response == "no" {
			return false, nil
		}
	}
}

func CheckFileExists(path string) bool {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return false
	}
	return true
}

var Version string

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
	sentry.Flush(time.Second * 5)
}

func HandlePanic() {
	if r := recover(); r != nil {
		attachLogFileToSentry("./keploy-logs.txt")
		sentry.CaptureException(errors.New(fmt.Sprint(r)))
		// Get the stack trace
		stackTrace := debug.Stack()

		log.Error(Emoji+"Recovered from:", r, "\nstack trace:\n", string(stackTrace))
		sentry.Flush(time.Second * 2)
	}
}

// GenerateGithubActions generates a GitHub Actions workflow file for Keploy
func GenerateGithubActions(logger *zap.Logger, path string, appCmd string) {
	// Determine the path based on the alias "keploy"
	logger.Info("Determining the path of the keploy binary based on the alias \"keploy\"")

	keployPath := "/usr/local/bin/keploy" // Default path
	aliasCmd := exec.Command("which", "keploy")
	aliasOutput, err := aliasCmd.Output()
	if err == nil && len(aliasOutput) > 0 {
		keployPath = strings.TrimSpace(string(aliasOutput))
	}
	logger.Info("Path of the keploy binary determined successfully", zap.String("path", keployPath))
	// Define the content of the GitHub Actions workflow file
	actionsFileContent := `name: Keploy
on:
  push:
    branches:
      - main
  pull_request:
    types: [opened, reopened, synchronize]
jobs:
  e2e-test:
    runs-on: ubuntu-latest
    steps:
      - name: Checkout
        uses: actions/checkout@v2
      - name: Test-Report
        uses: keploy/testgpt@main
        with:
          working-directory: ./
          keploy-path: ` + keployPath + `
          command: ` + appCmd + `
`

	// Define the file path where the GitHub Actions workflow file will be saved
	filePath := ".github/workflows/keploy.yml"

	// Write the content to the file
	if err := ioutil.WriteFile(filePath, []byte(actionsFileContent), 0644); err != nil {
		logger.Error("Error writing GitHub Actions workflow file", zap.Error(err))
		return
	}

	logger.Info("GitHub Actions workflow file generated successfully", zap.String("path", filePath))
}

var WarningSign = "\U000026A0"
