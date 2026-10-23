package tls

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// TestFileIsExactlyCert is the core safety property of legacy cleanup: it matches
// a single-certificate file by fingerprint, and never matches a multi-cert bundle
// (so a shared system bundle is never deleted) or a non-matching cert.
func TestFileIsExactlyCert(t *testing.T) {
	dir := t.TempDir()
	ca, err := generateRunCA()
	if err != nil {
		t.Fatal(err)
	}
	fp := sha256.Sum256(ca.der)

	// 1. A file that is exactly this cert matches its fingerprint.
	single := filepath.Join(dir, "single.crt")
	if err := os.WriteFile(single, ca.certPEM, 0644); err != nil {
		t.Fatal(err)
	}
	if !fileIsExactlyCert(single, fp) {
		t.Error("a single matching cert file should match its fingerprint")
	}

	// 2. A different fingerprint does not match.
	other, _ := generateRunCA()
	if fileIsExactlyCert(single, sha256.Sum256(other.der)) {
		t.Error("a non-matching fingerprint must not match")
	}

	// 3. A two-cert bundle never matches (guards against deleting a system bundle).
	bundle := filepath.Join(dir, "bundle.crt")
	twoCerts := append(append([]byte{}, ca.certPEM...), other.certPEM...)
	if err := os.WriteFile(bundle, twoCerts, 0644); err != nil {
		t.Fatal(err)
	}
	if fileIsExactlyCert(bundle, fp) {
		t.Error("a multi-cert bundle must never be matched for deletion")
	}

	// 4. A non-PEM file does not match.
	junk := filepath.Join(dir, "junk.crt")
	if err := os.WriteFile(junk, []byte("not a cert"), 0644); err != nil {
		t.Fatal(err)
	}
	if fileIsExactlyCert(junk, fp) {
		t.Error("a non-cert file must not match")
	}
}

// TestLegacyCAFingerprint checks the retired-CA fingerprint constant parses to 32
// bytes — a build-time invariant the whole sweep relies on.
func TestLegacyCAFingerprint(t *testing.T) {
	fp, ok := legacyCAFingerprint()
	if !ok {
		t.Fatal("legacy CA fingerprint constant failed to parse")
	}
	var zero [32]byte
	if fp == zero {
		t.Fatal("legacy CA fingerprint parsed to all zeros")
	}
}

// retiredCACertPEM is the retired static MITM CA's certificate (CN "My Custom
// CA"), as keploy shipped it in pkg/agent/proxy/tls/asset/ca.crt until #4682.
// It is public, as its key was: that is why it is retired. The sweep tests
// plant it in fake JDK keystores so the sweep matches it by the fingerprint
// it matches in production (legacyCASHA256Hex).
const retiredCACertPEM = `-----BEGIN CERTIFICATE-----
MIIBczCCARigAwIBAgIUe1yXSfGefdyA09reeQpit2S7TXMwCgYIKoZIzj0EAwIw
FzEVMBMGA1UEAxMMTXkgQ3VzdG9tIENBMB4XDTIzMDQyMDExMDYwMFoXDTMzMDQx
NzExMDYwMFowFzEVMBMGA1UEAxMMTXkgQ3VzdG9tIENBMFkwEwYHKoZIzj0CAQYI
KoZIzj0DAQcDQgAE+QJh3KtWSGRe9NtzZGcJQBcn0ipsjzpo7YlLvbaSFeI62H1b
5EPUjnbDInG4K0nO1Dq/gPtnsVXZcJx06oHj9qNCMEAwDgYDVR0PAQH/BAQDAgEG
MA8GA1UdEwEB/wQFMAMBAf8wHQYDVR0OBBYEFDejUAabmE7NFoKSOo5uHAHMumal
MAoGCCqGSM49BAMCA0kAMEYCIQDkGU6Oku9vLo7fLLquHWXR1rB/1YgWIBghnQXJ
fF5GawIhAK/0BaJCVm6rkRuYCdtahkN1N5DtTPr8AQoDV+MT0UmL
-----END CERTIFICATE-----
`

// The planted certificate is the retired CA: its fingerprint is the one the
// sweep removes.
func TestRetiredCACertIsTheOneTheSweepRemoves(t *testing.T) {
	block, _ := pem.Decode([]byte(retiredCACertPEM))
	if block == nil {
		t.Fatal("retiredCACertPEM is not PEM")
	}
	if _, err := x509.ParseCertificate(block.Bytes); err != nil {
		t.Fatal(err)
	}
	fp, ok := legacyCAFingerprint()
	if !ok || sha256.Sum256(block.Bytes) != fp {
		t.Fatal("retiredCACertPEM's fingerprint is not legacyCASHA256Hex")
	}
}

