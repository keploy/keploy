package main

import (
	"fmt"
	_ "net/http/pprof"
	"log"
	"time"
	"net"

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
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		log.Fatal(err)
	}
	var ipv4 string
	for _, address := range addrs {
		// Check for IPv4 address and not loopback
		if ipnet, ok := address.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				ipv4 = ipnet.IP.String()
				return
			}
		}
	}
	teleFS := fs.NewTeleFS()
	tele := telemetry.NewTelemetry(true, false, teleFS, nil, version, nil)
	tele.Ping(false, ipv4)
	fmt.Println(logo, " ")
	fmt.Printf("%v\n\n", version)
	//Initialise sentry.
	err = sentry.Init(sentry.ClientOptions{
		Dsn: "",
		// Set TracesSampleRate to 1.0 to capture 100%
		// of transactions for performance monitoring.
		// We recommend adjusting this value in production,
		TracesSampleRate: 1.0,
	  })
	  if err != nil {
		log.Fatalf("sentry.Init: %s", err)
	  }
	defer sentry.Recover()
	  defer sentry.Flush(2 * time.Second)
	cmd.Execute()
}
