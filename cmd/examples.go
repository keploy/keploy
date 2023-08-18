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
	sudo -E keploy record -c "/path/to/user/app/binary"
	
	Test:
	sudo -E keploy test -c "/path/to/user/app/binary" --delay 2

Node Application
	Record:
	sudo -E keploy record -c “npm start --prefix /path/to/node/app"
	
	Test:
	sudo -E keploy test -c “npm start --prefix /path/to/node/app" --delay 2

Java 
	Record:
	sudo -E keploy record -c "java -jar /path/to/java-project/target/jar"

	Test:
	sudo -E keploy test -c "java -jar /path/to/java-project/target/jar" --delay 2

Docker
	Alias:
	alias keploy='sudo docker run --name keploy-ebpf -p 16789:16789 --network keploy-network --privileged --pid=host -it -v "$(pwd)":/files -v /sys/fs/cgroup:/sys/fs/cgroup
	-v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock --rm ghcr.io/keploy/keploy'

	Record:
	keploy record -c "docker run -p 8080:8080 --name myContainerName --network myNetworkName --rm myApplicationImage"

	Test:
	keploy test -c "docker run -p 8080:8080  --name myContainerName --network myNetworkName --rm myApplicationImage" --delay 1
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
