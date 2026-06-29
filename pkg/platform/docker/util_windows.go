//go:build windows

package docker

import (
	"context"
	"os/exec"
	"strings"
)

func PrepareDockerCommand(ctx context.Context, keployAlias string) *exec.Cmd {
	var args []string
	args = append(args, "/C")

	// Split the alias by spaces, but filter out empty tokens that arise from
	// double-spaces in the volume-mount string (trailing space in tlsVolumeMount
	// concatenation). An empty token before "--rm" is treated by Docker as the
	// image reference and causes "docker: invalid reference format".
	for _, token := range strings.Split(keployAlias, " ") {
		if token != "" {
			args = append(args, token)
		}
	}

	// NOTE: Do NOT append os.Args[1:] here.
	// The non-Windows implementation (util_others.go) uses "sh -c <alias>" and
	// does not pass the CLI's own arguments to the agent container. Appending
	// os.Args[1:] (e.g. "record -c docker run ...") to the Docker run command
	// corrupts the image reference and the agent entrypoint args.

	// Use cmd.exe /C for Windows
	cmd := exec.CommandContext(
		ctx,
		"cmd.exe",
		args...,
	)

	return cmd
}
