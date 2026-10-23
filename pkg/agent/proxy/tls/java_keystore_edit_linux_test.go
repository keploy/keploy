//go:build linux

package tls

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// The helper process of TestConcurrentKeytoolEditsLoseNothing: one keploy
// agent's edit of a JDK keystore, in a process of its own, as agents on one
// host are. It does nothing unless the test started it.
func TestKeystoreEditHelperProcess(t *testing.T) {
	op := os.Getenv("KEPLOY_TEST_KEYSTORE_EDIT")
	if op == "" {
		return
	}
	trustStateDir = os.Getenv("KEPLOY_TEST_TRUST_STATE_DIR")
	js := javaStore{keytool: os.Getenv("KEPLOY_TEST_KEYTOOL"), cacerts: os.Getenv("KEPLOY_TEST_KEYSTORE")}
	alias := os.Getenv("KEPLOY_TEST_ALIAS")
	// Every helper of a trial says it is ready, then waits for a line on its
	// stdin, which the test writes to all of them at once; stdin's end (the
	// test is gone) ends it, so no helper outlives its test.
	_, _ = os.Stdout.WriteString("ready\n")
	in := bufio.NewReader(os.Stdin)
	if _, err := in.ReadString('\n'); err != nil {
		os.Exit(3)
	}
	go func() {
		_, _ = io.Copy(io.Discard, in)
		os.Exit(3)
	}()
	ctx := context.Background()
	switch op {
	case "import":
		if err := importJavaCA(ctx, zap.NewNop(), js, os.Getenv("KEPLOY_TEST_CERT"), alias); err != nil {
			fmt.Fprintln(os.Stderr, "import:", err)
			os.Exit(1)
		}
	case "delete":
		removeJavaAlias(ctx, zap.NewNop(), js, alias)
	}
	os.Exit(0)
}

// keytoolRun runs the real keytool on a keystore of the test's own (never a
// JDK's) and returns its output.
func keytoolRun(t *testing.T, keytool string, args ...string) (string, error) {
	t.Helper()
	out, err := exec.Command(keytool, args...).CombinedOutput()
	return string(out), err
}

// keytoolHolds reports whether keystore holds alias, by the real keytool.
func keytoolHolds(t *testing.T, keytool, keystore, alias string) bool {
	t.Helper()
	_, err := keytoolRun(t, keytool, "-list", "-keystore", keystore, "-storepass", javaStorePass, "-alias", alias)
	return err == nil
}

