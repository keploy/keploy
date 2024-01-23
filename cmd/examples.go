package cmd

import (
	"github.com/spf13/cobra"
	"go.keploy.io/server/utils"
	"go.uber.org/zap"
)

func NewCmdExample(logger *zap.Logger) *Example {
	return &Example{
		logger: logger,
	}
}

var customHelpTemplate = `
{{if .Example}}Examples:
{{.Example}}
{{end}}
{{if .HasAvailableSubCommands}}Guided Commands:{{range .Commands}}{{if .IsAvailableCommand}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}
{{end}}
{{if .HasAvailableFlags}}Flags:
{{.LocalFlags.FlagUsages | trimTrailingWhitespaces}}
{{end}}
Use "{{.CommandPath}} [command] --help" for more information about a command.
`

var withoutexampleOneClickInstall = `
Note: If installed keploy without One Click Install, use "keploy example --customSetup true"
`
var examples = `
Golang
	Record:
	sudo -E env PATH=$PATH keploy record -c "/path/to/user/app/binary"
	
	Test:
	sudo -E env PATH=$PATH keploy test -c "/path/to/user/app/binary" --delay 2

Node
	Record:
	sudo -E env PATH=$PATH keploy record -c “npm start --prefix /path/to/node/app"
	
	Test:
	sudo -E env PATH=$PATH keploy test -c “npm start --prefix /path/to/node/app" --delay 2

Java 
	Record:
	sudo -E env PATH=$PATH keploy record -c "java -jar /path/to/java-project/target/jar"

	Test:
	sudo -E env PATH=$PATH keploy test -c "java -jar /path/to/java-project/target/jar" --delay 2

Docker
	Alias:
	alias keploy='sudo docker run --name keploy-ebpf -p 16789:16789 --privileged --pid=host -it -v "$(pwd)":/files -v /sys/fs/cgroup:/sys/fs/cgroup
	-v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock --rm ghcr.io/keploy/keploy'

	Record:
	keploy record -c "docker run -p 8080:8080 --name <containerName> --network <networkName> <applicationImage>" --buildDelay 1m

	Test:
	keploy test -c "docker run -p 8080:8080 --name <containerName> --network <networkName> <applicationImage>" --delay 1 --buildDelay 1m

`

var exampleOneClickInstall = `
Golang
	Record:
	keploy record -c "/path/to/user/app/binary"
	
	Test:
	keploy test -c "/path/to/user/app/binary" --delay 2

Node
	Record:
	keploy record -c “npm start --prefix /path/to/node/app"
	
	Test:
	keploy test -c “npm start --prefix /path/to/node/app" --delay 2

Java 
	Record:
	keploy record -c "java -jar /path/to/java-project/target/jar"

	Test:
	keploy test -c "java -jar /path/to/java-project/target/jar" --delay 2

Docker
	Record:
	keploy record -c "docker run -p 8080:8080 --name <containerName> --network <networkName> <applicationImage>" --buildDelay 1m

	Test:
	keploy test -c "docker run -p 8080:8080 --name <containerName> --network <networkName> <applicationImage>" --delay 1 --buildDelay 1m
`

type Example struct {
	logger *zap.Logger
}

func (e *Example) GetCmd() *cobra.Command {
	var customSetup bool
	var exampleCmd = &cobra.Command{
		Use:   "example",
		Short: "Example to record and test via keploy",
		RunE: func(cmd *cobra.Command, args []string) error {
			customSetup, err := cmd.Flags().GetBool("customSetup")
			if err != nil {
				e.logger.Error("failed to read the customSetup flag")
				return err
			}
			modifiedLogger, err := utils.HideInfo()
			if err != nil {
				e.logger.Error("failed to initialize logger")
				return err
			}
			if customSetup {
				modifiedLogger.Info(examples)
			} else {
				modifiedLogger.Info(exampleOneClickInstall)
				modifiedLogger.Info(withoutexampleOneClickInstall)
			}
			return nil
		},
	}
	exampleCmd.SetHelpTemplate(customHelpTemplate)

	exampleCmd.Flags().Bool("customSetup", customSetup, "Check if the user is using one click install")

	return exampleCmd
}
