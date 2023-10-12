package cmd

import (
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"time"

	"github.com/TheZeroSlave/zapsentry"
	sentry "github.com/getsentry/sentry-go"
	"github.com/spf13/cobra"
	"go.keploy.io/server/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var Emoji = "\U0001F430" + " Keploy:"

type Root struct {
	logger *zap.Logger
	// subCommands holds a list of registered plugins.
	subCommands []Plugins
}

var debugMode bool

func setupLogger() *zap.Logger {
	logCfg := zap.NewDevelopmentConfig()

	// Customize the encoder config to put the emoji at the beginning.
	logCfg.EncoderConfig.EncodeTime = customTimeEncoder
  logCfg.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder

	logCfg.OutputPaths = []string{
		"stdout",
		"/tmp/keploy-logs.txt",
	}
  
	if debugMode {
		go func() {
			defer utils.HandlePanic()
			log.Println(http.ListenAndServe("localhost:6060", nil))
		}()

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

func modifyToSentryLogger(log *zap.Logger, client *sentry.Client) *zap.Logger {
	cfg := zapsentry.Configuration{
		Level:             zapcore.ErrorLevel, //when to send message to sentry
		EnableBreadcrumbs: true,               // enable sending breadcrumbs to Sentry
		BreadcrumbLevel:   zapcore.InfoLevel,  // at what level should we sent breadcrumbs to sentry
		Tags: map[string]string{
			"component": "system",
		},
	}
	core, err := zapsentry.NewCore(cfg, zapsentry.NewSentryClientFromClient(client))

	//in case of err it will return noop core. So we don't need to attach it to logger.
	if err != nil {
		log.Debug("failed to init zap", zap.Error(err))
		return log
	}

	log = zapsentry.AttachCoreToLogger(core, log)


	return log.With(zapsentry.NewScope())
}

func customTimeEncoder(t time.Time, enc zapcore.PrimitiveArrayEncoder) {
	emoji := "\U0001F430" + " Keploy:"
	enc.AppendString(emoji + " " + t.Format(time.RFC3339) + " ")
}

func newRoot() *Root {
	return &Root{
		subCommands: []Plugins{},
	}
}

// Execute adds all child commands to the root command.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	newRoot().execute()
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
keploy record -c "docker run -p 8080:8080 --name <containerName> --network keploy-network <applicationImage>" --containerName "<containerName>" --delay 1

Test:
keploy test --c "docker run -p 8080:8080 --name <containerName> --network keploy-network <applicationImage>" --delay 1
`

func checkForDebugFlag(args []string) bool {
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
		Example: rootExamples,
	}
	rootCmd.SetHelpTemplate(rootCustomHelpTemplate)

	rootCmd.PersistentFlags().BoolVar(&debugMode, "debug", false, "Run in debug mode")

	// Manually parse flags to determine debug mode early
	debugMode = checkForDebugFlag(os.Args[1:])
	// Now that flags are parsed, set up the l722ogger
	r.logger = setupLogger()
	r.logger = modifyToSentryLogger(r.logger, sentry.CurrentHub().Client())
	r.subCommands = append(r.subCommands, NewCmdRecord(r.logger), NewCmdTest(r.logger), NewCmdServe(r.logger), NewCmdExample(r.logger), NewCmdMockRecord(r.logger), NewCmdMockTest(r.logger))

	// add the registered keploy plugins as subcommands to the rootCmd
	for _, sc := range r.subCommands {
		rootCmd.AddCommand(sc.GetCmd())
	}

	if err := rootCmd.Execute(); err != nil {
		r.logger.Error("failed to start the CLI.", zap.Any("error", err.Error()))
		os.Exit(1)
	}
}

// Plugins is an interface used to define plugins.
type Plugins interface {
	GetCmd() *cobra.Command
}

// RegisterPlugin registers a plugin by appending it to the list of subCommands.
func (r *Root) RegisterPlugin(p Plugins) {
	r.subCommands = append(r.subCommands, p)
}