// legacyTestHost points the sweep's other targets (the system store, the temp
// directory it clears of the old client's files) at directories of the test's
// own, and gives back the PATH a test adds its JDKs' bin directories to.
func legacyTestHost(t *testing.T) (update string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the fake JDK is /bin/sh scripts, and the trust-store lock is Linux's")
	}
	t.Setenv("TMPDIR", t.TempDir())
	return nativeTrustStoreForTest(t)
}

// retiredJDK lays out a fake JDK at home whose keystore holds the retired CA
// as keployCA, beside an entry of the distro's, and returns it.
func retiredJDK(t *testing.T, home, sharedStore string, fail keytoolFails) fakeJDK {
	t.Helper()
	j := makeFakeJDKAt(t, home, sharedStore, fail)
	writeFakeKeystore(t, j.store, map[string][]byte{legacyCAAlias: []byte(retiredCACertPEM), "debian:distro-root.pem": testCACertPEM()})
	return j
}

// assertRetiredCARemoved fails unless j's keystore no longer holds the retired
// CA and still holds the distro's entry.
func assertRetiredCARemoved(t *testing.T, what string, j fakeJDK) {
	t.Helper()
	if holds(t, j.store, legacyCAAlias) {
		log, _ := os.ReadFile(j.keytool)
		t.Fatalf("%s left the retired CA (%s, whose key is public) in %s\n--- that JDK's keytool calls ---\n%s", what, legacyCAAlias, j.store, log)
	}
	if !holds(t, j.store, "debian:distro-root.pem") {
		t.Fatalf("%s removed an entry that is not the retired CA's from %s: it holds %v", what, j.store, aliasesIn(t, j.store))
	}
}

// A native run's start removes the retired CA from the JDK keystore the run
// imports its own CA into: the one an older keploy imported the retired CA
// into, found by the same rules. On a Debian or Ubuntu host that keystore is
// reached through PATH's keytool link to the JDK, where the old sweep never
// looked.
func TestNativeInstallRemovesTheRetiredCAFromTheJDKItImportsInto(t *testing.T) {
	update := legacyTestHost(t)
	root := t.TempDir()
	j := retiredJDK(t, filepath.Join(root, "usr", "lib", "jvm", "java-21-openjdk-amd64"),
		filepath.Join(root, "etc", "ssl", "certs", "java", "cacerts"), keytoolFails{})
	t.Setenv("PATH", linkThroughAlternatives(t, root, j)+string(os.PathListSeparator)+update)
	t.Setenv("JAVA_HOME", "")

	ca, err := generateRunCA()
	if err != nil {
		t.Fatal(err)
	}
	if err := setupNativeForApp(context.Background(), zap.NewNop(), ca, 0, ""); err != nil {
		t.Fatalf("setupNativeForApp: %v", err)
	}
	assertRetiredCARemoved(t, "the native install", j)
	if !holds(t, j.store, runAlias(ca)) {
		t.Fatalf("the native install did not import this run's CA into %s (it holds %v)", j.store, aliasesIn(t, j.store))
	}
	TeardownNativeCA(zap.NewNop())
}

// The docker-mode start (SweepLegacyCA) removes the retired CA from PATH's
// JDK, reached through its keytool's /etc/alternatives link, and from the JDK
// JAVA_HOME names, as main's sweep looked there first.
func TestDockerBootSweepRemovesTheRetiredCA(t *testing.T) {
	for _, withJavaHome := range []bool{false, true} {
		name := "PATH's JDK, through its alternatives link"
		if withJavaHome {
			name = "PATH's JDK and JAVA_HOME's"
		}
		t.Run(name, func(t *testing.T) {
			update := legacyTestHost(t)
			root := t.TempDir()
			onPath := retiredJDK(t, filepath.Join(root, "usr", "lib", "jvm", "java-21-openjdk-amd64"),
				filepath.Join(root, "etc", "ssl", "certs", "java", "cacerts"), keytoolFails{})
			jdks, javaHome := []fakeJDK{onPath}, ""
			if withJavaHome {
				other := retiredJDK(t, filepath.Join(root, "opt", "jdk-17"), "", keytoolFails{})
				javaHome, jdks = other.home, append(jdks, other)
			}
			t.Setenv("PATH", linkThroughAlternatives(t, root, onPath)+string(os.PathListSeparator)+update)
			t.Setenv("JAVA_HOME", javaHome)
			SweepLegacyCA(context.Background(), zap.NewNop(), false)
			for _, j := range jdks {
				assertRetiredCARemoved(t, "the docker-mode boot sweep", j)
			}
		})
	}
}

