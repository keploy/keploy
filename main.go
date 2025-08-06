// Package main is the entry point for the keploy application.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v2/cli"
	"go.keploy.io/server/v2/cli/provider"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/platform/auth"
	userDb "go.keploy.io/server/v2/pkg/platform/yaml/configdb/user"
	"go.keploy.io/server/v2/utils"
	"go.keploy.io/server/v2/utils/log"
	//pprof for debugging
	// _ "net/http/pprof"
)

// version is the version of the server and will be injected during build by ldflags, same with dsn
// see https://goreleaser.com/customization/build/

var version string
var dsn string
var apiServerURI = "http://localhost:8083"
var gitHubClientID = "Iv23liFBvIVhL29i9BAp"

// for testing purposes
var (
	osExit     = os.Exit
	startFn    = start
	logNew     = log.New
	executeCmd = func(cmd *cobra.Command) error { return cmd.Execute() }
	closeFile  = func(f *os.File) error {
		if f != nil {
			return f.Close()
		}
		return nil
	}
	deleteFile        = utils.DeleteFileIfNotExists
	getInstallationID = func(udb *userDb.Db, ctx context.Context) (string, error) {
		return udb.GetInstallationID(ctx)
	}
)

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
	startFn(ctx)
	osExit(utils.ErrCode)
}

func setVersion() {
	if version == "" {
		version = "2-dev"
	}
	utils.Version = version
	utils.VersionIdenitfier = "version"
}

func start(ctx context.Context) {
	logger, logFile, err := logNew()
	if err != nil {
		fmt.Println("Failed to start the logger for the CLI", err)
		return
	}
	defer func() {
		if logFile != nil {
			err := closeFile(logFile)
			if err != nil {
				utils.LogError(logger, err, "Failed to close log file")
				return
			}
		}
		if err := deleteFile(logger, "keploy-logs.txt"); err != nil {
			utils.LogError(logger, err, "Failed to delete Keploy Logs")
			return
		}
		if err := deleteFile(logger, "docker-compose-tmp.yaml"); err != nil {
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
	udb := userDb.New(logger, conf)
	conf.InstallationID, err = getInstallationID(udb, ctx)
	if err != nil {
		errMsg := "failed to get installation id"
		utils.LogError(logger, err, errMsg)
		osExit(1)
		return
	}
	authSvc := auth.New(conf.APIServerURL, conf.InstallationID, logger, conf.GitHubClientID)

	svcProvider := provider.NewServiceProvider(logger, conf, authSvc)
	cmdConfigurator := provider.NewCmdConfigurator(logger, conf)
	rootCmd := cli.Root(ctx, logger, svcProvider, cmdConfigurator)
	if err := executeCmd(rootCmd); err != nil {
		if strings.HasPrefix(err.Error(), "unknown command") || strings.HasPrefix(err.Error(), "unknown shorthand") {
			fmt.Println("Error: ", err.Error())
			fmt.Println("Run 'keploy --help' for usage.")
			osExit(1)
			return
		}
	}
}
