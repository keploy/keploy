//go:build windows

package tls

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
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