// keploy agents' edits of one JDK keystore, each in its own process and all
// started at the same instant, with the real keytool, 20 times over: in each
// trial two agents' starts import their runs' CAs while two others' teardowns
// delete theirs (two import/delete pairs). No edit may be lost. Without one
// lock around every keploy edit, keytool rewrote the keystore under the
// others' edits: on a copy of a CI host's keystore, a delete beside an import
// lost the import 4 times in 20 and the delete 13 times.
//
// It starts about 400 JVMs, one at a time under the lock (over a minute), so
// it runs only where KEPLOY_TEST_KEYTOOL_CONCURRENCY=1 says to: the go-test
// lane's unit-test step, and the remote test boxes. It needs keytool, and
// works on a keystore in the test's own directory, with a trust-store lock of
// its own.
func TestConcurrentKeytoolEditsLoseNothing(t *testing.T) {
	if os.Getenv("KEPLOY_TEST_KEYTOOL_CONCURRENCY") != "1" {
		t.Skip("set KEPLOY_TEST_KEYTOOL_CONCURRENCY=1 to run it: it starts about 400 JVMs")
	}
	keytool, err := exec.LookPath("keytool")
	if err != nil {
		// Asked for, it must run: a lane without keytool must not pass it.
		t.Fatalf("KEPLOY_TEST_KEYTOOL_CONCURRENCY=1, but no keytool on PATH: %v", err)
	}
	dir, lockDir := t.TempDir(), t.TempDir()
	ks := filepath.Join(dir, "cacerts")
	imports := []string{"keploy-mitm-00000000000000a1", "keploy-mitm-00000000000000a2"}
	deletes := []string{"keploy-mitm-00000000000000d1", "keploy-mitm-00000000000000d2"}
	cert := filepath.Join(dir, "ca.crt")
	ca, err := generateRunCA()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cert, ca.certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	importDirect := func(alias string) {
		t.Helper()
		if out, err := keytoolRun(t, keytool, "-importcert", "-noprompt", "-trustcacerts", "-storetype", "JKS",
			"-alias", alias, "-file", cert, "-keystore", ks, "-storepass", javaStorePass); err != nil {
			t.Fatalf("keytool -importcert %s: %v\n%s", alias, err, out)
		}
	}
	deleteDirect := func(alias string) {
		t.Helper()
		if out, err := keytoolRun(t, keytool, "-delete", "-noprompt", "-alias", alias, "-keystore", ks, "-storepass", javaStorePass); err != nil {
			t.Fatalf("keytool -delete %s: %v\n%s", alias, err, out)
		}
	}
	importDirect("debian:seed.pem")

	type helperProc struct {
		name  string
		cmd   *exec.Cmd
		stdin io.WriteCloser
		out   *bufio.Reader
		rest  strings.Builder
	}
	helper := func(op, alias string) *helperProc {
		h := &helperProc{name: op + " " + alias, cmd: exec.Command(os.Args[0], "-test.run=^TestKeystoreEditHelperProcess$", "-test.count=1")}
		h.cmd.Env = append(os.Environ(),
			"KEPLOY_TEST_KEYSTORE_EDIT="+op, "KEPLOY_TEST_TRUST_STATE_DIR="+lockDir,
			"KEPLOY_TEST_KEYTOOL="+keytool, "KEPLOY_TEST_KEYSTORE="+ks,
			"KEPLOY_TEST_ALIAS="+alias, "KEPLOY_TEST_CERT="+cert)
		h.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		h.cmd.Stderr = &h.rest
		var err error
		if h.stdin, err = h.cmd.StdinPipe(); err != nil {
			t.Fatal(err)
		}
		stdout, err := h.cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		h.out = bufio.NewReader(stdout)
		if err := h.cmd.Start(); err != nil {
			t.Fatal(err)
		}
		pgid := h.cmd.Process.Pid
		t.Cleanup(func() {
			// Only a helper not reaped yet: a reaped one's pid may be another
			// process's by now.
			if h.cmd.ProcessState == nil {
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
			}
		})
		if l, err := h.out.ReadString('\n'); err != nil || l != "ready\n" {
			t.Fatalf("the %s helper did not get ready: %q %v\n%s", h.name, l, err, &h.rest)
		}
		return h
	}

	const trials = 20
	var lostImport, lostDelete int
	for i := 0; i < trials; i++ {
		for _, a := range deletes {
			importDirect(a)
		}
		var hs []*helperProc
		for _, a := range imports {
			hs = append(hs, helper("import", a))
		}
		for _, a := range deletes {
			hs = append(hs, helper("delete", a))
		}
		// All are ready and wait for their go: give it to all at once.
		for _, h := range hs {
			if _, err := h.stdin.Write([]byte("go\n")); err != nil {
				t.Fatal(err)
			}
		}
		for _, h := range hs {
			rest, _ := io.ReadAll(h.out)
			err := h.cmd.Wait()
			_ = h.stdin.Close()
			if err != nil {
				t.Fatalf("trial %d: the %s helper: %v\n%s%s", i, h.name, err, rest, &h.rest)
			}
		}
		for _, a := range imports {
			if keytoolHolds(t, keytool, ks, a) {
				deleteDirect(a)
			} else {
				lostImport++
			}
		}
		for _, a := range deletes {
			if keytoolHolds(t, keytool, ks, a) {
				lostDelete++
				deleteDirect(a)
			}
		}
		if !keytoolHolds(t, keytool, ks, "debian:seed.pem") {
			t.Fatalf("trial %d: the keystore lost an entry no agent edited", i)
		}
	}
	t.Logf("%d trials of two imports beside two deletes: %d of %d imports lost, %d of %d deletes lost",
		trials, lostImport, trials*len(imports), lostDelete, trials*len(deletes))
	if lostImport != 0 || lostDelete != 0 {
		t.Fatalf("in %d trials of two agents' imports beside two agents' deletes, %d of %d imports and %d of %d deletes were lost",
			trials, lostImport, trials*len(imports), lostDelete, trials*len(deletes))
	}
}

// A teardown that cannot take the trust-store lock does not edit the keystore,
// and, the keystore still holding this run's CA, says so at Warn.
func TestATeardownThatCannotTakeTheLockWarns(t *testing.T) {
	const alias = "keploy-mitm-00000000000000aa"
	j, js := keystoreForEditTest(t, keytoolFails{}, map[string][]byte{alias: testCACertPEM()})
	breakTrustStoreLockForTest(t)
	core, logs := observer.New(zapcore.WarnLevel)
	removeJavaAlias(context.Background(), zap.New(core), js, alias)
	if !holds(t, j.store, alias) {
		t.Fatal("the teardown edited the keystore without the lock")
	}
	if !warnedWith(logs, "alias", alias) {
		t.Fatalf("no Warn naming %s, left in the keystore; Warn and above logged: %v", alias, logs.All())
	}
}

