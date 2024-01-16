package main

import (
	"fmt"
	_ "net/http/pprof"
	"strings"
	"time"

	"github.com/cloudflare/cfssl/log"
	sentry "github.com/getsentry/sentry-go"
	"go.keploy.io/server/cmd"
	"go.keploy.io/server/pkg/models"
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

	// Initialize sentry.
	err := sentry.Init(sentry.ClientOptions{
		Dsn:              dsn,
		TracesSampleRate: 1.0,
	})
	// Set the version
	utils.KeployVersion = version
	log.Level = 0
	if err != nil {
		log.Debug("Could not initialize sentry.", err)
	} else {
		// Check for the latest release version
		releaseInfo, err := utils.GetLatestGitHubRelease()
		if err != nil {
			log.Debug("Failed to fetch the latest release version", err)
			return
		}

		// Show update message only if it's not a dev version
		if releaseInfo.TagName != version && !strings.HasSuffix(version, "-dev") {
			updatetext := models.HighlightGrayString("keploy update")
			const msg string = `
           ╭─────────────────────────────────────╮
           │ New version available:              │
           │ %v  ---->   %v       │
           │ Run %v to update         │
           ╰─────────────────────────────────────╯
        `
			versionmsg := fmt.Sprintf(msg, version, releaseInfo.TagName, updatetext)
			fmt.Printf(versionmsg)
		}
	}
	defer utils.HandlePanic()
	defer sentry.Flush(2 * time.Second)
	cmd.Execute()
}
