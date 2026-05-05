// Package tls provides functionality for handling tls connetions.
package tls

import (
	"bytes"
	"context"
	"crypto"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"embed"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
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
)

//go:embed asset/ca.crt
var caCrt []byte //certificate

//go:embed asset/ca.key
var caPKey []byte //private key

// embeddedFallbackRoots is the Mozilla NSS root bundle, vendored from
// https://curl.se/ca/cacert.pem (curl's daily-refreshed extract of Mozilla's
// certdata.txt). Compiled into the binary via go:embed so the agent always
// has a valid trust store regardless of the runtime image's filesystem
// state. See pkg/agent/proxy/tls/data/REFRESH.md for the refresh procedure.
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
//
//go:embed data/mozilla_roots.pem
var embeddedFallbackRoots []byte

//go:embed asset
var _ embed.FS

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
	if isDocker {
		logger.Debug("Detected Docker Shared Volume mode. Exporting certs...", zap.String("path", "/tmp/keploy-tls"))
		return setupSharedVolume(ctx, logger, "/tmp/keploy-tls")
	}

	// Native Mode
	logger.Debug("Detected Native Mode. Installing to system store...")
	return setupNativeForApp(ctx, logger, appPID, javaHomeOverride)
}

// It extracts the cert to a temp file and sets the env vars.
func SetupCaCertEnv(logger *zap.Logger) error {
	tempPath, err := extractCertToTemp()
	if err != nil {
		utils.LogError(logger, err, "Failed to extract certificate to tmp folder")
		return err
	}
	return SetEnvForPath(logger, tempPath)
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
	return setEnvVars(logger, map[string]string{
		"NODE_EXTRA_CA_CERTS": path,
		"REQUESTS_CA_BUNDLE":  path,
		"SSL_CERT_FILE":       path,
		"CARGO_HTTP_CAINFO":   path,
	})
}

// setEnvForSharedVolume wires the language-runtime trust-store env vars for the
// docker/k8s shared-volume install path.
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
		if err := os.Setenv(key, val); err != nil {
			utils.LogError(logger, err, "Failed to set environment variable", zap.String("key", key))
			return err
		}
	}
	return nil
}

// systemCABundleSearchPaths is the ranked list of well-known locations where
// Linux/Unix distributions (and busybox/Alpine-derived images) place the OS
// trust store. The first readable, non-empty file wins.
//
// Ordering mirrors Go's crypto/x509 certFiles but is duplicated here because
// we need the file BYTES (not the parsed roots) — the shared volume is
// consumed by non-Go clients (curl, python/requests, node.js, rust/reqwest,
// ...) that each parse PEM directly.
var systemCABundleSearchPaths = []string{
	"/etc/ssl/certs/ca-certificates.crt",     // Debian, Ubuntu, Alpine
	"/etc/pki/tls/certs/ca-bundle.crt",       // RHEL, Fedora, CentOS
	"/etc/ssl/ca-bundle.pem",                 // openSUSE
	"/etc/pki/tls/cacert.pem",                // older RHEL
	"/usr/local/share/certs/ca-root-nss.crt", // FreeBSD
	"/etc/ssl/cert.pem",                      // Alpine alternate, macOS
}

// loadSystemCABundleFn is an indirection seam for tests — production code uses
// the default loadSystemCABundle implementation. Tests replace this with a
// stub that reads from a tempdir-backed search list.
var loadSystemCABundleFn = loadSystemCABundle

// distroIndicatorPaths are filesystem signals that the container has a
// "normal" Linux trust-store layout that the postinst trigger of the
// `ca-certificates` package would have populated. If at least one of these
// is present (as a directory or file), the absence of the expected bundle
// file is a build/runtime regression — the image *should* have it. If none
// of these is present, the image is almost certainly a deliberately
// stripped distroless / scratch base, where the missing bundle is
// expected. We use this signal to choose between ERROR (regression) and
// INFO (intentional minimal image).
//
// Keep this list narrow: every entry needs to be something a distroless
// image would lack. /etc/ssl/certs/ is the canonical Debian/Ubuntu/Alpine
// directory; /etc/pki/tls/certs/ is the RHEL/Fedora analogue. Both are
// created by the ca-certificates package at install time.
var distroIndicatorPaths = []string{
	"/etc/ssl/certs",
	"/etc/pki/tls/certs",
}

