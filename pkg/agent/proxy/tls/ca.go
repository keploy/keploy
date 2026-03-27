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
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	cfsslConfig "github.com/cloudflare/cfssl/config"
	"github.com/cloudflare/cfssl/csr"
	cfsslLog "github.com/cloudflare/cfssl/log"
	"github.com/cloudflare/cfssl/signer"
	"github.com/cloudflare/cfssl/signer/local"
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
		return SetEnvForPath(logger, tempCertPath)
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

	// Install CA in NSS database for Chromium/Firefox browsers
	if finalCAPath != "" {
		if err := installNSSCA(ctx, logger, finalCAPath); err != nil {
			logger.Warn("Failed to install CA in NSS database (Chromium/Firefox may not trust it)", zap.Error(err))
		}
	}

	// Set Env Vars pointing to the installed cert
	if finalCAPath != "" {
		return SetEnvForPath(logger, finalCAPath)
	}

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

// installNSSCA installs the CA certificate into NSS databases used by Chromium/Firefox on Linux.
// Chromium on Linux does NOT use the system CA store (/usr/local/share/ca-certificates/ etc.).
// Instead, it uses the NSS (Network Security Services) database at ~/.pki/nssdb/.
// This function ensures Keploy's CA is trusted by Chromium-based browsers (e.g., Playwright, Puppeteer).
func installNSSCA(ctx context.Context, logger *zap.Logger, certPath string) error {
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return nil
	}

	// Check if certutil (NSS tool) is available; auto-install if missing
	if !commandExists("certutil") {
		logger.Info("certutil not found, attempting to install libnss3-tools automatically...")
		if err := installNSSTools(ctx, logger); err != nil {
			logger.Warn("Failed to auto-install libnss3-tools. Chromium/Firefox may not trust Keploy's CA.",
				zap.Error(err))
			return nil
		}
		// Verify certutil is now available after installation
		if !commandExists("certutil") {
			logger.Warn("certutil still not found after installing libnss3-tools. " +
				"Chromium/Firefox may not trust Keploy's CA.")
			return nil
		}
		logger.Info("Successfully installed libnss3-tools (certutil)")
	}

	// When running with sudo, we need the original user's home directory, not root's.
	// The agent process runs with sudo, so $HOME is /root, but Chromium runs as the original user.
	homeDir := os.Getenv("HOME")
	sudoUser := os.Getenv("SUDO_USER")

	if sudoUser != "" {
		u, err := user.Lookup(sudoUser)
		if err == nil {
			homeDir = u.HomeDir
		} else {
			logger.Debug("Could not look up SUDO_USER home, falling back to HOME", zap.String("SUDO_USER", sudoUser), zap.Error(err))
		}
	}

	if homeDir == "" {
		logger.Debug("HOME directory not found, skipping NSS CA installation")
		return nil
	}

	// Chromium uses ~/.pki/nssdb/ ; some Firefox profiles use their own NSS DBs
	nssDBDirs := []string{filepath.Join(homeDir, ".pki", "nssdb")}

	// Also search for Firefox NSS databases in ~/.mozilla/firefox/
	mozDir := filepath.Join(homeDir, ".mozilla", "firefox")
	if util.IsDirectoryExist(mozDir) {
		entries, err := os.ReadDir(mozDir)
		if err == nil {
			for _, e := range entries {
				if e.IsDir() && strings.HasSuffix(e.Name(), ".default") || strings.Contains(e.Name(), ".default-") {
					nssDBDirs = append(nssDBDirs, filepath.Join(mozDir, e.Name()))
				}
			}
		}
	}

	const alias = "Keploy CA"

	for _, dbDir := range nssDBDirs {
		dbPath := "sql:" + dbDir

		// Create the NSS database directory if it doesn't exist
		if !util.IsDirectoryExist(dbDir) {
			if err := os.MkdirAll(dbDir, 0755); err != nil {
				logger.Debug("Failed to create NSS db directory, skipping", zap.String("path", dbDir), zap.Error(err))
				continue
			}
			// Initialize a new NSS database
			cmd := exec.CommandContext(ctx, "certutil", "-d", dbPath, "-N", "--empty-password")
			if output, err := cmd.CombinedOutput(); err != nil {
				logger.Debug("Failed to initialize NSS db, skipping", zap.String("path", dbDir), zap.String("output", string(output)), zap.Error(err))
				continue
			}
		}

		// Check if the cert is already installed
		cmd := exec.CommandContext(ctx, "certutil", "-d", dbPath, "-L", "-n", alias)
		if err := cmd.Run(); err == nil {
			logger.Debug("Keploy CA already installed in NSS database", zap.String("nssdb", dbDir))
			continue
		}

		// Install the certificate as a trusted CA
		cmd = exec.CommandContext(ctx, "certutil", "-d", dbPath, "-A", "-t", "C,,", "-n", alias, "-i", certPath)
		output, err := cmd.CombinedOutput()
		if err != nil {
			logger.Warn("Failed to install Keploy CA in NSS database", zap.String("nssdb", dbDir), zap.String("output", string(output)), zap.Error(err))
			continue
		}

		logger.Debug("Successfully installed Keploy CA in NSS database", zap.String("nssdb", dbDir))
	}

	// Fix ownership if running as sudo so the original user can access the NSS DB
	if sudoUser != "" {
		pki := filepath.Join(homeDir, ".pki")
		if util.IsDirectoryExist(pki) {
			cmd := exec.CommandContext(ctx, "chown", "-R", sudoUser, pki)
			if output, err := cmd.CombinedOutput(); err != nil {
				logger.Debug("Failed to fix NSS db ownership", zap.String("output", string(output)), zap.Error(err))
			}
		}
	}

	return nil
}

