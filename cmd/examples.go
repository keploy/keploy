package cmd

import (
	"github.com/spf13/cobra"
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

var examples = `
Golang Application
	Record:
	sudo -E keploy record -c "/path/to/user/app"
	
	Test:
	sudo -E keploy test -c "/path/to/user/app" --delay 2

Node Application
	Record:
	go run -exec "sudo -E"  <pathToKeployBinary> record -c “npm start --prefix /path/to/node/app"
	
	Test:
	go run -exec "sudo -E"  <pathToKeployBinary> test -c “npm start --prefix /path/to/node/app" --delay 2

Java 
	Record:
	sudo -E keploy <pathToKeployBinary> record -c "java -jar /path/to/java/jar"

	Test:
	sudo -E keploy <pathToKeployBinary> test -c "java -jar /path/to/java/jar" --delay 2

Docker
	Record:
	keployV2 record -c "docker run -p 8080:8080 --name <containerName> --network <networkName> --rm <applicationImage>"

	Test:
	keployV2 test -c "docker run -p 8080:8080  --name <containerName> --network <networkName> --rm <applicationImage>" --delay 1
`

type Example struct {
	logger *zap.Logger
}

func (r *Example) GetCmd() *cobra.Command {
	var recordCmd = &cobra.Command{
		Use:     "example",
		Short:   "Example to record and test via keploy",
		Example: examples,
	}
	recordCmd.SetHelpTemplate(customHelpTemplate)
	return recordCmd
}
