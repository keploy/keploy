// Package tls provides functionality for handling tls connetions.
package tls

import (
	"bytes"
	"context"
	"crypto"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cloudflare/cfssl/csr"
	cfsslLog "github.com/cloudflare/cfssl/log"
	"github.com/cloudflare/cfssl/signer"
	"github.com/cloudflare/cfssl/signer/local"
	expirable "github.com/hashicorp/golang-lru/v2/expirable"
	"github.com/pavlo-v-chernykh/keystore-go/v4"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	nssbundle "golang.org/x/crypto/x509roots/fallback/bundle"
)

// The MITM signing CA is no longer a committed, //go:embed-ed static cert+key
// (its private key was a public shared secret). It is generated per run and held
// in caState (see cagen.go); the agent reads it via getActiveCA / activeCADER.

// mitmPublishHook, if non-nil, is invoked by CertForClient once the
// MITM cert for a connection is determined (whether freshly minted or
// served from cache). The arguments are the connection-level identifier
// — the client's source port as a decimal string, matching what
// dialPostgresSSLUpstream uses — and the DER bytes of the leaf cert.
//
// The agent wires this to cbshim.RegisterMITM so the eBPF channel-
// binding shim can publish the (mitm_hash → real_hash) pair into the
// BPF map once both halves of the connection are known. Nil-safe: when
// unset, CertForClient is a no-op on the cbshim side and works exactly
// as it did before the shim existed.
//
// Synchronisation: the read path (publishMITM) holds an RLock around
// the nil-check + call, and SetMITMPublishHook holds the write Lock.
// This serialises hook clears against in-flight invocations so the
// cleanup sequence
//
//	SetMITMPublishHook(nil) → cb.Close()
//
// can't race a goroutine that already started publishMITM but hasn't
// yet returned from the hook (which would dereference a closed
// cbshim handle). Plain atomic.Pointer wasn't enough — it stops
// nil-deref between Load and call, but doesn't fence Close against
// the in-flight call body. RLock contention is in the noise next to
// the cost of the cert mint / SCRAM handshake on the same path.
var (
	hookMu          sync.RWMutex
	mitmPublishHook func(connID string, mitmCertDER []byte)
)

// SetMITMPublishHook installs (or clears, via nil) the hook the
// proxy's CertForClient path invokes after every MITM-cert mint /
// cache-hit. Callers MUST use this rather than touching the
// underlying var.
//
// When called with nil, blocks until any in-flight publishMITM call
// has returned — so cleanup paths can safely close the resource the
// hook closes over (e.g. StopProxyServer's cbshim handle) without
// racing concurrent CertForClient invocations.
func SetMITMPublishHook(h func(connID string, mitmCertDER []byte)) {
	hookMu.Lock()
	mitmPublishHook = h
	hookMu.Unlock()
}

// embeddedFallbackRoots is the Mozilla NSS root bundle as PEM, built at startup
// from golang.org/x/crypto/x509roots/fallback/bundle (the Go team's extract of
// Mozilla's certdata.txt) so the agent always has a valid trust store
// regardless of the runtime image's filesystem state. It is refreshed by
// bumping that module (.github/workflows/refresh-nss-roots.yml proposes a bump
// weekly), not by hand: a vendored copy went stale and kept trusting roots
// Mozilla had distrusted (the October 2026 eMudhra/emSign compromise).
//
// This bundle is the safety net that prevents the failure mode in
// keploy/k8s-proxy#375: even if /etc/ssl/certs/ca-certificates.crt is
// missing or shadowed at runtime (broken image, weird volume mount,
// SELinux policy, distroless), apps mutated by keploy still see real
// public roots in their trust store and can validate AWS / Google /
// Snowflake / etc. The merged bundle written to /tmp/keploy-tls/ca.crt
// becomes (system_or_embedded_roots ∪ keploy_mitm_ca) instead of
// (keploy_mitm_ca alone), which is what made the customer's pods fail
// CERTIFICATE_VERIFY_FAILED on every public-endpoint HTTPS call.
var embeddedFallbackRoots = nssRootsPEM()

// nssRootsPEM encodes every root of the NSS bundle as PEM. A few roots carry a
// "distrust after" constraint that X.509 cannot express; PEM consumers (the
// recorded app's TLS stack) trust them unconstrained, exactly as they would
// with curl's cacert.pem or a distro ca-certificates package.
func nssRootsPEM() []byte {
	var buf bytes.Buffer
	for root := range nssbundle.Roots() {
		_ = pem.Encode(&buf, &pem.Block{Type: "CERTIFICATE", Bytes: root.Certificate})
	}
	return buf.Bytes()
}

var caStorePath = []string{
	"/usr/local/share/ca-certificates/",
	"/etc/pki/ca-trust/source/anchors/",
	"/etc/ca-certificates/trust-source/anchors/",
	"/etc/pki/trust/anchors/",
	"/usr/local/share/certs/",
	"/etc/ssl/certs/",
}

var caStoreUpdateCmd = []string{
	"update-ca-certificates",
	"update-ca-trust",
	"trust extract-compat",
	"tools-ca-trust extract",
	"certctl rehash",
}

// caReadyOnce guards the single close of caReadyCh so markCAReady is safe to
// call from multiple goroutines and multiple times during a single agent
// lifecycle. caReadyCh is closed exactly once, on the first successful
// SetupCA path, and remains closed for the rest of the process lifetime.
//
// caFailure records a terminal SetupCA error so callers (e.g. the
// /agent/ready handler) can distinguish "CA not yet ready" from "CA
// setup failed and will never recover without an agent restart". It is
// written by MarkCAFailed on the error path of SetupCA (via
// pkg/agent/proxy), and read non-blockingly by CAStatus. It is never
// reset in production; tests call ResetCAReadyForTest to clear it.
var (
	caReadyOnce sync.Once
	caReadyCh   = make(chan struct{})
	caFailure   atomic.Pointer[error]
)

// CAReady returns a channel that is closed once the Keploy CA bundle has
// been written to disk (either to the shared volume at /tmp/keploy-tls/ca.crt
// in Docker/k8s mode, or into the system CA store in native mode).
//
// Callers that need to guarantee the CA file exists before signalling
// dependent systems (e.g. Kubernetes pod readiness, docker-compose
// healthchecks, or the /agent/ready HTTP endpoint) must wait on this
// channel. A non-blocking select against this channel is the canonical
// way to refuse readiness until SetupCA has completed.
//
// The channel is only closed on the SUCCESS path of SetupCA. If SetupCA
// fails, the channel stays open and readiness keeps failing; an operator
// must restart the agent (with fixed config) to recover. This is
// deliberate — silently writing the readiness marker when the CA bundle
// is missing would let app containers boot against a broken TLS trust
// chain and produce silent failures in HTTPS egress.
func CAReady() <-chan struct{} { return caReadyCh }

// CAStatus reports the current CA-readiness state in a single
// non-blocking call. It is the preferred check for HTTP handlers and
// other consumers that need to distinguish three states:
//
//   - ready==true,  err==nil  : the CA bundle has been written to disk
//     (shared volume in docker/k8s mode, or system CA store in native
//     mode) and downstream consumers can proceed.
//   - ready==false, err==nil  : SetupCA has not completed yet — the
//     caller should back off and retry (typical boot-time race).
//   - ready==false, err!=nil  : SetupCA completed with a terminal
//     error. The CA will NOT become ready in this process lifetime;
//     callers should surface a clear "misconfiguration" signal (e.g.
//     HTTP 503 with the error message) so operators don't wait
//     forever for readiness that will never come.
//
// CAStatus is safe to call concurrently from any goroutine.
//
// Contract: (ready==true, err==nil) is the only "success" shape. If the
// channel is closed we return nil error unconditionally, even if a
// previous MarkCAFailed recorded one — a successful markCAReady supersedes
// any earlier terminal failure (an upstream retry + success for example).
// That keeps the three documented shapes mutually exclusive so callers
// can switch on them without chasing a race between the signal fire
// order and the readiness observation.
func CAStatus() (ready bool, err error) {
	select {
	case <-caReadyCh:
		return true, nil
	default:
	}
	if p := caFailure.Load(); p != nil && *p != nil {
		err = *p
	}
	return false, err
}

// MarkCAFailed records a terminal SetupCA failure so future CAStatus
// calls can report it. Only the SetupCA caller in pkg/agent/proxy
// should invoke this — it is exposed (capitalised) because the error
// is returned to the caller, not handled inside this package. Calling
// MarkCAFailed with a non-nil err does NOT close caReadyCh: readiness
// stays unlatched so health checks keep failing, and the error gives
// operators a clear diagnostic instead of an open-ended timeout.
//
// Passing nil is a no-op (so callers can plumb `MarkCAFailed(err)`
// unconditionally on the SetupCA return). Subsequent non-nil calls
// overwrite the previous error — SetupCA is not currently retried,
// but if a future refactor adds retry this keeps the most recent
// diagnostic visible.
func MarkCAFailed(err error) {
	if err == nil {
		return
	}
	caFailure.Store(&err)
}

// markCAReady closes the CAReady channel exactly once. Safe to call from
// multiple goroutines and multiple times.
func markCAReady() { caReadyOnce.Do(func() { close(caReadyCh) }) }

// SetupCA setups custom certificate authority to handle TLS connections.
//
// Legacy entry point — SetupCAForApp is preferred when the app PID is
// known (native mode on Linux), because it lets the Java keystore import
// target the app's actual JDK instead of whatever keytool happens to be
// on PATH. This shim preserves the three-arg signature for the docker /
// shared-volume path (which writes a JKS from pure Go and doesn't shell
// out to keytool) and for external callers that pre-date the PID plumbing.
func SetupCA(ctx context.Context, logger *zap.Logger, isDocker bool) error {
	return SetupCAForApp(ctx, logger, isDocker, 0, "")
}

// SetupCAForApp is SetupCA plus two inputs the Java truststore install
// uses to target the right JDK:
//
//   - appPID: the process ID of the application being instrumented
//     (config.Agent.ClientNSPID). When >0 and javaHomeOverride is empty,
//     detectJavaHomeForPID reads /proc/<pid>/environ and /proc/<pid>/exe
//     to discover the JDK the app is actually using. 0 disables
//     detection (and we fall back to PATH keytool).
//
//   - javaHomeOverride: manual override from the CLI flag
//     --ca-java-home. When non-empty it SHORT-CIRCUITS auto-detection;
//     operators set this when an exotic launcher masks both JAVA_HOME
//     and the exe symlink.
//
// Docker / shared-volume mode ignores both inputs: the JKS written to
// /tmp/keploy-tls/truststore.jks is built in pure Go and consumed by the
// app via -Djavax.net.ssl.trustStore=, so the "which keytool" problem
// doesn't apply there.
func SetupCAForApp(ctx context.Context, logger *zap.Logger, isDocker bool, appPID int, javaHomeOverride string) error {
	// Before the native install adds keploy's CA to the system store, and
	// before SSL_CERT_FILE is pointed at keploy's files.
	loadOwnRoots()
	// Establish this run's CA (fresh in memory, or reused from the per-pod /
	// per-user backend) and publish it before anything signs or installs it.
	ca, store, err := loadOrGenerateActiveCA(logger, isDocker)
	if err != nil {
		return err
	}

	// Best-effort: remove the retired static CA this host may still trust from
	// older runs or the "bake it into the image" recipe. Runs before this run's
	// CA is installed so the trust-store refresh does not race our new anchor.
	SweepLegacyCA(ctx, logger, false)

	if isDocker {
		logger.Debug("Detected Docker Shared Volume mode. Exporting certs...", zap.String("path", "/tmp/keploy-tls"))
		return setupSharedVolume(ctx, logger, "/tmp/keploy-tls", ca)
	}

	// Native Mode
	logger.Debug("Detected Native Mode. Installing to system store...")
	return setupNativeForAppFn(ctx, logger, ca, store, appPID, javaHomeOverride)
}

