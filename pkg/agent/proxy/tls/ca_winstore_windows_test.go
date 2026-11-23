//go:build windows

package tls

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha1"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
	"unsafe"

	"go.uber.org/zap/zaptest"
	"golang.org/x/sys/windows"
)

// freshCertDER is a self-signed certificate no store holds.
func freshCertDER(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "keploy row " + t.Name()},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

// userRootEnds is the first and the last certificate of the current user's
// ROOT store, read here apart from userRootHolds.
func userRootEnds(t *testing.T) (first, last []byte) {
	t.Helper()
	return rootEnds(t, windows.CERT_SYSTEM_STORE_CURRENT_USER)
}

// rootEnds is the first and the last certificate of location's ROOT store,
// read here apart from rootStoreHolds.
func rootEnds(t *testing.T, location uint32) (first, last []byte) {
	t.Helper()
	name, _ := windows.UTF16PtrFromString("ROOT")
	store, err := windows.CertOpenStore(windows.CERT_STORE_PROV_SYSTEM, 0, 0,
		location|windows.CERT_STORE_READONLY_FLAG, uintptr(unsafe.Pointer(name)))
	runtime.KeepAlive(name)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = windows.CertCloseStore(store, 0) }()
	for c, _ := windows.CertEnumCertificatesInStore(store, nil); c != nil; c, _ = windows.CertEnumCertificatesInStore(store, c) {
		last = append([]byte(nil), unsafe.Slice(c.EncodedCert, c.Length)...)
		if first == nil {
			first = last
		}
	}
	if first == nil {
		t.Fatalf("the ROOT store of location %#x holds no certificate", location)
	}
	return first, last
}

func TestOnWindowsTheRootStoresAreReadForTheCAsTheyHold(t *testing.T) {
	first, last := userRootEnds(t)
	for name, der := range map[string][]byte{"first": first, "last": last} {
		if held, err := userRootHolds(der); err != nil || !held {
			t.Errorf("userRootHolds(the store's %s certificate) = %v, %v; want true", name, held, err)
		}
	}
	first, last = rootEnds(t, windows.CERT_SYSTEM_STORE_LOCAL_MACHINE)
	for name, der := range map[string][]byte{"first": first, "last": last} {
		if held, err := machineRootHolds(der); err != nil || !held {
			t.Errorf("machineRootHolds(the machine store's %s certificate) = %v, %v; want true", name, held, err)
		}
	}
	fresh := freshCertDER(t)
	if held, err := userRootHolds(fresh); err != nil || held {
		t.Errorf("userRootHolds(a fresh certificate) = %v, %v; want false, nil", held, err)
	}
	if held, err := machineRootHolds(fresh); err != nil || held {
		t.Errorf("machineRootHolds(a fresh certificate) = %v, %v; want false, nil", held, err)
	}
}

// certutil exits with the HRESULT it failed with; cmd's exit passes one on.
func TestOnWindowsCertutilsNotSupportedExitIsANoTrustPrompt(t *testing.T) {
	err := exec.Command("cmd", "/c", "exit", "-2147024846").Run()
	if !noTrustPrompt(nil, err) {
		t.Fatalf("noTrustPrompt(no output, exit 0x80070032 = %v) = false, want true", err)
	}
	if noTrustPrompt(nil, exec.Command("cmd", "/c", "exit", "-2147024891").Run()) {
		t.Fatal("noTrustPrompt(no output, exit 0x80070005) = true, want false")
	}
}

// On the Windows lane, run once as the agent's elevated administrator
// (KEPLOY_ROW_TOKEN_ELEVATED=1) and once as a standard user (=0):
// tokenElevated, which alone lets a run change the machine's trust, reads the
// token the run has.
func TestOnWindowsTokenElevatedReadsTheRunsToken(t *testing.T) {
	want, ok := map[string]bool{"1": true, "0": false}[os.Getenv("KEPLOY_ROW_TOKEN_ELEVATED")]
	if !ok {
		t.Skip("runs on the Windows lane with KEPLOY_ROW_TOKEN_ELEVATED=1 as an elevated administrator or =0 as a standard user")
	}
	if got := tokenElevated(); got != want {
		t.Fatalf("tokenElevated() = %v in a run whose token is elevated: %v", got, want)
	}
}

