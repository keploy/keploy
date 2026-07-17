//go:build windows

package docker

import (
	"context"
	"path/filepath"
	"testing"
)

func TestPrepareDockerCommand_Windows(t *testing.T) {
	ctx := context.Background()
	cmd := PrepareDockerCommand(ctx, "docker version")

	bin := filepath.Base(cmd.Path)

	if bin == "cmd.exe" || bin == "powershell.exe" {
		t.Fatalf("PrepareDockerCommand should not use shell binaries, got: %s", cmd.Path)
	}
}