// installNSSTools attempts to install the libnss3-tools (or nss-tools) package
// which provides the certutil command needed for NSS database management.
// Since Keploy runs with sudo (for eBPF), we have the permissions to install packages.
func installNSSTools(ctx context.Context, logger *zap.Logger) error {
	// Try apt-get first (Debian/Ubuntu)
	if commandExists("apt-get") {
		logger.Debug("Detected apt-get, installing libnss3-tools...")
		cmd := exec.CommandContext(ctx, "apt-get", "install", "-y", "libnss3-tools")
		cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
		output, err := cmd.CombinedOutput()
		if err != nil {
			logger.Debug("apt-get install libnss3-tools failed", zap.String("output", string(output)), zap.Error(err))
			return fmt.Errorf("apt-get install libnss3-tools failed: %w", err)
		}
		return nil
	}

	// Try yum (RHEL/CentOS/Fedora)
	if commandExists("yum") {
		logger.Debug("Detected yum, installing nss-tools...")
		cmd := exec.CommandContext(ctx, "yum", "install", "-y", "nss-tools")
		output, err := cmd.CombinedOutput()
		if err != nil {
			logger.Debug("yum install nss-tools failed", zap.String("output", string(output)), zap.Error(err))
			return fmt.Errorf("yum install nss-tools failed: %w", err)
		}
		return nil
	}

	// Try dnf (modern Fedora)
	if commandExists("dnf") {
		logger.Debug("Detected dnf, installing nss-tools...")
		cmd := exec.CommandContext(ctx, "dnf", "install", "-y", "nss-tools")
		output, err := cmd.CombinedOutput()
		if err != nil {
			logger.Debug("dnf install nss-tools failed", zap.String("output", string(output)), zap.Error(err))
			return fmt.Errorf("dnf install nss-tools failed: %w", err)
		}
		return nil
	}

	return fmt.Errorf("no supported package manager found (apt-get, yum, or dnf)")
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

// certCache caches generated TLS certificates keyed by SNI/host to avoid
// re-generating a certificate for every connection to the same host.
var certCache sync.Map

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

	// Generate a new server certificate and private key for the given hostname
	dstURL := clientHello.ServerName
	remoteAddr := clientHello.Conn.RemoteAddr().(*net.TCPAddr)
	sourcePort := remoteAddr.Port

	// Handle empty SNI: use IP from RemoteAddr as the host
	if dstURL == "" {
		host, _, err := net.SplitHostPort(clientHello.Conn.RemoteAddr().String())
		if err == nil {
			dstURL = host
		}
		logger.Debug("empty SNI, using IP from RemoteAddr", zap.String("dstURL", dstURL))
	}

	SrcPortToDstURL.Store(sourcePort, dstURL)

	// Check cert cache before generating a new certificate
	if cached, ok := certCache.Load(dstURL); ok {
		logger.Debug("using cached certificate", zap.String("host", dstURL))
		return cached.(*tls.Certificate), nil
	}

	serverReq := &csr.CertificateRequest{
		//Make the name accordng to the ip of the request
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

	// Use a custom signing profile without "key encipherment" to avoid
	// issues with modern TLS clients that reject RSA key encipherment.
	signingProfile := &cfsslConfig.Signing{
		Default: &cfsslConfig.SigningProfile{
			Usage:  []string{"digital signature", "server auth"},
			Expiry: 2 * 365 * 24 * time.Hour,
		},
	}
	signerd, err := local.NewSigner(cryptoSigner, caCertParsed, signer.DefaultSigAlgo(cryptoSigner), signingProfile)
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
		Profile:   "default",
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

	// Cache the certificate for future connections to the same host
	certCache.Store(dstURL, &serverTLSCert)

	return &serverTLSCert, nil
}
