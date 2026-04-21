// Package tls provides functionality for handling tls connetions.
package tls

import (
	"context"
	"crypto"
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

// SetupCA setups custom certificate authority to handle TLS connections
func SetupCA(ctx context.Context, logger *zap.Logger, isDocker bool) error {

	if isDocker {
		logger.Debug("Detected Docker Shared Volume mode. Exporting certs...", zap.String("path", "/tmp/keploy-tls"))
		return setupSharedVolume(ctx, logger, "/tmp/keploy-tls")
	}

	// Native Mode
	logger.Debug("Detected Native Mode. Installing to system store...")
	return setupNative(ctx, logger)
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
func SetEnvForPath(logger *zap.Logger, path string) error {
	envVars := map[string]string{
		"NODE_EXTRA_CA_CERTS": path,
		"REQUESTS_CA_BUNDLE":  path,
		"SSL_CERT_FILE":       path,
		"CARGO_HTTP_CAINFO":   path,
	}

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

// loadSystemCABundle returns the contents of the first readable, non-empty OS
// CA bundle file from systemCABundleSearchPaths (plus the path used). On
// failure (no readable non-empty file found) it logs a warning and returns
// (nil, "") — the caller should proceed with the Keploy CA alone rather than
// erroring.
//
// We intentionally DO NOT return an error: the shared-volume CA file is a
// best-effort merge. In the worst case (minimal distroless/scratch image with
// no OS bundle) the application falls back to the prior behaviour of trusting
// only the Keploy MITM CA — not worse than the status quo.
func loadSystemCABundle(logger *zap.Logger) ([]byte, string) {
	return loadSystemCABundleFromPaths(logger, systemCABundleSearchPaths)
}

// loadSystemCABundleFromPaths is the testable core of loadSystemCABundle —
// it scans the given ranked list instead of the production constant.
func loadSystemCABundleFromPaths(logger *zap.Logger, paths []string) ([]byte, string) {
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
	logger.Warn(
		"No system CA bundle found on any known path — the shared "+
			"/tmp/keploy-tls/ca.crt will contain ONLY the Keploy MITM CA. "+
			"Non-proxied HTTPS calls from the application (internal services, "+
			"public endpoints Keploy isn't proxying, DNS-over-HTTPS) will fail "+
			"CERTIFICATE_VERIFY_FAILED. Ensure the application image ships an OS "+
			"trust store (e.g. the `ca-certificates` package on Debian/Ubuntu).",
		zap.Strings("searched_paths", tried),
	)
	return nil, ""
}

func setupSharedVolume(_ context.Context, logger *zap.Logger, exportPath string) error {
	if err := os.MkdirAll(exportPath, 0755); err != nil {
		return fmt.Errorf("failed to create export dir: %w", err)
	}

	// Write ca.crt
	crtPath := filepath.Join(exportPath, "ca.crt")
	if err := os.WriteFile(crtPath, caCrt, 0644); err != nil {
		return fmt.Errorf("failed to write ca.crt to shared volume: %w", err)
	}

	if err := SetEnvForPath(logger, crtPath); err != nil {
		logger.Warn("Failed to set internal env vars for Agent", zap.Error(err))
	}

	// Generate Java Truststore
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
				logger.Warn("Failed to remove temporary certificate file", zap.String("path", tempCertPath), zap.Error(err))
			}
		}()

		// Install certificate using certutil
		if err = installWindowsCA(ctx, logger, tempCertPath); err != nil {
			utils.LogError(logger, err, "Failed to install CA certificate on Windows")
			return err
		}

		// install CA in the java keystore if java is installed
		if err = installJavaCA(ctx, logger, tempCertPath); err != nil {
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

		// install CA in the java keystore if java is installed
		if err := installJavaCA(ctx, logger, caPath); err != nil {
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

// generateTrustStoreNative creates a JKS file using pure Go, avoiding 'keytool' dependency
func generateTrustStore(certPath, jksPath string) error {
	// Read and Parse the PEM Certificate
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return fmt.Errorf("failed to read cert pem: %w", err)
	}

	block, _ := pem.Decode(certPEM)
	if block == nil {
		return fmt.Errorf("failed to decode PEM block containing certificate")
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("failed to parse x509 certificate: %w", err)
	}

	// Create the KeyStore
	ks := keystore.New()

	// Create a Trusted Certificate Entry
	entry := keystore.TrustedCertificateEntry{
		Certificate: keystore.Certificate{
			Type:    "X.509",
			Content: cert.Raw,
		},
	}

	// Add to KeyStore with alias "keploy-root"
	ks.SetTrustedCertificateEntry("keploy-root", entry)

	// Write to file
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
	cmd := exec.CommandContext(ctx, "keytool", "-list", "-keystore", cacertsPath, "-storepass", storepass, "-alias", alias)
	err := cmd.Run()
	select {
	case <-ctx.Done():
		return false
	default:
	}
	return err == nil
}

// installJavaCA installs the CA in the Java keystore
func installJavaCA(ctx context.Context, logger *zap.Logger, caPath string) error {
	// check if java is installed
	if util.IsJavaInstalled() {
		logger.Debug("checking java path from default java home")
		javaHome, err := util.GetJavaHome(ctx)
		if err != nil {
			utils.LogError(logger, err, "Java detected but failed to find JAVA_HOME")
			return err
		}

		// Assuming modern Java structure (without /jre/)
		// Use filepath.Join for proper cross-platform path handling (Windows uses backslashes)
		cacertsPath := filepath.Join(javaHome, "lib", "security", "cacerts")
		// You can modify these as per your requirements
		storePass := "changeit"
		alias := "keployCA"

		logger.Debug("", zap.String("java_home", javaHome), zap.String("caCertsPath", cacertsPath), zap.String("caPath", caPath))

		if isJavaCAExist(ctx, alias, storePass, cacertsPath) {
			logger.Debug("Java detected and CA already exists", zap.String("path", cacertsPath))
			return nil
		}

		cmd := exec.CommandContext(ctx, "keytool", "-import", "-trustcacerts", "-keystore", cacertsPath, "-storepass", storePass, "-noprompt", "-alias", alias, "-file", caPath)
		cmdOutput, err := cmd.CombinedOutput()
		if err != nil {
			utils.LogError(logger, err, "Java detected but failed to import CA", zap.String("output", string(cmdOutput)))
			return err
		}
		logger.Debug("Java detected and successfully imported CA", zap.String("path", cacertsPath), zap.String("output", string(cmdOutput)))
		logger.Debug("Successfully imported CA", zap.ByteString("output", cmdOutput))
	} else {
		logger.Debug("Java is not installed on the system")
	}
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
			logger.Debug("reusing cached certificate", zap.String("hostname", dstURL))
			return cached, nil
		}
	}

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
