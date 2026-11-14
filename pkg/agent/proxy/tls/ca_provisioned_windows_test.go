//go:build windows

package tls

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
)

// On the Windows lane, as a standard user in a session with no desktop (the
// agent's service, as a CI runner, an OpenSSH session or a scheduled task
// runs keploy), on an agent provisioned once with keploy's kept CA trusted in
// that user's own ROOT store: keploy's CA setup reuses that CA with no
// certutil at all, so it never asks the machine's store, and a Go client,
// which reads only the Windows store, trusts a leaf keploy mints from it.
//
// The provisioning is the lane's one-time setup, never part of a run: a
// person signed in to the agent's desktop as the agent's user runs
// 'certutil -user -addstore ROOT' on the kept CA and accepts Windows' prompt,
// with no administrator. Until then the row skips, naming that step, after
// checking that the setup asked only the user's store. Its first run on an
// agent makes the kept CA that step trusts, as any keploy run there would. A
// kept CA in the machine's store (where an elevated run with no desktop puts
// it) is not the provisioned agent either: the row skips then too, naming the
// administrator's removal of that copy before the user's step. It never
// passes unless the user's own store alone holds the kept CA.
func TestOnWindowsAStandardUserWithNoDesktopReusesTheCAProvisionedInTheirStore(t *testing.T) {
	if os.Getenv("KEPLOY_ROW_NO_DESKTOP_STANDARD_USER") != "1" {
		t.Skip("runs on the Windows lane as a standard user with no desktop (KEPLOY_ROW_NO_DESKTOP_STANDARD_USER=1)")
	}
	if tokenElevated() {
		t.Fatal("this row must run as a standard user: an elevated setup may change the machine's trust")
	}
	logger := zaptest.NewLogger(t)
	// The user's own LOCALAPPDATA, not a temporary one: the kept CA is the
	// one the agent's provisioning trusted.
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
	account := "the agent's user"
	if u, err := user.Current(); err == nil {
		account = u.Username
	}
	trust := fmt.Sprintf("sign in to its desktop as %s, run 'certutil -user -addstore ROOT %s' and accept Windows' prompt (no administrator)", account, kept)
	// The user's view of ROOT takes in the machine's: a CA in the machine's
	// store would pass every check below with the user's own store empty, so
	// that is not the provisioned agent either. An elevated run with no
	// desktop puts the kept CA there (installWindowsCA's fallback).
	machine, err := machineRootHolds(der)
	if err != nil {
		t.Fatalf("reading the machine's ROOT store: %v", err)
	}
	if machine {
		t.Skipf("keploy's kept CA %s (serial %s, made %s) is in the machine's ROOT store, which the user's view takes in, so this row cannot show that the user's own store holds it. Provision this agent once (its setup, never a run): an administrator removes it there with 'certutil -delstore ROOT %s', then %s",
			kept, cert.SerialNumber.Text(16), cert.NotBefore.UTC().Format(time.RFC3339), cert.SerialNumber.Text(16), trust)
	}
	held, err := userRootHolds(der)
	if err != nil {
		t.Fatalf("reading the user's ROOT store: %v", err)
	}
	if !held {
		if !reflect.DeepEqual(asked, []bool{true}) {
			t.Fatalf("certutil adds asked = %v (true: the user store), want the user store's only", asked)
		}
		if why := UntrustedStoreWhy(); !strings.Contains(why, kept) {
			t.Fatalf("UntrustedStoreWhy = %q, want the step for the kept CA %s", why, kept)
		}
		t.Skipf("keploy's kept CA %s is not in %s's ROOT store yet. Provision this agent once (its setup, never a run): %s", kept, account, trust)
	}
	if len(asked) != 0 {
		t.Fatalf("certutil adds asked = %v (true: the user store), want none: the user's store holds the kept CA", asked)
	}
	if why := UntrustedStoreWhy(); why != "" {
		t.Fatalf("UntrustedStoreWhy = %q, want \"\": the user's store holds the kept CA", why)
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