// On the Windows lane, as the agent's elevated administrator
// (KEPLOY_ROW_TOKEN_ELEVATED=1): a token restricted from that one with
// Administrators deny-only, and without the UAC limited (LUA) flag, still
// reports TokenElevation, yet cannot use the group. tokenElevated, read on a
// thread impersonating it, must say it may not change the machine's trust:
// TokenElevation would say it may.
func TestOnWindowsATokenWithAdministratorsDenyOnlyIsNotElevated(t *testing.T) {
	if os.Getenv("KEPLOY_ROW_TOKEN_ELEVATED") != "1" {
		t.Skip("runs on the Windows lane as an elevated administrator (KEPLOY_ROW_TOKEN_ELEVATED=1): it restricts that token")
	}
	if !tokenElevated() {
		t.Fatal("this row must run as an elevated administrator: it restricts that token")
	}
	var self windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_DUPLICATE|windows.TOKEN_QUERY|windows.TOKEN_ASSIGN_PRIMARY, &self); err != nil {
		t.Fatalf("OpenProcessToken: %v", err)
	}
	defer func() { _ = self.Close() }()
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		t.Fatalf("CreateWellKnownSid(Administrators): %v", err)
	}
	// DISABLE_MAX_PRIVILEGE (0x1) alone: with LUA_TOKEN (0x4) Windows would
	// report the token as not elevated, and the row could not tell the group
	// from TokenElevation.
	const disableMaxPrivilege = 0x1
	deny := windows.SIDAndAttributes{Sid: admins}
	var restricted windows.Token
	r, _, e := windows.NewLazySystemDLL("advapi32.dll").NewProc("CreateRestrictedToken").Call(
		uintptr(self), disableMaxPrivilege, 1, uintptr(unsafe.Pointer(&deny)), 0, 0, 0, 0, uintptr(unsafe.Pointer(&restricted)))
	if r == 0 {
		t.Fatalf("CreateRestrictedToken(Administrators deny-only): %v", e)
	}
	defer func() { _ = restricted.Close() }()
	var imp windows.Token
	if err := windows.DuplicateTokenEx(restricted, windows.TOKEN_IMPERSONATE|windows.TOKEN_QUERY, nil, windows.SecurityImpersonation, windows.TokenImpersonation, &imp); err != nil {
		t.Fatalf("DuplicateTokenEx: %v", err)
	}
	defer func() { _ = imp.Close() }()
	if !imp.IsElevated() {
		t.Fatal("the restricted token reports TokenElevation false here: this row needs one that reports it with Administrators deny-only")
	}
	if member, err := imp.IsMember(admins); err != nil || member {
		t.Fatalf("the restricted token's Administrators membership = %v, %v; want false, nil (deny-only)", member, err)
	}

	type answer struct {
		elevated bool
		err      error
	}
	done := make(chan answer, 1)
	go func() {
		// Never unlocked: Go ends a locked thread with its goroutine, so an
		// impersonation RevertToSelf failed to undo reaches no other code.
		runtime.LockOSThread()
		if err := windows.SetThreadToken(nil, imp); err != nil {
			done <- answer{err: fmt.Errorf("SetThreadToken: %w", err)}
			return
		}
		elevated := tokenElevated()
		if err := windows.RevertToSelf(); err != nil {
			done <- answer{elevated, fmt.Errorf("RevertToSelf: %w", err)}
			return
		}
		done <- answer{elevated: elevated}
	}()
	got := <-done
	if got.err != nil {
		t.Fatal(got.err)
	}
	if got.elevated {
		t.Fatal("tokenElevated() = true on a thread whose token has Administrators deny-only (TokenElevation true); want false: such a token cannot write the machine's store")
	}
}

