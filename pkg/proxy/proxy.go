package proxy

import (
	"bufio"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"os/exec"
	"strings"

	"github.com/cloudflare/cfssl/csr"
	"github.com/cloudflare/cfssl/helpers"
	"github.com/cloudflare/cfssl/signer"
	"github.com/cloudflare/cfssl/signer/local"
	"go.keploy.io/server/pkg/hooks"
	"go.keploy.io/server/pkg/proxy/integrations/httpparser"
	"go.keploy.io/server/pkg/proxy/integrations/mongoparser"
	"go.keploy.io/server/pkg/proxy/util"
	"go.uber.org/zap"
)

const (
	proxyAddress = "0.0.0.0"
)

type ProxySet struct {
	// PortList is the list of ports on which the keploy proxies are running
	PortList []uint32
	hook	*hooks.Hook
	logger *zap.Logger
}

type Conn struct {
	net.Conn
	r bufio.Reader
	// buffer       []byte
	// ReadComplete bool
	// pointer      int
}
func (c *Conn) Read(b []byte) (n int, err error) {
	return c.r.Read(b)

	// buffer, err := readBytes(c.conn)
	// if err != nil {
	// 	return 0, err
	// }
	// b = buffer
	// if len(buffer) == 0 && len(c.buffer) != 0 {
	// 	b = c.buffer
	// } else {
	// 	c.buffer = buffer
	// }
	// if c.ReadComplete {
	// 	b = c.buffer[c.pointer:(c.pointer + len(b))]

	// 	return 257, nil
	// }
	// n, err = c.Conn.Read(b)
	// if n > 0 {
	// 	c.buffer = append(c.buffer, b...)
	// }
	// if err != nil {
	// 	return n, err
	// }

	// return n, nil
}
func (ps *ProxySet) SetHook(hook	*hooks.Hook)  {
	ps.hook = hook
}

func getDistroInfo() string {
	osRelease, err := ioutil.ReadFile("/etc/os-release")
	if err != nil {
		fmt.Println("Error reading /etc/os-release:", err)
		return ""
	}
	lines := strings.Split(string(osRelease), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "PRETTY_NAME=") {
			return strings.Split(strings.Trim(line[len("PRETTY_NAME="):], `"`), " ")[0]

		}
	}
	return ""
}
// BootProxies starts proxy servers on the idle local ports and returns the list of ports on which proxies are running.
//
// "startingPort" represents the initial port number from which the system sequentially searches for available or idle ports.. Default: 5000
//
// "count" variable represents the number of proxies to be started. Default: 50
func BootProxies(logger *zap.Logger, opt Option) *ProxySet {
	// assign default values if not provided
	distro := getDistroInfo()
	cmd := exec.Command("sudo", "cp", caCertPath, caStorePath[distro])
	err := cmd.Run()
	if err != nil {
		log.Fatalf("Failed to copy the CA to the trusted store: %v", err)
	}
	// log.Printf("This is the value of cmd1: %v", cmd)
	//Update the trusted CAs store
	cmd = exec.Command("/usr/bin/sudo", caStoreUpdateCmd[distro])
	// log.Printf("This is the command2: %v", cmd)
	err = cmd.Run()
	if err != nil {
		log.Fatalf("Failed to update system trusted store: %v", err)
	}
	if opt.StartingPort == 0 {
		opt.StartingPort = 5000
	}
	if opt.Count == 0 {
		opt.Count = 50
	}

	var proxySet = ProxySet{
		PortList: []uint32{},
		logger: logger,
	}

	port := opt.StartingPort
	for i := 0; i < opt.Count; {
		if isPortAvailable(port) {
			go proxySet.startProxy(port)
			// adds the port number on which proxy has been started
			proxySet.PortList = append(proxySet.PortList, port)
			i++
		}
		port++
	}

	proxySet.logger.Info(fmt.Sprintf("Set of proxies have started at port range [%v:%v]", proxySet.PortList[0], proxySet.PortList[opt.Count-1]))

	return &proxySet
}

// isPortAvailable function checks whether a local port is occupied and returns a boolean value indicating its availability.
func isPortAvailable(port uint32) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%v", port))
	if err != nil {
		return false
	}
	defer ln.Close()
	return true
}

const (
	caCertPath       = "pkg/proxy/ca.crt"                           // Your CA certificate file path
	caPrivateKeyPath = "pkg/proxy/ca.key"                           // Your CA private key file path
)

