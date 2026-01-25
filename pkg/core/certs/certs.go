package certs

import (
	"bytes"
	"context"
	"crypto/x509"
	"embed"
	"encoding/pem"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/pavlo-v-chernykh/keystore-go/v4"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

//go:embed asset/ca.crt
var caCrt []byte //certificate

//go:embed asset/ca.key
var caPKey []byte //private key

//go:embed asset
var _ embed.FS

// Exported variables for access by other packages
var CACrt = caCrt
var CAPKey = caPKey

var caStoreUpdateCmd = []string{
	"update-ca-certificates",
	"update-ca-trust",
	"trust extract-compat",
	"tools-ca-trust extract",
	"certctl rehash",
}

// SetupCaCertEnv extracts the cert to a temp file and sets the env vars.
func SetupCaCertEnv(logger *zap.Logger) error {
	tempPath, err := extractCertToTemp()
	if err != nil {
		utils.LogError(logger, err, "Failed to extract certificate to tmp folder")
		return err
	}
	return SetEnvForPath(logger, tempPath)
}

// SetupCA setups custom certificate authority to handle TLS connections
func SetupCA(ctx context.Context, logger *zap.Logger, isDocker bool) error {

	if isDocker {
		logger.Debug("Detected Docker Shared Volume mode. Exporting certs...", zap.String("path", "/tmp/keploy-tls"))
		return SetupSharedVolume(ctx, logger, "/tmp/keploy-tls")
	}

	// Native Mode
	logger.Debug("Detected Native Mode. Installing to system store...")
	return SetupNative(ctx, logger)
}

// SetupNative exports logic for setting up CA natively
func SetupNative(ctx context.Context, logger *zap.Logger) error {
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
		if err = InstallJavaCA(ctx, logger, tempCertPath); err != nil {
			utils.LogError(logger, err, "Failed to install CA in the java keystore")
			return err
		}

		// Set environment variables for Node.js and Python to use the custom CA
		return SetEnvForPath(logger, tempCertPath)
	}

	// Linux/Unix Specific Logic
	caPaths, err := GetCaPaths()
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
		if err := InstallJavaCA(ctx, logger, caPath); err != nil {
			utils.LogError(logger, err, "Failed to install CA in the java keystore")
			return err
		}
	}

	// Update the system store
	if err := UpdateCaStore(ctx); err != nil {
		utils.LogError(logger, err, "Failed to update the CA store")
		return err
	}

	// Set Env Vars pointing to the installed cert
	if finalCAPath != "" {
		return SetEnvForPath(logger, finalCAPath)
	}

	return nil
}


// SetupSharedVolume exports the CA to a shared volume
func SetupSharedVolume(_ context.Context, logger *zap.Logger, exportPath string) error {
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
	if err := GenerateTrustStore(crtPath, jksPath); err != nil {
		logger.Error("Failed to generate Java truststore", zap.Error(err))
		return err
	}

	logger.Debug("TLS Certificates successfully exported to shared volume")
	return nil
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

// GetCaPaths returns the paths where CA certificates are stored on the system
func GetCaPaths() ([]string, error) {
	var caStorePath = []string{
		"/usr/local/share/ca-certificates/",
		"/etc/pki/ca-trust/source/anchors/",
		"/etc/ca-certificates/trust-source/anchors/",
		"/etc/pki/trust/anchors/",
		"/usr/local/share/certs/",
		"/etc/ssl/certs/",
	}

	var caPaths []string
	for _, dir := range caStorePath {
		if IsDirectoryExist(dir) {
			caPaths = append(caPaths, dir)
		}
	}
	if len(caPaths) == 0 {
		return nil, fmt.Errorf("no valid CA store path found")
	}
	return caPaths, nil
}

func IsDirectoryExist(path string) bool {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return false
	}
	return info.IsDir()
}

// Initialize CA CRT from embedded file if needed (usually handled by var init)
func GetCACrt() []byte {
	return caCrt
}

func GetCAPKey() []byte {
	return caPKey
}

func GetAssetFS() embed.FS {
	// Not directly returnable as the variable is _ embed.FS (blank identifier)
	// But we don't need it if we export the bytes.
	return embed.FS{}
}

// GenerateTrustStore creates a JKS file using pure Go, avoiding 'keytool' dependency
func GenerateTrustStore(certPath, jksPath string) error {
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

func UpdateCaStore(ctx context.Context) error {
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

// InstallJavaCA installs the CA in the Java keystore
func InstallJavaCA(ctx context.Context, logger *zap.Logger, caPath string) error {
	// check if java is installed
	if IsJavaInstalled() {
		logger.Debug("checking java path from default java home")
		javaHome, err := GetJavaHome(ctx)
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


// IsJavaInstalled checks if java is installed on the system
func IsJavaInstalled() bool {
	_, err := exec.LookPath("java")
	return err == nil
}

// GetJavaHome returns the JAVA_HOME path
func GetJavaHome(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "java", "-XshowSettings:properties", "-version")
	var out bytes.Buffer
	cmd.Stderr = &out // The output we need is printed to STDERR

	err := cmd.Run()
	if err != nil {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}
	}
	output := out.String()
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, "java.home =") {
			parts := strings.Split(line, "=")
			if len(parts) > 1 {
				return strings.TrimSpace(parts[1]), nil
			}
		}
	}
	return "", fmt.Errorf("failed to find java.home in output")
}