// `keploy ca clean` (SweepLegacyCAAggressive) removes the retired CA from the
// JDK JAVA_HOME names and from PATH's.
func TestCAcleanRemovesTheRetiredCAFromJAVA_HOMEsAndPATHsJDK(t *testing.T) {
	update := legacyTestHost(t)
	root := t.TempDir()
	onPath := retiredJDK(t, filepath.Join(root, "usr", "lib", "jvm", "java-21-openjdk-amd64"),
		filepath.Join(root, "etc", "ssl", "certs", "java", "cacerts"), keytoolFails{})
	javaHome := retiredJDK(t, filepath.Join(root, "opt", "jdk-17"), "", keytoolFails{})
	t.Setenv("PATH", linkThroughAlternatives(t, root, onPath)+string(os.PathListSeparator)+update)
	t.Setenv("JAVA_HOME", javaHome.home)

	SweepLegacyCAAggressive(context.Background(), zap.NewNop(), false)
	assertRetiredCARemoved(t, "keploy ca clean, in PATH's JDK", onPath)
	assertRetiredCARemoved(t, "keploy ca clean, in JAVA_HOME's JDK", javaHome)
}

// A keployCA entry that is not the retired CA (a user's own, under the alias
// keploy once used) is left alone.
func TestRetiredCASweepLeavesAnotherKeployCAEntry(t *testing.T) {
	update := legacyTestHost(t)
	root := t.TempDir()
	j := makeFakeJDKAt(t, filepath.Join(root, "opt", "jdk-21"), "", keytoolFails{})
	writeFakeKeystore(t, j.store, map[string][]byte{legacyCAAlias: testCACertPEM()})
	t.Setenv("PATH", filepath.Join(j.home, "bin")+string(os.PathListSeparator)+update)
	t.Setenv("JAVA_HOME", "")

	SweepLegacyCAAggressive(context.Background(), zap.NewNop(), false)
	if !holds(t, j.store, legacyCAAlias) {
		t.Fatalf("the sweep removed a %s entry that is not the retired CA", legacyCAAlias)
	}
}

// A sweep that finds the retired CA in a JDK keystore and cannot delete it
// says so at Warn, naming the alias and the keystore: that JDK goes on
// trusting a CA whose key is public.
func TestRetiredCASweepWarnsWhenItCannotDeleteTheRetiredCA(t *testing.T) {
	update := legacyTestHost(t)
	root := t.TempDir()
	j := retiredJDK(t, filepath.Join(root, "opt", "jdk-21"), "", keytoolFails{delete: true})
	t.Setenv("PATH", filepath.Join(j.home, "bin")+string(os.PathListSeparator)+update)
	t.Setenv("JAVA_HOME", "")

	core, logs := observer.New(zapcore.WarnLevel)
	SweepLegacyCA(context.Background(), zap.New(core), false)
	if !holds(t, j.store, legacyCAAlias) {
		t.Fatal("the fake keytool's -delete was to fail; the test is wrong")
	}
	for _, e := range logs.All() {
		f := e.ContextMap()
		if cacerts, _ := f["cacerts"].(string); f["alias"] == legacyCAAlias && strings.HasSuffix(cacerts, filepath.Join("lib", "security", "cacerts")) {
			return
		}
	}
	t.Fatalf("no Warn naming %s and its keystore; Warn and above logged: %v", legacyCAAlias, logs.All())
}

