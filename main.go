package main

import (
    "context"
    "fmt"
    "os"
    "strings"

    "go.keploy.io/server/v2/cli"
    "go.keploy.io/server/v2/cli/provider"
    "go.keploy.io/server/v2/config"
    "go.keploy.io/server/v2/pkg/platform/auth"
    userDb "go.keploy.io/server/v2/pkg/platform/yaml/configdb/user"
    "go.keploy.io/server/v2/utils"
    "go.keploy.io/server/v2/utils/log"

    "github.com/spf13/cobra"
)

var version string
var dsn string
var apiServerURI = "http://localhost:8083"
var gitHubClientID = "Iv23liFBvIVhL29i9BAp"

// Variables to allow mocking in tests
var osExit = os.Exit
var logNew = log.New
var fmtPrintln = fmt.Println

// New variables for testability
var getInstallationID = func(ctx context.Context, db *userDb.Db) (string, error) {
    return db.GetInstallationID(ctx)
}

var rootCmdExecute = func(cmd *cobra.Command) error {
    return cmd.Execute()
}

var deleteFileIfNotExists = utils.DeleteFileIfNotExists

func main() {
    setVersion()
    ctx := utils.NewCtx()
    start(ctx)
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
    logger, err := logNew()
    if err != nil {
        fmtPrintln("Failed to start the logger for the CLI", err)
        osExit(1)
        return
    }
    defer func() {
        if err := deleteFileIfNotExists(logger, "keploy-logs.txt"); err != nil {
            utils.LogError(logger, err, "Failed to delete Keploy Logs")
            return
        }
        if err := deleteFileIfNotExists(logger, "docker-compose-tmp.yaml"); err != nil {
            utils.LogError(logger, err, "Failed to delete Temporary Docker Compose")
            return
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
    conf.InstallationID, err = getInstallationID(ctx, userDb)
    if err != nil {
        errMsg := "failed to get installation id"
        utils.LogError(logger, err, errMsg)
        osExit(1)
        return
    }
    auth := auth.New(conf.APIServerURL, conf.InstallationID, logger, conf.GitHubClientID)

    svcProvider := provider.NewServiceProvider(logger, conf, auth)
    cmdConfigurator := provider.NewCmdConfigurator(logger, conf)
    rootCmd := cli.Root(ctx, logger, svcProvider, cmdConfigurator)
    if err := rootCmdExecute(rootCmd); err != nil {
        if strings.HasPrefix(err.Error(), "unknown command") || strings.HasPrefix(err.Error(), "unknown shorthand") {
            fmtPrintln("Error: ", err.Error())
            fmtPrintln("Run 'keploy --help' for usage.")
            osExit(1)
        }
    }
}
