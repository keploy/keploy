package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"strings"
	"time"

	"github.com/cloudflare/cfssl/log"
	"github.com/fatih/color"
	sentry "github.com/getsentry/sentry-go"
	"go.keploy.io/server/cmd"
	"go.keploy.io/server/utils"
)

// version is the version of the server and will be injected during build by ldflags
// see https://goreleaser.com/customization/build/

var version string
var dsn string

const logo string = `
       ▓██▓▄
    ▓▓▓▓██▓█▓▄
     ████████▓▒
          ▀▓▓███▄      ▄▄   ▄               ▌
         ▄▌▌▓▓████▄    ██ ▓█▀  ▄▌▀▄  ▓▓▌▄   ▓█  ▄▌▓▓▌▄ ▌▌   ▓
       ▓█████████▌▓▓   ██▓█▄  ▓█▄▓▓ ▐█▌  ██ ▓█  █▌  ██  █▌ █▓
      ▓▓▓▓▀▀▀▀▓▓▓▓▓▓▌  ██  █▓  ▓▌▄▄ ▐█▓▄▓█▀ █▓█ ▀█▄▄█▀   █▓█
       ▓▌                           ▐█▌                   █▌
        ▓
`

func main() {
	if version == "" {
		version = "2-dev"
	}
	utils.KeployVersion = version
	fmt.Println(logo, " ")
	fmt.Printf("version: %v\n\n", version)
	latestVersion, err2 := getLatestGitHubRelease()
	if err2 != nil {
		log.Debug("Failed to fetch latest release version", err2)
	}
	graytext := color.New(color.FgHiBlack)
	updatetext := graytext.Sprint("keploy update")
	const msg string = `
	   ╭─────────────────────────────────────╮
	   │ New version available:              │		
	   │ %v  ---->   %v       │
	   │ Run %v to update         │
	   ╰─────────────────────────────────────╯
	`
	versionmsg := fmt.Sprintf(msg, strings.TrimSpace(version), strings.TrimSpace(latestVersion), updatetext)
	if latestVersion != version {
		fmt.Printf(versionmsg)
	}

	//Initialise sentry.
	err := sentry.Init(sentry.ClientOptions{
		Dsn:              dsn,
		TracesSampleRate: 1.0,
	})
	//Set the version
	utils.KeployVersion = version
	log.Level = 0
	if err != nil {
		log.Debug("Could not initialise sentry.", err)
	}
	defer utils.HandlePanic()
	defer sentry.Flush(2 * time.Second)
	cmd.Execute()
}

type GitHubRelease struct {
	TagName string `json:"tag_name"`
}

// getLatestGitHubRelease fetches the latest version from GitHub releases.
func getLatestGitHubRelease() (string, error) {
	// GitHub repository details
	repoOwner := "keploy"
	repoName := "keploy"

	// GitHub API URL for latest release
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", repoOwner, repoName)

	// Create an HTTP client
	client := http.Client{}

	// Create a GET request to the GitHub API
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return "", err
	}

	// Send the request
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	// Decode the response JSON
	var release GitHubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return "", err
	}

	return release.TagName, nil
}