// On the Windows lane, as a standard user in a session with no desktop (the
// agent's service): keploy's CA setup asks only the user's store, which cannot
// ask, and names the step for the CA it keeps.
func TestOnWindowsAStandardUserWithNoDesktopAsksOnlyTheUserStore(t *testing.T) {
	if os.Getenv("KEPLOY_ROW_NO_DESKTOP_STANDARD_USER") != "1" {
		t.Skip("runs on the Windows lane as a standard user with no desktop (KEPLOY_ROW_NO_DESKTOP_STANDARD_USER=1)")
	}
	if tokenElevated() {
		t.Fatal("this row must run as a standard user: an elevated setup may change the machine's trust")
	}
	local := t.TempDir()
	t.Setenv("LOCALAPPDATA", local)
	ResetCAReadyForTest()
	t.Cleanup(ResetCAReadyForTest)
	var asked []bool
	real := certutilAddFn
	certutilAddFn = func(ctx context.Context, user bool, path string) ([]byte, error) {
		asked = append(asked, user)
		return real(ctx, user, path)
	}
	t.Cleanup(func() { certutilAddFn = real; noteUntrustedStore("") })
	logger := zaptest.NewLogger(t)
	if err := SetupCA(context.Background(), logger, false); err != nil {
		t.Fatalf("SetupCA = %v, want nil", err)
	}
	t.Cleanup(func() { TeardownNativeCA(logger) })
	if !reflect.DeepEqual(asked, []bool{true}) {
		t.Fatalf("certutil adds asked = %v (true: the user store), want the user store's only", asked)
	}
	kept := filepath.Join(local, "Keploy", "ca", caCertFileName)
	if why := UntrustedStoreWhy(); !strings.Contains(why, kept) {
		t.Fatalf("UntrustedStoreWhy = %q, want the step for the kept CA %s", why, kept)
	}
}

// certutil exits with ERROR_CANCELLED's HRESULT when the user answers No to
// Windows' prompt to trust the CA it adds; cmd's exit passes one on.
func TestOnWindowsCertutilsCancelledExitIsAUserNo(t *testing.T) {
	err := exec.Command("cmd", "/c", "exit", "-2147023673").Run()
	if !userDeclined(nil, err) {
		t.Fatalf("userDeclined(no output, exit 0x800704C7 = %v) = false, want true", err)
	}
	if noTrustPrompt(nil, err) {
		t.Fatal("noTrustPrompt(no output, exit 0x800704C7) = true, want false: a No is not a session with no desktop")
	}
	if userDeclined(nil, exec.Command("cmd", "/c", "exit", "-2147024846").Run()) {
		t.Fatal("userDeclined(no output, exit 0x80070032) = true, want false")
	}
}

// A removal Windows' prompt refused is named by its Win32 code or its
// HRESULT; any other failure is passed on as it is.
func TestOnWindowsRemovalErrorsNameWhatThePromptRefused(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want error
	}{
		{windows.ERROR_CANCELLED, errRemovalDeclined},
		{windows.Errno(hrCancelled), errRemovalDeclined},
		{windows.ERROR_NOT_SUPPORTED, errRemovalNoPrompt},
		{windows.Errno(hrNotSupported), errRemovalNoPrompt},
	} {
		if got := removalError(tc.err); !errors.Is(got, tc.want) {
			t.Errorf("removalError(%#x) = %v, want %v", uint32(tc.err.(windows.Errno)), got, tc.want)
		}
	}
	if got := removalError(windows.ERROR_ACCESS_DENIED); got != windows.ERROR_ACCESS_DENIED {
		t.Errorf("removalError(ERROR_ACCESS_DENIED) = %v, want it as it is", got)
	}
	if removalError(nil) != nil {
		t.Error("removalError(nil) != nil")
	}
}

