package csharp

import (
	"context"
	"fmt"
	"os"
	"os/exec"
)

func downloadDotnetCoverage(ctx context.Context) error {
	args := []string{
		"dotnet",
		"tool",
		"install",
		"--global",
		"dotnet-coverage",
	}

	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to install dotnet-coverage: %w", err)
	}

	return nil
}
