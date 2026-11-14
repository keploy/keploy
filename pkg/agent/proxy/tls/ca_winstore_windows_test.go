//go:build windows

package tls

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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
	name, _ := windows.UTF16PtrFromString("ROOT")
	store, err := windows.CertOpenStore(windows.CERT_STORE_PROV_SYSTEM, 0, 0,
		windows.CERT_SYSTEM_STORE_CURRENT_USER|windows.CERT_STORE_READONLY_FLAG, uintptr(unsafe.Pointer(name)))
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
		t.Fatal("the current user's ROOT store holds no certificate")
	}
	return first, last
}

func TestOnWindowsTheUserStoreIsReadForTheCAItHolds(t *testing.T) {
	first, last := userRootEnds(t)
	for name, der := range map[string][]byte{"first": first, "last": last} {
		if held, err := userRootHolds(der); err != nil || !held {
			t.Errorf("userRootHolds(the store's %s certificate) = %v, %v; want true", name, held, err)
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