// setupNativeForAppFn is an indirection seam for tests: production code uses
// setupNativeForApp, which needs root and changes the host's trust stores.
var setupNativeForAppFn = setupNativeForApp

// SetupCaCertEnv writes the run CA's public certificate, and a bundle of that
// certificate and the roots keploy's loader reads, to a directory of this
// run's own in the temp directory, and points the language-runtime trust env
// vars at them (client side, native mode): NODE_EXTRA_CA_CERTS at the
// certificate, the variables that replace a runtime's whole trust store at the
// bundle. The directory is removed when ctx ends; one a run that died left
// behind is removed by the next run's call (reapEndedRunDirs).
//
// certPEM is the PUBLIC certificate of the running agent's CA, fetched from the
// agent over the control-plane API (the client no longer carries its own copy of
// a shared CA). An empty certPEM is a programmer error — the caller must obtain
// the cert from the agent first — and is refused rather than silently producing
// an empty trust file.
func SetupCaCertEnv(ctx context.Context, logger *zap.Logger, certPEM []byte) error {
	if len(certPEM) == 0 {
		return fmt.Errorf("SetupCaCertEnv: no CA certificate provided (fetch it from the agent first)")
	}
	loadOwnRoots()
	reapEndedRunDirs(logger)
	run, err := newRunTrustDir()
	if err != nil {
		utils.LogError(logger, err, "Failed to make this run's CA directory in the tmp folder")
		return err
	}
	run.removeWhenDone(ctx, logger)
	tempPath, err := run.write(runCAFile, certPEM)
	if err != nil {
		_ = run.remove()
		utils.LogError(logger, err, "Failed to extract certificate to tmp folder")
		return err
	}
	// Python (requests), OpenSSL and curl, and Cargo read their variable as the
	// WHOLE trust store, so pointed at the lone keploy CA an app could trust no
	// endpoint keploy does not intercept, and lost the operator's corporate CA.
	// They get one bundle of the roots keploy's loader reads (the operator's
	// SSL_CERT_FILE and SSL_CERT_DIR, the system bundle, or the Mozilla roots
	// built into keploy) plus keploy's CA, as in docker and k8s. Node adds
	// NODE_EXTRA_CA_CERTS to its own roots, so it gets keploy's CA alone.
	systemBundle, _ := loadSystemCABundleFn(logger)
	bundlePath, err := run.write(runBundleFile, mergeCABundle(systemBundle, certPEM))
	if err != nil {
		_ = run.remove()
		utils.LogError(logger, err, "Failed to write the app's CA bundle to the tmp folder")
		return err
	}
	if err := setEnvForSharedVolume(logger, bundlePath, tempPath); err != nil {
		_ = run.remove()
		return err
	}
	// Java: point the JVM at a merged truststore via JAVA_TOOL_OPTIONS instead of
	// importing the keploy CA into the JDK's own cacerts. Best-effort and additive
	// — a machine with no JDK, or a failure to build the store, must not fail CA
	// setup for a Node/Python/Go app, and any existing JAVA_TOOL_OPTIONS is kept.
	if err := setupJavaTrustStoreEnv(logger, certPEM); err != nil {
		logger.Warn("could not set up the Java truststore via JAVA_TOOL_OPTIONS; Java HTTPS interception may not work, but other runtimes are unaffected",
			zap.Error(err))
	}
	return nil
}

// EnvJavaToolOptions is the environment variable a JVM reads extra launch flags
// from at startup. Keploy uses it to point Java at a merged truststore instead
// of importing its CA into the JDK's cacerts, which mutated the user's JDK with
// no uninstall path and needed a keytool that is not always present.
const EnvJavaToolOptions = "JAVA_TOOL_OPTIONS"

// mergeCABundle concatenates the system trust bundle and the keploy CA cert into
// one PEM blob, inserting a newline between them when the system bundle lacks a
// trailing one so the keploy block starts on its own line. It grows the result
// with append rather than a capacity-hinted make whose size is a sum of input
// lengths — that expression is a false-positive allocation-overflow pattern to
// static analysis, and append is just as efficient here.
func mergeCABundle(systemBundle, keployCertPEM []byte) []byte {
	merged := append([]byte(nil), systemBundle...)
	if len(merged) > 0 && merged[len(merged)-1] != '\n' {
		merged = append(merged, '\n')
	}
	return append(merged, keployCertPEM...)
}

// setupJavaTrustStoreEnv builds a merged Java truststore — the system roots plus
// the keploy MITM CA — in pure Go, and points the application's JVM at it via
// JAVA_TOOL_OPTIONS. -Djavax.net.ssl.trustStore REPLACES the JDK's default
// cacerts wholesale, so the store must carry the system roots too or a Java app
// could no longer validate any non-keploy-proxied HTTPS.
//
// Nothing in the JDK is modified: the store lives in a temp file and is thrown
// away with it. keploy's flags are appended to any JAVA_TOOL_OPTIONS the user
// already set rather than replacing it.
//
// Note: every JVM the app spawns prints "Picked up JAVA_TOOL_OPTIONS: ..." to
// its stderr — harmless, but a build tool that parses child-JVM stderr may see
// it. That is inherent to JAVA_TOOL_OPTIONS and is the accepted cost of not
// mutating the JDK.
func setupJavaTrustStoreEnv(logger *zap.Logger, certPEM []byte) error {
	systemBundle, srcPath := loadSystemCABundleFn(logger)
	merged := mergeCABundle(systemBundle, certPEM)

	mergedPEM, err := os.CreateTemp("", "keploy-java-cas-*.pem")
	if err != nil {
		return fmt.Errorf("failed to create the merged CA pem: %w", err)
	}
	mergedPath := mergedPEM.Name()
	// The merged PEM is only the input to generateTrustStore; the JKS is what
	// the JVM reads, so the PEM can go once the store is built.
	defer func() { _ = os.Remove(mergedPath) }()
	if _, err := mergedPEM.Write(merged); err != nil {
		_ = mergedPEM.Close()
		return fmt.Errorf("failed to write the merged CA pem: %w", err)
	}
	if err := mergedPEM.Close(); err != nil {
		return fmt.Errorf("failed to write the merged CA pem: %w", err)
	}

	// Deterministic per-user path, overwritten each run, so runs do not
	// accumulate a new JKS in the temp dir every time (there is no teardown hook
	// to remove a random-named one). The contents are the same public bundle
	// each run, so a concurrent overwrite is harmless.
	//
	// The uid in the NAME is what makes "per-user" true. It used to be a single
	// shared filename, which is not per-user at all: os.TempDir() is shared and
	// world-writable with the sticky bit, so the first run to create it owns it
	// forever. One `sudo keploy` — or any rootful-docker run — left a root-owned
	// file that every later run as the normal user failed to rewrite:
	//
	//	failed to build the Java truststore: failed to create jks file:
	//	open /tmp/keploy-java-truststore.jks: permission denied
	//
	// and Java interception stayed broken on that machine until someone removed
	// it by hand. Keying the name by uid means two users, or the same user with
	// and without sudo, never contend for one inode. The store holds only public
	// certificates, so a per-uid copy costs nothing.
	jksPath := filepath.Join(os.TempDir(), fmt.Sprintf("keploy-java-truststore-%d.jks", os.Geteuid()))
	if err := generateTrustStore(mergedPath, jksPath, decodeFirstCertDER(certPEM)); err != nil {
		return fmt.Errorf("failed to build the Java truststore: %w", err)
	}
	// Readable by the app, which may run as the same user; the store contains
	// only public certificates, no secrets.
	if err := os.Chmod(jksPath, 0644); err != nil {
		logger.Debug("could not chmod the Java truststore", zap.String("path", jksPath), zap.Error(err))
	}

	// The JVM splits JAVA_TOOL_OPTIONS on whitespace with no quoting, so a store
	// path containing a space would make the JVM refuse to start. Rather than
	// ever break a Java app, decline when the path cannot be made space-free
	// (Windows still has its keytool/cacerts path as a fallback; other platforms
	// simply get no Java interception, which is the pre-existing behaviour for a
	// machine without keytool).
	safePath, ok := envSafePath(jksPath)
	if !ok {
		logger.Warn("the Java truststore path contains a space and cannot be made space-free; skipping JAVA_TOOL_OPTIONS so the JVM still starts",
			zap.String("truststore", jksPath))
		return nil
	}
	opts := fmt.Sprintf("-Djavax.net.ssl.trustStore=%s -Djavax.net.ssl.trustStorePassword=changeit", safePath)
	if existing := os.Getenv(EnvJavaToolOptions); existing != "" {
		if strings.Contains(existing, "-Djavax.net.ssl.trustStore=") {
			// Already set (a second SetupCaCertEnv in the same process): leave it
			// rather than appending a duplicate flag and leaking work.
			return nil
		}
		opts = existing + " " + opts
	}
	logger.Info("pointing Java at a merged keploy truststore via JAVA_TOOL_OPTIONS (the JDK's cacerts is left untouched)",
		zap.String("truststore", safePath), zap.String("system_source", srcPath))
	return os.Setenv(EnvJavaToolOptions, opts)
}

// SetEnvForPath sets the environment variables to point to a SPECIFIC path.
//
// This exported entrypoint applies the SAME path to every language-runtime
// CA variable, which is correct for the native / single-file install paths
// (SetupCA on Linux/macOS without the shared volume, and the Windows temp-
// file path) where there is no distinct Keploy-only bundle on disk.
//
// Callers wiring the shared-volume / k8s-proxy flow — where the merged
// system+Keploy bundle and the Keploy-only bundle live in separate files —
// should invoke SetupCA with the isDocker flag instead of calling this
// helper directly. SetupCA internally routes NODE_EXTRA_CA_CERTS (which
// Node.js ADDS to, rather than replaces, the default trust store) at the
// Keploy-only file while pointing the other variables at the merged
// bundle; that split is the whole reason the shared-volume path exists
// and cannot be replicated by any sequence of SetEnvForPath calls.
func SetEnvForPath(logger *zap.Logger, path string) error {
	envs := map[string]string{
		"NODE_EXTRA_CA_CERTS": path,
		"REQUESTS_CA_BUNDLE":  path,
		"SSL_CERT_FILE":       path,
		"CARGO_HTTP_CAINFO":   path,
	}
	return setEnvVars(logger, envs)
}

