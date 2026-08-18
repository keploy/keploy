//go:build !windows

package utils

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// pathWith points exec.LookPath at a controlled directory (via PATH) so the
// tests can deterministically make `sh` present or absent. t.Setenv restores
// PATH after the test.
func pathWith(t *testing.T, dir string, execs ...string) {
	t.Helper()
	for _, name := range execs {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/true\n"), 0o755); err != nil {
			t.Fatalf("seed executable %s: %v", p, err)
		}
	}
	t.Setenv("PATH", dir)
}

func TestCommandContext_UsesShWhenAvailable(t *testing.T) {
	dir := t.TempDir()
	pathWith(t, dir, "sh")

	cmd, err := CommandContext(context.Background(), "docker compose -f - up")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Shell present: preserve the exact previous behaviour — sh -c <cmd>, with
	// the bare "sh" as argv[0] (not the resolved absolute path).
	want := []string{"sh", "-c", "docker compose -f - up"}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Fatalf("args = %v, want %v", cmd.Args, want)
	}
}

func TestCommandContext_DirectExecWhenNoShell(t *testing.T) {
	dir := t.TempDir() // no `sh` seeded
	pathWith(t, dir)

	// The real cloud-replay command that triggered the bug.
	cmd, err := CommandContext(context.Background(),
		"docker compose -f - up --abort-on-container-exit --exit-code-from global-shipment-master")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{
		"docker", "compose", "-f", "-", "up",
		"--abort-on-container-exit", "--exit-code-from", "global-shipment-master",
	}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Fatalf("args = %v, want %v", cmd.Args, want)
	}
}

func TestCommandContext_NoShellRespectsQuotes(t *testing.T) {
	dir := t.TempDir()
	pathWith(t, dir)

	cmd, err := CommandContext(context.Background(), `myapp --msg "hello world" -n 1`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"myapp", "--msg", "hello world", "-n", "1"}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Fatalf("args = %v, want %v", cmd.Args, want)
	}
}

func TestCommandContext_NoShellRejectsShellConstructs(t *testing.T) {
	dir := t.TempDir()
	pathWith(t, dir)

	// Each of these genuinely needs a shell; without one we must fail loudly
	// rather than pass the operator to the binary as a literal argument.
	for _, cmdStr := range []string{
		"make && ./app",
		"cat file | grep x",
		"app > out.log",
		"app < in.txt",
		"echo $HOME",
		"app; cleanup",
		"(sub shell)",
		"echo `date`",
	} {
		if _, err := CommandContext(context.Background(), cmdStr); err == nil {
			t.Errorf("expected error for %q without a shell, got nil", cmdStr)
		}
	}
}

func TestCommandContext_NoShellEmptyCommand(t *testing.T) {
	dir := t.TempDir()
	pathWith(t, dir)

	if _, err := CommandContext(context.Background(), "   "); err == nil {
		t.Error("expected error for an empty command, got nil")
	}
}
