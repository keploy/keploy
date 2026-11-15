//go:build linux || darwin

package utils

import (
	"context"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// keployFolderOutOfReach makes a keploy folder holding a subfolder the
// current user cannot read, the way a root-owned file from an older sudo run
// looks to it, and puts a sudo first on PATH that notes every call in the
// returned log and succeeds.
func keployFolderOutOfReach(t *testing.T) (keployPath, sudoLog string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-0000 folder, so it cannot stand for a file out of the user's reach")
	}
	root := t.TempDir()
	keployPath = filepath.Join(root, "my app", "keploy")
	locked := filepath.Join(keployPath, "test-set-0")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	sudoLog = filepath.Join(root, "sudo.log")
	script := "#!/bin/sh\necho \"$*\" >> '" + sudoLog + "'\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "sudo"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return keployPath, sudoLog
}

func sudoCalls(t *testing.T, sudoLog string) string {
	t.Helper()
	b, err := os.ReadFile(sudoLog)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func currentUsername(t *testing.T) string {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Skipf("no current user to name: %v", err)
	}
	return u.Username
}

func TestAKeployFolderOutOfReachOnMacOSIsRefusedWithTheCommandNotASudoPrompt(t *testing.T) {
	keployPath, sudoLog := keployFolderOutOfReach(t)
	core, logs := observer.New(zap.InfoLevel)

	err := ensureKeployFolderPermissions(context.Background(), zap.New(core), keployPath, "darwin")

	if err == nil {
		t.Fatal("a keploy folder the user cannot read was accepted on macOS")
	}
	if calls := sudoCalls(t, sudoLog); calls != "" {
		t.Fatalf("keploy ran sudo on macOS, where it must never ask for an administrator password:\n%s", calls)
	}
	var refusal *FolderOutOfReachError
	if !errors.As(err, &refusal) {
		t.Fatalf("the refusal is not a FolderOutOfReachError, so a caller cannot tell it apart: %T %v", err, err)
	}
	if refusal.Folder != keployPath || refusal.User != currentUsername(t) || refusal.Paths != 1 {
		t.Fatalf("the refusal carries folder %q, user %q, %d path(s); want %q, %q, 1", refusal.Folder, refusal.User, refusal.Paths, keployPath, currentUsername(t))
	}
	want := "sudo chown -R " + shellWord(currentUsername(t)) + " '" + keployPath + "'"
	if refusal.FixCommand() != want {
		t.Fatalf("the refusal's command is %q, want %q", refusal.FixCommand(), want)
	}
	if !strings.HasSuffix(err.Error(), "then run keploy again: "+want) {
		t.Fatalf("the refusal does not end with the command for the person to run, %q:\n%v", want, err)
	}
	for _, part := range []string{"1 path(s) in the keploy folder " + keployPath, "does not ask for an administrator password on macOS", "in your own terminal"} {
		if !strings.Contains(err.Error(), part) {
			t.Errorf("the refusal does not say %q:\n%v", part, err)
		}
	}
	if logs.FilterMessageSnippet(filepath.Join(keployPath, "test-set-0")).Len() == 0 {
		t.Errorf("the refusal did not name the folder out of reach; logged: %v", logs.All())
	}
}

func TestAKeployFolderOutOfReachOnLinuxStillAsksSudoToGiveItBack(t *testing.T) {
	keployPath, sudoLog := keployFolderOutOfReach(t)

	if err := ensureKeployFolderPermissions(context.Background(), zap.NewNop(), keployPath, "linux"); err != nil {
		t.Fatalf("the fake sudo succeeded, yet: %v", err)
	}
	want := "-v\n-n chown -R " + currentUsername(t) + " " + keployPath + "\n"
	if calls := sudoCalls(t, sudoLog); calls != want {
		t.Fatalf("sudo calls on Linux = %q, want %q", calls, want)
	}
}

func TestEnsureKeployFolderPermissionsAnswersForThePlatformItRunsOn(t *testing.T) {
	keployPath, sudoLog := keployFolderOutOfReach(t)

	err := EnsureKeployFolderPermissions(context.Background(), zap.NewNop(), keployPath)

	calls := sudoCalls(t, sudoLog)
	if runtime.GOOS == "darwin" {
		if err == nil || calls != "" {
			t.Fatalf("on macOS: err %v, sudo calls %q; want a refusal and no sudo", err, calls)
		}
		return
	}
	if err != nil || calls == "" {
		t.Fatalf("on %s: err %v, sudo calls %q; want sudo asked and no error", runtime.GOOS, err, calls)
	}
}

func TestAKeployFolderInReachAsksNothingOnAnyPlatform(t *testing.T) {
	_, sudoLog := keployFolderOutOfReach(t)
	keployPath := filepath.Join(t.TempDir(), "keploy")
	if err := os.MkdirAll(filepath.Join(keployPath, "test-set-0"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, goos := range []string{"darwin", "linux"} {
		if err := ensureKeployFolderPermissions(context.Background(), zap.NewNop(), keployPath, goos); err != nil {
			t.Errorf("%s: a folder the user owns was refused: %v", goos, err)
		}
	}
	if calls := sudoCalls(t, sudoLog); calls != "" {
		t.Fatalf("sudo was run for a folder the user owns:\n%s", calls)
	}
}

func TestNoPathOutOfReachLogsNoFileList(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	logPermissionErrors(zap.New(core), nil)
	if logs.FilterMessageSnippet("Files with permission issues").Len() != 0 {
		t.Fatalf("an empty list was logged as files with permission issues: %v", logs.All())
	}
}
