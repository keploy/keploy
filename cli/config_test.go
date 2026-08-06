package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	"go.uber.org/zap"
)

// configConfigurator registers only the flags Config's RunE reads back.
type configConfigurator struct{}

func (configConfigurator) AddFlags(cmd *cobra.Command) error {
	cmd.Flags().Bool("generate", false, "")
	cmd.Flags().Bool("force", false, "")
	return nil
}
func (configConfigurator) ValidateFlags(context.Context, *cobra.Command) error { return nil }
func (configConfigurator) Validate(context.Context, *cobra.Command) error      { return nil }

// countingFactory records whether the command got as far as building a service,
// i.e. whether it decided to go ahead and write the config.
type countingFactory struct{ called int }

func (f *countingFactory) GetService(context.Context, string) (interface{}, error) {
	f.called++
	// Returning a non-Service value stops the command right here. The tests below
	// only care WHETHER we got this far — i.e. whether the command decided to go
	// ahead and write the config — so they assert on `called`, not on the error
	// the type assertion then (correctly) produces.
	return struct{}{}, nil
}

// withStdin points os.Stdin at a file holding the given content ("" == immediate
// EOF, which is what a CI runner, `docker run` without -i, systemd and cron all
// deliver).
func withStdin(t *testing.T, content string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write stdin file: %v", err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatalf("open stdin file: %v", err)
	}
	old := os.Stdin
	os.Stdin = f
	t.Cleanup(func() { os.Stdin = old; _ = f.Close() })
}

func runConfig(t *testing.T, dir string, f *countingFactory, args ...string) error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := Config(ctx, zap.NewNop(), &config.Config{Path: dir}, f, configConfigurator{})
	if cmd == nil {
		t.Fatal("Config returned a nil command")
	}
	cmd.SetArgs(args)
	cmd.SetOut(nopWriter{})
	cmd.SetErr(nopWriter{})
	return cmd.Execute()
}

func seedConfig(t *testing.T) (dir, path, content string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, "keploy.yml")
	content = "version: original\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	return dir, path, content
}

// Nothing answers the prompt when stdin is at EOF — no terminal, nothing piped.
// That is every scripted invocation (CI, Makefile, `docker run` without -i,
// systemd, cron). It used to surface as a confirmation ERROR, so the command
// failed for a reason the caller could not fix. Keep the config and succeed.
func TestConfig_GenerateWithExistingFileAndNoAnswer_KeepsConfigAndSucceeds(t *testing.T) {
	dir, path, original := seedConfig(t)
	withStdin(t, "")
	f := &countingFactory{}

	if err := runConfig(t, dir, f, "--generate"); err != nil {
		t.Fatalf("non-interactive config --generate failed: %v", err)
	}
	if f.called != 0 {
		t.Error("proceeded to overwrite an existing config without any confirmation")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back config: %v", err)
	}
	if string(got) != original {
		t.Errorf("existing config was modified without confirmation: %q", string(got))
	}
}

// --force is the sanctioned non-interactive override: without it there is no way
// to regenerate an existing config from a script.
func TestConfig_ForceOverwritesWithoutAsking(t *testing.T) {
	dir, _, _ := seedConfig(t)
	withStdin(t, "")
	f := &countingFactory{}

	_ = runConfig(t, dir, f, "--generate", "--force") // stub service errors after this point
	if f.called != 1 {
		t.Errorf("--force did not proceed to regenerate the config (factory calls = %d)", f.called)
	}
}

// A piped answer must still work — `echo y | keploy config --generate` is the
// standard scripted way to answer a prompt, and keying on EOF (rather than on
// "is stdin a terminal") is what preserves it.
func TestConfig_PipedYesStillOverwrites(t *testing.T) {
	dir, _, _ := seedConfig(t)
	withStdin(t, "y\n")
	f := &countingFactory{}

	_ = runConfig(t, dir, f, "--generate") // stub service errors after this point
	if f.called != 1 {
		t.Errorf("piped 'y' did not overwrite (factory calls = %d)", f.called)
	}
}

// A piped "n" must be honoured, and must not be confused with "could not ask".
func TestConfig_PipedNoKeepsConfig(t *testing.T) {
	dir, path, original := seedConfig(t)
	withStdin(t, "n\n")
	f := &countingFactory{}

	if err := runConfig(t, dir, f, "--generate"); err != nil {
		t.Fatalf("piped-no run failed: %v", err)
	}
	if f.called != 0 {
		t.Error("piped 'n' still overwrote the config")
	}
	got, _ := os.ReadFile(path)
	if string(got) != original {
		t.Errorf("config modified after declining: %q", string(got))
	}
}

// With no config present there is nothing to confirm, so generation proceeds
// even at EOF.
func TestConfig_NoExistingFileGeneratesAtEOF(t *testing.T) {
	dir := t.TempDir()
	withStdin(t, "")
	f := &countingFactory{}

	_ = runConfig(t, dir, f, "--generate") // stub service errors after this point
	if f.called != 1 {
		t.Errorf("did not generate when no config existed (factory calls = %d)", f.called)
	}
}
