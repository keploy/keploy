package proxy

import (
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"
	"net"

	"github.com/cloudflare/cfssl/csr"
	"github.com/cloudflare/cfssl/signer"
	"github.com/cloudflare/cfssl/signer/local"
	"go.keploy.io/server/pkg/hooks"
	"go.keploy.io/server/pkg/proxy/integrations/httpparser"
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

func (ps *ProxySet) SetHook(hook	*hooks.Hook)  {
	ps.hook = hook
}


// BootProxies starts proxy servers on the idle local ports and returns the list of ports on which proxies are running. 
// 
// "startingPort" represents the initial port number from which the system sequentially searches for available or idle ports.. Default: 5000
// 
// "count" variable represents the number of proxies to be started. Default: 50
func BootProxies(logger *zap.Logger, opt Option) *ProxySet {
	// assign default values if not provided
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
	dst, err = net.Dial("tcp", actualAddress)
	if err != nil {
		ps.logger.Error("failed to dial the connection to destination server", zap.Error(err), zap.Any("proxy port", port), zap.Any("server address", actualAddress))
		conn.Close()
		return
	}


	switch  {
	case httpparser.IsOutgoingHTTP(buffer):
		// capture the otutgoing http text messages]
		ps.hook.AppendDeps(httpparser.CaptureHTTPMessage(buffer, conn, dst, ps.logger))
		// case 
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
