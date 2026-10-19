package provider

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.keploy.io/server/v3/config"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

const testPlatformNotice = "the proven model is not on by default here"

// noticeRun runs ValidateFlags for the command path (["test"], ["mock",
// "record"], ...) on a platform whose notice is notice, and returns how often
// keploy said it.
func noticeRun(t *testing.T, notice string, path ...string) int {
	t.Helper()
	saved := platformNotice
	platformNotice = func() string { return notice }
	t.Cleanup(func() { platformNotice = saved })
	original := NativeCommandSupported
	t.Cleanup(func() { NativeCommandSupported = original })
	RegisterNativeCommandSupport(func(string, string) bool { return true })
	viper.Reset()
	t.Cleanup(viper.Reset)

	cfg := config.New()
	core, logs := observer.New(zapcore.InfoLevel)
	c := NewCmdConfigurator(zap.New(core), cfg)
	var cmd *cobra.Command
	for _, name := range path {
		next := &cobra.Command{Use: name}
		if cmd != nil {
			cmd.AddCommand(next)
		}
		cmd = next
	}
	if err := c.AddFlags(cmd); err != nil {
		t.Fatalf("AddFlags: %v", err)
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "keploy"), 0o755); err != nil {
		t.Fatal(err)
	}
	args := []string{"-p", dir}
	if cmd.Flags().Lookup("command") != nil {
		args = append(args, "-c", "python app.py")
		cfg.Command = "python app.py"
	}
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	cfg.Path = dir
	_ = c.ValidateFlags(context.Background(), cmd)
	return logs.FilterMessage(testPlatformNotice).Len()
}

// On a platform that stays out of the flip (Windows, macOS: P221) a run that
// records or judges says so once; other commands, and a platform that says
// nothing (Linux), say nothing.
func TestARunThatRecordsOrJudgesSaysOnceThatItsPlatformStaysOutOfTheFlip(t *testing.T) {
	for _, path := range [][]string{{"test"}, {"record"}, {"mock", "record"}, {"mock", "replay"}} {
		if n := noticeRun(t, testPlatformNotice, path...); n != 1 {
			t.Fatalf("keploy %v said the platform notice %d times, want once", path, n)
		}
		if n := noticeRun(t, "", path...); n != 0 {
			t.Fatalf("keploy %v on Linux said a platform notice %d times", path, n)
		}
	}
	for _, path := range [][]string{{"report"}, {"contract", "test"}, {"config"}} {
		if n := noticeRun(t, testPlatformNotice, path...); n != 0 {
			t.Fatalf("keploy %v, which neither records nor judges, said the platform notice %d times", path, n)
		}
	}
}