// systemRootCount is how many certificates location's ROOT system store
// shows, read here apart from rootStoreCerts. The user's takes in the
// machine's roots.
func systemRootCount(t *testing.T, location uint32) int {
	t.Helper()
	name, _ := windows.UTF16PtrFromString("ROOT")
	store, err := windows.CertOpenStore(windows.CERT_STORE_PROV_SYSTEM, 0, 0,
		location|windows.CERT_STORE_READONLY_FLAG, uintptr(unsafe.Pointer(name)))
	runtime.KeepAlive(name)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = windows.CertCloseStore(store, 0) }()
	n := 0
	for c, _ := windows.CertEnumCertificatesInStore(store, nil); c != nil; c, _ = windows.CertEnumCertificatesInStore(store, c) {
		n++
	}
	return n
}

// The sweep lists the machine's ROOT store whole, and the user's own: every
// certificate it lists for the user is one the user's view holds, and the
// list leaves out the machine's roots that view takes in (a removal through
// that view could reach the machine's store).
func TestOnWindowsTheRootStoresAreListedForTheSweep(t *testing.T) {
	machine, err := rootStoreCerts(machineRootStore)
	if err != nil {
		t.Fatalf("rootStoreCerts(the machine's) = %v", err)
	}
	first, last := rootEnds(t, windows.CERT_SYSTEM_STORE_LOCAL_MACHINE)
	for name, want := range map[string][]byte{"first": first, "last": last} {
		found := false
		for _, der := range machine {
			found = found || string(der) == string(want)
		}
		if !found {
			t.Errorf("rootStoreCerts(the machine's) lacks that store's %s certificate", name)
		}
	}
	user, err := rootStoreCerts(userRootStore)
	if err != nil {
		t.Fatalf("rootStoreCerts(the user's) = %v", err)
	}
	if view := systemRootCount(t, windows.CERT_SYSTEM_STORE_CURRENT_USER); len(user) >= view {
		t.Errorf("rootStoreCerts(the user's) lists %d certificates, the user's view of ROOT shows %d: want fewer, the user's own store without the machine's roots", len(user), view)
	}
	for i, der := range user {
		if held, err := userRootHolds(der); err != nil || !held {
			t.Errorf("the user's certificate %d is not in the user's view of ROOT: %v, %v", i, held, err)
		}
	}
}

