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

	//
	"time"
)

// const (
// 	proxyAddress = "0.0.0.0"
// )

type ProxySet struct {
	IP        uint32
	Port      uint32
	hook      *hooks.Hook
	logger    *zap.Logger
	FilterPid bool
	Listener  net.Listener
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
func (ps *ProxySet) SetHook(hook *hooks.Hook) {
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
	if opt.Port == 0 {
		opt.Port = 16789
	}

	localIp, err := util.GetLocalIP()
	if err != nil {
		log.Fatalln("Failed to get the local Ip address", err)
	}

	proxyAddr, ok := util.ConvertToIPV4(localIp)
	if !ok {
		log.Fatalf("Failed to convert local Ip to IPV4")
	}

	var proxySet = ProxySet{
		Port:   opt.Port,
		IP:     proxyAddr,
		logger: logger,
	}

	if isPortAvailable(opt.Port) {
		go proxySet.startProxy()
	} else {
		// TODO: Release eBPF resources if failed abruptly
		log.Fatalf("Failed to start Proxy at [Port:%v]: %v", opt.Port, err)
	}

	proxySet.logger.Info(fmt.Sprintf("Proxy started at port:%v", proxySet.Port))

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
	caCertPath       = "pkg/proxy/ca.crt" // Your CA certificate file path
	caPrivateKeyPath = "pkg/proxy/ca.key" // Your CA private key file path
)

var caStorePath = map[string]string{
	"Ubuntu":   "/usr/local/share/ca-certificates/",
	"Debian":   "/usr/local/share/ca-certificates/",
	"CentOS":   "/etc/pki/ca-trust/source/anchors/",
	"Red Hat":  "/etc/pki/ca-trust/source/anchors/",
	"Fedora":   "/etc/pki/ca-trust/source/anchors/",
	"Arch ":    "/etc/ca-certificates/trust-source/anchors/",
	"openSUSE": "/etc/pki/trust/anchors/",
	"SUSE ":    "/etc/pki/trust/anchors/",
	"Oracle ":  "/etc/pki/ca-trust/source/anchors/",
	"Alpine ":  "/usr/local/share/ca-certificates/",
	"Gentoo ":  "/usr/local/share/ca-certificates/",
	"FreeBSD":  "/usr/local/share/certs/",
	"OpenBSD":  "/etc/ssl/certs/",
	"macOS":    "/usr/local/share/ca-certificates/",
}

var caStoreUpdateCmd = map[string]string{
	"Ubuntu":   "update-ca-certificates",
	"Debian":   "update-ca-certificates",
	"CentOS":   "update-ca-trust",
	"Red Hat":  "update-ca-trust",
	"Fedora":   "update-ca-trust",
	"Arch ":    "trust extract-compat",
	"openSUSE": "update-ca-certificates",
	"SUSE ":    "update-ca-certificates",
	"Oracle ":  "update-ca-trust",
	"Alpine ":  "update-ca-certificates",
	"Gentoo ":  "update-ca-certificates",
	"FreeBSD":  "trust extract-compat",
	"OpenBSD":  "trust extract-compat",
	"macOS":    "security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain",
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
func (ps *ProxySet) startProxy() {

	port := ps.Port
	proxyAddress := fmt.Sprint(ps.IP)
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
	ps.Listener = listener

	ps.logger.Debug(fmt.Sprintf("Proxy server is listening on %s", fmt.Sprintf(proxyAddress+":%v", port)))

	// TODO: integerate method For TLS connections
	// config := &tls.Config{
	// 	GetCertificate: certForClient,
	// }
	// listener = tls.NewListener(listener, config)

	// retry := 0
	for {
		conn, err := listener.Accept()
		if err != nil {
			ps.logger.Error("failed to accept connection to the proxy", zap.Error(err))
			// retry++
			// if retry < 5 {
			// 	continue
			// }
			break
		}

		go ps.handleConnection(conn, port)
	}
}

func isTLSHandshake(data []byte) bool {
	if len(data) < 5 {
		return false
	}
	return data[0] == 0x16 && data[1] == 0x03 && (data[2] == 0x00 || data[2] == 0x01 || data[2] == 0x02 || data[2] == 0x03)
}

func handleTLSConnection(conn net.Conn) (net.Conn, error) {
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

	println("Filtering in Proxy:", ps.FilterPid)

	time.Sleep(1 * time.Second)

	remoteAddr := conn.RemoteAddr().(*net.TCPAddr)
	sourcePort := remoteAddr.Port

	// ps.hook.PrintRedirectProxyMap()

	println("GOT SOURCE PORT:", sourcePort, " at time:", time.Now().Unix())

	ps.logger.Debug("Inside handleConnection of Proxy server:", zap.Int("Source port", sourcePort))

	destInfo, err := ps.hook.GetDestinationInfo(uint16(sourcePort))
	if err != nil {
		ps.logger.Error("failed to fetch the destination info", zap.Any("Source port", sourcePort), zap.Any("err:", err))
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
	buffer, err := util.ReadBytes(reader)
	if err != nil {
		ps.logger.Error("failed to read the request message in proxy", zap.Error(err), zap.Any("proxy port", port))
		return
	}

	// dst stores the connection with actual destination for the outgoing network call
	var (
		dst           net.Conn
		actualAddress = fmt.Sprintf("%v:%v", util.ToIPAddressStr(destInfo.DestIp), destInfo.DestPort)
	)
	//Dialing for tls connection
	if isTLS {
		fmt.Println("isTLS: ", isTLS)
		config := &tls.Config{
			InsecureSkipVerify: false,
			ServerName:         destinationUrl,
		}
		dst, err = tls.Dial("tcp", fmt.Sprintf("%v:%v", destinationUrl, destInfo.DestPort), config)
		if err != nil {
			ps.logger.Error("failed to dial the connection to destination server", zap.Error(err), zap.Any("proxy port", port), zap.Any("server address", actualAddress))
			conn.Close()
			return
		}
	} else {
		dst, err = net.Dial("tcp", actualAddress)
		if err != nil {
			ps.logger.Error("failed to dial the connection to destination server", zap.Error(err), zap.Any("proxy port", port), zap.Any("server address", actualAddress))
			conn.Close()
			return
		}
	}

	// Do not capture anything until filtering is enabled
	if !ps.FilterPid {
		println("Calling Next on address", actualAddress)
		err := callNext(buffer, conn, dst, ps.logger)
		if err != nil {
			ps.logger.Error("failed to call next", zap.Error(err))
			conn.Close()
			return
		}
	} else {

		switch {
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
	}
	// releases the occupied source port
	ps.hook.CleanProxyEntry(uint16(sourcePort))

}

func callNext(requestBuffer []byte, clientConn, destConn net.Conn, logger *zap.Logger) error {
	defer destConn.Close()

	// write the request message to the actual destination server
	_, err := destConn.Write(requestBuffer)
	if err != nil {
		logger.Error("failed to write request message to the destination server", zap.Error(err))
		return err
	}

	// read the response from the actual server
	respBuffer, err := util.ReadBytes(destConn)
	if err != nil {
		logger.Error("failed to read the response message from the destination server", zap.Error(err))
		return err
	}

	// write the response message to the user client
	_, err = clientConn.Write(respBuffer)
	if err != nil {
		logger.Error("failed to write response message to the user client", zap.Error(err))
		return err
	}
	return nil
}

func (ps *ProxySet) StopProxyServer() {
	err := ps.Listener.Close()
	if err != nil {
		ps.logger.Error("failed to stop proxy server", zap.Error(err))
	}
	ps.logger.Info("proxy stopped...")
}