// setEnvForSharedVolume wires the language-runtime trust-store env vars for the
// docker/k8s shared-volume install path, and for the app the native-mode
// client starts (SetupCaCertEnv).
//
// Most runtimes (Python/requests, OpenSSL/libcurl, Cargo) treat their
// respective env var as a FULL REPLACEMENT for the default trust store —
// they must see the MERGED bundle (system roots + Keploy MITM CA) or
// non-proxied HTTPS calls fail CERTIFICATE_VERIFY_FAILED.
//
// Node.js is the exception: NODE_EXTRA_CA_CERTS is ADDED to the default
// bundle at startup, so the minimal correct input is the Keploy-only file.
// Pointing it at the merged bundle works (Node is happy to see system roots
// listed twice) but wastes memory and obscures intent.
func setEnvForSharedVolume(logger *zap.Logger, mergedPath, keployOnlyPath string) error {
	return setEnvVars(logger, map[string]string{
		"NODE_EXTRA_CA_CERTS": keployOnlyPath,
		"REQUESTS_CA_BUNDLE":  mergedPath,
		"SSL_CERT_FILE":       mergedPath,
		"CARGO_HTTP_CAINFO":   mergedPath,
	})
}

func setEnvVars(logger *zap.Logger, envVars map[string]string) error {
	for key, val := range envVars {
		if key == envSSLCertFile {
			noteKeployCertFile(val)
		}
		if err := os.Setenv(key, val); err != nil {
			utils.LogError(logger, err, "Failed to set environment variable", zap.String("key", key))
			return err
		}
	}
	return nil
}

// systemCABundleSearchPaths is the ranked list of well-known locations where
// Linux/Unix distributions (and busybox/Alpine-derived images) place the OS
// trust store. The first file holding a certificate wins.
//
// Ordering mirrors Go's crypto/x509 certFiles but is duplicated here because
// we need the certificates as PEM (re-encoded by appendTrustedCerts), not a
// parsed pool — the shared volume is consumed by non-Go clients (curl,
// python/requests, node.js, rust/reqwest, ...) that each parse PEM directly.
var systemCABundleSearchPaths = []string{
	"/etc/ssl/certs/ca-certificates.crt",                // Debian, Ubuntu, Alpine
	"/etc/pki/tls/certs/ca-bundle.crt",                  // RHEL, Fedora, CentOS
	"/etc/ssl/ca-bundle.pem",                            // openSUSE
	"/etc/pki/tls/cacert.pem",                           // older RHEL
	"/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem", // CentOS/RHEL 7
	"/usr/local/share/certs/ca-root-nss.crt",            // FreeBSD
	"/etc/ssl/cert.pem",                                 // Alpine alternate, macOS
}

// loadSystemCABundleFn is an indirection seam for tests — production code uses
// the default loadSystemCABundle implementation. Tests replace this with a
// stub that reads from a tempdir-backed search list.
var loadSystemCABundleFn = loadSystemCABundle

// The environment variables through which an operator names the trust store,
// read as Go's crypto/x509 reads them on Linux: SSL_CERT_FILE is one PEM bundle
// that replaces the default bundle paths, SSL_CERT_DIR a list of directories of
// PEM files.
const (
	envSSLCertFile = "SSL_CERT_FILE"
	envSSLCertDir  = "SSL_CERT_DIR"
)

// certEnv is the trust store an operator named through the environment.
type certEnv struct {
	file string // SSL_CERT_FILE
	dir  string // SSL_CERT_DIR, or systemCABundleSearchDirs when it is unset
	// defaultDir says dir is systemCABundleSearchDirs, not the operator's:
	// a default directory that adds nothing is no misconfiguration.
	defaultDir bool
}

// systemCABundleSearchDirs are the certificate directories crypto/x509 reads
// when SSL_CERT_DIR is unset (crypto/x509/root_linux.go). A CA dropped into one
// of them (with no bundle file rebuilt) is trusted by Go programs, so it must
// reach the apps' bundle too. Elsewhere keploy reads no default directory.
var systemCABundleSearchDirs = defaultCertDirs(runtime.GOOS)

func defaultCertDirs(goos string) []string {
	if goos == "linux" {
		return []string{"/etc/ssl/certs", "/etc/pki/tls/certs"}
	}
	return nil
}

// certFileEnv tracks keploy's own writes of SSL_CERT_FILE in this process.
//
// keploy points SSL_CERT_FILE at files it wrote itself: the merged bundle in
// the shared volume, or the lone keploy CA in native mode (SetEnvForPath). Read
// back as "the operator's trust store", such a file would feed keploy's CA, or
// nothing but keploy's CA, into the next bundle built in this process — the
// client builds its Java truststore right after SetEnvForPath. So the value
// keploy replaced is kept, and operatorCertEnv reads that one instead.
var certFileEnv struct {
	sync.Mutex
	ours     map[string]bool // values keploy set SSL_CERT_FILE to
	operator string          // the last value of SSL_CERT_FILE keploy did not set
}

// loadOwnRoots makes crypto/x509 load this process's system roots now, before
// keploy changes what it would read. crypto/x509 reads SSL_CERT_FILE once, on
// the process's first use of the system pool — on Linux, and since Go 1.27 on
// macOS and Windows too — and on Linux reads the system bundle files then as
// well. keploy points SSL_CERT_FILE at files meant for the app, and in native
// mode adds its CA to the Linux system bundle. Loaded after that, keploy's own
// TLS (its upstream verification, its calls to the keploy API) would trust
// keploy's MITM CA, or with SSL_CERT_FILE at the lone keploy CA only it. (On
// macOS and Windows with SSL_CERT_FILE unset, the platform verifier reads the
// OS store at verify time; loading early does not change that.) main calls it
// first thing; these calls cover other embedders. Later calls are a cheap copy
// of the loaded pool.
func loadOwnRoots() { _, _ = x509.SystemCertPool() }

// noteKeployCertFile records that keploy is about to set SSL_CERT_FILE to path,
// loading this process's own roots first (loadOwnRoots) in case no caller did.
func noteKeployCertFile(path string) {
	loadOwnRoots()
	certFileEnv.Lock()
	defer certFileEnv.Unlock()
	if certFileEnv.ours == nil {
		certFileEnv.ours = make(map[string]bool)
	}
	if cur := os.Getenv(envSSLCertFile); !certFileEnv.ours[cur] {
		certFileEnv.operator = cur
	}
	certFileEnv.ours[path] = true
}

// operatorCertEnv returns SSL_CERT_FILE and SSL_CERT_DIR as the operator (or a
// wrapper such as the enterprise agent's trust-root setup) set them: when
// SSL_CERT_FILE currently names a file keploy wrote, the value it replaced.
func operatorCertEnv() certEnv {
	certFileEnv.Lock()
	defer certFileEnv.Unlock()
	file := os.Getenv(envSSLCertFile)
	if certFileEnv.ours[file] {
		file = certFileEnv.operator
	}
	return certEnv{file: file, dir: os.Getenv(envSSLCertDir)}
}

// systemCABundleSourceEmbedded is the sentinel returned in the source-path
// position of loadSystemCABundle when no CA bundle file was found and the
// embeddedFallbackRoots blob stands in for one. Operators see this string in
// startup logs and in the merged-bundle source field.
const systemCABundleSourceEmbedded = "<embedded:mozilla-roots.pem>"

// corporateCAHint is how an operator adds a CA of their own. It is a hint, not
// a fix: a missing system bundle needs none. It is logged only where no CA
// bundle file was found, and names the one setting every part of keploy reads
// the same way: SSL_CERT_FILE takes the place of the built-in roots in the
// apps' bundle and in keploy's upstream verification (the loader), and in
// keploy's own TLS (crypto/x509, which then uses no fallback roots). A
// directory in SSL_CERT_DIR is not offered: with no bundle file, crypto/x509
// trusts only that directory's certificates, so keploy's own calls (to the
// keploy API, say) would lose the public roots the apps keep.
const corporateCAHint = "to trust a corporate CA as well, mount a PEM bundle holding both the public roots and " +
	"the corporate CA into the keploy agent container and set SSL_CERT_FILE to it: the apps and keploy's " +
	"own connections then trust that bundle in place of these built-in roots"

// loadSystemCABundle returns trust-anchor PEM bytes for merging with the
// Keploy MITM CA into the shared-volume bundle (and the native Java
// truststore), plus a source identifier for logging. It reads the trust store
// the way Go's crypto/x509 does on Linux:
//
//  1. The bundle file: SSL_CERT_FILE when the operator set it, otherwise the
//     first file in systemCABundleSearchPaths holding a certificate. When
//     there is neither — an image that ships no CA bundle, like the keploy
//     enterprise agent image — embeddedFallbackRoots, the Mozilla NSS roots
//     compiled into the binary, stands in for it. That is the normal path for
//     such an image, not a fault.
//  2. Plus every certificate in the directories SSL_CERT_DIR names, or when it
//     is unset in systemCABundleSearchDirs, not already in the bundle.
//
// Certificates read from files are re-encoded, without what appendTrustedCerts
// leaves out: anything but a certificate, and keploy's own CAs.
//
// Three deliberate differences from crypto/x509: an SSL_CERT_FILE that cannot
// be used is reported and skipped rather than leaving no bundle file; a fixed
// bundle path holding no certificate (an empty file, say) is skipped where Go
// stops at the first readable one; and the embedded roots stand in for a
// missing bundle file even when the directories add certificates. Each way the
// apps keep public roots: the merged bundle is never just the Keploy MITM CA,
// which is what made every public-endpoint HTTPS call fail
// CERTIFICATE_VERIFY_FAILED in keploy/k8s-proxy#375.
func loadSystemCABundle(logger *zap.Logger) ([]byte, string) {
	env := operatorCertEnv()
	if env.dir == "" && len(systemCABundleSearchDirs) > 0 {
		env.dir, env.defaultDir = strings.Join(systemCABundleSearchDirs, string(filepath.ListSeparator)), true
	}
	return loadCABundle(logger, env, systemCABundleSearchPaths, embeddedFallbackRoots)
}

// loadSystemCABundleFromPaths is the disk-only path used by tests: no
// environment and no embedded fallback, so they assert the disk search in
// isolation. Production code goes through loadSystemCABundle.
func loadSystemCABundleFromPaths(logger *zap.Logger, paths []string) ([]byte, string) {
	return loadCABundle(logger, certEnv{}, paths, nil)
}

