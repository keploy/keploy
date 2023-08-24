package cmd

import (
	"log"
	"os"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

var Emoji = "\U0001F430" + " Keploy:"

type Root struct {
	Logger *zap.Logger
	// SubCommands holds a list of registered plugins.
	SubCommands []Plugins
}

var DebugMode bool

func SetupLogger() *zap.Logger {
	logCfg := zap.NewDevelopmentConfig()
	if DebugMode {
		logCfg.Level = zap.NewAtomicLevelAt(zap.DebugLevel)
		logCfg.DisableStacktrace = false
	} else {
		logCfg.Level = zap.NewAtomicLevelAt(zap.InfoLevel)
		logCfg.DisableStacktrace = true
	}

	logger, err := logCfg.Build()
	if err != nil {
		log.Panic(Emoji, "failed to start the logger for the CLI")
		return nil
	}
	return logger
}

func NewRoot() *Root {
	return &Root{
		SubCommands: []Plugins{},
	}
}

// Execute adds all child commands to the root command.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	NewRoot().execute()
}

var RootCustomHelpTemplate = `{{.Short}}

Usage:{{if .Runnable}}
  {{.UseLine}}{{end}}{{if .HasAvailableSubCommands}}
  {{.CommandPath}} [command]{{end}}{{if gt (len .Aliases) 0}}

Aliases:
  {{.NameAndAliases}}{{end}}{{if .HasExample}}

Available Commands:{{range .Commands}}{{if .IsAvailableCommand}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{if .HasAvailableFlags}}

Flags:
{{.LocalFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasAvailableLocalFlags}}

Guided Commands:{{range .Commands}}{{if not .IsAvailableCommand}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}

Examples:
{{.Example}}

Use "{{.CommandPath}} [command] --help" for more information about a command.{{end}}
`

var RootExamples = `
Record:
keployV2 record -c "docker run -p 8080:8080 --name <containerName> --network keploy-network --rm <applicationImage>" --containerName "<containerName>" --delay 1

Test:
keployV2 test --c "docker run -p 8080:8080  --name <containerName> --network keploy-network --rm <applicationImage>" --delay 1
`

func CheckForDebugFlag(args []string) bool {
	for _, arg := range args {
		if arg == "--debug" {
			return true
		}
	}
	return false
}

func (r *Root) execute() {

	// Root command
	var rootCmd = &cobra.Command{
		Use:     "keploy",
		Short:   "Keploy CLI",
		Example: RootExamples,
	}
	rootCmd.SetHelpTemplate(RootCustomHelpTemplate)

	// rootCmd.Flags().IntP("pid", "", 0, "Please enter the process id on which your application is running.")

	rootCmd.PersistentFlags().BoolVar(&DebugMode, "debug", false, "Run in debug mode")

	// Manually parse flags to determine debug mode early
	DebugMode = CheckForDebugFlag(os.Args[1:])
	// Now that flags are parsed, set up the l722ogger
	r.Logger = SetupLogger()

	r.SubCommands = append(r.SubCommands, NewCmdExample(r.Logger), NewCmdTest(r.Logger), NewCmdRecord(r.Logger))

	// add the registered keploy plugins as subcommands to the rootCmd
	for _, sc := range r.SubCommands {
		rootCmd.AddCommand(sc.GetCmd())
	}

	if err := rootCmd.Execute(); err != nil {
		r.Logger.Error(Emoji+"failed to start the CLI.", zap.Any("error", err.Error()))
		os.Exit(1)
	}
}

// Plugins is an interface used to define plugins.
type Plugins interface {
	GetCmd() *cobra.Command
}

// RegisterPlugin registers a plugin by appending it to the list of subCommands.
func (r *Root) RegisterPlugin(p Plugins) {
	r.SubCommands = append(r.SubCommands, p)
}
