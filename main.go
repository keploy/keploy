// Package main is the entry point for the keploy application.
package main

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"runtime/pprof"
	"strings"

	"go.keploy.io/server/v3/cli"
	"go.keploy.io/server/v3/cli/provider"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/platform/auth"
	userDb "go.keploy.io/server/v3/pkg/platform/yaml/configdb/user"
	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/log"
	"go.uber.org/zap"
)

// These are injected during build using ldflags
var version string
var dsn string

var apiServerURI = "http://localhost:8083"
var gitHubClientID = "Iv23liFBvIVhL29i9BAp"

func main() {
	setVersion()
	ctx := utils.NewCtx()
	start(ctx)
	os.Exit(utils.ErrCode)
}

func setVersion() {
	if version == "" {
		version = "3-dev"
	}
	utils.Version = version
	utils.VersionIdentifier = "version"
}

func start(ctx context.Context) {
	logger, logFile, err := log.New()
	if err != nil {
		fmt.Println("Failed to start the logger for the CLI:", err)
		return
	}
	utils.LogFile = logFile

	// Re-exec with sudo if needed (Docker commands)
	if utils.ShouldReexecWithSudo() {
		utils.ReexecWithSudo(logger)
		return
	}

	// CPU Profiling
	if cpuProfile := os.Getenv("CPU_PROFILE"); cpuProfile != "" {
		f, err := os.Create(cpuProfile)
		if err != nil {
			logger.Error("could not create CPU profile", zap.Error(err))
		} else {
			if err := pprof.StartCPUProfile(f); err != nil {
				logger.Error("could not start CPU profile", zap.Error(err))
				f.Close()
			} else {
				defer func() {
					pprof.StopCPUProfile()
					f.Close()
				}()
			}
		}
	}

	// Heap Profiling
	if heapProfile := os.Getenv("HEAP_PROFILE"); heapProfile != "" {
		defer func() {
			f, err := os.Create(heapProfile)
			if err != nil {
				logger.Error("could not create heap profile", zap.Error(err))
				return
			}
			defer f.Close()
			runtime.GC()
			if err := pprof.WriteHeapProfile(f); err != nil {
				logger.Error("could not write heap profile", zap.Error(err))
			}
		}()
	}

	// Cleanup
	defer func() {
		if os.Getenv("KEPLOY_INDOCKER") != "true" {
			if utils.LogFile != nil {
				_ = utils.LogFile.Close()
			}
			_ = utils.DeleteFileIfNotExists(logger, "keploy-logs.txt")
			_ = utils.DeleteFileIfNotExists(logger, "docker-compose-tmp.yaml")
		}
	}()

	defer utils.Recover(logger)

	// Set Umask
	oldMask := utils.SetUmask()
	defer utils.RestoreUmask(oldMask)

	// Initialize Sentry if DSN provided
	if dsn != "" {
		utils.SentryInit(logger, dsn)
	}

	// Load config
	conf := config.New()
	conf.APIServerURL = apiServerURI
	conf.GitHubClientID = gitHubClientID
	conf.Test.CmdUsed = utils.GetFullCommandUsed()

	// Get Installation ID
	userDB := userDb.New(logger, conf)
	conf.InstallationID, err = userDB.GetInstallationID(ctx)
	if err != nil {
		utils.LogError(logger, err, "failed to get installation id")
		os.Exit(1)
	}

	// Auth setup
	authSvc := auth.New(conf.APIServerURL, conf.InstallationID, logger, conf.GitHubClientID)

	// Service Providers
	svcProvider := provider.NewServiceProvider(logger, conf, authSvc)
	cmdConfigurator := provider.NewCmdConfigurator(logger, conf)

	// Root CLI Command
	rootCmd := cli.Root(ctx, logger, svcProvider, cmdConfigurator)

	if err := rootCmd.Execute(); err != nil {
		if strings.HasPrefix(err.Error(), "unknown command") ||
			strings.HasPrefix(err.Error(), "unknown shorthand") {
			fmt.Println("Error:", err.Error())
			fmt.Println("Run 'keploy --help' for usage.")
			os.Exit(1)
		}
	}

	// Restore folder ownership (for sudo runs)
	if conf.Path != "" {
		utils.RestoreKeployFolderOwnership(logger, conf.Path)
	}
}