// A sweep that cannot take the trust-store lock (a non-root `keploy ca clean`,
// a wait given up behind a wedged agent) removes none of the retired CA's
// anchors, which it removes only under the lock, and says so at Warn, naming
// them: not as a failed rebuild, whose next step (a rebuild) would not help
// while they are there.
func TestASweepThatCannotTakeTheLockSaysTheAnchorsStay(t *testing.T) {
	update := legacyTestHost(t)
	anchor, bundle := retiredAnchorForTest(t)
	ran := sweepTestRebuild(t, update, bundle, "clean")
	t.Setenv("PATH", update)
	t.Setenv("JAVA_HOME", "")
	breakTrustStoreLockForTest(t)
	core, logs := observer.New(zapcore.WarnLevel)
	SweepLegacyCA(context.Background(), zap.New(core), false)
	if _, err := os.Stat(anchor); err != nil {
		t.Fatalf("the sweep removed the retired CA's anchor without the lock (stat: %v)", err)
	}
	if _, err := os.Stat(ran); err == nil {
		t.Fatal("the sweep rebuilt the bundle without the lock")
	}
	found := false
	for _, e := range logs.FilterMessageSnippet("could not take the trust-store lock").All() {
		if anchors, _ := e.ContextMap()["anchors"].([]interface{}); len(anchors) == 1 && anchors[0] == anchor {
			found = true
		}
	}
	if !found || logs.FilterMessageSnippet("could not rebuild").Len() != 0 {
		t.Fatalf("want one Warn that the lock could not be taken, naming %s, and none of a failed rebuild; Warn and above: %v", anchor, logs.All())
	}

	// With no anchor left, the lock was for the rebuild: a rebuild owed (the
	// next start tries again), or `keploy ca clean`'s.
	if err := os.Remove(anchor); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name           string
		owed, explicit bool
	}{{"a rebuild owed", true, false}, {"keploy ca clean", false, true}} {
		if err := os.Remove(legacyRebuildMarker); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if tc.owed {
			if err := oweLegacyRebuild(); err != nil {
				t.Fatal(err)
			}
		}
		core, logs := observer.New(zapcore.WarnLevel)
		if tc.explicit {
			SweepLegacyCAAggressive(context.Background(), zap.New(core), true)
		} else {
			SweepLegacyCA(context.Background(), zap.New(core), false)
		}
		w := logs.FilterMessageSnippet("could not take the trust-store lock to rebuild").All()
		if len(w) != 1 || strings.Contains(w[0].Message, "next keploy start") != tc.owed || w[0].ContextMap()["anchors"] != nil {
			t.Fatalf("%s: want one Warn that the lock to rebuild could not be taken, promising the next start only when owed, naming no anchor; Warn and above: %v", tc.name, logs.All())
		}
	}
}

