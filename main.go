package main

import (
	"context"
	"fmt"
	"os"

	sentry "github.com/getsentry/sentry-go"
	"go.keploy.io/server/v2/cli"
	"go.keploy.io/server/v2/pkg/platform/yaml/configdb"
	"go.keploy.io/server/v2/utils"
	"go.keploy.io/server/v2/utils/log"
	"go.uber.org/zap"
)

// version is the version of the server and will be injected during build by ldflags, same with dsn
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

func start(ctx context.Context) {
	logger := log.New()
	defer log.DeleteLogs(logger)
	defer utils.Recover(logger)
	configDb := configdb.NewConfigDb(logger)
	if dsn != "" {
		utils.SentryInit(logger, dsn)
		logger = utils.ModifyToSentryLogger(ctx, logger, sentry.CurrentHub().Client(), configDb)
	}
	svcProvider := cli.NewServiceProvider(logger, configDb)
	cmdConfigurator := cli.NewCmdConfigurator(logger)
	rootCmd := cli.Root(ctx, logger, svcProvider, cmdConfigurator)
	if err := rootCmd.Execute(); err != nil {
		// TODO: remove this log statement because cobra will log things anyways
		logger.Error("failed to start the CLI.", zap.Any("error", err.Error()))
		os.Exit(1)
	}
}
