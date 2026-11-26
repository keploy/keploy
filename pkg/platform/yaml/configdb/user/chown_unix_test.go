//go:build unix

package user

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"go.uber.org/zap"
)

type chownCall struct {
	target string
	uid    int
	gid    int
}

func withSudoHooks(t *testing.T, euid int, homeOwner uint32, pathFn func(string, int, int) error, fileFn func(*os.File, int, int) error) {
	t.Helper()
	oldEuid, oldOwner, oldPath, oldFile := euidFn, dirOwnerUIDFn, chownPathFn, chownFileFn
	euidFn = func() int { return euid }
	dirOwnerUIDFn = func(string) (uint32, bool) { return homeOwner, true }
	chownPathFn = pathFn
	chownFileFn = fileFn
	t.Cleanup(func() {
		euidFn = oldEuid
		dirOwnerUIDFn = oldOwner
		chownPathFn = oldPath
		chownFileFn = oldFile
	})
}

func TestGetInstallationIDSudoOwnershipHandback(t *testing.T) {
	t.Run("sudo creates new keploy dir and chowns both dir and temp file to SUDO_UID/SUDO_GID", func(t *testing.T) {
		home := t.TempDir()
		setHomeEnv(t, home)
		clearFallbackCIEnv(t)
		t.Setenv("KEPLOY_INDOCKER", "")
		t.Setenv("INSTALLATION_ID", "")
		t.Setenv("SUDO_UID", "1000")
		t.Setenv("SUDO_GID", "1001")
		withMachineID(t, "", errors.New("no machine id"))

		var dirCalls, fileCalls []chownCall
		withSudoHooks(t, 0, 1000,
			func(p string, uid, gid int) error {
				dirCalls = append(dirCalls, chownCall{target: p, uid: uid, gid: gid})
				return nil
			},
			func(f *os.File, uid, gid int) error {
				fileCalls = append(fileCalls, chownCall{target: f.Name(), uid: uid, gid: gid})
				return nil
			},
		)

		got, err := New(zap.NewNop(), nil).GetInstallationID(context.Background())
		if err != nil {
			t.Fatalf("GetInstallationID: %v", err)
		}
		assertHex64(t, got)

		wantDir := filepath.Join(home, ".keploy")
		if len(dirCalls) != 1 || dirCalls[0].target != wantDir || dirCalls[0].uid != 1000 || dirCalls[0].gid != 1001 {
			t.Fatalf("dir chown calls = %+v, want single chown of %s to 1000:1001", dirCalls, wantDir)
		}
		if len(fileCalls) != 1 || fileCalls[0].uid != 1000 || fileCalls[0].gid != 1001 {
			t.Fatalf("file chown calls = %+v, want single fchown to 1000:1001", fileCalls)
		}
	})

	t.Run("sudo with preexisting keploy dir only chowns newly written file", func(t *testing.T) {
		home := t.TempDir()
		setHomeEnv(t, home)
		clearFallbackCIEnv(t)
		t.Setenv("KEPLOY_INDOCKER", "")
		t.Setenv("INSTALLATION_ID", "")
		t.Setenv("SUDO_UID", "1000")
		t.Setenv("SUDO_GID", "")
		withMachineID(t, "", errors.New("no machine id"))

		if err := os.MkdirAll(filepath.Join(home, ".keploy"), 0o700); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}

		var dirCalls, fileCalls []chownCall
		withSudoHooks(t, 0, 1000,
			func(p string, uid, gid int) error {
				dirCalls = append(dirCalls, chownCall{target: p, uid: uid, gid: gid})
				return nil
			},
			func(f *os.File, uid, gid int) error {
				fileCalls = append(fileCalls, chownCall{target: f.Name(), uid: uid, gid: gid})
				return nil
			},
		)

		got, err := New(zap.NewNop(), nil).GetInstallationID(context.Background())
		if err != nil {
			t.Fatalf("GetInstallationID: %v", err)
		}
		assertHex64(t, got)
		if len(dirCalls) != 0 {
			t.Fatalf("preexisting dir should not be chowned, got %+v", dirCalls)
		}
		if len(fileCalls) != 1 || fileCalls[0].uid != 1000 || fileCalls[0].gid != -1 {
			t.Fatalf("file chown calls = %+v, want single fchown to 1000:-1", fileCalls)
		}
	})

	t.Run("sudo -H (HOME owned by root) does not chown to SUDO_UID", func(t *testing.T) {
		home := t.TempDir()
		setHomeEnv(t, home)
		clearFallbackCIEnv(t)
		t.Setenv("KEPLOY_INDOCKER", "")
		t.Setenv("INSTALLATION_ID", "")
		t.Setenv("SUDO_UID", "1000")
		t.Setenv("SUDO_GID", "1000")
		withMachineID(t, "", errors.New("no machine id"))

		var calls int
		withSudoHooks(t, 0, 0,
			func(string, int, int) error { calls++; return nil },
			func(*os.File, int, int) error { calls++; return nil },
		)

		got, err := New(zap.NewNop(), nil).GetInstallationID(context.Background())
		if err != nil {
			t.Fatalf("GetInstallationID: %v", err)
		}
		assertHex64(t, got)
		if calls != 0 {
			t.Fatalf("expected 0 chown calls when HOME is owned by root, got %d", calls)
		}
	})

	t.Run("invalid or out-of-range SUDO_UID refuses root write and falls through", func(t *testing.T) {
		for _, badUID := range []string{"nobody", "4294967295", "2147483648"} {
			t.Run(badUID, func(t *testing.T) {
				home := t.TempDir()
				setHomeEnv(t, home)
				clearFallbackCIEnv(t)
				t.Setenv("KEPLOY_INDOCKER", "")
				t.Setenv("INSTALLATION_ID", "")
				t.Setenv("SUDO_UID", badUID)
				withMachineID(t, "", errors.New("no machine id"))
				withHostname(t, "sudo-bad-uid-host", nil)
				withProcFiles(t, nil)

				withSudoHooks(t, 0, 1000,
					func(string, int, int) error { t.Fatal("unexpected dir chown"); return nil },
					func(*os.File, int, int) error { t.Fatal("unexpected file chown"); return nil },
				)

				got, err := New(zap.NewNop(), nil).GetInstallationID(context.Background())
				if err != nil {
					t.Fatalf("GetInstallationID: %v", err)
				}
				assertHex64(t, got)
				if _, err := os.Stat(filepath.Join(home, ".keploy")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("bad SUDO_UID %q must not create ~/.keploy as root, stat err = %v", badUID, err)
				}
			})
		}
	})

	t.Run("dir chown failure removes newly created dir and falls through", func(t *testing.T) {
		home := t.TempDir()
		setHomeEnv(t, home)
		clearFallbackCIEnv(t)
		t.Setenv("KEPLOY_INDOCKER", "")
		t.Setenv("INSTALLATION_ID", "")
		t.Setenv("SUDO_UID", "1000")
		withMachineID(t, "", errors.New("no machine id"))
		withHostname(t, "chown-fail-host", nil)
		withProcFiles(t, nil)

		withSudoHooks(t, 0, 1000,
			func(string, int, int) error { return errors.New("chown denied") },
			func(*os.File, int, int) error { return nil },
		)

		got, err := New(zap.NewNop(), nil).GetInstallationID(context.Background())
		if err != nil {
			t.Fatalf("GetInstallationID: %v", err)
		}
		assertHex64(t, got)
		if _, err := os.Stat(filepath.Join(home, ".keploy")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed dir chown left ~/.keploy behind, stat err = %v", err)
		}
	})
}

func TestGetInstallationIDRejectsFIFOWithoutBlocking(t *testing.T) {
	home := t.TempDir()
	setHomeEnv(t, home)
	clearFallbackCIEnv(t)
	t.Setenv("KEPLOY_INDOCKER", "true")
	t.Setenv("INSTALLATION_ID", "")
	withHostname(t, "fifo-guard-host", nil)
	withProcFiles(t, nil)

	keployDir := filepath.Join(home, ".keploy")
	if err := os.MkdirAll(keployDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	fifoPath := filepath.Join(keployDir, installationIDFileName)
	if err := syscall.Mkfifo(fifoPath, 0o600); err != nil {
		t.Fatalf("Mkfifo: %v", err)
	}

	got, err := New(zap.NewNop(), nil).GetInstallationID(context.Background())
	if err != nil {
		t.Fatalf("GetInstallationID: %v", err)
	}
	assertHex64(t, got)
}