// editJavaKeystore against the real keytool, on keystores of the test's own
// (never a JDK's), JKS and PKCS12: the "Alias name:" lines it reads are the
// real keytool's (run in English, its -J options first), the import and the
// delete land, and the keystore keeps its mode and, on a host with SELinux, a
// label of its own that a file created in its directory would not get.
func TestKeystoreEditAgainstRealKeytool(t *testing.T) {
	keytool, err := exec.LookPath("keytool")
	if err != nil {
		t.Skip("no keytool on PATH")
	}
	trustStoreLockForTest(t)
	for _, storeType := range []string{"JKS", "PKCS12"} {
		t.Run(storeType, func(t *testing.T) {
			dir := t.TempDir()
			ks := filepath.Join(t.TempDir(), "cacerts")
			seed, live := filepath.Join(dir, "seed.crt"), filepath.Join(dir, "live.crt")
			for _, p := range []string{seed, live} {
				ca, err := generateRunCA()
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, ca.certPEM, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if out, err := keytoolRun(t, keytool, "-importcert", "-noprompt", "-trustcacerts", "-storetype", storeType,
				"-alias", "debian:seed.pem", "-file", seed, "-keystore", ks, "-storepass", javaStorePass); err != nil {
				t.Fatalf("keytool -importcert: %v\n%s", err, out)
			}
			if err := os.Chmod(ks, 0o644); err != nil {
				t.Fatal(err)
			}
			label := ""
			if _, err := exec.LookPath("chcon"); err == nil {
				if out, err := exec.Command("chcon", "-t", "cert_t", ks).CombinedOutput(); err == nil {
					label = testXattr(t, ks, "security.selinux")
					t.Logf("the keystore is labelled %s", label)
				} else {
					t.Logf("no SELinux label set (%v: %s): its copy is not checked", err, bytes.TrimSpace(out))
				}
			}
			js := javaStore{keytool: keytool, cacerts: ks}
			ctx := context.Background()

			const alias = "keploy-mitm-0123456789abcdef"
			if err := importJavaCA(ctx, zap.NewNop(), js, live, alias); err != nil {
				t.Fatalf("importJavaCA: %v", err)
			}
			got, err := listJavaAliases(ctx, ctx, keytool, ks)
			if err != nil || !got[alias] || !got["debian:seed.pem"] || len(got) != 2 {
				t.Fatalf("after the import the keystore holds %v (%v); want debian:seed.pem and %s", got, err, alias)
			}
			fi, err := os.Stat(ks)
			if err != nil {
				t.Fatal(err)
			}
			if fi.Mode().Perm() != 0o644 {
				t.Errorf("the edited keystore's mode is %v, was 0644", fi.Mode().Perm())
			}
			if label != "" {
				if now := testXattr(t, ks, "security.selinux"); now != label {
					t.Errorf("the edited keystore is labelled %q, was %q", now, label)
				}
			}
			if ents, _ := os.ReadDir(filepath.Dir(ks)); len(ents) != 1 {
				t.Errorf("the edit left files beside the keystore: %v", ents)
			}

			core, logs := observer.New(zapcore.WarnLevel)
			removeJavaAlias(ctx, zap.New(core), js, alias)
			if got, err := listJavaAliases(ctx, ctx, keytool, ks); err != nil || got[alias] || !got["debian:seed.pem"] {
				t.Fatalf("after the delete the keystore holds %v (%v); want debian:seed.pem alone", got, err)
			}
			if logs.Len() != 0 {
				t.Fatalf("the delete warned: %v", logs.All())
			}
		})
	}
}

// An edit made under a context that holds the trust-store lock already (the
// whole of an install's change of the store, as draft #4755 takes it) does
// not take it again: the lock is not reentrant, and a second take would wait
// for itself for ever.
func TestAnEditUnderTheLockDoesNotTakeItAgain(t *testing.T) {
	_, js := keystoreForEditTest(t, keytoolFails{}, nil)
	work, unlock, err := lockTrustStore(context.Background(), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	waits := make(chan struct{})
	var once sync.Once
	core, _ := observer.New(zapcore.DebugLevel)
	logger := zap.New(core, zap.Hooks(func(e zapcore.Entry) error {
		if e.Message == msgTrustStoreBusy {
			once.Do(func() { close(waits) })
		}
		return nil
	}))
	caPath := certFileForTest(t, testCACertPEM())
	done := make(chan error, 1)
	go func() { done <- importJavaCA(work, logger, js, caPath, "keploy-mitm-00000000000000aa") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("importJavaCA under the lock: %v", err)
		}
	case <-waits:
		t.Fatal("an edit under the lock's own context waited for the lock it holds")
	}
}

// The steps of an edit (each keytool run) are steps a process queued behind
// it sees, so that a start or a stop waiting its turn is not taken for a
// stalled one while the edit ahead of it makes progress.
func TestAnEditsStepsAreSeenByTheProcessesQueuedBehindIt(t *testing.T) {
	const alias = "keploy-mitm-00000000000000aa"
	hold := newKeytoolHold(t, "-import", alias, false)
	_, js := keystoreForEditTest(t, keytoolFails{hold: hold}, nil)
	var moves atomic.Int64
	old := trustStoreQueueMoved
	trustStoreQueueMoved = func() { moves.Add(1) }
	t.Cleanup(func() { trustStoreQueueMoved = old })

	caPath := certFileForTest(t, testCACertPEM())
	editDone := make(chan error, 1)
	go func() { editDone <- importJavaCA(context.Background(), zap.NewNop(), js, caPath, alias) }()
	hold.waitReached(t, editDone)

	// A second process's turn, queued behind the edit (in this process: a
	// flock is per open file, so it waits as another process would).
	waiting := make(chan struct{})
	var once sync.Once
	core, _ := observer.New(zapcore.DebugLevel)
	logger := zap.New(core, zap.Hooks(func(e zapcore.Entry) error {
		if e.Message == msgTrustStoreBusy {
			once.Do(func() { close(waiting) })
		}
		return nil
	}))
	type result struct {
		unlock func()
		err    error
	}
	got := make(chan result, 1)
	go func() {
		_, unlock, err := lockTrustStore(context.Background(), logger)
		got <- result{unlock, err}
	}()
	select {
	case <-waiting:
	case r := <-got:
		t.Fatalf("took the lock the edit holds (err %v)", r.err)
	}
	seen := moves.Load()
	hold.letGo(t)
	if err := <-editDone; err != nil {
		t.Fatalf("importJavaCA: %v", err)
	}
	r := <-got
	if r.err != nil {
		t.Fatal(r.err)
	}
	r.unlock()
	if moves.Load() == seen {
		t.Fatal("the process queued behind the edit saw none of its steps: to the CLI it waited without progress")
	}
}
