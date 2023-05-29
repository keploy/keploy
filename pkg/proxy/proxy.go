package proxy

import (
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"log"
	"net"

	// "github.com/cilium/ebpf"
	"github.com/cloudflare/cfssl/csr"
	"github.com/cloudflare/cfssl/helpers"
	"github.com/cloudflare/cfssl/signer"
	"github.com/cloudflare/cfssl/signer/local"
	"go.keploy.io/server/pkg/hooks"
	// "github.com/keploy/go-sdk/mock"
	// "go.keploy.io/server/pkg/hooks/keploy"
	// proto "go.keploy.io/server/grpc/regression"
	// "go.keploy.io/server/pkg/models"
)

var currentPort uint32 = 5000

const (
	proxyAddress = "0.0.0.0"
	response     = "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 12\r\n\r\nHello World!"
)

var runningPorts = []uint32{}

// var Deps = map[string]*proto.Mock{}

// starts a number of proxies on the unused ports
func bootProxies() {
	// log.Println("bootProxies is called")
	for i := 0; i < 50; {
		if isPortAvailable(currentPort) {
			go startProxy(currentPort)
			runningPorts = append(runningPorts, currentPort)
			i++
		}
		currentPort++
	}
	// log.Println("runningPorts after booting are: ", runningPorts)
}

func isPortAvailable(port uint32) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%v", port))
	if err != nil {
		return false
	}
	defer ln.Close()
	return true
}

const (
	caCertPath       = "ca.crt"                           // Replace with your CA certificate file path
	caPrivateKeyPath = "ca.key"                           // Replace with your CA private key file path
	caStorePath      = "/usr/local/share/ca-certificates" // Replace with your CA store path
)

type certKeyPair struct {
	cert tls.Certificate
	host string
}

var (
	caPrivKey      interface{}
	caCertParsed   *x509.Certificate
	destinationUrl string
)

func certForClient(clientHello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	// Generate a new server certificate and private key for the given hostname
	log.Printf("This is the server name: %s", clientHello.ServerName)
	destinationUrl = clientHello.ServerName
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
		log.Printf("Error in typecasting the caPrivKey")
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

func startProxy(port uint32) {
	// Read the CA certificate and private key from files
	caCert, err := ioutil.ReadFile(caCertPath)
	if err != nil {
		log.Fatalf("Failed to read CA certificate: %v", err)
	}

	caKey, err := ioutil.ReadFile(caPrivateKeyPath)
	if err != nil {
		log.Fatalf("Failed to read CA private key: %v", err)
	}
	caPrivKey, err = helpers.ParsePrivateKeyPEM(caKey)
	if err != nil {
		log.Fatalf("Failed to parse CA private key: %v", err)
	}
	caCertParsed, err = helpers.ParseCertificatePEM(caCert)
	if err != nil {
		log.Fatalf("Failed to parse CA certificate: %v", err)
	}
	listener, err := net.Listen("tcp", fmt.Sprintf(proxyAddress+":%v", port))
	if err != nil {
		log.Fatalf("Error listening on %s: %v", proxyAddress, err)
	}
	defer listener.Close()

	log.Printf("Proxy server is listening on %s", fmt.Sprintf(proxyAddress+":%v", port))
	config := &tls.Config{
		GetCertificate: certForClient,
	}
	listener = tls.NewListener(listener, config)

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("Error accepting connection: %v", err)
			continue
		}

		go handleConnection(conn, port)
	}
}

func handleConnection(conn net.Conn, port uint32) {
	// port := getRemotePort(conn)

	var (
	// tmpPort uint32
	// indx    uint32 = 0
	)
	var (
		tmpPort = hooks.Vaccant_port{}
		indx    = -1
	)

	for i := 0; i < len(runningPorts); i++ {

		if err := objs.VaccantPorts.Lookup(uint32(i), &tmpPort); err != nil {
			log.Printf("reading map: %v", err)
		}
		if tmpPort.Port == port {
			indx = i
			// log.Printf("Vacant_ports %T, port at index 0: %v", objs.VaccantPorts, tmpPort)
			break
		}
	}
}