var caStorePath = map[string]string{
	"Ubuntu":       "/usr/local/share/ca-certificates/",
	"Debian":       "/usr/local/share/ca-certificates/",
	"CentOS":       "/etc/pki/ca-trust/source/anchors/",
	"Red Hat":      "/etc/pki/ca-trust/source/anchors/",
	"Fedora":       "/etc/pki/ca-trust/source/anchors/",
	"Arch ":   "/etc/ca-certificates/trust-source/anchors/",
	"openSUSE":     "/etc/pki/trust/anchors/",
	"SUSE ":   "/etc/pki/trust/anchors/",
	"Oracle ": "/etc/pki/ca-trust/source/anchors/",
	"Alpine ": "/usr/local/share/ca-certificates/",
	"Gentoo ": "/usr/local/share/ca-certificates/",
	"FreeBSD":      "/usr/local/share/certs/",
	"OpenBSD":      "/etc/ssl/certs/",
	"macOS":        "/usr/local/share/ca-certificates/",
}

var caStoreUpdateCmd = map[string]string{
	"Ubuntu":       "update-ca-certificates",
	"Debian":       "update-ca-certificates",
	"CentOS":       "update-ca-trust",
	"Red Hat":      "update-ca-trust",
	"Fedora":       "update-ca-trust",
	"Arch ":   "trust extract-compat",
	"openSUSE":     "update-ca-certificates",
	"SUSE ":   "update-ca-certificates",
	"Oracle ": "update-ca-trust",
	"Alpine ": "update-ca-certificates",
	"Gentoo ": "update-ca-certificates",
	"FreeBSD":      "trust extract-compat",
	"OpenBSD":      "trust extract-compat",
	"macOS":        "security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain",
}

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

// startProxy function initiates a proxy on the specified port to handle redirected outgoing network calls.
func (ps *ProxySet) startProxy(port uint32) {
	// Read the CA certificate and private key from files
	// caCert, err := ioutil.ReadFile(caCertPath)
	// if err != nil {
	// 	log.Fatalf("Failed to read CA certificate: %v", err)
	// }

	// caKey, err := ioutil.ReadFile(caPrivateKeyPath)
	// if err != nil {
	// 	log.Fatalf("Failed to read CA private key: %v", err)
	// }
	// caPrivKey, err = helpers.ParsePrivateKeyPEM(caKey)
	// if err != nil {
	// 	log.Fatalf("Failed to parse CA private key: %v", err)
	// }
	// caCertParsed, err = helpers.ParseCertificatePEM(caCert)
	// if err != nil {
	// 	log.Fatalf("Failed to parse CA certificate: %v", err)
	// }
	listener, err := net.Listen("tcp", fmt.Sprintf(proxyAddress+":%v", port))
	if err != nil {
		ps.logger.Error(fmt.Sprintf("failed to start proxy on port:%v", port), zap.Error(err))
	}
	defer listener.Close()


	ps.logger.Debug(fmt.Sprintf("Proxy server is listening on %s", fmt.Sprintf(proxyAddress+":%v", port)))

	// TODO: integerate method For TLS connections
	// config := &tls.Config{
	// 	GetCertificate: certForClient,
	// }
	// listener = tls.NewListener(listener, config)

	for {
		conn, err := listener.Accept()
		if err != nil {
			ps.logger.Error("failed to accept connection to the proxy", zap.Error(err))
			continue
		}

		go ps.handleConnection(conn, port)
	}
}

func isTLSHandshake (data []byte) bool {
	if len(data) < 5 {
		return false
	}
	return data[0] == 0x16 && data[1] == 0x03 && (data[2] == 0x00 || data[2] == 0x01 || data[2] == 0x02 || data[2] == 0x03)
}

