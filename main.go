// Package main is the entry point for the keploy application.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"go.keploy.io/server/v2/cli"
	"go.keploy.io/server/v2/cli/provider"
	"go.keploy.io/server/v2/config"
	userDb "go.keploy.io/server/v2/pkg/platform/yaml/configdb/user"

	"go.keploy.io/server/v2/utils"
	"go.keploy.io/server/v2/utils/log"
	//pprof for debugging
	//_ "net/http/pprof"
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
       ▓▌                           ▐█▌   	                █▌
        ▓
`

func main() {

	// Uncomment the following code to enable pprof for debugging
	// go func() {
	// 	fmt.Println("Starting pprof server for debugging...")
	// 	err := http.ListenAndServe("localhost:6060", nil)
	// 	if err != nil {
	// 		fmt.Println("Failed to start the pprof server for debugging", err)
	// 		return
	// 	}
	// }()

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
	}
}

func start(ctx context.Context) {
	logger, err := log.New()
	if err != nil {
		fmt.Println("Failed to start the logger for the CLI", err)
		return
	}
	defer func() {
		if err := utils.DeleteFileIfNotExists(logger, "keploy-logs.txt"); err != nil {
			utils.LogError(logger, err, "Failed to delete Keploy Logs")
			return
		}
		if err := utils.DeleteFileIfNotExists(logger, "docker-compose-tmp.yaml"); err != nil {
			utils.LogError(logger, err, "Failed to delete Temporary Docker Compose")
			return
		}

	}()
	defer utils.Recover(logger)

	// The 'umask' command is commonly used in various operating systems to regulate the permissions of newly created files.
	// These 'umask' values subtract from the permissions assigned by the process, effectively lowering the permissions.
	// For example, if a file is created with permissions '777' and the 'umask' is '022', the resulting permissions will be '755',
	// reducing certain permissions for security purposes.
	// Setting 'umask' to '0' ensures that 'keploy' can precisely control the permissions of the files it creates.
	// However, it's important to note that this approach may not work in scenarios involving mounted volumes,
	// as the 'umask' is set by the host system, and cannot be overridden by 'keploy' or individual processes.
	oldMask := utils.SetUmask()
	defer utils.RestoreUmask(oldMask)

	userDb := userDb.New(logger)
	if dsn != "" {
		utils.SentryInit(logger, dsn)
		//logger = utils.ModifyToSentryLogger(ctx, logger, sentry.CurrentHub().Client(), configDb)
	}
	conf := config.New()

	svcProvider := provider.NewServiceProvider(logger, userDb, conf)
	cmdConfigurator := provider.NewCmdConfigurator(logger, conf)
	rootCmd := cli.Root(ctx, logger, svcProvider, cmdConfigurator)
	if err := rootCmd.Execute(); err != nil {
		if strings.HasPrefix(err.Error(), "unknown command") || strings.HasPrefix(err.Error(), "unknown shorthand") {
			fmt.Println("Error: ", err.Error())
			fmt.Println("Run 'keploy --help' for usage.")
			os.Exit(1)
		}
	}
}
