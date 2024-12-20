//go:build windows

// Package tls provides functionality for handling TLS connections.
package tls

import (
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/cloudflare/cfssl/csr"
	cfsslLog "github.com/cloudflare/cfssl/log"
	"github.com/cloudflare/cfssl/signer"
	"github.com/cloudflare/cfssl/signer/local"
	"go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

//go:embed asset/ca.crt
var caCrt []byte // Certificate

//go:embed asset/ca.key
var caPKey []byte // Private key

//go:embed asset
var _ embed.FS

// Writes the CA certificate to a temporary file and returns its path.
func extractCertToTemp() (string, error) {
	tempFile, err := os.CreateTemp("", "ca.crt")
	if err != nil {
		return "", err
	}
	defer tempFile.Close()

	err = os.Chmod(tempFile.Name(), 0666)
	if err != nil {
		return "", err
	}

	_, err = tempFile.Write(caCrt)
	if err != nil {
		return "", err
	}
	return tempFile.Name(), nil
}

// Adds the CA certificate to the Windows Certificate Store using certutil.
func updateCaStore(ctx context.Context) error {
	// Write the CA certificate to a temporary file
	tempCertPath, err := extractCertToTemp()
	if err != nil {
		return fmt.Errorf("failed to extract certificate to temporary path: %v", err)
	}

	// Use certutil to add the certificate to the Root CA store
	cmd := exec.CommandContext(ctx, "certutil", "-addstore", "Root", tempCertPath)
	if err := cmd.Run(); err != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return fmt.Errorf("failed to add certificate to Windows Root store: %v", err)
		}
	}
	return nil
}

// Checks if the CA already exists in the specified Java keystore.
func isJavaCAExist(ctx context.Context, alias, storepass, cacertsPath string) bool {
	cmd := exec.CommandContext(ctx, "keytool", "-list", "-keystore", cacertsPath, "-storepass", storepass, "-alias", alias)
	err := cmd.Run()
	return err == nil
}

// Installs the CA in the Java keystore if Java is installed on the system.
func installJavaCA(ctx context.Context, logger *zap.Logger, caPath string) error {
	if !util.IsJavaInstalled() {
		logger.Debug("Java is not installed on the system")
		return nil
	}

	// Get JAVA_HOME path
	javaHome, err := util.GetJavaHome(ctx)
	if err != nil {
		utils.LogError(logger, err, "Java detected but failed to find JAVA_HOME")
		return err
	}

	// Construct the path to keytool within JAVA_HOME
	keytoolPath := filepath.Join(javaHome, "bin", "keytool.exe")
	if _, err := os.Stat(keytoolPath); os.IsNotExist(err) {
		fmt.Println(err)
		return fmt.Errorf("keytool not found in JAVA_HOME at %s", keytoolPath)
	}

	// Set up the path to Java's cacerts file
	cacertsPath := filepath.Join(javaHome, "lib", "security", "cacerts")
	storePass := "changeit"
	alias := "keployCA"

	if isJavaCAExist(ctx, alias, storePass, cacertsPath) {
		logger.Info("Java detected and CA already exists", zap.String("path", cacertsPath))
		return nil
	}

	// Run keytool command to import the CA
	cmd := exec.CommandContext(ctx, keytoolPath, "-import", "-trustcacerts", "-keystore", cacertsPath, "-storepass", storePass, "-noprompt", "-alias", alias, "-file", caPath)
	cmdOutput, err := cmd.CombinedOutput()
	if err != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			utils.LogError(logger, err, "Java detected but failed to import CA", zap.String("output", string(cmdOutput)))
			return err
		}
	}

	logger.Info("Java detected and successfully imported CA", zap.String("path", cacertsPath), zap.String("output", string(cmdOutput)))
	return nil
}


// SetupCA configures the custom certificate authority for handling TLS connections.
func SetupCA(ctx context.Context, logger *zap.Logger) error {
	// Update the CA store with the certificate using certutil
	err := updateCaStore(ctx)
	if err != nil {
		utils.LogError(logger, err, "Failed to update the CA store")
		return err
	}

	// Extract certificate to a temporary file
	tempCertPath, err := extractCertToTemp()
	if err != nil {
		utils.LogError(logger, err, "Failed to extract certificate to temporary folder")
		return err
	}

	// Set environment variables for Node.js and Python to trust the CA certificate
	err = os.Setenv("NODE_EXTRA_CA_CERTS", tempCertPath)
	if err != nil {
		utils.LogError(logger, err, "Failed to set environment variable NODE_EXTRA_CA_CERTS")
		return err
	}

	err = os.Setenv("REQUESTS_CA_BUNDLE", tempCertPath)
	if err != nil {
		utils.LogError(logger, err, "Failed to set environment variable REQUESTS_CA_BUNDLE")
		return err
	}

	// Install CA in Java keystore if Java is installed
	err = installJavaCA(ctx, logger, tempCertPath)
	if err != nil {
		utils.LogError(logger, err, "Failed to install CA in the Java keystore")
		return err
	}

	return nil
}

var (
	caPrivKey    interface{}
	caCertParsed *x509.Certificate
	DstURL       string
)

// CertForClient generates a new server certificate and private key for the specified hostname.
func CertForClient(clientHello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	DstURL = clientHello.ServerName

	cfsslLog.Level = cfsslLog.LevelError

	serverReq := &csr.CertificateRequest{
		CN: clientHello.ServerName,
		Hosts: []string{
			clientHello.ServerName,
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

	serverCert, err := signerd.Sign(signer.SignRequest{
		Hosts:   serverReq.Hosts,
		Request: string(serverCsr),
		Profile: "web",
	})
	if err != nil {
		return nil, fmt.Errorf("failed to sign server certificate: %v", err)
	}

	serverTLSCert, err := tls.X509KeyPair(serverCert, serverKey)
	if err != nil {
		return nil, fmt.Errorf("failed to load server certificate and key: %v", err)
	}

	return &serverTLSCert, nil
}
