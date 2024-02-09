package cmd

import (
	"errors"
	_ "net/http/pprof"
	"os"

	sentry "github.com/getsentry/sentry-go"
	"github.com/spf13/cobra"
	"go.keploy.io/server/utils"
	"go.uber.org/zap"
)


type Root struct {
}


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

func checkForDebugFlag(args []string) bool {
	for _, arg := range args {
		if arg == "--debug" {
			return true
		}
	}
	return false
}

func (r *Root) GetCmd(logger *zap.Logger) *cobra.Command {
	// Root command
	rootCmd := &cobra.Command{
		Use:     "keploy",
		Short:   "Keploy CLI",
		Example: rootExamples,
		Version: utils.Version,
	}

	rootCmd.CompletionOptions.DisableDefaultCmd = true

	rootCmd.SetHelpTemplate(rootCustomHelpTemplate)

	rootCmd.PersistentFlags().BoolVar(&debugMode, "debug", false, "Run in debug mode")

	// Manually parse flags to determine debug mode
	// TODO why parse the flags manually?
	debugMode = checkForDebugFlag(os.Args[1:])

	// Set the version template for version command
	rootCmd.SetVersionTemplate(`{{with .Version}}{{printf "Keploy %s" .}}{{end}}{{"\n"}}`)
	return rootCmd
}

func Execute() {
	// Now that flags are parsed, set up the logger
	logger := utils.SetupLogger()
	logger = utils.ModifyToSentryLogger(logger, sentry.CurrentHub().Client())
	defer utils.DeleteLogs(logger)

	// Setup root command
	root := &Root{}
	rootCmd := root.GetCmd(logger)
	for _, c := range RegisteredCmds {
		rootCmd.AddCommand(c.GetCmd(logger))
	}

	if err := rootCmd.Execute(); err != nil {
		logger.Error("failed to start the CLI.", zap.Any("error", err.Error()))
		os.Exit(1)
	}
}
