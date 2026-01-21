// Package main is the entry point for the keploy application.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"go.keploy.io/server/v3/cli"
	"go.keploy.io/server/v3/cli/provider"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/platform/auth"
	userDb "go.keploy.io/server/v3/pkg/platform/yaml/configdb/user"
	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/log"
	"go.uber.org/zap"
	//pprof for debugging
	// "net/http"
	// _ "net/http/pprof"
)

// version is the version of the server and will be injected during build by ldflags, same with dsn
// see https://goreleaser.com/customization/build/

var version string
var dsn string
var apiServerURI = "http://localhost:8083"
var gitHubClientID = "Iv23liFBvIVhL29i9BAp"

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
	setVersion()
	ctx := utils.NewCtx()
	start(ctx)
	os.Exit(utils.ErrCode)
}

func setVersion() {
	if version == "" {
		version = "2-dev"
	}
	utils.Version = version
	utils.VersionIdentifier = "version"
}

func start(ctx context.Context) {
	logger, logFile, err := log.New()
	if err != nil {
		fmt.Println("Failed to start the logger for the CLI", err)
		return
	}
	utils.LogFile = logFile
	isAgent := len(os.Args) > 1 && os.Args[1] == "agent"

	defer func() {
		inDocker := os.Getenv("KEPLOY_INDOCKER")
		if inDocker != "true" {
			cleanupLogger := logger
			if stderrLogger, err := log.NewStderrLogger(log.LogCfg.Level.Level()); err == nil {
				cleanupLogger = stderrLogger
			} else {
				cleanupLogger = zap.NewNop()
			}
			if utils.LogFile != nil {
				err := utils.LogFile.Close()
				if err != nil {
					utils.LogError(cleanupLogger, err, "Failed to close Keploy Logs")
				}
				utils.LogFile = nil
			}
			if !isAgent {
				if err := utils.DeleteFileIfNotExists(cleanupLogger, "keploy-logs.txt"); err != nil {
					return
				}
				if err := utils.DeleteFileIfNotExists(cleanupLogger, "docker-compose-tmp.yaml"); err != nil {
					return
				}
			}
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

	// Capture the full command used for test runs (to be stored in report)
	conf.Test.CmdUsed = utils.GetFullCommandUsed()
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
			os.Exit(1)
		}
	}
}
