//go:build windows

package tls

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest"
	"go.uber.org/zap/zaptest/observer"
)

// On the Windows lane, as a standard user in a session with no desktop (the
// agent's service, as a CI runner, an OpenSSH session or a scheduled task
// runs keploy), on an agent whose administrator put keploy's kept CA in the
// machine's ROOT store (LocalMachine\Root, where a company's group policy
// rolls a CA out to every user): keploy's CA setup reuses that CA with no
// certutil at all, so it never asks the machine's store, warns of nothing and
// leaves no hint, and a Go client, which reads only the Windows store, trusts
// a leaf keploy mints from it.
//
// The lane provisions the machine's store around this row only (ca-rows.sh,
// as the agent's administrator: 'certutil -addstore Root' on the kept CA,
// removed again after it on every path) and says so with
// KEPLOY_ROW_CA_IN_MACHINE_STORE=1: the row then fails unless the machine's
// store holds the kept CA. Without that variable a kept CA no store holds
// skips the row, naming that step, after checking that the setup asked only
// the user's store and named the step that trusts it there. The row's first
// run on an agent makes the kept CA, as any keploy run there would.
func TestOnWindowsAStandardUserWithNoDesktopReusesTheCAProvisionedInTheMachineStore(t *testing.T) {
	if os.Getenv("KEPLOY_ROW_NO_DESKTOP_STANDARD_USER") != "1" {
		t.Skip("runs on the Windows lane as a standard user with no desktop (KEPLOY_ROW_NO_DESKTOP_STANDARD_USER=1)")
	}
	if tokenElevated() {
		t.Fatal("this row must run as a standard user: an elevated setup may change the machine's trust")
	}
	provisioned := os.Getenv("KEPLOY_ROW_CA_IN_MACHINE_STORE") == "1"
	warned, warnings := observer.New(zapcore.WarnLevel)
	logger := zaptest.NewLogger(t, zaptest.WrapOptions(zap.WrapCore(func(c zapcore.Core) zapcore.Core {
		return zapcore.NewTee(c, warned)
	})))
	// The user's own LOCALAPPDATA, not a temporary one: the kept CA is the
	// one the lane puts in the machine's store.
	dir := windowsCAKeyDir(logger)
	if dir == "" {
		t.Fatal("no per-user directory for keploy's kept CA (LOCALAPPDATA is not set)")
	}
	kept := filepath.Join(dir, caCertFileName)
	ResetCAReadyForTest()
	t.Cleanup(ResetCAReadyForTest)
	var asked []bool
	orig := certutilAddFn
	certutilAddFn = func(ctx context.Context, toUser bool, path string) ([]byte, error) {
		asked = append(asked, toUser)
		return orig(ctx, toUser, path)
	}
	t.Cleanup(func() { certutilAddFn = orig; noteUntrustedStore("") })
	noteUntrustedStore("a hint from an earlier setup")
	if err := SetupCA(context.Background(), logger, false); err != nil {
		t.Fatalf("SetupCA = %v, want nil", err)
	}
	t.Cleanup(func() { TeardownNativeCA(logger) })

	b, err := os.ReadFile(kept)
	if err != nil {
		t.Fatalf("keploy keeps no CA at %s after its setup: %v", kept, err)
	}
	block, _ := pem.Decode(b)
	if block == nil {
		t.Fatalf("the kept CA %s is not PEM", kept)
	}
	der := block.Bytes
	if ca := getActiveCA(logger); !bytes.Equal(ca.der, der) {
		t.Fatalf("SetupCA's CA is not the one kept at %s", kept)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("the kept CA %s: %v", kept, err)
	}
	machine, err := machineRootHolds(der)
	if err != nil {
		t.Fatalf("reading the machine's ROOT store: %v", err)
	}
	if provisioned && !machine {
		t.Fatalf("KEPLOY_ROW_CA_IN_MACHINE_STORE=1 says the lane put keploy's kept CA %s (serial %s) in the machine's ROOT store, and that store does not hold it", kept, cert.SerialNumber.Text(16))
	}
	trusted := machine
	if !machine {
		if trusted, err = userRootHolds(der); err != nil {
			t.Fatalf("reading the user's ROOT store: %v", err)
		}
	}
	if !trusted {
		if !reflect.DeepEqual(asked, []bool{true}) {
			t.Fatalf("certutil adds asked = %v (true: the user store), want the user store's only", asked)
		}
		if why := UntrustedStoreWhy(); !strings.Contains(why, kept) {
			t.Fatalf("UntrustedStoreWhy = %q, want the step for the kept CA %s", why, kept)
		}
		t.Skipf("no ROOT store holds keploy's kept CA %s (serial %s, made %s). Provision this agent's machine store around this row (ca-rows.sh with CA_PROVISIONED=1 does it): as an administrator, 'certutil -addstore Root %s', then 'certutil -delstore Root %s' after it",
			kept, cert.SerialNumber.Text(16), cert.NotBefore.UTC().Format(time.RFC3339), kept, cert.SerialNumber.Text(16))
	}
	t.Logf("keploy's kept CA %s (serial %s) is trusted: in the machine's ROOT store %v", kept, cert.SerialNumber.Text(16), machine)
	if len(asked) != 0 {
		t.Fatalf("certutil adds asked = %v (true: the user store), want none: a ROOT store holds the kept CA", asked)
	}
	if warnings.Len() != 0 {
		t.Fatalf("warnings = %v, want none: a ROOT store holds the kept CA", warnings.All())
	}
	if why := UntrustedStoreWhy(); why != "" {
		t.Fatalf("UntrustedStoreWhy = %q, want \"\": a ROOT store holds the kept CA", why)
	}

	// A Go client with no roots of its own verifies against the Windows store.
	ca := getActiveCA(logger)
	if cerr, serr := goHandshake(t, logger, ca, "provisioned-ca-row.keploy.test"); cerr != nil || serr != nil {
		t.Fatalf("a Go client's handshake with a leaf from the kept CA: client %v, server %v; want both nil", cerr, serr)
	}
	// It asked the store: a leaf from a CA no store holds is refused.
	fresh, err := generateRunCA()
	if err != nil {
		t.Fatal(err)
	}
	cerr, _ := goHandshake(t, logger, fresh, "untrusted-ca-row.keploy.test")
	var verr *tls.CertificateVerificationError
	if !errors.As(cerr, &verr) {
		t.Fatalf("a Go client's handshake with a leaf from a CA no store holds = %v, want a certificate verification error", cerr)
	}
}

// goHandshake serves one TLS connection on loopback with a leaf CertForClient
// mints from ca for the client's SNI, dials it as a Go client asking for name
// with the system's roots, and returns the client's and the server's
// handshake errors.
func goHandshake(t *testing.T, logger *zap.Logger, ca *caState, name string) (client, server error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	served := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			served <- err
			return
		}
		defer func() { _ = c.Close() }()
		_ = c.SetDeadline(time.Now().Add(30 * time.Second))
		srv := tls.Server(c, &tls.Config{GetCertificate: func(h *tls.ClientHelloInfo) (*tls.Certificate, error) {
			return CertForClient(logger, h, ca.signer, ca.cert, time.Time{})
		}})
		served <- srv.Handshake()
	}()
	d := &net.Dialer{Timeout: 30 * time.Second}
	c, client := tls.DialWithDialer(d, "tcp", ln.Addr().String(), &tls.Config{ServerName: name, MinVersion: tls.VersionTLS12})
	if client == nil {
		_ = c.Close()
	}
	select {
	case server = <-served:
	case <-time.After(45 * time.Second):
		server = errors.New("the server's handshake did not end in 45s")
	}
	return client, server
}
