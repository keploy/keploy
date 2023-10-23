package main

import (
	"fmt"
	_ "net/http/pprof"
	"os"
	"time"

	sentry "github.com/getsentry/sentry-go"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/cloudflare/cfssl/log"
	v "github.com/hashicorp/go-version"
	"go.keploy.io/server/cmd"
	"go.keploy.io/server/utils"
)

// version is the version of the server and will be injected during build by ldflags
// see https://goreleaser.com/customization/build/

var version string
var Dsn string

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

func getKeployVersion() string {

	repo, err := git.PlainOpen(".")
	if err != nil {
		return "v0.1.0-dev"
	}

	tagIter, err := repo.Tags()
	if err != nil {
		return "v0.1.0-dev"
	}
	var latestTag string
	var latestTagVersion *v.Version

	err = tagIter.ForEach(func(tagRef *plumbing.Reference) error {
		tagName := tagRef.Name().Short()
		tagVersion, err := v.NewVersion(tagName)
		if err == nil {
			if latestTagVersion == nil || latestTagVersion.LessThan(tagVersion) {
				latestTagVersion = tagVersion
				latestTag = tagName
			}
		}
		return nil
	})

	if err != nil {
		return "v0.1.0-dev"
	}

	return latestTag + "-dev"
}


func main() {
	if version == "" {
		version = getKeployVersion()
	}
	utils.VersionForTelemetry = version
	fmt.Println(logo, " ")
	fmt.Printf("%v\n\n", version)
	isDocker := os.Getenv("IS_DOCKER_CMD")
	if isDocker != "" {
		Dsn = os.Getenv("Dsn")
	}
	//Initialise sentry.
	err := sentry.Init(sentry.ClientOptions{
		Dsn:              Dsn,
		TracesSampleRate: 1.0,
	})
	log.Level = 0
	if err != nil {
		log.Debug("Could not initialise sentry.", err)
	}
	defer utils.HandlePanic()
	defer sentry.Flush(2 * time.Second)
	cmd.Execute()
}
