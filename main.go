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
	"sort"
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
		fmt.Println("Error opening the repo:", err)
		return "v0.1.0-dev"
	}

	tagIter, err := repo.Tags()
	if err != nil {
		fmt.Println("Error getting the tags:", err)
		return "v0.1.0-dev"
	}

	var versions v.Collection

	err = tagIter.ForEach(func(tagRef *plumbing.Reference) error {
		tagName := tagRef.Name().Short()
		tagVersion, err := v.NewVersion(tagName)
		fmt.Println("This is the tag name and this is the tag version:", tagName, tagVersion)
		if err == nil {
			versions = append(versions, tagVersion)
		} else {
			fmt.Printf("Error parsing version from tag %s: %v\n", tagName, err)
		}
		return nil
	})

	if err != nil {
		fmt.Println("Error iterating through the tags:", err)
		return "v0.1.0-dev"
	}

	if len(versions) == 0 {
		return "v0.1.0-dev"
	}

	sort.Sort(versions)
	latestVersion := versions[len(versions)-1]
	return latestVersion.String() + "-dev"
}


func main() {
	if version == "" {
		version = getKeployVersion()
	}
	fmt.Println(logo, " ")
	fmt.Printf("%v\n\n", version)
	isDocker := os.Getenv("IS_DOCKER_CMD")
	if isDocker != "" {
		Dsn = os.Getenv("Dsn")
	}
	fmt.Println("This is the value of Dsn:", Dsn)
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
