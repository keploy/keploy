package utils

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"github.com/cloudflare/cfssl/log"
	sentry "github.com/getsentry/sentry-go"
)

type GitHubRelease struct {
	TagName string `json:"tag_name"`
	Body    string `json:"body"`
}

var ErrGitHubAPIUnresponsive = errors.New("GitHub API is unresponsive")

var Emoji = "\U0001F430" + " Keploy:"
var ConfigGuide = `
  # Example on using globalNoise
  # globalNoise:
  #    global:
  #      body: {
  #         # to ignore some values for a field,
  #         # pass regex patterns to the corresponding array value
  #         "url": ["https?://\S+", "http://\S+"],
  #      }
  #      header: {
  #         # to ignore the entire field, pass an empty array
  #         "Date": [],
  #       }
  #     # to ignore fields or the corresponding values for a specific test-set,
  #     # pass the test-set-name as a key to the "test-sets" object and
  #     # populate the corresponding "body" and "header" objects
  #     test-sets:
  #       test-set-1:
  #         body: {
  #           # ignore all the values for the "url" field
  #           "url": []
  #         }
  #         header: {
  #           # we can also pass the exact value to ignore for a field
  #           "User-Agent": ["PostmanRuntime/7.34.0"]
  #         }
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

// getLatestGitHubRelease fetches the latest version and release body from GitHub releases with a timeout.
func GetLatestGitHubRelease() (GitHubRelease, error) {
	// GitHub repository details
	repoOwner := "keploy"
	repoName := "keploy"

	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", repoOwner, repoName)

	client := http.Client{
		Timeout: 4 * time.Second,
	}

	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return GitHubRelease{}, err
	}

	resp, err := client.Do(req)
	if err != nil {
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			return GitHubRelease{}, ErrGitHubAPIUnresponsive
		}
		return GitHubRelease{}, err
	}
	defer resp.Body.Close()

	var release GitHubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return GitHubRelease{}, err
	}
	return release, nil
}