// hasDistroTrustLayout reports whether the runtime image *appears* to have
// the kind of base layout where the OS CA bundle would normally be
// installed by `ca-certificates`. This is a heuristic — it can produce a
// false-positive ERROR if an operator is intentionally running on a
// non-distroless image without `ca-certificates` installed, and a
// false-negative (silent INFO) if a distroless image happens to have an
// empty /etc/ssl/certs directory. Both edge cases are rare and the
// remediation message in both log paths is identical, so the cost of a
// misclassification is low.
//
// Indirected via an fn-var so tests can stub the result without mocking
// the filesystem.
var hasDistroTrustLayoutFn = hasDistroTrustLayout

// Treat anything OTHER than "file not present" (ENOENT) as "indicator
// present". A permission-denied / IO-error Stat result on /etc/ssl/certs
// is overwhelmingly more likely on a distro-shaped image where the
// bundle directory exists but the agent process can't traverse it
// (SELinux, container-user-namespace remap, weird overlay) than on a
// distroless image. Treating those errors as "absent" would silently
// downgrade ERROR → INFO and let the misconfiguration that #375 was
// raised against slip past alert pipelines.
func hasDistroTrustLayout() bool {
	for _, p := range distroIndicatorPaths {
		if _, err := os.Stat(p); err == nil || !os.IsNotExist(err) {
			return true
		}
	}
	return false
}

// systemCABundleSourceEmbedded is the sentinel returned in the source-path
// position of loadSystemCABundle when the disk search produced no readable
// bundle and we fell back to the embeddedFallbackRoots blob. Operators see
// this string in startup logs and in the merged-bundle source field.
const systemCABundleSourceEmbedded = "<embedded:mozilla-roots.pem>"

// severity_reason values. These are stable, enum-like strings so alert
// rules and downstream log parsers can key off them without breaking when
// the human-readable wording in severity_explanation is tightened. Add a
// new constant rather than reusing one with a different meaning.
const (
	severityReasonDistroLayoutPresent = "distro_layout_present"
	severityReasonNoDistroLayout      = "no_distro_layout"
)

// loadSystemCABundle returns trust-anchor PEM bytes for merging with the
// Keploy MITM CA into the shared-volume bundle, plus a source identifier
// for logging. Lookup order:
//
//  1. Disk paths in systemCABundleSearchPaths — the well-known Linux
//     locations where `ca-certificates` (Debian/Ubuntu/Alpine) or
//     equivalent (RHEL/Fedora/openSUSE/FreeBSD) installs the OS trust
//     store. First readable, non-empty file wins.
//  2. embeddedFallbackRoots — Mozilla NSS roots vendored from curl.se,
//     compiled into the agent binary via go:embed. Used when every disk
//     path was missing/empty.
//
// This function used to return (nil, "") on miss, leaving the merged
// bundle to contain only the Keploy MITM CA. That state was the tail
// half of the failure mode in keploy/k8s-proxy#375: an orphan-mutated
// pod whose agent had no system trust store served apps a trust bundle
// that contained only Keploy's MITM root. Combined with an inactive
// eBPF redirect (the head half of that incident), every public-endpoint
// HTTPS call fell back to direct-to-internet routing and validated the
// real cert chain against keploy-only roots, producing
// CERTIFICATE_VERIFY_FAILED. The embedded fallback closes that failure
// class regardless of disk state, distroless vs not, weird volume
// mounts, SELinux denials, or future regressions in image builds.
//
// We still log when disk search comes up empty — that's a useful signal
// for operators to investigate WHY the disk bundle is missing — but the
// log severity differs:
//
//   - Distro-shaped image (presence of /etc/ssl/certs or similar)
//     missing its bundle is a build/runtime regression. ERROR.
//   - Distroless / scratch image with no trust-store layout is an
//     intentional operator choice. INFO.
//
// In either case the embedded fallback is now used so the app's trust
// store still works.
func loadSystemCABundle(logger *zap.Logger) ([]byte, string) {
	return loadSystemCABundleFromPathsAndFallback(logger, systemCABundleSearchPaths, embeddedFallbackRoots)
}

