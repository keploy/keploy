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
	releaseInfo, err2 := utils.GetLatestGitHubRelease()
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
	versionmsg := fmt.Sprintf(msg, strings.TrimSpace(version), strings.TrimSpace(releaseInfo.TagName), updatetext)
	if releaseInfo.TagName != version {
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