// addToMachineRoot puts der in the machine's ROOT store (an administrator's
// run), and removes it when the row ends with certutil, apart from the code
// the row tests, and fails the row if it is still there.
func addToMachineRoot(t *testing.T, der []byte) {
	t.Helper()
	name, _ := windows.UTF16PtrFromString("ROOT")
	store, err := windows.CertOpenStore(windows.CERT_STORE_PROV_SYSTEM, 0, 0, windows.CERT_SYSTEM_STORE_LOCAL_MACHINE, uintptr(unsafe.Pointer(name)))
	runtime.KeepAlive(name)
	if err != nil {
		t.Fatalf("opening the machine's ROOT store to write: %v", err)
	}
	defer func() { _ = windows.CertCloseStore(store, 0) }()
	c, err := windows.CertCreateCertificateContext(windows.X509_ASN_ENCODING|windows.PKCS_7_ASN_ENCODING, &der[0], uint32(len(der)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = windows.CertFreeCertificateContext(c) }()
	if err := windows.CertAddCertificateContextToStore(store, c, windows.CERT_STORE_ADD_REPLACE_EXISTING, nil); err != nil {
		t.Fatalf("adding to the machine's ROOT store: %v", err)
	}
	t.Cleanup(func() {
		thumb := sha1.Sum(der)
		out, err := exec.Command("certutil", "-delstore", "ROOT", hex.EncodeToString(thumb[:])).CombinedOutput()
		if held, herr := machineRootHolds(der); herr != nil || held {
			t.Errorf("the row's certificate %x is still in the machine's ROOT store (%v, %v); certutil -delstore said %v: %s", thumb, held, herr, err, out)
		}
	})
}

// On the Windows lane, as an elevated administrator (KEPLOY_ROW_TOKEN_ELEVATED=1):
// with the retired CA and another certificate of its subject in the
// machine's ROOT store, the sweep removes the retired CA and leaves the other.
func TestOnWindowsAnElevatedSweepRemovesOnlyTheRetiredCAFromTheMachineStore(t *testing.T) {
	if os.Getenv("KEPLOY_ROW_TOKEN_ELEVATED") != "1" {
		t.Skip("runs on the Windows lane as an elevated administrator (KEPLOY_ROW_TOKEN_ELEVATED=1), on a throwaway machine: it puts the retired CA, whose private key is public, in the machine's ROOT store for the row, and a test binary killed before its cleanup leaves it there")
	}
	if !tokenElevated() {
		t.Fatal("this row must run as an elevated administrator: it writes the machine's ROOT store")
	}
	legacy := legacyCADER(t)
	sameSubject := selfSignedDER(t, "My Custom CA")
	addToMachineRoot(t, legacy)
	addToMachineRoot(t, sameSubject)
	for name, der := range map[string][]byte{"the retired CA": legacy, "the other": sameSubject} {
		if held, err := machineRootHolds(der); err != nil || !held {
			t.Fatalf("before the sweep the machine's store holds %s: %v, %v; want true", name, held, err)
		}
	}
	sweepLegacyWindowsRoots(zaptest.NewLogger(t), mustLegacyFP(t))
	if held, err := machineRootHolds(legacy); err != nil || held {
		t.Fatalf("after the sweep the machine's store holds the retired CA: %v, %v; want false", held, err)
	}
	if held, err := machineRootHolds(sameSubject); err != nil || !held {
		t.Fatalf("after the sweep the machine's store holds the other certificate of its subject: %v, %v; want true", held, err)
	}
}

// On a Windows host, as an elevated administrator in a session with no
// desktop (an OpenSSH session), with a No an earlier run recorded beside the
// CA keploy keeps: keploy's CA setup asks only the user's store, which cannot
// ask, and puts the CA in no store, the machine's included. Before the No was
// recorded, such a run put it in the machine's store, which asks nobody.
func TestOnWindowsAnElevatedRunThatCannotAskAfterANoWritesNoStore(t *testing.T) {
	if os.Getenv("KEPLOY_ROW_ELEVATED_NO_DESKTOP") != "1" {
		t.Skip("runs as an elevated administrator in a session with no desktop (KEPLOY_ROW_ELEVATED_NO_DESKTOP=1)")
	}
	if !tokenElevated() {
		t.Fatal("this row must run as an elevated administrator: the machine's store is the fallback it must not take")
	}
	local := t.TempDir()
	t.Setenv("LOCALAPPDATA", local)
	dir := filepath.Join(local, "Keploy", "ca")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, declinedRecordName), []byte("an earlier No\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ResetCAReadyForTest()
	t.Cleanup(ResetCAReadyForTest)
	var asked []bool
	real := certutilAddFn
	certutilAddFn = func(ctx context.Context, user bool, path string) ([]byte, error) {
		asked = append(asked, user)
		return real(ctx, user, path)
	}
	t.Cleanup(func() { certutilAddFn = real; noteUntrustedStore("") })
	logger := zaptest.NewLogger(t)
	if err := SetupCA(context.Background(), logger, false); err != nil {
		t.Fatalf("SetupCA = %v, want nil: the run goes on", err)
	}
	t.Cleanup(func() { TeardownNativeCA(logger) })
	der := activeCADER()
	if der == nil {
		t.Fatal("no active CA after SetupCA")
	}
	t.Cleanup(func() {
		// Were the fix broken, the machine's store would hold the CA: take it out.
		thumb := sha1.Sum(der)
		_ = exec.Command("certutil", "-delstore", "ROOT", hex.EncodeToString(thumb[:])).Run()
	})
	if !reflect.DeepEqual(asked, []bool{true}) {
		t.Fatalf("certutil adds asked = %v (true: the user store), want the user store's only: no machine-store write after a recorded No", asked)
	}
	if held, err := machineRootHolds(der); err != nil || held {
		t.Fatalf("the machine's ROOT store holds keploy's CA: %v, %v; want false", held, err)
	}
	if why := UntrustedStoreWhy(); !strings.Contains(why, "answered No") {
		t.Fatalf("UntrustedStoreWhy = %q, want the No", why)
	}
}
