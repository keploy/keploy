package main

import (
	"fmt"
	_ "net/http/pprof"
	"time"

	"github.com/cloudflare/cfssl/log"
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
	fmt.Println(logo, " ")
	fmt.Printf("version: %v\n\n", version)
	//Initialise sentry.
	err := sentry.Init(sentry.ClientOptions{
		Dsn:              dsn,
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
