// Package main is the entry point for the keploy application.
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
	_ "net/http/pprof" // Enable pprof endpoint support
)

var version string
var dsn string
var apiServerURI = "http://localhost:8083"
var gitHubClientID = "Iv23liFBvIVhL29i9BAp"

func main() {
	// Optional: Enable pprof server for debugging (available at localhost:6060/debug/pprof)
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

func start(ctx context.Context) error {
	logger, err := log.New()
	if err != nil {
		return fmt.Errorf("failed to start the logger for the CLI: %w", err)
	}
	defer func() {
		if err := utils.DeleteFileIfNotExists(logger, "keploy-logs.txt"); err != nil {
			utils.LogError(logger, err, "Failed to delete Keploy Logs")
		}
		if err := utils.DeleteFileIfNotExists(logger, "docker-compose-tmp.yaml"); err != nil {
			utils.LogError(logger, err, "Failed to delete Temporary Docker Compose")
		}
	}()
	defer utils.Recover(logger)

	oldMask := utils.SetUmask()
	defer utils.RestoreUmask(oldMask)

	if dsn != "" {
		utils.SentryInit(logger, dsn)
	}

	conf := config.New()
	conf.APIServerURL = apiServerURI
	conf.GitHubClientID = gitHubClientID

	userDb := userDb.New(logger, conf)
	conf.InstallationID, err = userDb.GetInstallationID(ctx)
	if err != nil {
		utils.LogError(logger, err, "Failed to get installation ID")
		return err
	}

	auth := auth.New(conf.APIServerURL, conf.InstallationID, logger, conf.GitHubClientID)
	svcProvider := provider.NewServiceProvider(logger, conf, auth)
	cmdConfigurator := provider.NewCmdConfigurator(logger, conf)
	rootCmd := cli.Root(ctx, logger, svcProvider, cmdConfigurator)

	if err := rootCmd.Execute(); err != nil {
		if strings.HasPrefix(err.Error(), "unknown command") || strings.HasPrefix(err.Error(), "unknown shorthand") {
			fmt.Println("Error:", err.Error())
			fmt.Println("Run 'keploy --help' for usage.")
		}
		return err
	}

	return nil
}
