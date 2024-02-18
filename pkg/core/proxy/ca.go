package proxy

import (
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"embed"
	"fmt"
	"github.com/cloudflare/cfssl/csr"
	cfsslLog "github.com/cloudflare/cfssl/log"
	"github.com/cloudflare/cfssl/signer"
	"github.com/cloudflare/cfssl/signer/local"
	"go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.uber.org/zap"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
)

//go:embed asset/ca.crt
var caCrt []byte //certificate

//go:embed asset/ca.key
var caPKey []byte //private key

//go:embed asset
var caFolder embed.FS

var caStorePath = []string{
	"/usr/local/share/ca-certificates/",
	"/etc/pki/ca-trust/source/anchors/",
	"/etc/ca-certificates/trust-source/anchors/",
	"/etc/pki/trust/anchors/",
	"/etc/pki/ca-trust/source/anchors/",
	"/usr/local/share/certs/",
	"/etc/ssl/certs/",
}

var caStoreUpdateCmd = []string{
	"tools-ca-certificates",
	"tools-ca-trust",
	"trust extract-compat",
	"tools-ca-trust extract",
	"certctl rehash",
}

type certKeyPair struct {
	cert tls.Certificate
	host string
}

func commandExists(cmd string) bool {
	_, err := exec.LookPath(cmd)
	return err == nil
}

func updateCaStore() error {
	commandRun := false
	for _, cmd := range caStoreUpdateCmd {
		if commandExists(cmd) {
			commandRun = true
			_, err := exec.Command(cmd).CombinedOutput()
			if err != nil {
				return err
			}
		}
	}
	if !commandRun {
		return fmt.Errorf("no valid CA store tools command found")
	}
	return nil
}

func getCaPaths() ([]string, error) {
	var caPaths = []string{}
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

// to extract ca certificate to temp
func ExtractCertToTemp() (string, error) {
	tempFile, err := ioutil.TempFile("", "ca.crt")
	if err != nil {
		return "", err
	}
	defer tempFile.Close()

	// Change the file permissions to allow read access for all users
	err = os.Chmod(tempFile.Name(), 0666)
	if err != nil {
		return "", err
	}

	// Write to the file
	_, err = tempFile.Write(caCrt)
	if err != nil {
		return "", err
	}

	// Close the file
	err = tempFile.Close()
	if err != nil {
		return "", err
	}
	return tempFile.Name(), nil
}

// IsJavaCAExists checks if the CA is already installed in the specified Java keystore
func IsJavaCAExists(alias, storepass, cacertsPath string) bool {
	cmd := exec.Command("keytool", "-list", "-keystore", cacertsPath, "-storepass", storepass, "-alias", alias)

	err := cmd.Run()

	return err == nil
}

// InstallJavaCA installs the CA in the Java keystore
func InstallJavaCA(logger *zap.Logger, caPath string) error {
	// check if java is installed
	if util.IsJavaInstalled() {
		logger.Debug("checking java path from default java home")
		javaHome, err := util.GetJavaHome()

		if err != nil {
			logger.Error("Java detected but failed to find JAVA_HOME", zap.Error(err))
			return err
		}

		// Assuming modern Java structure (without /jre/)
		cacertsPath := fmt.Sprintf("%s/lib/security/cacerts", javaHome)
		// You can modify these as per your requirements
		storePass := "changeit"
		alias := "keployCA"

		logger.Debug("", zap.Any("java_home", javaHome), zap.Any("caCertsPath", cacertsPath), zap.Any("caPath", caPath))

		if IsJavaCAExists(alias, storePass, cacertsPath) {
			logger.Info("Java detected and CA already exists", zap.String("path", cacertsPath))
			return nil
		}

		cmd := exec.Command("keytool", "-import", "-trustcacerts", "-keystore", cacertsPath, "-storepass", storePass, "-noprompt", "-alias", alias, "-file", caPath)

		cmdOutput, err := cmd.CombinedOutput()

		if err != nil {
			logger.Error("Java detected but failed to import CA", zap.Error(err), zap.String("output", string(cmdOutput)))
			return err
		}

		logger.Info("Java detected and successfully imported CA", zap.String("path", cacertsPath), zap.String("output", string(cmdOutput)))
		logger.Info("Successfully imported CA", zap.Any("", cmdOutput))
	} else {
		logger.Debug("Java is not installed on the system")
	}
	return nil
}

// SetupCA setups custom certificate authority to handle TLS connections
func SetupCA(logger *zap.Logger) error {
	caPaths, err := getCaPaths()
	if err != nil {
		logger.Error("Failed to find the CA store path", zap.Error(err))
		return err
	}

	for _, path := range caPaths {
		caPath := filepath.Join(path, "ca.crt")

		fs, err := os.Create(caPath)
		if err != nil {
			logger.Error("failed to create path for ca certificate", zap.Error(err), zap.Any("root store path", path))
			return err
		}

		_, err = fs.Write(caCrt)
		if err != nil {
			logger.Error("failed to write custom ca certificate", zap.Error(err), zap.Any("root store path", path))
			return err
		}

		// install CA in the java keystore if java is installed
		err = InstallJavaCA(logger, caPath)
		if err != nil {
			logger.Error("failed to install CA in the java keystore", zap.Error(err))
			return err
		}
	}

	// Update the trusted CAs store
	err = updateCaStore()
	if err != nil {
		logger.Error("Failed to tools the CA store", zap.Error(err))
		return err
	}

	tempCertPath, err := ExtractCertToTemp()
	if err != nil {
		logger.Error("Failed to extract certificate to tmp folder: %v", zap.Any("failed to extract certificate", err))
		return err
	}

	// for node
	err = os.Setenv("NODE_EXTRA_CA_CERTS", tempCertPath)
	if err != nil {
		logger.Error("Failed to set environment variable NODE_EXTRA_CA_CERTS: %v", zap.Any("failed to certificate path in environment", err))
		return err
	}

	// for python
	err = os.Setenv("REQUESTS_CA_BUNDLE", tempCertPath)
	if err != nil {
		logger.Error("Failed to set environment variable REQUESTS_CA_BUNDLE: %v", zap.Any("failed to certificate path in environment", err))
		return err
	}
	return nil
}

var (
	caPrivKey      interface{}
	caCertParsed   *x509.Certificate
	destinationUrl string
)

func certForClient(clientHello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	// Generate a new server certificate and private key for the given hostname
	destinationUrl = clientHello.ServerName

	cfsslLog.Level = cfsslLog.LevelError

	serverReq := &csr.CertificateRequest{
		//Make the name accordng to the ip of the request
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

	// Load the server certificate and private key
	serverTlsCert, err := tls.X509KeyPair(serverCert, serverKey)
	if err != nil {
		return nil, fmt.Errorf("failed to load server certificate and key: %v", err)
	}

	return &serverTlsCert, nil
}