func handleTLSConnection(conn net.Conn)(net.Conn, error){
	fmt.Println("Handling TLS connection from", conn.RemoteAddr().String())
	//Load the CA certificate and private key
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


	// Create a TLS configuration
	config := &tls.Config{
		GetCertificate: certForClient,
	}

	// Wrap the TCP connection with TLS
	tlsConn := tls.Server(conn, config)

	req := make([]byte, 1024)
	fmt.Println("before the parsed req: ", string(req))

	// _, err = tlsConn.Read(req)
	if err != nil {
		log.Panic("failed reading the request message with error: ", err)
	}
	fmt.Println("after the parsed req: ", string(req))
	// Perform the TLS handshake
	// err = tlsConn.Handshake()
	// if err != nil {
	// 	log.Println("Error performing TLS handshake:", err)
	// 	return
	// }

	// Use the tlsConn for further communication
	// For example, you can read and write data using tlsConn.Read() and tlsConn.Write()

	// Here, we simply close the connection
	// tlsConn.Close()
	return tlsConn, nil
}
// handleConnection function executes the actual outgoing network call and captures/forwards the request and response messages.
func (ps *ProxySet) handleConnection(conn net.Conn, port uint32) {
	var (
		proxyState *hooks.PortState
		indx    = -1
		err error
	)

	for i := 0; i < len(ps.PortList); i++ {

		// if err = ps.hook.ProxyPorts.Lookup(uint32(i), &proxyState); err != nil {
		// 	ps.logger.Error(fmt.Sprintf("failed to fetch the state of proxy running at port: %v", port), zap.Error(err))
		// 	break
		// }
		proxyState, err =  ps.hook.GetProxyState(uint32(i))
		if err != nil {
			ps.logger.Error("failed to lookup the proxy state from map", zap.Error(err))
			break
		}

		if proxyState.Port == port {
			indx = i
			break
		}
	}

	if indx == -1 {
		ps.logger.Error("failed to fetch the state of proxy", zap.Any("port", port))
		return
	}
	reader := bufio.NewReader(conn)
	initialData := make([]byte, 5)
	testBuffer, err := reader.Peek(len(initialData))
	if err != nil {
		ps.logger.Error("failed to read the request message in proxy", zap.Error(err), zap.Any("proxy port", port))
		return
	}
	isTLS := isTLSHandshake(testBuffer)
	if isTLS {
		connWrapped := Conn{r: *reader, Conn: conn}
		conn, err = handleTLSConnection(&connWrapped)
		if err != nil {
			ps.logger.Error("failed to handle TLS connection", zap.Error(err))
			return
		}
	}
	buffer, err := util.ReadBytes(conn)
	if err != nil {
		ps.logger.Error("failed to read the request message in proxy", zap.Error(err), zap.Any("proxy port", port))
		return
	}

	// dst stores the connection with actual destination for the outgoing network call
	var (
		dst net.Conn
		actualAddress = fmt.Sprintf("%v:%v", util.ToIPAddressStr(proxyState.Dest_ip), proxyState.Dest_port)
	)
	//Dialing for tls connection
	if isTLS {
		fmt.Println("isTLS: ",isTLS)
		config := &tls.Config{
			InsecureSkipVerify: false,
			ServerName: destinationUrl,
		}
		dst, err = tls.Dial("tcp", fmt.Sprintf("%v:%v", destinationUrl, proxyState.Dest_port), config)
		if err != nil {
			ps.logger.Error("failed to dial the connection to destination server", zap.Error(err), zap.Any("proxy port", port), zap.Any("server address", actualAddress))
			conn.Close()
			return
		}
	}else{
		dst, err = net.Dial("tcp", actualAddress)
		if err != nil {
			ps.logger.Error("failed to dial the connection to destination server", zap.Error(err), zap.Any("proxy port", port), zap.Any("server address", actualAddress))
			conn.Close()
			return
		}
	}


	switch  {
	case httpparser.IsOutgoingHTTP(buffer):
		// capture the otutgoing http text messages]
		ps.hook.AppendDeps(httpparser.CaptureHTTPMessage(buffer, conn, dst, ps.logger))
	case mongoparser.IsOutgoingMongo(buffer):
		deps := mongoparser.CaptureMongoMessage(buffer, conn, dst, ps.logger)
		for _, v := range deps {
			ps.hook.AppendDeps(v)
		}
	default:
	}
	// releases the occupied proxy
	proxyState.Occupied = 0
	proxyState.Dest_ip = 0
	proxyState.Dest_port = 0
	ps.hook.UpdateProxyState(uint32(indx), proxyState)
	// err = ps.hook.ProxyPorts.Update(uint32(indx), proxyState, ebpf.UpdateLock)
	// if err != nil {
	// 	ps.logger.Error("failed to release the occupied proxy", zap.Error(err), zap.Any("proxy port", port))
	// 	return
	// }
}