// sweepTestRebuild makes the fake trust-store rebuild of a sweep test: it
// records that it ran in a file it returns, and does to the bundle what the
// test says: "clean" (writes it without the retired CA), "rewrite" (writes it
// again as it was), "fail" (exits 1), or nothing ("").
func sweepTestRebuild(t *testing.T, update, bundle, does string) (ran string) {
	t.Helper()
	cp, err := exec.LookPath("cp")
	if err != nil {
		t.Skipf("the fake rebuild needs cp: %v", err)
	}
	if err := os.MkdirAll(update, 0o755); err != nil {
		t.Fatal(err)
	}
	ran = filepath.Join(t.TempDir(), "rebuilt")
	script := "#!/bin/sh\n: > '" + ran + "'\n"
	switch does {
	case "clean":
		if err := os.WriteFile(bundle+".new", testCACertPEM(), 0o644); err != nil {
			t.Fatal(err)
		}
		script += "'" + cp + "' '" + bundle + ".new' '" + bundle + "'\n"
	case "rewrite":
		script += "'" + cp + "' '" + bundle + "' '" + bundle + ".new' && '" + cp + "' '" + bundle + ".new' '" + bundle + "'\n"
	case "fail":
		script += "exit 1\n"
	}
	if err := os.WriteFile(filepath.Join(update, "keploy-test-update-ca"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return ran
}

// A sweep rebuilds the system bundle while an earlier removal of the retired
// CA's anchors is still owed its rebuild (legacyRebuildMarker): a sweep or a
// `keploy ca clean` whose rebuild a stop, a failure or a crash cut short
// removed the anchor, and the next, finding no anchor, never rebuilt the
// bundle, which went on trusting a CA whose key is public. A bundle that holds
// it after the rebuild, or with nothing owed, is a Warn that says why, and is
// not rebuilt at every start; a clean one is left alone.
func TestASweepRebuildsTheBundleItOwesARebuild(t *testing.T) {
	type outcome struct{ rebuilt, warned, stillOwed bool }
	for _, tc := range []struct {
		name    string
		owed    bool   // an earlier removal's rebuild is owed
		holds   bool   // the bundle holds the retired CA
		rebuild string // what the fake rebuild does (sweepTestRebuild)
		want    outcome
	}{
		{"owed: the rebuild cleans the bundle", true, true, "clean", outcome{rebuilt: true}},
		{"owed, but the rebuild does not write the bundle", true, true, "", outcome{rebuilt: true, warned: true}},
		{"owed, and an anchor keploy does not manage keeps it", true, true, "rewrite", outcome{rebuilt: true, warned: true}},
		{"owed, and the rebuild fails: still owed", true, true, "fail", outcome{rebuilt: true, stillOwed: true}},
		{"nothing owed, and the bundle holds it all the same", false, true, "clean", outcome{warned: true}},
		{"nothing owed, a clean bundle", false, false, "clean", outcome{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			update := legacyTestHost(t)
			bundle := legacyCABundlePaths[0]
			content := testCACertPEM()
			if tc.holds {
				content = append(append([]byte{}, content...), []byte(retiredCACertPEM)...)
			}
			if err := os.WriteFile(bundle, content, 0o644); err != nil {
				t.Fatal(err)
			}
			if tc.owed {
				if err := oweLegacyRebuild(); err != nil {
					t.Fatal(err)
				}
			}
			ran := sweepTestRebuild(t, update, bundle, tc.rebuild)
			t.Setenv("PATH", update) // the fake rebuild alone: no JDK, no host tool
			t.Setenv("JAVA_HOME", "")

			core, logs := observer.New(zapcore.WarnLevel)
			SweepLegacyCA(context.Background(), zap.New(core), false)
			_, statErr := os.Stat(ran)
			got := outcome{rebuilt: statErr == nil, warned: warnedWith(logs, "bundle", bundle), stillOwed: legacyRebuildOwed()}
			if got != tc.want {
				t.Fatalf("rebuilt %v, warned of the bundle %v, still owed %v; want %v, %v, %v (Warn and above: %v)",
					got.rebuilt, got.warned, got.stillOwed, tc.want.rebuilt, tc.want.warned, tc.want.stillOwed, logs.All())
			}
		})
	}
}

// A removal of the retired CA's anchor owes its rebuild until a rebuild has
// succeeded: one whose rebuild fails leaves it owed, and the next sweep, which
// finds no anchor left, makes it.
func TestARemovedAnchorsRebuildIsOwedUntilItSucceeds(t *testing.T) {
	update := legacyTestHost(t)
	bundle := legacyCABundlePaths[0]
	if err := os.WriteFile(bundle, append(testCACertPEM(), []byte(retiredCACertPEM)...), 0o644); err != nil {
		t.Fatal(err)
	}
	anchor := filepath.Join(caStorePath[0], "ca.crt")
	if err := os.WriteFile(anchor, []byte(retiredCACertPEM), 0o644); err != nil {
		t.Fatal(err)
	}
	sweepTestRebuild(t, update, bundle, "fail")
	ran := sweepTestRebuild(t, update+"-next", bundle, "clean") // the next sweep's, made now: PATH is narrowed below
	t.Setenv("PATH", update)
	t.Setenv("JAVA_HOME", "")
	SweepLegacyCA(context.Background(), zap.NewNop(), false)
	if _, err := os.Stat(anchor); !os.IsNotExist(err) {
		t.Fatalf("the sweep left the retired CA's anchor (stat: %v)", err)
	}
	if !legacyRebuildOwed() {
		t.Fatal("a removal whose rebuild failed does not owe the rebuild: the next sweep, with no anchor to remove, would never make it")
	}

	t.Setenv("PATH", update+"-next")
	SweepLegacyCA(context.Background(), zap.NewNop(), false)
	if _, err := os.Stat(ran); err != nil {
		t.Fatalf("the next sweep did not make the rebuild it owed (stat: %v)", err)
	}
	if legacyRebuildOwed() {
		t.Fatal("a rebuild that succeeded is still owed")
	}
}

// retiredAnchorForTest writes the retired CA as an anchor in the fake host's
// first CA directory, and a bundle that holds it, and returns both.
func retiredAnchorForTest(t *testing.T) (anchor, bundle string) {
	t.Helper()
	bundle = legacyCABundlePaths[0]
	if err := os.WriteFile(bundle, append(testCACertPEM(), []byte(retiredCACertPEM)...), 0o644); err != nil {
		t.Fatal(err)
	}
	anchor = filepath.Join(caStorePath[0], "ca.crt")
	if err := os.WriteFile(anchor, []byte(retiredCACertPEM), 0o644); err != nil {
		t.Fatal(err)
	}
	return anchor, bundle
}

// A sweep that removes the retired CA's anchor rebuilds the bundle even when
// it cannot record the rebuild it owes (a full or read-only /var, a
// /var/lib/keploy that is not a directory), and says so at Warn: the record
// only makes a rebuild that this sweep does not finish happen at the next
// start. The sweep used to read its own missing record as a rebuild made
// meanwhile, and left the bundle trusting a CA whose key is public.
func TestASweepThatCannotRecordTheRebuildItOwesStillRebuilds(t *testing.T) {
	update := legacyTestHost(t)
	anchor, bundle := retiredAnchorForTest(t)
	notADir := filepath.Join(t.TempDir(), "keploy")
	if err := os.WriteFile(notADir, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	legacyRebuildMarker = filepath.Join(notADir, "retired-ca-rebuild-owed") // restored by legacyTestHost's cleanup
	ran := sweepTestRebuild(t, update, bundle, "clean")
	t.Setenv("PATH", update)
	t.Setenv("JAVA_HOME", "")

	core, logs := observer.New(zapcore.WarnLevel)
	SweepLegacyCA(context.Background(), zap.New(core), false)
	if _, err := os.Stat(anchor); !os.IsNotExist(err) {
		t.Fatalf("the sweep left the retired CA's anchor (stat: %v)", err)
	}
	if _, err := os.Stat(ran); err != nil {
		t.Fatalf("a sweep that removed the retired CA's anchor but could not record the rebuild it owes did not rebuild the bundle (stat: %v)", err)
	}
	if logs.FilterMessageSnippet("is owed a rebuild").Len() != 1 {
		t.Fatalf("no Warn that the owed rebuild could not be recorded at %s; Warn and above: %v", legacyRebuildMarker, logs.All())
	}
	if n := logs.FilterMessageSnippet("owes no rebuild").Len(); n != 0 {
		t.Fatalf("a Warn that a record which could not be written cannot be cleared, and that every start rebuilds: %v", logs.All())
	}
}

// An anchor of the retired CA that the sweep cannot delete (a read-only
// mount, a single-file bind mount) owes no rebuild: its removal's Warn says
// so, and no start rebuilds the bundle for it, or warns that keploy has no
// anchor of it. A rebuild owed from before is still made, once.
func TestAnAnchorTheSweepCannotRemoveOwesNoRebuild(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root removes a file from a read-only directory")
	}
	update := legacyTestHost(t)
	anchor, bundle := retiredAnchorForTest(t)
	dir := filepath.Dir(anchor)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if err := oweLegacyRebuild(); err != nil { // an earlier sweep's removal, still owed its rebuild
		t.Fatal(err)
	}
	ran := sweepTestRebuild(t, update, bundle, "rewrite")
	t.Setenv("PATH", update)
	t.Setenv("JAVA_HOME", "")

	for _, sweep := range []struct {
		name    string
		rebuilt bool
	}{{"with a rebuild owed from before", true}, {"with nothing owed", false}} {
		_ = os.Remove(ran)
		core, logs := observer.New(zapcore.WarnLevel)
		SweepLegacyCA(context.Background(), zap.New(core), false)
		_, statErr := os.Stat(ran)
		switch {
		case (statErr == nil) != sweep.rebuilt:
			t.Fatalf("sweep %s: rebuilt %v, want %v", sweep.name, statErr == nil, sweep.rebuilt)
		case !warnedWith(logs, "path", anchor):
			t.Fatalf("sweep %s: no Warn naming the anchor %s it could not remove; Warn and above: %v", sweep.name, anchor, logs.All())
		case warnedWith(logs, "bundle", bundle):
			t.Fatalf("sweep %s warned of the bundle, though it is the anchor keploy could not remove that keeps the CA there: %v", sweep.name, logs.All())
		case legacyRebuildOwed():
			t.Fatalf("sweep %s left a rebuild owed for an anchor it could not remove", sweep.name)
		}
	}
	// Where the debt cannot be recorded either (one read-only mount for both),
	// the removal's Warn is the only one: nothing was removed, so nothing is
	// owed that could be lost.
	notADir := filepath.Join(t.TempDir(), "keploy")
	if err := os.WriteFile(notADir, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	legacyRebuildMarker = filepath.Join(notADir, "retired-ca-rebuild-owed") // restored by legacyTestHost's cleanup
	core, logs := observer.New(zapcore.WarnLevel)
	SweepLegacyCA(context.Background(), zap.New(core), false)
	if logs.Len() != 1 || !warnedWith(logs, "path", anchor) {
		t.Fatalf("a sweep that could neither remove the anchor nor record a debt: want only the removal's Warn, got %v", logs.All())
	}
}

// On a host with none of the trust-store rebuild tools and no bundle that
// holds the retired CA, a removal owes no rebuild: there is nothing to
// rebuild, and no later start could. The sweep says so, at Info: nothing
// trusts the CA. It warns of nothing, then or at a later start.
func TestARemovalOnAHostWithNoRebuildToolOwesNothing(t *testing.T) {
	legacyTestHost(t)
	anchor, bundle := retiredAnchorForTest(t)
	if err := os.Remove(bundle); err != nil { // none: nothing here writes one
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir()) // no rebuild tool, no JDK
	t.Setenv("JAVA_HOME", "")

	core, logs := observer.New(zapcore.InfoLevel)
	SweepLegacyCA(context.Background(), zap.New(core), false)
	if _, err := os.Stat(anchor); !os.IsNotExist(err) {
		t.Fatalf("the sweep left the retired CA's anchor (stat: %v)", err)
	}
	if legacyRebuildOwed() {
		t.Fatal("a removal on a host with no rebuild tool owes a rebuild no start can make")
	}
	if n := logs.FilterLevelExact(zapcore.WarnLevel).Len(); n != 0 {
		t.Fatalf("the sweep warned of a rebuild there is nothing to make: %v", logs.All())
	}
	if n := logs.FilterMessageSnippet("no bundle keploy knows holds").Len(); n != 1 {
		t.Fatalf("no Info that there is nothing to rebuild: %v", logs.All())
	}
	core, logs = observer.New(zapcore.WarnLevel)
	SweepLegacyCA(context.Background(), zap.New(core), false)
	if logs.Len() != 0 {
		t.Fatalf("the next sweep, with nothing to remove or rebuild, warned: %v", logs.All())
	}
}

// A rebuild owed that cannot be cleared once it is made (its record cannot be
// removed) says so at Warn: every later start rebuilds the bundle until the
// record is removed by hand.
func TestARebuildOwedThatCannotBeClearedWarns(t *testing.T) {
	update := legacyTestHost(t)
	bundle := legacyCABundlePaths[0]
	if err := os.WriteFile(bundle, testCACertPEM(), 0o644); err != nil {
		t.Fatal(err)
	}
	// A record that os.Remove cannot remove, root or not: a directory with a
	// file in it.
	if err := os.MkdirAll(filepath.Join(legacyRebuildMarker, "held"), 0o755); err != nil {
		t.Fatal(err)
	}
	ran := sweepTestRebuild(t, update, bundle, "clean")
	t.Setenv("PATH", update)
	t.Setenv("JAVA_HOME", "")
	core, logs := observer.New(zapcore.WarnLevel)
	SweepLegacyCA(context.Background(), zap.New(core), false)
	if _, err := os.Stat(ran); err != nil {
		t.Fatalf("the owed rebuild was not made (stat: %v)", err)
	}
	if !warnedWith(logs, "path", legacyRebuildMarker) {
		t.Fatalf("no Warn that the owed rebuild's record %s could not be cleared; Warn and above: %v", legacyRebuildMarker, logs.All())
	}
}

// A listed anchor of the retired CA that is gone by the time it is removed
// (something that does not take keploy's lock removed it meanwhile: an older
// keploy, an operator) is gone, not a removal that failed: no Warn that it
// stays trusted, and its rebuild is owed.
func TestAnAnchorGoneBeforeItsRemovalIsRemoved(t *testing.T) {
	legacyTestHost(t)
	gone := filepath.Join(caStorePath[0], "ca.crt") // listed, never written
	core, logs := observer.New(zapcore.WarnLevel)
	removed, failed := removeLegacyAnchors(zap.New(core), []string{gone})
	if removed != 1 || failed != 0 {
		t.Fatalf("an anchor gone before its removal: removed %d, failed %d; want 1, 0", removed, failed)
	}
	if logs.Len() != 0 {
		t.Fatalf("an anchor gone before its removal warned: %v", logs.All())
	}
	if !legacyRebuildOwed() {
		t.Fatal("an anchor gone before its removal owes no rebuild")
	}
}

// `keploy ca clean`, asked by hand, rebuilds a bundle that holds the retired
// CA though nothing is owed and no anchor is left (an older keploy's cleanup
// whose rebuild a stop cut short left it so); the automatic sweep only says
// why the bundle holds it, and does not rebuild at every start.
func TestCAcleanRebuildsABundleThatHoldsTheRetiredCA(t *testing.T) {
	update := legacyTestHost(t)
	bundle := legacyCABundlePaths[0]
	if err := os.WriteFile(bundle, append(testCACertPEM(), []byte(retiredCACertPEM)...), 0o644); err != nil {
		t.Fatal(err)
	}
	ran := sweepTestRebuild(t, update, bundle, "clean")
	t.Setenv("PATH", update)
	t.Setenv("JAVA_HOME", "")
	SweepLegacyCA(context.Background(), zap.NewNop(), false)
	if _, err := os.Stat(ran); err == nil {
		t.Fatal("the automatic sweep rebuilt a bundle with nothing owed")
	}
	core, logs := observer.New(zapcore.WarnLevel)
	SweepLegacyCAAggressive(context.Background(), zap.New(core), true)
	if _, err := os.Stat(ran); err != nil {
		t.Fatalf("keploy ca clean did not rebuild a bundle that holds the retired CA (stat: %v)", err)
	}
	fp, _ := legacyCAFingerprint()
	if held := legacyBundleHolding(fp) != ""; held || logs.Len() != 0 {
		t.Fatalf("after keploy ca clean the bundle holds the retired CA %v, Warns %v; want neither", held, logs.All())
	}
}

// A removal whose rebuild tool is not on this process's PATH (a root shell
// without /usr/sbin), where a bundle that holds the retired CA shows a tool
// built it, stays owed: the next start, or a `keploy ca clean` with the right
// PATH, makes it. The Warn says so.
func TestARemovalWhoseRebuildToolIsNotOnPATHStaysOwed(t *testing.T) {
	legacyTestHost(t)
	anchor, _ := retiredAnchorForTest(t)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("JAVA_HOME", "")
	core, logs := observer.New(zapcore.WarnLevel)
	SweepLegacyCA(context.Background(), zap.New(core), false)
	if _, err := os.Stat(anchor); !os.IsNotExist(err) {
		t.Fatalf("the sweep left the retired CA's anchor (stat: %v)", err)
	}
	if !legacyRebuildOwed() {
		t.Fatal("a removal whose rebuild tool was not on PATH, with a bundle holding the retired CA, owes no rebuild")
	}
	if logs.FilterMessageSnippet("next keploy start").Len() != 1 {
		t.Fatalf("no Warn that the next start makes the rebuild: %v", logs.All())
	}
}

// A start stopped before its sweep removes the retired CA's anchors removes
// none: removing them without the rebuild after would leave the bundle
// trusting the CA with, where the debt cannot be recorded, nothing to show
// for it. The anchors left make the next sweep do the whole of the work.
func TestAStopBeforeTheSweepRemovesNoAnchor(t *testing.T) {
	update := legacyTestHost(t)
	anchor, bundle := retiredAnchorForTest(t)
	ran := sweepTestRebuild(t, update, bundle, "clean")
	t.Setenv("PATH", update)
	t.Setenv("JAVA_HOME", "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	core, logs := observer.New(zapcore.WarnLevel)
	SweepLegacyCA(ctx, zap.New(core), false)
	if _, err := os.Stat(anchor); err != nil {
		t.Fatalf("a stopped sweep removed the retired CA's anchor (stat: %v)", err)
	}
	if _, err := os.Stat(ran); err == nil {
		t.Fatal("a stopped sweep rebuilt the bundle")
	}
	if legacyRebuildOwed() || logs.Len() != 0 {
		t.Fatalf("a stopped sweep that did nothing owes a rebuild %v, or warned: %v", legacyRebuildOwed(), logs.All())
	}
}

// `keploy ca clean` whose anchor removal and rebuild both fail leaves no debt:
// an anchor it could not delete owes no rebuild, so later starts do not retry
// a rebuild at each start for it.
func TestCAcleanThatRemovesNothingLeavesNoDebt(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root removes a file from a read-only directory")
	}
	update := legacyTestHost(t)
	anchor, bundle := retiredAnchorForTest(t)
	dir := filepath.Dir(anchor)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	ran := sweepTestRebuild(t, update, bundle, "fail")
	t.Setenv("PATH", update)
	t.Setenv("JAVA_HOME", "")
	SweepLegacyCAAggressive(context.Background(), zap.NewNop(), true)
	if _, err := os.Stat(ran); err != nil {
		t.Fatalf("keploy ca clean did not rebuild (stat: %v)", err)
	}
	if legacyRebuildOwed() {
		t.Fatal("keploy ca clean that removed nothing left a rebuild owed: every later start would retry it")
	}
}

// The automatic sweep, finding a bundle that holds the retired CA with no
// anchor and nothing owed, says both things that can keep it there (a rebuild
// that never followed an earlier removal, or an anchor keploy does not
// manage), and points to `keploy ca clean`, which rebuilds it.
func TestABundleLeftHoldingTheRetiredCAPointsToCAclean(t *testing.T) {
	legacyTestHost(t)
	bundle := legacyCABundlePaths[0]
	if err := os.WriteFile(bundle, append(testCACertPEM(), []byte(retiredCACertPEM)...), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	t.Setenv("JAVA_HOME", "")
	core, logs := observer.New(zapcore.WarnLevel)
	SweepLegacyCA(context.Background(), zap.New(core), false)
	for _, e := range logs.All() {
		if next, _ := e.ContextMap()["next_step"].(string); e.ContextMap()["bundle"] == bundle &&
			strings.Contains(e.Message, "not rebuilt since an earlier removal") && strings.Contains(next, "keploy ca clean") {
			return
		}
	}
	t.Fatalf("no Warn naming the bundle that says it may not have been rebuilt and points to keploy ca clean: %v", logs.All())
}

// `keploy ca clean` with no rebuild tool on PATH, on a host whose bundle holds
// the retired CA, warns that the bundle trusts it: it does not say, as for a
// host where no bundle holds it, that there is nothing to rebuild.
func TestCAcleanWithNoRebuildToolWarnsOfABundleThatHoldsTheCA(t *testing.T) {
	legacyTestHost(t)
	bundle := legacyCABundlePaths[0]
	if err := os.WriteFile(bundle, append(testCACertPEM(), []byte(retiredCACertPEM)...), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	t.Setenv("JAVA_HOME", "")
	core, logs := observer.New(zapcore.InfoLevel)
	SweepLegacyCAAggressive(context.Background(), zap.New(core), true)
	w := logs.FilterLevelExact(zapcore.WarnLevel).FilterMessageSnippet(bundle + " trusts that CA")
	if w.Len() != 1 || logs.FilterMessageSnippet("no bundle keploy knows holds").Len() != 0 {
		t.Fatalf("want one Warn that %s trusts the retired CA, and no line that no bundle holds it: %v", bundle, logs.All())
	}
}

// `keploy ca clean` on a host where nothing holds the retired CA, whose
// rebuild fails (a read-only /etc/ssl/certs in a container) or whose lock it
// cannot take (run without sudo), does not warn that the bundle trusts the
// CA: nothing keploy knows does. It says so at Info.
func TestCAcleanOnACleanHostSaysNothingTrustsTheCA(t *testing.T) {
	for _, tc := range []string{"the rebuild fails", "the lock cannot be taken"} {
		t.Run(tc, func(t *testing.T) {
			update := legacyTestHost(t)
			bundle := legacyCABundlePaths[0]
			if err := os.WriteFile(bundle, testCACertPEM(), 0o644); err != nil {
				t.Fatal(err)
			}
			ran := sweepTestRebuild(t, update, bundle, "fail")
			t.Setenv("PATH", update)
			t.Setenv("JAVA_HOME", "")
			if tc == "the lock cannot be taken" {
				breakTrustStoreLockForTest(t)
			}
			core, logs := observer.New(zapcore.InfoLevel)
			SweepLegacyCAAggressive(context.Background(), zap.New(core), true)
			if _, err := os.Stat(ran); (err == nil) != (tc == "the rebuild fails") {
				t.Fatalf("rebuilt %v", err == nil)
			}
			if n := logs.FilterLevelExact(zapcore.WarnLevel).Len(); n != 0 {
				t.Fatalf("keploy ca clean on a host where nothing holds the retired CA warned: %v", logs.All())
			}
			if logs.FilterMessageSnippet("no bundle keploy knows holds").Len() != 1 {
				t.Fatalf("no Info that no bundle keploy knows holds the retired CA: %v", logs.All())
			}
		})
	}
}

// A stop that cuts `keploy ca clean` short on a host with nothing left of the
// retired CA does not promise that the next start finishes it: there is
// nothing for one to do. One with an anchor left does.
func TestAStoppedCleanupPromisesTheNextStartOnlyWhatIsLeft(t *testing.T) {
	for _, tc := range []struct {
		name   string
		anchor bool
	}{{"nothing left", false}, {"an anchor left", true}} {
		t.Run(tc.name, func(t *testing.T) {
			legacyTestHost(t)
			if tc.anchor {
				retiredAnchorForTest(t)
			}
			t.Setenv("PATH", t.TempDir())
			t.Setenv("JAVA_HOME", "")
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			core, logs := observer.New(zapcore.DebugLevel)
			SweepLegacyCAAggressive(ctx, zap.New(core), true)
			stopped := logs.FilterMessageSnippet("the stop cut short the cleanup").All()
			if len(stopped) != 1 || strings.Contains(stopped[0].Message, "next keploy start") != tc.anchor {
				t.Fatalf("want one line that the stop cut the cleanup short, promising the next start only with an anchor left (%v): %v", tc.anchor, logs.All())
			}
		})
	}
}
