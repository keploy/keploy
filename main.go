package main

import (
	"fmt"
	_ "net/http/pprof"
	"log"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	v "github.com/hashicorp/go-version"
	sentry "github.com/getsentry/sentry-go"
	"go.keploy.io/server/cmd"
	"go.keploy.io/server/pkg/platform/telemetry"
	"go.keploy.io/server/pkg/platform/fs"
)

// version is the version of the server and will be injected during build by ldflags
// see https://goreleaser.com/customization/build/

var version string

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
	teleFS := fs.NewTeleFS()
	tele := telemetry.NewTelemetry(true, false, teleFS, nil, version, nil)
	tele.Ping(false)
	fmt.Println(logo, " ")
	fmt.Printf("%v\n\n", version)
	//Initialise sentry.
	err := sentry.Init(sentry.ClientOptions{
		Dsn: "https://fff27a8c908bcd82ac8174e1860d2222@o4505956922556416.ingest.sentry.io/4505956925702144",
		TracesSampleRate: 1.0,
	  })
	  if err != nil {
		log.Fatalf("sentry.Init: %s", err)
	  }
	defer sentry.Recover()
	  defer sentry.Flush(2 * time.Second)
	cmd.Execute()
}