// loadSystemCABundleFromPaths is retained as the disk-only path used by
// existing tests. It does NOT consult the embedded fallback — those tests
// assert disk-search behavior in isolation. Production code goes through
// loadSystemCABundleFromPathsAndFallback.
func loadSystemCABundleFromPaths(logger *zap.Logger, paths []string) ([]byte, string) {
	return loadSystemCABundleFromPathsAndFallback(logger, paths, nil)
}

// loadSystemCABundleFromPathsAndFallback is the testable core. fallback
// may be nil to disable the embedded path (used by disk-only tests).
func loadSystemCABundleFromPathsAndFallback(logger *zap.Logger, paths []string, fallback []byte) ([]byte, string) {
	tried := make([]string, 0, len(paths))
	for _, p := range paths {
		tried = append(tried, p)
		data, err := os.ReadFile(p)
		if err != nil {
			// ENOENT / EACCES / etc. — most hosts only have one of these
			// candidates; silently try the next.
			continue
		}
		if len(data) == 0 {
			// Present-but-empty is almost always a broken image build; fall
			// through rather than silently producing a zero-trust bundle.
			continue
		}
		return data, p
	}

	// Disk search exhausted. Decide log severity based on whether the
	// image *should* have had a disk bundle (distro-shaped) vs whether
	// the absence was expected (distroless / scratch). When fallback is
	// non-nil (production code path) both severities continue with the
	// embedded Mozilla NSS roots so apps still get real public roots.
	// When fallback is nil (the disk-only test entry point and any
	// future caller that explicitly opts out of the embedded blob) the
	// log message must NOT claim a fallback happened — there isn't one
	// — so we vary the body on len(fallback) > 0.
	//
	// Keeping the actionable remediation guidance identical across the
	// two severities means operators don't have to learn two variants;
	// only the level and the leading sentence differ.
	const remediation = " To restore the disk path, ensure the Keploy " +
		"AGENT container (the writer of this shared volume — not the " +
		"app container) has access to an OS trust bundle at one of the " +
		"searched paths: install `ca-certificates` in the agent image " +
		"(`apt-get install -y ca-certificates` on Debian/Ubuntu, " +
		"`apk add ca-certificates` on Alpine), mount the host's bundle " +
		"into the agent pod, or rebuild from a base image that ships " +
		"trust roots. Fixing this in the app image does NOT help: " +
		"REQUESTS_CA_BUNDLE / SSL_CERT_FILE etc. are wired to this " +
		"shared file, so they replace whatever the app image already " +
		"trusts."

	var msg string
	if len(fallback) > 0 {
		msg = "No system CA bundle found on any known disk path — falling " +
			"back to the agent's embedded Mozilla NSS roots. The merged " +
			"/tmp/keploy-tls/ca.crt will contain (embedded_roots ∪ " +
			"Keploy MITM CA), so the app's trust store is still valid for " +
			"public endpoints." + remediation
	} else {
		// fallback==nil: no embedded blob is being used (the disk-only
		// test entry point loadSystemCABundleFromPaths takes this
		// branch). Saying we "fell back to the embedded Mozilla NSS
		// roots" here would be misleading — the merged bundle in this
		// shape contains only the Keploy MITM CA.
		msg = "No system CA bundle found on any known disk path and no " +
			"embedded fallback configured — the merged " +
			"/tmp/keploy-tls/ca.crt will contain only the Keploy MITM " +
			"CA, which is NOT a valid trust store for public endpoints." +
			remediation
	}

	fields := []zap.Field{
		zap.Strings("searched_paths", tried),
		zap.Strings("distro_indicator_paths", distroIndicatorPaths),
		zap.Int("embedded_fallback_bytes", len(fallback)),
	}

	// severity_explanation describes WHY this severity was chosen — it
	// must not claim the embedded fallback was used when it wasn't.
	// Whether the fallback is in effect is already conveyed by the
	// `embedded_fallback_bytes` structured field and by the leading
	// sentence of `msg`, so the explanation stays focused on the
	// distro-shape decision.
	if hasDistroTrustLayoutFn() {
		// Image looks distro-shaped (Debian/Ubuntu/Alpine/RHEL family) —
		// missing disk bundle is a real misconfiguration. ERROR so
		// alerting catches it. We still produce a working trust store
		// via the embedded fallback; ERROR is the operator-facing signal
		// to investigate the agent image / volume mounts / SELinux.
		logger.Error(msg, append(fields,
			zap.String("severity_reason", severityReasonDistroLayoutPresent),
			zap.String("severity_explanation", "image looks distro-shaped (one of the distro_indicator_paths exists) so the missing disk bundle is a regression rather than an intentional minimal-image choice"),
		)...)
	} else {
		// Truly distroless — the operator deliberately stripped the trust
		// store; raising the level here would create alert fatigue.
		logger.Info(msg, append(fields,
			zap.String("severity_reason", severityReasonNoDistroLayout),
			zap.String("severity_explanation", "image has no distro trust-store layout (distroless / scratch); the missing disk bundle is expected"),
		)...)
	}

	if len(fallback) > 0 {
		return fallback, systemCABundleSourceEmbedded
	}
	// fallback==nil only happens via the disk-only test entry point.
	// Production callers always pass embeddedFallbackRoots which is
	// non-empty by build-time go:embed.
	return nil, ""
}

