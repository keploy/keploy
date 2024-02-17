package cli

import (
	"context"
	"errors"
	"fmt"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
	_ "net/http/pprof"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var rootCustomHelpTemplate = `{{.Short}}

Usage:{{if .Runnable}}
  {{.UseLine}}{{end}}{{if .HasAvailableSubCommands}}
  {{.CommandPath}} [command]{{end}}{{if gt (len .Aliases) 0}}

Aliases:
  {{.NameAndAliases}}{{end}}{{if .HasExample}}

Available Commands:{{range .Commands}}{{if .IsAvailableCommand}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{if .HasAvailableFlags}}

Flags:
{{.LocalFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasAvailableLocalFlags}}

Guided Commands:{{range .Commands}}{{if and (not .IsAvailableCommand) (not .Hidden)}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}

Examples:
{{.Example}}

Use "{{.CommandPath}} [command] --help" for more information about a command.{{end}}
`

var rootExamples = `
  Record:
	keploy record -c "docker run -p 8080:8080 --name <containerName> --network keploy-network <applicationImage>" --containerName "<containerName>" --delay 1 --buildDelay 1m

  Test:
	keploy test --c "docker run -p 8080:8080 --name <containerName> --network keploy-network <applicationImage>" --delay 1 --buildDelay 1m

  Generate-Config:
	keploy generate-config -p "/path/to/localdir"
`

func SetFlags(logger *zap.Logger, cmd *cobra.Command, conf *config.Config) error {
	cmd.PersistentFlags().Bool("debug", conf.Debug, "Run in debug mode")
	cmd.PersistentFlags().Bool("telemetry", conf.Telemetry, "Run in telemetry mode")
	cmd.PersistentFlags().String("configPath", conf.ConfigPath, "Path to the local directory where keploy configuration file is stored")
	cmd.PersistentFlags().StringP("path", "p", conf.Path, "Path to local directory where generated testcases/mocks are stored")
	cmd.PersistentFlags().Uint32("port", conf.Port, "GraphQL server port used for executing testcases in unit test library integration")
	cmd.PersistentFlags().MarkHidden("port")
	cmd.PersistentFlags().Uint32("proxyPort", conf.ProxyPort, "Port used by the Keploy proxy server to intercept the outgoing dependency calls")
	cmd.PersistentFlags().StringP("command", "c", conf.Command, "Command to start the user application")
	cmd.PersistentFlags().DurationP("buildDelay", "bd", conf.BuildDelay, "User provided time to wait docker container build")
	cmd.PersistentFlags().String("containerName", conf.ContainerName, "Name of the application's docker container")
	cmd.PersistentFlags().StringP("networkName", "n", conf.NetworkName, "Name of the application's docker network")
	cmd.PersistentFlags().UintSlice("bypassPorts", config.GetByPassPorts(conf), "Ports to bypass the proxy server and ignore the traffic")

	err := cmd.PersistentFlags().MarkHidden("telemetry")
	if err != nil {
		logger.Error("failed to mark hidden flag", zap.Error(err))
		return nil
	}

	err = viper.BindPFlags(cmd.PersistentFlags())
	if err != nil {
		logger.Error("failed to bind flags to config", zap.Error(err))
		return err
	}

	return nil
}

func LogExample(example string) string {
	return fmt.Sprintf("Example usage: %s", example)

}

func CheckCommand(logger *zap.Logger, conf *config.Config, example string) error {
	if conf.Command != "" {
		return nil
	}
	logger.Error("Couldn't find the application command to test")
	if conf.InDocker {
		logger.Info(`Example usage: keploy test -c "docker run -p 8080:8080 --network myNetworkName myApplicationImageName" --delay 6`)
	} else {
		logger.Info(LogExample(example))
	}
	return errors.New("missing required -c flag or appCmd in config file")
}

func CheckPersistent(logger *zap.Logger, conf *config.Config, cmd *cobra.Command) error {
	if err := viper.Unmarshal(conf); err != nil {
		logger.Error("failed to unmarshal the config", zap.Error(err))
		return err
	}
	// get the example for particular command

	err := CheckCommand(logger, conf, cmd.Example)
	if err != nil {
		return err
	}
	bypassPorts, err := cmd.Flags().GetUintSlice("passThroughPorts")
	if err != nil {
		logger.Error("failed to read the ports of outgoing calls to be ignored")
		return err
	}
	config.SetByPassPorts(conf, bypassPorts)

	if conf.InDocker {
		if len(conf.Path) > 0 {
			curDir, err := os.Getwd()
			if err != nil {
				logger.Error("failed to get current working directory", zap.Error(err))
				return err
			}
			// Check if the path contains the moving up directory (..)
			if strings.Contains(conf.Path, "..") {
				conf.Path, err = filepath.Abs(filepath.Clean(conf.Path))
				if err != nil {
					logger.Error("failed to get the absolute path from relative path", zap.Error(err), zap.String("path:", conf.Path))
					return err
				}
				relativePath, err := filepath.Rel(curDir, conf.Path)
				if err != nil {
					logger.Error("failed to get the relative path from absolute path", zap.Error(err), zap.String("path:", conf.Path))
					return err
				}
				if relativePath == ".." || strings.HasPrefix(relativePath, "../") {
					logger.Error("path provided is not a subdirectory of current directory. Keploy only supports recording testcases in the current directory or its subdirectories", zap.String("path:", conf.Path))
					return err
				}
			}
		}
		if conf.BuildDelay <= 30*time.Second {
			logger.Warn(fmt.Sprintf("buildDelay is set to %v, incase your docker container takes more time to build use --buildDelay to set custom delay", conf.BuildDelay))
			logger.Info(`Example usage: keploy record -c "docker-compose up --build" --buildDelay 35s`)
		}

		if strings.Contains(conf.Command, "--name") {
			if conf.ContainerName == "" {
				logger.Error("Couldn't find containerName")
				logger.Info(`Example usage: keploy record -c "docker run -p 8080:8080 --network myNetworkName myApplicationImageName" --delay 6`)
				return errors.New("missing required --containerName flag or containerName in config file")
			}
		}

	}

	logger.Debug("initialized with configuration", zap.Any("conf", conf))

	err = utils.StartInDocker(logger, conf)
	if err != nil {
		return err
	}

	absPath, err := filepath.Abs(conf.Path)
	if err != nil {
		logger.Error("failed to get the absolute path from relative path", zap.String("path received", conf.Path), zap.Error(err))
		return err
	}
	conf.Path = absPath + "/keploy"

	return nil
}

func Root(ctx context.Context, logger *zap.Logger, svc Services) *cobra.Command {
	conf := config.New()

	// Root command
	var rootCmd = &cobra.Command{
		Use:     "keploy",
		Short:   "Keploy CLI",
		Example: rootExamples,
		Version: utils.Version,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			return CheckPersistent(logger, conf, cmd)
		},
	}

	rootCmd.CompletionOptions.DisableDefaultCmd = true

	rootCmd.SetHelpTemplate(rootCustomHelpTemplate)

	// Set the version template for version command
	rootCmd.SetVersionTemplate(`{{with .Version}}{{printf "Keploy %s" .}}{{end}}{{"\n"}}`)

	//utils.BindFlagsToViper(log, rootCmd, "")

	err := SetFlags(logger, rootCmd, conf)
	if err != nil {
		logger.Error("failed to set flags", zap.Error(err))
		return nil
	}

	for _, cmd := range Registered {
		c := cmd(ctx, logger, conf, svc)
		utils.BindFlagsToViper(logger, c, "")
		rootCmd.AddCommand(c)
	}
	return rootCmd
}
