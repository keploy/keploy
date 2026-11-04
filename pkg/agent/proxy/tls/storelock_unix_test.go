//go:build !windows

package tls

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"
)

// probingCommand writes an executable /bin/sh script at path that logs, to
// log, its first argument (or its own name, with none) and whether the store
// lock is held while it runs (probeStoreLock). As a keytool it prints the
// lines of the file list for a -list of the whole keystore (and exits 1 for
// any other -list, or with no such file), and the file pem for an
// -exportcert. It uses only shell builtins and this test binary: the tests'
// PATH has no cat or grep.
func probingCommand(t *testing.T, path, log, list, pem string) {
	t.Helper()
	q := func(p string) string { return "'" + p + "'" }
	script := "#!/bin/sh\n" +
		"p=$(KEPLOY_TEST_PROBE_STORE_LOCK=" + q(caRunsDir) + " " + q(os.Args[0]) + ")\n" +
		"n=${0##*/}; echo \"${1:-$n} $p\" >> " + q(log) + "\n" +
		"case \"$1\" in\n" +
		"-list)\n" +
		"  [ -f " + q(list) + " ] || exit 1\n" +
		"  for x in \"$@\"; do [ \"$x\" = -alias ] && exit 1; done\n" +
		"  while IFS= read -r l; do printf '%s\\n' \"$l\"; done < " + q(list) + " ;;\n" +
		"-exportcert)\n" +
		"  [ -f " + q(pem) + " ] || exit 1\n" +
		"  while IFS= read -r l; do printf '%s\\n' \"$l\"; done < " + q(pem) + " ;;\n" +
		"esac\nexit 0\n"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// probes is what each run of a probing command logged, by its first argument.
func probes(t *testing.T, log string) map[string][]string {
	t.Helper()
	got := map[string][]string{}
	for _, l := range keytoolCalls(t, log) {
		cmd, probe, _ := strings.Cut(l, " ")
		got[cmd] = append(got[cmd], probe)
	}
	return got
}

// Every command that rewrites a trust store runs holding the store lock: the
// install's keytool -import and its sweep's -delete, the teardown's -delete,
// the retired CA's -delete, and the system store's update and refresh (with
// update-ca-certificates, and with the other tools).
func TestStoreWritesHoldTheStoreLock(t *testing.T) {
	useRunsDir(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	nop := zap.NewNop()
	dir := t.TempDir()
	home := filepath.Join(dir, "jdk")
	keytool := filepath.Join(home, "bin", "keytool")
	cacerts := filepath.Join(home, "lib", "security", "cacerts")
	if err := os.MkdirAll(filepath.Dir(cacerts), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cacerts, []byte("keystore"), 0o644); err != nil {
		t.Fatal(err)
	}
	ks := javaKeystore{keytool: keytool, cacerts: cacerts}
	const alias = "keploy-mitm-0a1b2c"

	check := func(t *testing.T, log, cmd string, n int) {
		t.Helper()
		got := probes(t, log)[cmd]
		if len(got) != n {
			t.Fatalf("%s ran %d times, want %d (logged %v)", cmd, len(got), n, keytoolCalls(t, log))
		}
		for _, p := range got {
			if p != "held" {
				t.Fatalf("%s ran with the store lock %s, want held (logged %v)", cmd, p, keytoolCalls(t, log))
			}
		}
	}

	t.Run("import", func(t *testing.T) {
		log := filepath.Join(t.TempDir(), "log")
		probingCommand(t, keytool, log, "", "")
		if err := importJavaCA(ctx, nop, ks, "ca.crt", alias); err != nil {
			t.Fatal(err)
		}
		check(t, log, "-import", 1)
	})

	t.Run("sweep and teardown deletes", func(t *testing.T) {
		tmp := t.TempDir()
		log, list := filepath.Join(tmp, "log"), filepath.Join(tmp, "list")
		// A dead run's alias (no lock file: TestSweepsLeaveALiveRunsEntries
		// has the live ones) and this run's.
		if err := os.WriteFile(list, []byte("keploy-mitm-dead01, Jan 1, 2026, trustedCertEntry,\n"+alias+", Jan 1, 2026, trustedCertEntry,\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		probingCommand(t, keytool, log, list, "")
		if err := importJavaCA(ctx, nop, ks, "ca.crt", alias); err != nil {
			t.Fatal(err)
		}
		check(t, log, "-delete", 1)
		removeJavaAlias(ctx, nop, ks, alias)
		check(t, log, "-delete", 2)
	})

	t.Run("the retired CA's delete", func(t *testing.T) {
		tmp := t.TempDir()
		log, pemFile := filepath.Join(tmp, "log"), filepath.Join(tmp, "pem")
		der := testCADER()
		if err := os.WriteFile(pemFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
			t.Fatal(err)
		}
		probingCommand(t, keytool, log, "", pemFile)
		t.Setenv("JAVA_HOME", home)
		removeLegacyJavaAlias(ctx, nop, sha256.Sum256(der))
		check(t, log, "-delete", 1)
	})

	t.Run("system store", func(t *testing.T) {
		bin := t.TempDir()
		log := filepath.Join(bin, "log")
		probingCommand(t, filepath.Join(bin, "keploy-test-update-ca"), log, "", "")
		t.Setenv("PATH", bin)
		oldCmds := caStoreUpdateCmd
		caStoreUpdateCmd = []string{"keploy-test-update-ca"}
		t.Cleanup(func() { caStoreUpdateCmd = oldCmds })
		if err := updateCaStore(ctx, nop); err != nil {
			t.Fatal(err)
		}
		if err := refreshCaStore(ctx, nop, false); err != nil {
			t.Fatal(err)
		}
		check(t, log, "keploy-test-update-ca", 2)
		probingCommand(t, filepath.Join(bin, "update-ca-certificates"), log, "", "")
		if err := refreshCaStore(ctx, nop, true); err != nil {
			t.Fatal(err)
		}
		check(t, log, "--fresh", 1)
	})
}

// rmwKeytool writes a fake keytool that keeps its keystore as a text file of
// `keytool -list` lines and rewrites it whole on an -import, as keytool does:
// it reads the file, says so (<keystore>.read.<alias>), waits until another
// import has read it too (up to 3 s), and writes back what it read plus its
// alias. Two imports that run at once both read the file before either
// writes, and the one that writes last drops the other's alias.
func rmwKeytool(t *testing.T, dir string) (keytool, keystore string) {
	t.Helper()
	keytool, keystore = filepath.Join(dir, "keytool"), filepath.Join(dir, "cacerts")
	q := func(p string) string { return "'" + p + "'" }
	if err := os.WriteFile(keystore, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"a=; prev=; for x in \"$@\"; do [ \"$prev\" = -alias ] && a=$x; prev=$x; done\n" +
		"case \"$1\" in\n" +
		"-list)\n" +
		"  [ -z \"$a\" ] || exit 1\n" +
		"  while IFS= read -r l; do printf '%s\\n' \"$l\"; done < " + q(keystore) + " ;;\n" +
		"-delete) exit 0 ;;\n" +
		"-import)\n" +
		"  old=; while IFS= read -r l; do old=\"$old$l\n\"; done < " + q(keystore) + "\n" +
		"  : > " + q(keystore) + ".read.\"$a\"\n" +
		"  i=0\n" +
		"  while [ $i -lt 60 ]; do\n" +
		"    for f in " + q(keystore) + ".read.*; do [ \"$f\" != " + q(keystore) + ".read.\"$a\" ] && [ -e \"$f\" ] && break 2; done\n" +
		"    /bin/sleep 0.05; i=$((i+1))\n" +
		"  done\n" +
		"  printf '%s%s, Jan 1, 2026, trustedCertEntry,\\n' \"$old\" \"$a\" > " + q(keystore) + " ;;\n" +
		"esac\nexit 0\n"
	if err := os.WriteFile(keytool, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return keytool, keystore
}

// Two keploy runs that import their CAs into one JDK keystore at once both
// keep their alias there: the imports take turns (the store lock), where the
// one that wrote last used to drop the other's.
func TestTwoRunsImportsIntoOneKeystoreBothStay(t *testing.T) {
	useRunsDir(t)
	keytool, keystore := rmwKeytool(t, t.TempDir())
	ids := []string{"0a0a01", "0b0b02"}
	type run struct {
		cmd   *exec.Cmd
		in    *os.File
		lines *bufio.Reader
	}
	runs := make([]run, len(ids))
	for i, id := range ids {
		cmd := exec.Command(os.Args[0])
		cmd.Env = append(os.Environ(), "KEPLOY_TEST_IMPORT="+id, "KEPLOY_TEST_RUN_LOCK_DIR="+caRunsDir,
			"KEPLOY_TEST_KEYTOOL="+keytool, "KEPLOY_TEST_CACERTS="+keystore, "KEPLOY_TEST_CA_FILE=ca.crt")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		inR, inW, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stdin = inR
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		_ = inR.Close()
		t.Cleanup(func() {
			_ = inW.Close()
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Wait()
		})
		runs[i] = run{cmd, inW, bufio.NewReader(out)}
	}
	for i, r := range runs {
		if line, err := r.lines.ReadString('\n'); err != nil || line != "ready\n" {
			t.Fatalf("run %s said %q (%v), want \"ready\"", ids[i], line, err)
		}
	}
	for _, r := range runs {
		if _, err := r.in.WriteString("go\n"); err != nil {
			t.Fatal(err)
		}
	}
	for i, r := range runs {
		if line, err := r.lines.ReadString('\n'); err != nil || line != "imported <nil>\n" {
			t.Fatalf("run %s said %q (%v), want \"imported <nil>\"", ids[i], line, err)
		}
	}
	b, err := os.ReadFile(keystore)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if !strings.Contains(string(b), "keploy-mitm-"+id+",") {
			t.Fatalf("the keystore lost run %s's alias:\n%s", id, b)
		}
	}
}

// The store lock's file is root's alone on Linux (0600): a user who could open
// it could hold it, and keep every run's change of a trust store waiting.
func TestStoreLockFileIsItsOwnersAlone(t *testing.T) {
	useRunsDir(t)
	if err := withStoreLock(context.Background(), zap.NewNop(), func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(caRunsDir, storeLockName))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("the store lock's mode is %v, want 0600", fi.Mode().Perm())
	}
}