// loadCABundle is the testable core of loadSystemCABundle. fallback may be nil
// to disable the embedded roots (the disk-only tests).
func loadCABundle(logger *zap.Logger, env certEnv, paths []string, fallback []byte) ([]byte, string) {
	var bundle []byte
	var sources []string

	if env.file != "" {
		data, err := readCertBundleFile(env.file)
		switch {
		case err == nil:
			bundle, sources = data, append(sources, env.file)
		case errors.Is(err, errOnlyKeployCAs):
			// keploy's own output, inherited from a keploy process (or left by
			// an earlier run): not the operator's trust store.
			logger.Debug("SSL_CERT_FILE names a file holding only keploy's own CA; leaving it out of the trust bundle",
				zap.String("ssl_cert_file", env.file))
		case warnOnce(envSSLCertFile + "=" + env.file):
			logger.Warn("SSL_CERT_FILE names no usable CA bundle, so keploy leaves it out of the trust bundle it gives the app",
				zap.String("ssl_cert_file", env.file),
				zap.Error(err),
				zap.String("next_step", "point SSL_CERT_FILE at a readable PEM bundle where keploy runs (for docker or k8s, mount it into the keploy agent container), or unset it to use the system bundle or keploy's built-in Mozilla roots"))
		}
	}
	if bundle == nil {
		if data, p := firstCertBundle(paths); data != nil {
			bundle, sources = data, append(sources, p)
		}
	}
	if bundle == nil && len(fallback) > 0 {
		bundle, sources = fallback, append(sources, systemCABundleSourceEmbedded)
		logger.Debug("no CA bundle file found; the app's trust bundle uses the Mozilla root list built into keploy",
			zap.Strings("searched_paths", paths),
			zap.Int("embedded_roots_bytes", len(fallback)),
			zap.String("corporate_ca", corporateCAHint))
	}

	if env.dir != "" {
		extra, dirs, found, keploys := readCertDirs(env.dir, bundle)
		if len(extra) > 0 {
			bundle, sources = mergeCABundle(bundle, extra), append(sources, dirs...)
		}
		switch {
		case found > 0, env.defaultDir:
		case keploys > 0:
			logger.Debug("SSL_CERT_DIR holds only keploy's own CA; it adds nothing to the trust bundle",
				zap.String("ssl_cert_dir", env.dir))
		case warnOnce(envSSLCertDir + "=" + env.dir):
			logger.Warn("SSL_CERT_DIR names no directory holding a PEM certificate, so it adds nothing to the trust bundle keploy gives the app",
				zap.String("ssl_cert_dir", env.dir),
				zap.String("next_step", "put the CA certificates (PEM files) in a directory where keploy runs (for docker or k8s, mount it into the keploy agent container) and point SSL_CERT_DIR at it, or unset it"))
		}
	}

	if len(bundle) > 0 {
		return bundle, strings.Join(sources, ",")
	}
	// Reachable only with a nil fallback (the disk-only tests): production
	// always passes embeddedFallbackRoots, which is never empty.
	logger.Warn("no CA bundle found and no embedded fallback configured — the merged bundle will contain only the Keploy MITM CA, which is NOT a valid trust store for public endpoints",
		zap.Strings("searched_paths", paths))
	return nil, ""
}

// firstCertBundle returns the certificates (readCertBundleFile) of the first
// file in paths that holds one, and its path.
func firstCertBundle(paths []string) ([]byte, string) {
	for _, p := range paths {
		// A missing or unreadable candidate is normal — most hosts have only
		// one of them. One holding no certificate (empty: almost always a
		// broken image build) is skipped rather than produce a zero-trust
		// bundle, and so is one holding only keploy's CA.
		if data, err := readCertBundleFile(p); err == nil {
			return data, p
		}
	}
	return nil, ""
}

// certEnvWarned holds the SSL_CERT_FILE / SSL_CERT_DIR values already reported
// as unusable: the bundle is built more than once in a process (the shared
// volume, the upstream roots), and one Warn per value is enough.
var certEnvWarned sync.Map

// warnOnce reports whether key has not been warned about yet in this process.
func warnOnce(key string) bool {
	_, seen := certEnvWarned.LoadOrStore(key, true)
	return !seen
}

// errOnlyKeployCAs is a bundle whose only certificates are keploy's own CAs.
var errOnlyKeployCAs = errors.New("the bundle holds only keploy's own CA")

// readCertBundleFile returns the certificates in a bundle file, as PEM (see
// appendTrustedCerts for what is left out). A file Go's crypto/x509 would load
// no root from is an error.
func readCertBundleFile(path string) ([]byte, error) {
	data, err := readRegularFile(path)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	found, keploys := appendTrustedCerts(&out, data, make(map[[32]byte]bool))
	if found == 0 {
		if keploys > 0 {
			return nil, errOnlyKeployCAs
		}
		blocks, _ := countPEMCertificateBlocks(data)
		return nil, fmt.Errorf("%s holds no certificate (%d bytes, %d PEM CERTIFICATE block(s), none of them a certificate)", path, len(data), blocks)
	}
	return out.Bytes(), nil
}

// readCertDirs returns, as PEM, every certificate in the files of the
// directories in dirList (read as crypto/x509 and OpenSSL read SSL_CERT_DIR)
// that is not already in have, plus the directories that contributed one, how
// many certificates it found in all (those already in have included), and how
// many of keploy's own it left out. A
// directory such as /etc/ssl/certs holds each root several times over (the
// bundle, the per-root file and its hash links), so certificates are
// de-duplicated.
func readCertDirs(dirList string, have []byte) ([]byte, []string, int, int) {
	seen := make(map[[32]byte]bool)
	forEachPEMCertificate(have, func(der []byte) { seen[sha256.Sum256(der)] = true })

	var out bytes.Buffer
	var used []string
	total, keployTotal := 0, 0
	for _, dir := range filepath.SplitList(dirList) {
		if dir == "" {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		before := out.Len()
		for _, ent := range entries {
			data, err := readRegularFile(filepath.Join(dir, ent.Name()))
			if err != nil {
				continue // a subdirectory, a FIFO, or unreadable
			}
			found, keploys := appendTrustedCerts(&out, data, seen)
			total += found
			keployTotal += keploys
		}
		if out.Len() > before {
			used = append(used, dir)
		}
	}
	return out.Bytes(), used, total, keployTotal
}

// maxCertFile bounds what keploy reads from one trust-store file, as the
// enterprise agent's trust-root setup bounds it. The loader reads every file in
// SSL_CERT_DIR and in the default certificate directories, and one large stray
// file there must not be read into memory whole; a full CA bundle is a few
// hundred KB.
const maxCertFile = 16 << 20

// readRegularFile reads path, links followed, refusing anything that is not a
// regular file of at most maxCertFile bytes. It opens without blocking and
// checks the file it opened: a FIFO, even one swapped in for a file, would
// block the open or the read forever.
func readRegularFile(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() // read-only
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxCertFile+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxCertFile {
		return nil, fmt.Errorf("%s is larger than %d bytes, more than a trust-store file holds", path, maxCertFile)
	}
	return data, nil
}

// appendTrustedCerts PEM-encodes into out every certificate in data that is
// not in seen, and adds it to seen. A certificate is a block crypto/x509
// parses, or one with a certificate's shape that Go's stricter parser rejects
// (looksLikeCertificate): a negative serial number, which OpenSSL and Java
// accept and macOS's /etc/ssl/cert.pem ships — the apps' bundle must keep it,
// as generateTrustStore does. It leaves out:
//
//   - every block that is not a certificate: a private key in a combined
//     cert+key file must never reach the bundle, which is world-readable and
//     mounted into every app container;
//   - keploy's own CAs (this or an earlier run's MITM CA, or the retired static
//     one): a bundle keploy wrote, read back through SSL_CERT_FILE, or the
//     system bundle keploy's native install added its CA to, must not carry a
//     stale MITM CA into the next one. This run's CA is added by the caller
//     anyway. keploy's MITM CAs always parse; the retired one is matched by
//     fingerprint.
//
// It returns how many certificates other than keploy's it found (those already
// in seen included) and how many of keploy's it left out.
func appendTrustedCerts(out *bytes.Buffer, data []byte, seen map[[32]byte]bool) (found, keploys int) {
	legacy, legacyOK := legacyCAFingerprint()
	forEachPEMCertificate(data, func(der []byte) {
		sum := sha256.Sum256(der)
		if legacyOK && sum == legacy {
			keploys++
			return
		}
		if seen[sum] {
			// Only certificates that are not keploy's enter seen, so this one
			// was already found; /etc/ssl/certs repeats every root this way.
			found++
			return
		}
		if cert, err := x509.ParseCertificate(der); err == nil {
			if strings.HasPrefix(cert.Subject.CommonName, caCNPrefix) {
				keploys++
				return
			}
		} else if !looksLikeCertificate(der) {
			return
		}
		found++
		seen[sum] = true
		_ = pem.Encode(out, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	})
	return found, keploys
}

// forEachPEMCertificate calls fn with the DER of every block in data that
// AppendCertsFromPEM would consider: type CERTIFICATE, no headers.
func forEachPEMCertificate(data []byte, fn func(der []byte)) {
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			return
		}
		if block.Type == "CERTIFICATE" && len(block.Headers) == 0 {
			fn(block.Bytes)
		}
	}
}

func setupSharedVolume(_ context.Context, logger *zap.Logger, exportPath string, ca *caState) error {
	if err := os.MkdirAll(exportPath, 0755); err != nil {
		return fmt.Errorf("failed to create export dir: %w", err)
	}

	// Load the trust roots the apps need besides keploy's CA: the operator's
	// SSL_CERT_FILE / SSL_CERT_DIR, the OS bundle, or — on an image that ships
	// none — the Mozilla roots built into keploy.
	systemBundle, sourcePath := loadSystemCABundleFn(logger)

	// Build the merged PEM bundle that goes into <exportPath>/ca.crt.
	//
	// This is the file the k8s-proxy admission webhook wires into the app
	// container via REQUESTS_CA_BUNDLE, SSL_CERT_FILE, CARGO_HTTP_CAINFO
	// (which REPLACE their respective runtime's default trust store) and —
	// with different routing — NODE_EXTRA_CA_CERTS (which is ADDED to
	// Node.js's default trust store rather than replacing it; see the
	// separate keploy-ca.crt write below for where Node is sent). So for
	// the replacement-style consumers, writing only the Keploy MITM CA
	// (the previous behaviour) makes every non-proxied HTTPS call
	// (internal cluster services, public endpoints Keploy isn't proxying,
	// DoH, ...) fail with CERTIFICATE_VERIFY_FAILED.
	//
	// Concatenating PEM blocks with a separating newline is the standard way
	// to merge trust anchors — OpenSSL, BoringSSL, NSS, Go's crypto/x509,
	// and every language runtime that honours these env vars parses
	// multi-cert PEM bundles by walking successive BEGIN/END blocks.
	merged := mergeCABundle(systemBundle, ca.certPEM)

	crtPath := filepath.Join(exportPath, "ca.crt")
	if err := os.WriteFile(crtPath, merged, 0644); err != nil {
		return fmt.Errorf("failed to write ca.crt to shared volume: %w", err)
	}

	// Also write the Keploy MITM CA alone to keploy-ca.crt. This file is for
	// consumers that ADD to the system trust store rather than REPLACE it —
	// notably Node.js's NODE_EXTRA_CA_CERTS, which is merged with the default
	// bundle at startup. Pointing NODE_EXTRA_CA_CERTS at the merged file
	// would double-trust the system roots (harmless but wasteful); pointing
	// it at keploy-ca.crt is the minimal correct input.
	keployOnlyPath := filepath.Join(exportPath, "keploy-ca.crt")
	if err := os.WriteFile(keployOnlyPath, ca.certPEM, 0644); err != nil {
		return fmt.Errorf("failed to write keploy-ca.crt to shared volume: %w", err)
	}

	if len(systemBundle) > 0 {
		logger.Info("Merged system CA bundle with Keploy MITM CA",
			zap.Int("system_bytes", len(systemBundle)),
			zap.Int("keploy_bytes", len(ca.certPEM)),
			zap.String("source_path", sourcePath),
			zap.String("output", crtPath),
			zap.String("keploy_only_output", keployOnlyPath),
		)
	}
	// When len(systemBundle) == 0 we deliberately emit NO log here: the
	// loader already logged why, so a second "Wrote Keploy MITM CA only"
	// message would be redundant noise.

	if err := setEnvForSharedVolume(logger, crtPath, keployOnlyPath); err != nil {
		logger.Debug("Failed to set internal env vars for Agent", zap.Error(err))
	}

	// Generate Java Truststore from the MERGED bundle (system roots +
	// Keploy MITM CA). Java's -Djavax.net.ssl.trustStore= REPLACES the
	// default $JAVA_HOME/lib/security/cacerts keystore wholesale, so
	// building the JKS from the Keploy CA alone would leave Java apps
	// unable to validate any non-Keploy-proxied HTTPS (AWS STS,
	// Snowflake, Maven Central, …). generateTrustStore iterates every
	// CERTIFICATE PEM block in crtPath and adds each as a trusted entry
	// — the Keploy root keeps its stable "keploy-root" alias, every
	// system root gets a "system-<sha>" alias so the bundle survives
	// rebuilds deterministically.
	jksPath := filepath.Join(exportPath, "truststore.jks")
	if err := generateTrustStore(crtPath, jksPath, ca.der); err != nil {
		logger.Error("Failed to generate Java truststore", zap.Error(err))
		return err
	}

	logger.Debug("TLS Certificates successfully exported to shared volume")
	// Signal CA-bundle readiness to downstream consumers (e.g. the
	// /agent/ready HTTP handler) only after the ca.crt + truststore
	// have been written successfully. On the error paths above we
	// return early without signalling — readiness stays unlatched so
	// operators can restart the agent with fixed config.
	markCAReady()
	return nil
}

