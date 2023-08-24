package main

import (
	"github.com/spf13/cobra"
	"go.keploy.io/server/cmd"
	"go.uber.org/zap"
	"os"
	"testing"
)

func codeCoverTestMode(t *testing.T) {
	r := cmd.NewRoot()
	var rootCmd = &cobra.Command{
		Use:     "keploy",
		Short:   "Keploy CLI",
		Example: cmd.RootExamples,
	}
	rootCmd.SetHelpTemplate(cmd.RootCustomHelpTemplate)

	// rootCmd.Flags().IntP("pid", "", 0, "Please enter the process id on which your application is running.")

	rootCmd.PersistentFlags().BoolVar(&cmd.DebugMode, "debug", false, "Run in debug mode")

	// Manually parse flags to determine debug mode early
	cmd.DebugMode = cmd.CheckForDebugFlag(os.Args[1:])
	// Now that flags are parsed, set up the l722ogger
	r.Logger = cmd.SetupLogger()

	r.SubCommands = append(r.SubCommands, cmd.NewCmdExample(r.Logger), cmd.NewCmdTest(r.Logger), cmd.NewCmdRecord(r.Logger))

	// add the registered keploy plugins as subcommands to the rootCmd
	for _, sc := range r.SubCommands {
		rootCmd.AddCommand(sc.GetCmd())
	}

	rootCmd.SetArgs([]string{"test", "-c", "../samples-go/grpc/client/client"})
	if err := rootCmd.Execute(); err != nil {
		r.Logger.Error(cmd.Emoji+"failed to start the CLI.", zap.Any("error", err.Error()))
		os.Exit(1)
	}
}
func codeCoverRecordMode(t *testing.T) {
	// config or other setup before running keploy
	// Root command
	r := cmd.NewRoot()
	var rootCmd = &cobra.Command{
		Use:     "keploy",
		Short:   "Keploy CLI",
		Example: cmd.RootExamples,
	}
	rootCmd.SetHelpTemplate(cmd.RootCustomHelpTemplate)

	// rootCmd.Flags().IntP("pid", "", 0, "Please enter the process id on which your application is running.")

	rootCmd.PersistentFlags().BoolVar(&cmd.DebugMode, "debug", false, "Run in debug mode")

	// Manually parse flags to determine debug mode early
	cmd.DebugMode = cmd.CheckForDebugFlag(os.Args[1:])
	// Now that flags are parsed, set up the l722ogger
	r.Logger = cmd.SetupLogger()

	r.SubCommands = append(r.SubCommands, cmd.NewCmdExample(r.Logger), cmd.NewCmdTest(r.Logger), cmd.NewCmdRecord(r.Logger))

	// add the registered keploy plugins as subcommands to the rootCmd
	for _, sc := range r.SubCommands {
		rootCmd.AddCommand(sc.GetCmd())
	}

	rootCmd.SetArgs([]string{"record", "-c", "../samples-go/grpc/client/client", "--path", "keployTest990"})
	if err := rootCmd.Execute(); err != nil {
		r.Logger.Error(cmd.Emoji+"failed to start the CLI.", zap.Any("error", err.Error()))
		os.Exit(1)
	}
	// assertions after running keploy
}
