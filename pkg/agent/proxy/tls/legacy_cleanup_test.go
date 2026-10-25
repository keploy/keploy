package tls

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"go.uber.org/zap"
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

// TestRemoveLegacyTempFiles_TakesEveryKeployTruststoreOnlyWhenAsked: `keploy
// ca clean` removes every Java truststore keploy left in the temp dir, the
// shared name the oldest versions used included, and the run files of ended
// runs, and nothing else: not a live run's store; the boot sweep, which may
// run beside a live run of an older keploy, removes none of them.
func TestRemoveLegacyTempFiles_TakesEveryKeployTruststoreOnlyWhenAsked(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("os.TempDir does not honour TMPDIR on Windows")
	}
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	stores := []string{"keploy-java-truststore.jks", "keploy-java-truststore-1000.jks", "keploy-java-truststore-123456.jks", "keploy-java-truststore--1.jks"}
	const other = "the-users-own.jks"
	for _, name := range append([]string{other}, stores...) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("jks"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	exists := func(name string) bool {
		_, err := os.Stat(filepath.Join(dir, name))
		return err == nil
	}
	// A live run's store, under its lock, and run files of ended runs, in
	// this user's private dir; and an unlocked run's store there, under an
	// older name.
	priv := privateDirIn(t, dir)
	jks, live, err := createRunFile(zap.NewNop(), priv, runTrustStorePattern)
	if err != nil {
		t.Fatal(err)
	}
	_ = jks.Close()
	t.Cleanup(func() { _ = live.remove() })
	deadRuns := []string{"keploy-java-truststore-run-5.jks", "keploy-ca-run-6.crt"}
	const unlocked = "keploy-java-truststore-77.jks"
	for _, name := range append([]string{unlocked}, deadRuns...) {
		if err := os.WriteFile(filepath.Join(priv, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A run-file name in the temp dir itself, where no run makes one and
	// another user could have: no sweep takes it.
	const notInPriv = "keploy-ca-run-8.crt"
	if err := os.WriteFile(filepath.Join(dir, notInPriv), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	removeLegacyTempFiles(zap.NewNop(), [32]byte{}, false)
	for _, name := range stores {
		if !exists(name) {
			t.Errorf("the boot sweep removed %s, which a live run may use", name)
		}
	}
	if !exists(filepath.Join(filepath.Base(priv), unlocked)) {
		t.Errorf("the boot sweep removed %s in the private dir, which a live run that could not lock may use", unlocked)
	}

	removeLegacyTempFiles(zap.NewNop(), [32]byte{}, true)
	for _, name := range stores {
		if exists(name) {
			t.Errorf("keploy ca clean left the keploy truststore %s", name)
		}
	}
	if !exists(other) {
		t.Errorf("keploy ca clean removed %s, which is not keploy's", other)
	}
	for _, name := range append([]string{unlocked}, deadRuns...) {
		if exists(filepath.Join(filepath.Base(priv), name)) {
			t.Errorf("keploy ca clean left %s in the private dir, which no run holds", name)
		}
	}
	if !exists(notInPriv) {
		t.Errorf("keploy ca clean swept %s in the temp dir itself; the sweep runs only in the private dir", notInPriv)
	}
	if !exists(filepath.Join(filepath.Base(priv), filepath.Base(live.path))) {
		t.Errorf("keploy ca clean removed %s, the store of a live run", live.path)
	}
}

// TestRemoveLegacyTempFiles_TakesOlderNameCAFilesOnlyWhenAsked: `keploy ca
// clean` removes the CA files and merged PEMs keploy versions before the lock
// left under keploy-ca-<N>.crt and keploy-java-cas-<N>.pem, and only the
// exact older names: not a file whose name merely looks like one, not a
// directory or a symlink at such a name (nor the symlink's target), not a
// live run's file. The boot sweep removes none of them, since an older keploy
// running at the same time may still use one.
func TestRemoveLegacyTempFiles_TakesOlderNameCAFilesOnlyWhenAsked(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("os.TempDir does not honour TMPDIR on Windows")
	}
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	older := []string{"keploy-ca-123.crt", "keploy-ca-0.crt", "keploy-java-cas-4567.pem"}
	others := []string{"keploy-ca.crt", "keploy-ca-.crt", "keploy-ca-12a.crt", "keploy-ca-12.crt.bak",
		"my-keploy-ca-1.crt", "keploy-ca-7.pem", "keploy-java-cas-8.crt",
		"keploy-java-truststore-1000.jks.bak", "keploy-java-truststore-x.jks", "keploy-java-truststore--2.jks"}
	for _, name := range append(append([]string{}, older...), others...) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("pem"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	const aDir = "keploy-ca-9.crt"
	if err := os.Mkdir(filepath.Join(dir, aDir), 0o700); err != nil {
		t.Fatal(err)
	}
	others = append(others, aDir)
	// A symlink at an older name, to a file elsewhere: a root clean in a
	// sticky /tmp must take neither.
	target := filepath.Join(t.TempDir(), "elsewhere.crt")
	if err := os.WriteFile(target, []byte("pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	const aLink = "keploy-ca-10.crt"
	if err := os.Symlink(target, filepath.Join(dir, aLink)); err != nil {
		t.Fatal(err)
	}
	others = append(others, aLink)
	exists := func(name string) bool {
		_, err := os.Lstat(filepath.Join(dir, name))
		return err == nil
	}
	// A live run's CA file, under its lock.
	ca, live, err := createRunFile(zap.NewNop(), privateDirIn(t, dir), runCAFilePattern)
	if err != nil {
		t.Fatal(err)
	}
	_ = ca.Close()
	t.Cleanup(func() { _ = live.remove() })

	removeLegacyTempFiles(zap.NewNop(), [32]byte{}, false)
	for _, name := range older {
		if !exists(name) {
			t.Errorf("the boot sweep removed %s, which a live older keploy may use", name)
		}
	}

	removeLegacyTempFiles(zap.NewNop(), [32]byte{}, true)
	for _, name := range older {
		if exists(name) {
			t.Errorf("keploy ca clean left %s, an older keploy's CA trust file", name)
		}
	}
	for _, name := range others {
		if !exists(name) {
			t.Errorf("keploy ca clean removed %s, which is not an older keploy's CA trust file", name)
		}
	}
	if _, err := os.Lstat(live.path); err != nil {
		t.Errorf("keploy ca clean removed %s, the CA file of a live run: %v", live.path, err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("keploy ca clean removed %s, the target of a symlink at an older name: %v", target, err)
	}
}