// setupNativeDarwin installs the keploy CA for a native macOS run.
//
// It exists as its own function so it can be unit-tested on Linux (the darwin
// build's tests never run in CI — go_macos.yaml only builds).
//
// Why this is not the Linux path: that path writes into
// /usr/local/share/ca-certificates or /etc/ssl/certs and runs
// update-ca-certificates. On macOS the only one of those directories that
// exists is /etc/ssl/certs, it is root-owned, and none of the update commands
// exist — so falling through left a stray root-owned file or an EACCES, and in
// both cases markCAReady was never reached and the run reported a CA failure.
//
// What each runtime needs, and how it is met here (all measured against a
// keploy MITM leaf on macOS):
//
//	Node    NODE_EXTRA_CA_CERTS   set by the CLIENT (SetupCaCertEnv), works
//	Python  SSL_CERT_FILE etc.    set by the CLIENT (SetupCaCertEnv), works
//	Java    the JDK's cacerts     imported here, works
//	Go      Security.framework    handled by the injected shim's SecTrust
//	                              override, not by any CA env var — Go on darwin
//	                              ignores SSL_CERT_FILE entirely
//
// So the only thing this function must do on the AGENT side is the Java
// keystore import; the Node/Python env vars are the client's job (a separate
// process — the agent's own environment never reaches the app), and Go is the
// shim's. The Java import is best-effort: a machine with no JDK, or a Go/Node
// developer who is not testing Java, must not have CA setup fail underneath
// them, so a keystore failure is logged and the run proceeds.
func setupNativeDarwin(_ context.Context, logger *zap.Logger, _ string) error {
	// Nothing to do on the agent side. Node/Python get their CA via client env
	// vars (SetupCaCertEnv), Go via the injected shim's SecTrust override, and
	// Java via a merged truststore the client points JAVA_TOOL_OPTIONS at
	// (setupJavaTrustStoreEnv) — none of which touches this machine's JDK, CA
	// store, or requires keytool. Keeping this as its own function preserves the
	// darwin-specific readiness path (the Linux CA-store writes do not apply).
	logger.Debug("macOS native CA setup complete (Java via JAVA_TOOL_OPTIONS, no JDK cacerts change)")
	markCAReady()
	return nil
}

// runCertFileName / runAlias derive this run's unique system-store filename and
// JDK alias from the CA's run id. They replace the old shared "ca.crt" filename
// (which clobbered any user file of that name and could not be cleaned up
// selectively) and the fixed "keployCA" alias (whose exists-check silently
// skipped importing a new CA where a prior entry already sat).
func runCertFileName(ca *caState) string { return "keploy-mitm-" + ca.runID + ".crt" }
func runAlias(ca *caState) string        { return "keploy-mitm-" + ca.runID }

// nativeInstall records what this run wrote into the OS trust store so teardown
// can remove exactly that and nothing else. remove is false for a persisted
// per-user CA (Windows), which is kept so the user is not re-prompted next run.
type nativeInstall struct {
	certFile string // system-store filename (Linux)
	nssDir   string // NSS shared database the CA was added to (Linux)
	alias    string // JDK keystore alias
	cn       string // certificate CN, for the Windows ROOT-store removal
	remove   bool
}

var (
	installedNativeMu sync.Mutex
	installedNative   *nativeInstall
)

func recordNativeInstall(n *nativeInstall) {
	installedNativeMu.Lock()
	installedNative = n
	installedNativeMu.Unlock()
}

