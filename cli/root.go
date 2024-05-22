package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v2/cli/provider"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func Root(ctx context.Context, logger *zap.Logger, svcFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	conf := config.New()

	var rootCmd = &cobra.Command{
		Use:     "keploy",
		Short:   "Keploy CLI",
		Example: provider.RootExamples,
		Version: utils.Version,
	}

	rootCmd.CompletionOptions.DisableDefaultCmd = true

	rootCmd.SetHelpTemplate(provider.RootCustomHelpTemplate)

	rootCmd.SetVersionTemplate(provider.VersionTemplate)

	currentVersion := utils.Version
	// Show update message only if it's not a dev version
	if !strings.HasSuffix(currentVersion, "dev") {
		// Check for the latest release version
		releaseInfo, err1 := utils.GetLatestGitHubRelease(ctx, logger)
		if err1 != nil {
			logger.Debug("Failed to fetch the latest release version", zap.Error(err1))
		} else {
			if releaseInfo.TagName != currentVersion {
				versionMsg := utils.VersionMsg(releaseInfo.TagName, currentVersion)
				fmt.Println(versionMsg)
			}
		}
	}

	err := cmdConfigurator.AddFlags(rootCmd)
	if err != nil {
		utils.LogError(logger, err, "failed to set flags")
		return nil
	}

	for _, cmd := range Registered {
		c := cmd(ctx, logger, conf, svcFactory, cmdConfigurator)
		rootCmd.AddCommand(c)
	}
	return rootCmd
}
