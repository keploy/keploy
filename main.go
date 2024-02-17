package main

import (
	"context"
	"fmt"
	sentry "github.com/getsentry/sentry-go"
	"go.keploy.io/server/v2/cli"
	updateSvc "go.keploy.io/server/v2/pkg/service/tools"
	"go.keploy.io/server/v2/utils"
	"go.keploy.io/server/v2/utils/log"
	"go.uber.org/zap"
	_ "net/http/pprof"
	"os"
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
	printLogo()
	ctx := utils.NewCtx()
	start(ctx)
}

func printLogo() {
	if version == "" {
		version = "2-dev"
	}
	utils.Version = version
	if binaryToDocker := os.Getenv("BINARY_TO_DOCKER"); binaryToDocker != "true" {
		fmt.Println(logo, " ")
		fmt.Printf("version: %v\n\n", version)
	} else {
		fmt.Println("Starting keploy in docker environment.")
	}
}

// setup and hook the different flags
func start(ctx context.Context) {
	// Now that flags are parsed, set up the log
	logger := log.New()
	logger = utils.ModifyToSentryLogger(logger, sentry.CurrentHub().Client())
	defer log.DeleteLogs(logger)

	utils.SentryInit(logger, dsn)
	defer utils.HandlePanic(logger)

	svc := cli.NewServices(updateSvc.NewUpdater(logger))

	rootCmd := cli.Root(ctx, logger, svc)
	if err := rootCmd.Execute(); err != nil {
		logger.Error("failed to start the CLI.", zap.Any("error", err.Error()))
		os.Exit(1)
	}
}