// setupNativeForApp is the PID-aware native-install path. It installs this run's
// generated CA public certificate into the OS trust store under a unique
// filename for the duration of the run (removed at teardown; crashed-run copies
// are inert because the key died with the process and are swept at next start).
func setupNativeForApp(ctx context.Context, logger *zap.Logger, ca *caState, store caStore, appPID int, javaHomeOverride string) error {
	// Resolve the target JDK's java.home once, up front. Order:
	//   1. --ca-java-home CLI override wins (opts.Agent.CAJavaHome).
	//   2. /proc/<appPID>/environ JAVA_HOME, then /proc/<appPID>/exe.
	//   3. Empty — installJavaCAForHome falls back to PATH keytool.
	// We log the chosen source at Debug so operators can see why a
	// particular cacerts file was targeted without turning on trace.
	resolvedJavaHome := resolveAppJavaHome(logger, appPID, javaHomeOverride)
	alias := runAlias(ca)

	// Windows Specific Logic
	if runtime.GOOS == "windows" {
		// Extract certificate to a temporary file
		tempCertPath, err := extractCertToTemp(ca.certPEM)
		if err != nil {
			utils.LogError(logger, err, "Failed to extract certificate to temp folder")
			return err
		}
		defer func() {
			if err := os.Remove(tempCertPath); err != nil {
				logger.Debug("Failed to remove temporary certificate file", zap.String("path", tempCertPath), zap.Error(err))
			}
		}()

		// Recorded before the install, as on Linux below: whatever it leaves
		// in the ROOT store is removed by this run's teardown however the
		// install ends (removeWindowsCAByName finds nothing to remove when it
		// added nothing). A persisted per-user CA is kept across runs (so the
		// ROOT-store prompt fires at most once); an ephemeral one is removed
		// at teardown.
		recordNativeInstall(&nativeInstall{alias: alias, cn: ca.cert.Subject.CommonName, remove: !store.persistsKey()})

		// Install certificate using certutil
		if err = installWindowsCA(ctx, logger, tempCertPath); err != nil {
			utils.LogError(logger, err, "Failed to install CA certificate on Windows")
			return err
		}

		// install CA in the java keystore if java is installed. Non-fatal: Java
		// also trusts the keploy CA via the JAVA_TOOL_OPTIONS merged truststore
		// the client sets (SetupCaCertEnv), so a missing/failing keytool must not
		// fail CA setup for a run that may not even use Java.
		if err = installJavaCAForHome(ctx, logger, tempCertPath, resolvedJavaHome, alias); err != nil {
			logger.Warn("could not import the keploy CA into the Java keystore; Java uses the JAVA_TOOL_OPTIONS truststore instead", zap.Error(err))
		}

		// Set environment variables for Node.js and Python to use the custom CA
		if err := SetEnvForPath(logger, tempCertPath); err != nil {
			return err
		}
		// Mirror the shared-volume path: only signal readiness when the
		// full install chain (cert import + java keystore + env vars)
		// has succeeded.
		markCAReady()
		return nil
	}

	// macOS Specific Logic
	if runtime.GOOS == "darwin" {
		return setupNativeDarwin(ctx, logger, resolvedJavaHome)
	}
	// Linux/Unix Specific Logic
	caPaths, err := getCaPaths()
	if err != nil {
		utils.LogError(logger, err, "Failed to find the CA store path")
		return err
	}

	// Remove any keploy-mitm-*.crt a previous run left behind on a crash before
	// installing this run's. Those copies are inert (their signing key died with
	// the process), but leaving them accumulates dead anchors.
	sweepRunLeftovers(logger, ca.runID)

	certFileName := runCertFileName(ca)
	// Recorded before the first anchor is written, so that whatever this
	// install leaves in the trust store -- an anchor, the JDK alias -- is
	// removed by this run's teardown (TeardownNativeCA) however the install
	// ends: also when it fails part way, or the stop cuts it short (the stop
	// cancels ctx, which kills the store's update after the anchors are
	// written). Recorded only once the update had succeeded, such an install
	// left its anchor trusted until the next run's sweep.
	nssDir := nssDBDir(appPID)
	recordNativeInstall(&nativeInstall{certFile: certFileName, alias: alias, nssDir: nssDir, remove: true})
	var finalCAPath string
	for _, path := range caPaths {
		caPath := filepath.Join(path, certFileName)
		finalCAPath = caPath // Keep one valid path for env vars

		// Write the cert with explicit 0644 (NOT os.Create's 0666, which under
		// keploy's process-wide umask 0 left a world-WRITABLE anchor any local
		// user could swap). O_TRUNC so a stale same-name file is overwritten.
		fs, err := os.OpenFile(caPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
		if err != nil {
			utils.LogError(logger, err, "Failed to create path for ca certificate", zap.Any("root store path", path))
			return err
		}
		if _, err = fs.Write(ca.certPEM); err != nil {
			fs.Close()
			utils.LogError(logger, err, "Failed to write custom ca certificate", zap.Any("root store path", path))
			return err
		}
		fs.Close()

		// install CA in the java keystore if java is installed. Non-fatal for the
		// same reason as the Windows path above: JAVA_TOOL_OPTIONS is the primary
		// Java mechanism now, so a missing keytool degrades gracefully.
		if err := installJavaCAForHome(ctx, logger, caPath, resolvedJavaHome, alias); err != nil {
			logger.Warn("could not import the keploy CA into the Java keystore; Java uses the JAVA_TOOL_OPTIONS truststore instead", zap.Error(err))
		}
	}

	// Update the system store
	if err := updateCaStore(ctx); err != nil {
		utils.LogError(logger, err, "Failed to update the CA store")
		return err
	}

	// Set Env Vars pointing to the installed cert.
	//
	// By construction finalCAPath is non-empty here: getCaPaths() returns
	// an error (caught above) when it can't find any CA store path, so the
	// per-path loop that assigns finalCAPath always runs at least once on
	// this code path. Treating the empty case as a programmer error via a
	// defensive guard keeps this obvious to future readers who might
	// otherwise re-introduce the "no CA store found but SetupCA succeeded"
	// inconsistency, which would silently allow /agent/ready to latch
	// while apps still distrust the Keploy MITM CA.
	if finalCAPath == "" {
		return fmt.Errorf("setupNative: finalCAPath is empty after ranging %d CA store paths — this is a programmer error (getCaPaths should have returned an error)", len(caPaths))
	}
	if err := SetEnvForPath(logger, finalCAPath); err != nil {
		return err
	}
	// Chromium-based browsers and other NSS clients read neither the system
	// store nor the variables above (ca_nss.go).
	installNSSCA(ctx, logger, nssDir, finalCAPath, alias)

	// Native mode completed successfully — the CA is now installed in
	// the system store (and the Java keystore if Java is present).
	// Signal readiness so downstream consumers (e.g. /agent/ready) can
	// unblock.
	markCAReady()
	return nil
}

// TeardownNativeCA removes the CA this run installed into the OS trust store so
// trust does not persist after the run. It is called from the proxy's shutdown
// path, where the run's context is already cancelled — so it runs the trust-store
// and keytool commands under its own short-lived context rather than the dead
// one. Safe to call when nothing was installed (docker/k8s modes, or a persisted
// per-user CA): it is then a no-op.
func TeardownNativeCA(logger *zap.Logger) {
	installedNativeMu.Lock()
	n := installedNative
	installedNative = nil
	installedNativeMu.Unlock()
	if n == nil || !n.remove {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	removedAny := false
	if n.certFile != "" {
		for _, dir := range caStorePath {
			p := filepath.Join(dir, n.certFile)
			if err := os.Remove(p); err == nil {
				removedAny = true
				logger.Debug("removed this run's CA anchor", zap.String("path", p))
			}
		}
	}
	if removedAny {
		// A plain refresh (not --fresh): this run's anchors are uniquely named
		// keploy-mitm-<id>.crt, removed directly here, so a full bundle rebuild is
		// unnecessary. --fresh is reserved for `keploy ca clean`, where pruning
		// stale hash-links of a generically-named legacy anchor matters.
		if err := refreshCaStore(ctx, false); err != nil {
			logger.Debug("could not refresh the CA store after removing this run's anchor", zap.Error(err))
		}
	}
	removeJavaAlias(ctx, logger, n.alias)
	removeNSSCA(ctx, logger, n.nssDir, n.alias)
	if runtime.GOOS == "windows" {
		removeWindowsCAByName(ctx, logger, n.cn)
	}
}

// removeWindowsCAByName removes a CA from the Windows ROOT store by its subject
// CN, best-effort. certutil exists only on Windows; elsewhere LookPath fails and
// this is a no-op, so the function compiles on every platform.
func removeWindowsCAByName(ctx context.Context, logger *zap.Logger, cn string) {
	if cn == "" {
		return
	}
	if _, err := exec.LookPath("certutil"); err != nil {
		return
	}
	// Try the per-user store first (where installWindowsCA adds it), then the
	// machine store fallback.
	for _, args := range [][]string{
		{"-user", "-delstore", "ROOT", cn},
		{"-delstore", "ROOT", cn},
	} {
		if err := exec.CommandContext(ctx, "certutil", args...).Run(); err == nil {
			logger.Debug("removed this run's CA from the Windows ROOT store", zap.String("cn", cn))
			return
		}
	}
}

// sweepRunLeftovers removes keploy-mitm-*.crt anchors from a previous crashed
// run (any run id other than the current one). They are inert — their signing
// key is gone — so this is housekeeping, not a security fix.
func sweepRunLeftovers(logger *zap.Logger, currentRunID string) {
	current := "keploy-mitm-" + currentRunID + ".crt"
	for _, dir := range caStorePath {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if name == current || !strings.HasPrefix(name, "keploy-mitm-") || !strings.HasSuffix(name, ".crt") {
				continue
			}
			if err := os.Remove(filepath.Join(dir, name)); err == nil {
				logger.Debug("swept a crashed run's CA anchor", zap.String("path", filepath.Join(dir, name)))
			}
		}
	}
}

// removeJavaAlias deletes alias from the resolvable JDK's cacerts, best-effort.
func removeJavaAlias(ctx context.Context, logger *zap.Logger, alias string) {
	keytool, cacerts, ok := resolveKeytoolAndCacerts()
	if !ok {
		return
	}
	cmd := exec.CommandContext(ctx, keytool, "-delete", "-alias", alias, "-keystore", cacerts, "-storepass", "changeit", "-noprompt")
	if out, err := cmd.CombinedOutput(); err != nil {
		logger.Debug("could not delete this run's JDK alias", zap.String("alias", alias), zap.ByteString("output", out), zap.Error(err))
	}
}

// extractCertToTemp writes the run CA's public certificate to a temp file for
// certutil and keytool to read (Windows native install), which removes it once
// they have.
//
// Mode 0644 (world-readable, owner-writable) — NOT the old 0666, which under
// keploy's process-wide umask 0 left the file world-WRITABLE so any local user
// could replace the cert an app was about to trust. The certificate is public,
// so world-readable is fine; world-writable was the bug.
func extractCertToTemp(certPEM []byte) (string, error) {
	tempFile, err := os.CreateTemp("", "keploy-ca-*.crt")
	if err != nil {
		return "", err
	}
	defer tempFile.Close()

	if err = os.Chmod(tempFile.Name(), 0644); err != nil {
		return "", err
	}

	if _, err = tempFile.Write(certPEM); err != nil {
		return "", err
	}

	return tempFile.Name(), nil
}

// generateTrustStore creates a JKS file from every CERTIFICATE PEM block
// found in certPath, using pure Go (no 'keytool' dependency).
//
// The input file MAY contain multiple concatenated PEM blocks — e.g. the
// merged system-CA-bundle + Keploy MITM CA that setupSharedVolume writes
// to /tmp/keploy-tls/ca.crt. Every CERTIFICATE block is parsed and added
// as a trusted entry so Java apps pointed at this keystore via
// -Djavax.net.ssl.trustStore= can validate both public TLS endpoints
// (e.g. AWS STS, Snowflake) and Keploy-proxied endpoints.
//
// Alias scheme:
//   - "keploy-root" for the Keploy MITM CA (matched by comparing the
//     SHA-256 fingerprint of the run CA DER passed in as keployDER against
//     sha256.Sum256(cert.Raw). Using the fingerprint rather than raw
//     byte equality is robust to Subject-name renaming between
//     releases and survives any cosmetic PEM encoding differences.)
//     This alias remains the same as the
//     legacy single-cert implementation so external callers that
//     keytool-list for it continue to work.
//   - "system-<sha256-hex>" for every other cert (the SHA-256 of the
//     DER bytes, lowercase hex, 64 chars — guaranteed unique, survives
//     re-runs deterministically).
//
// Non-CERTIFICATE PEM blocks (keys, CRLs, DH params, ...) that might
// appear in unusual bundles are skipped; a parse error on any single
// block returns an error so we fail loudly rather than silently ship a
// partial trust store. A truncated/malformed -----BEGIN----- armour in
// the trailing unparsed bytes is likewise treated as a fatal error
// (otherwise pem.Decode's nil-on-malformed return would let the last
// certificate in a corrupted bundle disappear from the JKS silently).
// looksLikeCertificate reports whether der is structurally an X.509 certificate
// — a SEQUENCE of { tbsCertificate, signatureAlgorithm, signatureValue } — using
// only the shape, not Go's strict field validation (which is what rejected the
// certificate in the first place, e.g. a negative serial number inside the TBS).
func looksLikeCertificate(der []byte) bool {
	var shape struct {
		TBS      asn1.RawValue
		SigAlg   asn1.RawValue
		SigValue asn1.BitString
	}
	rest, err := asn1.Unmarshal(der, &shape)
	return err == nil && len(rest) == 0
}

// generateTrustStore builds a JKS from every CERTIFICATE in certPath. keployDER
// is the DER of this run's MITM CA, used to give it the stable "keploy-root"
// alias; a nil keployDER just means every entry gets the generic "system-<sha>"
// alias (degraded naming, still correct).
func generateTrustStore(certPath, jksPath string, keployDER []byte) error {
	pemBytes, err := os.ReadFile(certPath)
	if err != nil {
		return fmt.Errorf("failed to read cert pem: %w", err)
	}

	ks := keystore.New()
	rest := pemBytes
	// pemBlockIdx counts every PEM block we encountered in the input — both
	// CERTIFICATE blocks that end up in the keystore AND non-CERTIFICATE
	// blocks (keys, CRLs, DH params, …) we skip. It's what we report in
	// diagnostic error messages so the offset in the error matches what a
	// human counts when eyeballing the file.
	//
	// added counts only the CERTIFICATE blocks that parsed cleanly AND
	// landed in the keystore. We use it at the end to distinguish "input
	// contained only non-CERTIFICATE noise" from "input contained at least
	// one valid certificate".
	pemBlockIdx := 0
	added := 0
	var keployFingerprint [32]byte
	haveKeployFP := len(keployDER) > 0
	if haveKeployFP {
		keployFingerprint = sha256.Sum256(keployDER)
	}
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			// pem.Decode returns nil for "no more PEM blocks" AND for
			// "malformed PEM block" — the two are indistinguishable from
			// the return. A whitespace-only or comment-only trailer is
			// the benign case (openssl tools commonly append a trailing
			// newline, and trust bundles often carry a human-readable
			// header before the first -----BEGIN----- line). But if the
			// unparsed tail STILL contains a BEGIN armour, we likely have
			// a truncated or corrupted CERTIFICATE block at the end of
			// the file — silently dropping it would produce a partial
			// JKS where one or more trust anchors are missing at
			// runtime. Fail loudly instead.
			if bytes.Contains(rest, []byte("-----BEGIN")) {
				return fmt.Errorf("generateTrustStore: malformed PEM armour in trailing data of %s (found unparseable -----BEGIN marker after %d successfully-decoded block(s)); trust store would be incomplete", certPath, pemBlockIdx)
			}
			break
		}
		pemBlockIdx++
		if block.Type != "CERTIFICATE" {
			continue
		}
		// The keystore entry only needs the DER, and the alias is derived from it,
		// so a full parse is not required — and must not be stricter than keytool.
		// A real trust bundle can carry a root Go's parser rejects but Java
		// accepts, most commonly a negative serial number, which macOS's
		// /etc/ssl/cert.pem still ships. Parse only to drop genuine garbage:
		// on a parse error keep the DER anyway if it is at least an ASN.1
		// SEQUENCE, so one legacy root can no longer sink the whole store.
		der := block.Bytes
		if _, err := x509.ParseCertificate(der); err != nil {
			// Go's strict parser rejects some real roots a JVM accepts (most
			// commonly a negative serial number, which macOS's /etc/ssl/cert.pem
			// ships). Keep such a root — but ONLY if the DER is structurally an
			// X.509 certificate. A well-formed but non-certificate SEQUENCE must
			// be dropped: Java's keystore load is all-or-nothing, so one bad
			// entry would make KeyStore.load throw and break ALL of the app's
			// Java TLS — far worse than losing one legacy root.
			if !looksLikeCertificate(der) {
				continue
			}
		}

		alias := ""
		if haveKeployFP && sha256.Sum256(der) == keployFingerprint {
			alias = "keploy-root"
		} else {
			alias = fmt.Sprintf("system-%x", sha256.Sum256(der))
		}

		ks.SetTrustedCertificateEntry(alias, keystore.TrustedCertificateEntry{
			Certificate: keystore.Certificate{
				Type:    "X.509",
				Content: der,
			},
		})
		added++
	}
	if added == 0 {
		return fmt.Errorf("no CERTIFICATE PEM blocks found in %s", certPath)
	}

	f, err := os.Create(jksPath)
	if err != nil {
		return fmt.Errorf("failed to create jks file: %w", err)
	}
	defer f.Close()

	password := []byte("changeit")
	if err := ks.Store(f, password); err != nil {
		return fmt.Errorf("failed to store jks: %w", err)
	}

	return nil
}