func setupSharedVolume(_ context.Context, logger *zap.Logger, exportPath string) error {
	if err := os.MkdirAll(exportPath, 0755); err != nil {
		return fmt.Errorf("failed to create export dir: %w", err)
	}

	// Load the OS-provided CA bundle (best-effort — returns (nil, "") if no
	// standard path is populated, which is normal on distroless/scratch).
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
	merged := make([]byte, 0, len(systemBundle)+len(caCrt)+1)
	merged = append(merged, systemBundle...)
	if len(systemBundle) > 0 && systemBundle[len(systemBundle)-1] != '\n' {
		merged = append(merged, '\n')
	}
	merged = append(merged, caCrt...)

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
	if err := os.WriteFile(keployOnlyPath, caCrt, 0644); err != nil {
		return fmt.Errorf("failed to write keploy-ca.crt to shared volume: %w", err)
	}

	if len(systemBundle) > 0 {
		logger.Info("Merged system CA bundle with Keploy MITM CA",
			zap.Int("system_bytes", len(systemBundle)),
			zap.Int("keploy_bytes", len(caCrt)),
			zap.String("source_path", sourcePath),
			zap.String("output", crtPath),
			zap.String("keploy_only_output", keployOnlyPath),
		)
	}
	// When len(systemBundle) == 0 we deliberately emit NO log here:
	// loadSystemCABundleFromPaths already logged an Info entry with the full
	// list of searched paths and actionable remediation steps, so a second
	// "Wrote Keploy MITM CA only" message would be redundant noise.

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
	if err := generateTrustStore(crtPath, jksPath); err != nil {
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

func setupNative(ctx context.Context, logger *zap.Logger) error {
	return setupNativeForApp(ctx, logger, 0, "")
}

// setupNativeForApp is the PID-aware native-install path. appPID and
// javaHomeOverride are passed through to the Java keystore install so
// the Keploy MITM CA lands in the app's actual truststore (see
// SetupCAForApp doc comment for the full rationale).
func setupNativeForApp(ctx context.Context, logger *zap.Logger, appPID int, javaHomeOverride string) error {
	// Resolve the target JDK's java.home once, up front. Order:
	//   1. --ca-java-home CLI override wins (opts.Agent.CAJavaHome).
	//   2. /proc/<appPID>/environ JAVA_HOME, then /proc/<appPID>/exe.
	//   3. Empty — installJavaCAForHome falls back to PATH keytool.
	// We log the chosen source at Debug so operators can see why a
	// particular cacerts file was targeted without turning on trace.
	resolvedJavaHome := resolveAppJavaHome(logger, appPID, javaHomeOverride)

	// Windows Specific Logic
	if runtime.GOOS == "windows" {
		// Extract certificate to a temporary file
		tempCertPath, err := extractCertToTemp()
		if err != nil {
			utils.LogError(logger, err, "Failed to extract certificate to temp folder")
			return err
		}
		defer func() {
			if err := os.Remove(tempCertPath); err != nil {
				logger.Debug("Failed to remove temporary certificate file", zap.String("path", tempCertPath), zap.Error(err))
			}
		}()

		// Install certificate using certutil
		if err = installWindowsCA(ctx, logger, tempCertPath); err != nil {
			utils.LogError(logger, err, "Failed to install CA certificate on Windows")
			return err
		}

		// install CA in the java keystore if java is installed
		if err = installJavaCAForHome(ctx, logger, tempCertPath, resolvedJavaHome); err != nil {
			utils.LogError(logger, err, "Failed to install CA in the java keystore")
			return err
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

	// Linux/Unix Specific Logic
	caPaths, err := getCaPaths()
	if err != nil {
		utils.LogError(logger, err, "Failed to find the CA store path")
		return err
	}

	var finalCAPath string
	for _, path := range caPaths {
		caPath := filepath.Join(path, "ca.crt")
		finalCAPath = caPath // Keep one valid path for env vars

		// Write directly to store
		fs, err := os.Create(caPath)
		if err != nil {
			utils.LogError(logger, err, "Failed to create path for ca certificate", zap.Any("root store path", path))
			return err
		}
		if _, err = fs.Write(caCrt); err != nil {
			fs.Close()
			utils.LogError(logger, err, "Failed to write custom ca certificate", zap.Any("root store path", path))
			return err
		}
		fs.Close()

		// install CA in the java keystore if java is installed.
		// resolvedJavaHome may be "" — installJavaCAForHome falls back
		// to PATH keytool in that case, preserving legacy behaviour.
		if err := installJavaCAForHome(ctx, logger, caPath, resolvedJavaHome); err != nil {
			utils.LogError(logger, err, "Failed to install CA in the java keystore")
			return err
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

	// Native mode completed successfully — the CA is now installed in
	// the system store (and the Java keystore if Java is present).
	// Signal readiness so downstream consumers (e.g. /agent/ready) can
	// unblock.
	markCAReady()
	return nil
}

// extractCertToTemp writes the embedded CA to a temporary file
func extractCertToTemp() (string, error) {
	tempFile, err := os.CreateTemp("", "ca.crt")
	if err != nil {
		return "", err
	}
	defer tempFile.Close()

	// 0666 allows read access for all users
	if err = os.Chmod(tempFile.Name(), 0666); err != nil {
		return "", err
	}

	if _, err = tempFile.Write(caCrt); err != nil {
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
//     SHA-256 fingerprint of the embedded Keploy CA DER — returned by
//     embeddedKeployCADER() and cached in-process — against
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
func generateTrustStore(certPath, jksPath string) error {
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
	keployFingerprint := sha256.Sum256(embeddedKeployCADER())
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
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return fmt.Errorf("failed to parse x509 certificate at PEM block #%d (type=%q): %w", pemBlockIdx, block.Type, err)
		}

		alias := ""
		if sha256.Sum256(cert.Raw) == keployFingerprint {
			alias = "keploy-root"
		} else {
			alias = fmt.Sprintf("system-%x", sha256.Sum256(cert.Raw))
		}

		ks.SetTrustedCertificateEntry(alias, keystore.TrustedCertificateEntry{
			Certificate: keystore.Certificate{
				Type:    "X.509",
				Content: cert.Raw,
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

// keployCADEROnce caches the DER encoding of the embedded Keploy MITM CA
// so generateTrustStore doesn't re-parse the same in-memory PEM on every
// call. The parse is O(cert-size) — trivial individually, but every agent
// restart hits generateTrustStore once per startup and every unit test
// that exercises generateTrustStore would re-parse too.
//
// If the embedded PEM is malformed or not a CERTIFICATE block,
// keployCADERBytes remains nil. Consumers (generateTrustStore) treat a
// nil DER as "can't identify the Keploy root" and fall back to the
// generic "system-<sha>" alias for every entry. That degrades the
// human-readable alias naming but keeps the trust-store functionally
// correct — so we don't surface the decode failure as an error here.
var (
	keployCADEROnce  sync.Once
	keployCADERBytes []byte
)

// embeddedKeployCADER returns the DER encoding of the embedded Keploy MITM
// CA. Used by generateTrustStore to identify which block in a merged
// PEM bundle is the Keploy root (so it gets the stable "keploy-root"
// alias). Parsing happens once per process via sync.Once; subsequent
// calls return the cached DER. If the embedded PEM is malformed or not
// a CERTIFICATE block, the cached value is nil — callers treat that as
// "can't identify the Keploy root" and fall back to the generic
// "system-<sha>" alias for every entry. That is a degraded but
// non-broken trust-store.
func embeddedKeployCADER() []byte {
	keployCADEROnce.Do(func() {
		block, _ := pem.Decode(caCrt)
		if block == nil || block.Type != "CERTIFICATE" {
			return
		}
		keployCADERBytes = block.Bytes
	})
	return keployCADERBytes
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
			if _, err := c.CombinedOutput(); err != nil {
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
// Signature preserved for backward compat (external callers + tests).
func installJavaCA(ctx context.Context, logger *zap.Logger, caPath string) error {
	return installJavaCAForHome(ctx, logger, caPath, "")
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
func installJavaCAForHome(ctx context.Context, logger *zap.Logger, caPath, javaHome string) error {
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
	alias := "keployCA"

	logger.Debug("", zap.String("java_home", javaHome), zap.String("keytool", keytoolBin), zap.String("caCertsPath", cacertsPath), zap.String("caPath", caPath))

	if isJavaCAExistWithTool(ctx, keytoolBin, alias, storePass, cacertsPath) {
		logger.Debug("Java detected and CA already exists", zap.String("path", cacertsPath))
		return nil
	}

	cmd := exec.CommandContext(ctx, keytoolBin, "-import", "-trustcacerts", "-keystore", cacertsPath, "-storepass", storePass, "-noprompt", "-alias", alias, "-file", caPath)
	cmdOutput, err := cmd.CombinedOutput()
	if err != nil {
		utils.LogError(logger, err, "Java detected but failed to import CA", zap.String("output", string(cmdOutput)))
		return err
	}
	logger.Debug("Java detected and successfully imported CA", zap.String("path", cacertsPath), zap.String("output", string(cmdOutput)))
	logger.Debug("Successfully imported CA", zap.ByteString("output", cmdOutput))
	return nil
}

// installWindowsCA installs the CA certificate in Windows certificate store using certutil
func installWindowsCA(ctx context.Context, logger *zap.Logger, certPath string) error {
	cmd := exec.CommandContext(ctx, "certutil", "-addstore", "-f", "ROOT", certPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		utils.LogError(logger, err, "Failed to install CA certificate using certutil", zap.String("output", string(output)))
		return err
	}
	logger.Debug("Successfully installed CA certificate in Windows ROOT store", zap.String("output", string(output)))
	return nil
}

// SrcPortToDstURL map is used to store the mapping between source port and DstURL for the TLS connection
var SrcPortToDstURL = sync.Map{}

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
	// the hostname stored by handleConnectTunnel in SrcPortToDstURL.
	dstURL := clientHello.ServerName
	remoteAddr := clientHello.Conn.RemoteAddr().(*net.TCPAddr)
	sourcePort := remoteAddr.Port

	if dstURL == "" {
		if stored, ok := SrcPortToDstURL.Load(sourcePort); ok {
			if s, ok := stored.(string); ok && s != "" {
				dstURL = s
			}
		}
	}

	SrcPortToDstURL.Store(sourcePort, dstURL)

	// Check the cert cache before generating a new certificate.
	if dstURL != "" {
		if cached, ok := getCertCache().Get(dstURL); ok {
			probeCert(logger, "cache-hit", dstURL, 0)
			logger.Debug("reusing cached certificate", zap.String("hostname", dstURL))
			return cached, nil
		}
	}
	probeCert(logger, "mint-start", dstURL, 0)
	mintStart := time.Now()
	defer func() {
		probeCert(logger, "mint-done", dstURL, time.Since(mintStart).Nanoseconds())
	}()

	serverReq := &csr.CertificateRequest{
		CN: dstURL,
		Hosts: []string{
			dstURL,
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

	// Cache the generated certificate for reuse by subsequent connections
	// to the same hostname, avoiding redundant key generation and signing.
	// NOTE: The cache key only uses hostname. In practice, caPrivKey and
	// caCertParsed are constant for the lifetime of a Proxy instance, and
	// backdate is constant per test run, so hostname is sufficient.
	if dstURL != "" {
		getCertCache().Add(dstURL, &serverTLSCert)
	}

	return &serverTLSCert, nil
}