// decodeFirstCertDER returns the DER of the first CERTIFICATE block in certPEM,
// or nil if there is none. Used to identify the keploy root when building a Java
// truststore from a client-fetched cert.
func decodeFirstCertDER(certPEM []byte) []byte {
	rest := certPEM
	for {
		block, r := pem.Decode(rest)
		if block == nil {
			return nil
		}
		if block.Type == "CERTIFICATE" {
			return block.Bytes
		}
		rest = r
	}
}

func commandExists(cmd string) bool {
	_, err := exec.LookPath(cmd)
	return err == nil
}

func updateCaStore(ctx context.Context) error {
	commandRun := false
	for _, cmd := range caStoreUpdateCmd {
		if commandExists(cmd) {
			commandRun = true
			c := exec.CommandContext(ctx, cmd)
			_, err := c.CombinedOutput()
			utils.BootStep(cmd + " is over") // the trust store's update
			if err != nil {
				return err
			}
			break
		}
	}
	if !commandRun {
		return fmt.Errorf("no valid CA store tools command found")
	}
	return nil
}

func getCaPaths() ([]string, error) {
	var caPaths []string
	for _, dir := range caStorePath {
		if util.IsDirectoryExist(dir) {
			caPaths = append(caPaths, dir)
		}
	}
	if len(caPaths) == 0 {
		return nil, fmt.Errorf("no valid CA store path found")
	}
	return caPaths, nil
}

func isJavaCAExist(ctx context.Context, alias, storepass, cacertsPath string) bool {
	return isJavaCAExistWithTool(ctx, "keytool", alias, storepass, cacertsPath)
}

// isJavaCAExistWithTool checks whether the given alias already exists in
// cacertsPath, using a specific keytool executable.
//
// The split vs isJavaCAExist exists so callers with a resolved
// $javaHome/bin/keytool (installJavaCAForHome) use the SAME keytool for
// the existence check as for the import — otherwise a PATH keytool's
// different default store format or cacerts layout can produce a false
// negative and we'd re-import over and over across agent restarts.
func isJavaCAExistWithTool(ctx context.Context, keytoolBin, alias, storepass, cacertsPath string) bool {
	cmd := exec.CommandContext(ctx, keytoolBin, "-list", "-keystore", cacertsPath, "-storepass", storepass, "-alias", alias)
	err := cmd.Run()
	utils.BootStep("keytool -list is over")
	select {
	case <-ctx.Done():
		return false
	default:
	}
	return err == nil
}

// installJavaCA installs the Keploy MITM CA into the Java keystore
// resolved from PATH (`java -XshowSettings:properties`).
//
// Legacy entry point — prefer installJavaCAForHome when the app PID or
// operator override is available. On multi-JDK hosts (SDKMAN, Maven
// wrappers, fat-jar launches with absolute JDK paths) the PATH JDK may
// differ from the one the app actually runs with, which silently causes
// the Keploy MITM certificate to be trusted in the wrong cacerts file
// — TLS handshakes from the app then fail cert-verify even though this
// function returned nil.
//
// Signature preserved for backward compat (external callers + tests). It uses
// the legacy fixed alias; production callers pass a per-run alias instead.
func installJavaCA(ctx context.Context, logger *zap.Logger, caPath string) error {
	return installJavaCAForHome(ctx, logger, caPath, "", legacyCAAlias)
}

// installJavaCAForHome installs the Keploy MITM CA into a specific JDK's
// cacerts truststore.
//
// javaHome semantics:
//   - "" (empty): legacy behaviour — fall back to
//     util.GetJavaHome(ctx), which runs `java` from PATH. Used when the
//     agent has no better signal (pre-registration boot, or the
//     auto-detector returned empty).
//   - non-empty: use $javaHome/bin/keytool as the executable and
//     $javaHome/lib/security/cacerts as the target keystore, bypassing
//     PATH. This is the path that fixes the SDKMAN/Maven-wrapper
//     divergence described at the top of java_detect.go.
//
// The split matters because keytool reads and writes cacerts using its
// own JDK's security providers; on multi-JDK hosts the PATH keytool can
// silently write to a different file than the app's JVM reads at
// startup, producing "unable to find valid certification path" at TLS
// handshake time even though the import appeared to succeed.
func installJavaCAForHome(ctx context.Context, logger *zap.Logger, caPath, javaHome, alias string) error {
	if javaHome == "" {
		// Legacy path — fall back to PATH keytool after the usual
		// "is java installed?" guard. When no PATH java exists this
		// is a no-op (Debug-logged), which is correct on hosts that
		// don't run Java workloads at all.
		if !util.IsJavaInstalled() {
			logger.Debug("Java is not installed on the system")
			return nil
		}
		logger.Debug("checking java path from default java home")
		var err error
		javaHome, err = util.GetJavaHome(ctx)
		if err != nil {
			utils.LogError(logger, err, "Java detected but failed to find JAVA_HOME")
			return err
		}
	}

	// Resolve the JDK-specific keytool binary so we write to the
	// cacerts of THIS JDK, not whatever happens to be first on PATH.
	// On hosts with a single JDK the two resolve to the same thing;
	// on multi-JDK / SDKMAN hosts they diverge and the bug we're
	// fixing shows up as a TLS handshake failure in the app despite
	// SetupCA appearing to succeed.
	keytoolBin := filepath.Join(javaHome, "bin", "keytool")
	if runtime.GOOS == "windows" {
		keytoolBin = filepath.Join(javaHome, "bin", "keytool.exe")
	}
	// If the expected keytool binary is absent (e.g. a JRE-only layout
	// or a malformed javaHome path), fall back to PATH keytool so we
	// don't regress single-JDK hosts. We still target the resolved
	// cacerts path — that's the part that actually determines which
	// truststore the app's JVM reads.
	if _, statErr := os.Stat(keytoolBin); statErr != nil {
		logger.Debug("resolved keytool binary not found in javaHome; falling back to PATH keytool",
			zap.String("expected", keytoolBin), zap.Error(statErr))
		keytoolBin = "keytool"
	}

	// Assuming modern Java structure (without /jre/).
	// Use filepath.Join for proper cross-platform path handling (Windows uses backslashes).
	cacertsPath := filepath.Join(javaHome, "lib", "security", "cacerts")
	// You can modify these as per your requirements
	storePass := "changeit"

	logger.Debug("", zap.String("java_home", javaHome), zap.String("keytool", keytoolBin), zap.String("caCertsPath", cacertsPath), zap.String("caPath", caPath), zap.String("alias", alias))

	if isJavaCAExistWithTool(ctx, keytoolBin, alias, storePass, cacertsPath) {
		logger.Debug("Java detected and CA already exists", zap.String("path", cacertsPath))
		return nil
	}

	cmd := exec.CommandContext(ctx, keytoolBin, "-import", "-trustcacerts", "-keystore", cacertsPath, "-storepass", storePass, "-noprompt", "-alias", alias, "-file", caPath)
	cmdOutput, err := cmd.CombinedOutput()
	utils.BootStep("keytool -import is over")
	if err != nil {
		utils.LogError(logger, err, "Java detected but failed to import CA", zap.String("output", string(cmdOutput)))
		return err
	}
	logger.Debug("Java detected and successfully imported CA", zap.String("path", cacertsPath), zap.String("output", string(cmdOutput)))
	logger.Debug("Successfully imported CA", zap.ByteString("output", cmdOutput))
	return nil
}

// installWindowsCA installs the CA certificate into a Windows trust store.
//
// The per-user store is tried FIRST, and it is the one that normally succeeds.
// `certutil -addstore ROOT` without -user targets the machine-wide store, which
// requires Administrator: on an ordinary terminal it fails with access denied,
// SetupCA returns that error, and the run continues with a proxy that MITMs
// every TLS connection using a leaf nothing trusts. The application's HTTPS
// dependencies then fail certificate verification — a worse outcome than not
// intercepting at all, and one that used to be masked only because the previous
// Windows backend needed a kernel driver and was therefore always elevated.
//
// The per-user store is also the CORRECT scope, not merely the reachable one.
// Keploy launches the application itself, as the invoking user, so that is the
// only trust store that has to contain the CA — and installing there does not
// alter machine-wide trust for every other user and service on the host.
//
// The machine store is kept as a fallback for setups where the per-user store
// is unavailable (a service account with no loaded profile, say), which is also
// what preserves the previous behaviour for an elevated run.
func installWindowsCA(ctx context.Context, logger *zap.Logger, certPath string) error {
	userCmd := exec.CommandContext(ctx, "certutil", "-user", "-addstore", "-f", "ROOT", certPath)
	// Windows asks the user, in a dialog, whether to trust a certificate added
	// to their ROOT store, and certutil waits for the answer: the agent's
	// start waits for the user (utils.BeginBootPrompt), however long they take.
	answered := utils.BeginBootPrompt("Windows' prompt to trust keploy's CA")
	userOut, userErr := userCmd.CombinedOutput()
	answered()
	if userErr == nil {
		logger.Debug("installed the keploy CA in the current user's Windows ROOT store",
			zap.String("output", string(userOut)))
		return nil
	}
	logger.Debug("could not install the keploy CA in the per-user Windows ROOT store; trying the machine store",
		zap.Error(userErr), zap.String("output", string(userOut)))

	machineCmd := exec.CommandContext(ctx, "certutil", "-addstore", "-f", "ROOT", certPath)
	machineOut, machineErr := machineCmd.CombinedOutput()
	utils.BootStep("certutil -addstore is over")
	if machineErr != nil {
		utils.LogError(logger, machineErr, "Failed to install CA certificate using certutil",
			zap.String("user_store_output", string(userOut)),
			zap.String("machine_store_output", string(machineOut)),
			zap.String("next_step", "the machine-wide certificate store needs Administrator; re-run in an elevated terminal, or import "+certPath+" into your user's Trusted Root store manually. Without the CA installed, the application's HTTPS calls will fail certificate verification."))
		return machineErr
	}
	logger.Debug("installed the keploy CA in the machine-wide Windows ROOT store",
		zap.String("output", string(machineOut)))
	return nil
}

// SrcPortToDstURL holds, under a client's source port, the name the client
// asked for on its latest connection through keploy's MITM: the target of
// its CONNECT request once it made one, then, from its handshake, the SNI of
// its ClientHello, else that target ("" for neither).
//
// A source port is not a connection: another client with the same port (a pod
// of the same node reaching the DaemonSet agent, or a connection that took a
// port before the previous one's entry went) reads or overwrites this one's
// entry. So nothing in this module reads it. A connection's names are read off
// the connection: its SNI from the TLS connection keploy served it
// (util.ClientServerName), its CONNECT target from its context
// (ConnectTargetFrom). It is still filed for the readers outside this module
// (keploy/integrations' Postgres v3 recorder).
var SrcPortToDstURL = sync.Map{}

// srcPortOwner tracks which live connection currently owns a source port,
// keyed by source port → owner token (the connection's unique id). It exists
// solely to make the delete-on-close of SrcPortToDstURL safe against source
// port recycling: the OS reuses a client source port once the previous
// connection's socket is closed, and that close happens (LIFO) before the
// previous connection's deferred cleanup runs — so without an ownership check
// the older connection's cleanup would clobber the NEW connection's freshly
// stored mapping. CompareAndDelete on the owner token is the linearization
// point: whichever connection currently owns the port is the only one allowed
// to delete its mapping.
var srcPortOwner = sync.Map{}

// ClaimSrcPort records token as the current owner of port. The newest
// connection on a (possibly recycled) source port wins, overwriting any stale
// owner left by a previous connection whose cleanup has not yet run.
func ClaimSrcPort(port int, token int64) {
	srcPortOwner.Store(port, token)
}

// ReleaseSrcPortIfOwner deletes port's SrcPortToDstURL mapping, but ONLY if
// token still owns the port — i.e. no newer connection has reclaimed the
// recycled port in the meantime. Returns true when it actually deleted.
// Safe to call for non-CONNECT/non-TLS connections (the keys are simply absent).
func ReleaseSrcPortIfOwner(port int, token int64) bool {
	if srcPortOwner.CompareAndDelete(port, token) {
		SrcPortToDstURL.Delete(port)
		return true
	}
	return false
}

var setLogLevelOnce sync.Once

// certCache caches generated TLS certificates by hostname to avoid regenerating
// a certificate for every connection to the same host. Without this cache,
// N concurrent CONNECT tunnels to the same host cause N parallel cert generations,
// saturating CPU in resource-constrained environments (e.g., K8s pods).
var (
	certCache     *expirable.LRU[string, *tls.Certificate]
	certCacheOnce sync.Once
)

// probeCertOnce, probeCertEnabled: see pkg/agent/proxy/proxy.go probeOn
// for the sibling toggle. Replicated here because ca.go is a dependency
// of proxy and cannot import it. Both gates read the same env var, so
// they flip in lockstep across a single run.
var (
	probeCertOnce    sync.Once
	probeCertEnabled atomic.Bool
)

func probeCertOn() bool {
	probeCertOnce.Do(func() {
		if os.Getenv("KEPLOY_PROBE_FANOUT") == "1" {
			probeCertEnabled.Store(true)
		}
	})
	return probeCertEnabled.Load()
}

func probeCert(logger *zap.Logger, phase, sni string, durNs int64, fields ...zap.Field) {
	if !probeCertOn() {
		return
	}
	base := []zap.Field{
		zap.String("probe", "cert"),
		zap.String("phase", phase),
		zap.String("sni", sni),
		zap.Int64("dur_ns", durNs),
		zap.Int64("ts_ns", time.Now().UnixNano()),
	}
	logger.Info("[PROBE/cert]", append(base, fields...)...)
}

const (
	certCacheMaxSize = 1024
	certCacheTTL     = 24 * time.Hour
)

func getCertCache() *expirable.LRU[string, *tls.Certificate] {
	certCacheOnce.Do(func() {
		certCache = expirable.NewLRU[string, *tls.Certificate](certCacheMaxSize, nil, certCacheTTL)
	})
	return certCache
}

func CertForClient(logger *zap.Logger, clientHello *tls.ClientHelloInfo, caPrivKey any, caCertParsed *x509.Certificate, backdate time.Time) (*tls.Certificate, error) {
	return certForClient(logger, clientHello, caPrivKey, caCertParsed, backdate, "", nil)
}

// certForClient is CertForClient with what the connection itself says of its
// destination: the host its CONNECT request named ("" when it made none) and
// the IP it dialled (nil when unknown). The leaf is named from the SNI, else
// the CONNECT target; with neither, it is named for dstIP, because a client
// that dialled an IP literal sends no SNI and checks the leaf's IP SANs.
//
// Nothing else names it. The name used to fall back to the one filed under
// the client's source port (SrcPortToDstURL), and a port is not a connection:
// another client on the same port (a pod of the same node, or a connection
// whose port was reused) had its host put on this client's leaf.
func certForClient(logger *zap.Logger, clientHello *tls.ClientHelloInfo, caPrivKey any, caCertParsed *x509.Certificate, backdate time.Time, connectTarget string, dstIP net.IP) (*tls.Certificate, error) {
	// Ensure log level is set only once

	/*
	* Since multiple goroutines can call this function concurrently, we need to ensure that the log level is set only once.
	 */
	setLogLevelOnce.Do(func() {
		// * Set the log level to error to avoid unnecessary logs. like below...

		// 2025/03/18 20:54:25 [INFO] received CSR
		// 2025/03/18 20:54:25 [INFO] generating key: ecdsa-256
		// 2025/03/18 20:54:25 [INFO] received CSR
		// 2025/03/18 20:54:25 [INFO] generating key: ecdsa-256
		// 2025/03/18 20:54:25 [INFO] encoded CSR
		// 2025/03/18 20:54:25 [INFO] encoded CSR
		// 2025/03/18 20:54:25 [INFO] signed certificate with serial number 435398774381835435678674951099961010543769077102
		cfsslLog.Level = cfsslLog.LevelError
	})

	// Generate a new server certificate and private key for the given hostname.
	// When the client omits SNI (common after CONNECT tunnel setup where the
	// client already knows the target from the CONNECT request), fall back to
	// the host its CONNECT request named.
	dstURL := clientHello.ServerName
	if dstURL == "" {
		dstURL = connectTarget
	}
	// The client's source port names nothing here. It is the key the
	// channel-binding shim pairs this leaf under (publishMITM) and
	// SrcPortToDstURL files the name under, which a connection that is not
	// TCP has none of (0: neither is done).
	sourcePort := 0
	if remoteAddr, ok := clientHello.Conn.RemoteAddr().(*net.TCPAddr); ok {
		sourcePort = remoteAddr.Port
	}
	if sourcePort != 0 {
		// Filed as it always was, for the map's readers outside this module:
		// what this handshake was asked for, "" included.
		SrcPortToDstURL.Store(sourcePort, dstURL)
	}

	// The name the leaf is minted for. With no SNI and no CONNECT target the
	// client dialled an IP literal, and it verifies the leaf against an IP SAN,
	// which cfssl makes of an IP host.
	certHost := dstURL
	if certHost == "" && dstIP != nil {
		certHost = dstIP.String()
	}

	// Cache key = host + backdate bucket + CA identity. Keying on the hostname
	// alone was wrong twice over: (1) it ignored backdate, so a frozen-time
	// replay (NotBefore = backdate−1y) could be served a leaf minted for a
	// different instant and rejected by an app with a frozen clock; (2) it
	// ignored which CA signed the leaf, so a regenerated CA could serve a stale
	// leaf. A zero backdate — the live record path — buckets to one "live" key so
	// repeated connections to the same host still hit the cache.
	cacheKey := certCacheKey(certHost, backdate, caCertParsed)

	// Check the cert cache before generating a new certificate.
	if certHost != "" {
		if cached, ok := getCertCache().Get(cacheKey); ok {
			probeCert(logger, "cache-hit", dstURL, 0)
			logger.Debug("reusing cached certificate", zap.String("hostname", certHost))
			publishMITM(sourcePort, cached)
			return cached, nil
		}
	}
	probeCert(logger, "mint-start", dstURL, 0)
	mintStart := time.Now()
	defer func() {
		probeCert(logger, "mint-done", dstURL, time.Since(mintStart).Nanoseconds())
	}()

	serverReq := &csr.CertificateRequest{
		CN: certHost,
		Hosts: []string{
			certHost,
		},
		KeyRequest: csr.NewKeyRequest(),
	}

	serverCsr, serverKey, err := csr.ParseRequest(serverReq)
	if err != nil {
		return nil, fmt.Errorf("failed to create server CSR: %v", err)
	}
	cryptoSigner, ok := caPrivKey.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("failed to typecast the caPrivKey")
	}
	signerd, err := local.NewSigner(cryptoSigner, caCertParsed, signer.DefaultSigAlgo(cryptoSigner), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create signer: %v", err)
	}

	if backdate.IsZero() {
		logger.Debug("backdate is zero, using current time")
		backdate = time.Now()
	}

	// Case: time freezing (an Ent. feature) is enabled,
	// If application time is frozen in past, and the certificate is signed today, then the certificate will be invalid.
	// This results in a certificate error during tls handshake.
	// To avoid this, we set the certificate’s validity period (NotBefore and NotAfter)
	// by referencing the testcase request time of the application (backdate) instead of the current real time.
	//
	// Note: If you have recorded test cases before April 20, 2024 (http://www.sslchecker.com/certdecoder?su=269725513dfeb137f6f29b8488f17ca9)
	// and are using time freezing, please reach out to us if you get tls handshake error.
	signReq := signer.SignRequest{
		Hosts:     serverReq.Hosts,
		Request:   string(serverCsr),
		Profile:   "web",
		NotBefore: backdate.AddDate(-1, 0, 0),
		NotAfter:  time.Now().AddDate(1, 0, 0),
	}

	serverCert, err := signerd.Sign(signReq)
	if err != nil {
		return nil, fmt.Errorf("failed to sign server certificate: %v", err)
	}

	logger.Debug("signed the certificate for a duration of 2 years", zap.String("notBefore", signReq.NotBefore.String()), zap.String("notAfter", signReq.NotAfter.String()))

	// Load the server certificate and private key
	serverTLSCert, err := tls.X509KeyPair(serverCert, serverKey)
	if err != nil {
		return nil, fmt.Errorf("failed to load server certificate and key: %v", err)
	}

	// Pre-populate Leaf to avoid a data race: crypto/tls lazily parses
	// Certificate.Leaf on first use, which is not goroutine-safe. Parsing
	// it here, before caching, ensures all concurrent consumers see a
	// fully-initialized certificate.
	if serverTLSCert.Leaf == nil {
		leaf, parseErr := x509.ParseCertificate(serverTLSCert.Certificate[0])
		if parseErr != nil {
			logger.Debug("failed to pre-parse certificate leaf, skipping cache", zap.Error(parseErr))
			return &serverTLSCert, nil
		}
		serverTLSCert.Leaf = leaf
	}

	// Cache the generated certificate for reuse by subsequent connections to the
	// same host, under the composite key (host + backdate bucket + CA identity)
	// computed above — so a frozen-time replay and a live run never share a leaf.
	if certHost != "" {
		getCertCache().Add(cacheKey, &serverTLSCert)
	}

	publishMITM(sourcePort, &serverTLSCert)
	return &serverTLSCert, nil
}

// certCacheKey composes the cert-cache key from the target host, the backdate
// (a zero backdate — the live record path — buckets to "live" so repeated
// connections hit the cache; a frozen-replay backdate keys on its exact value)
// and the signing CA's identity (first 8 bytes of its DER SHA-256), so a leaf is
// never reused across a different instant or a different CA.
func certCacheKey(host string, backdate time.Time, caCert *x509.Certificate) string {
	bucket := "live"
	if !backdate.IsZero() {
		bucket = strconv.FormatInt(backdate.UnixNano(), 10)
	}
	caID := "nil"
	if caCert != nil {
		sum := sha256.Sum256(caCert.Raw)
		caID = fmt.Sprintf("%x", sum[:8])
	}
	return host + "\x00" + bucket + "\x00" + caID
}

// publishMITM notifies the registered MITMPublishHook (typically
// cbshim.RegisterMITM) of the freshly-determined MITM cert for this
// client connection. Nil-safe everywhere: a nil hook, a cert without
// a usable leaf, or a zero-port source addr all silently no-op.
func publishMITM(sourcePort int, cert *tls.Certificate) {
	if cert == nil || len(cert.Certificate) == 0 || sourcePort == 0 {
		return
	}
	hookMu.RLock()
	defer hookMu.RUnlock()
	if mitmPublishHook == nil {
		return
	}
	mitmPublishHook(strconv.Itoa(sourcePort), cert.Certificate[0])
}
