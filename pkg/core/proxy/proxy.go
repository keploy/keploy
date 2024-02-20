package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"sync"

	"go.keploy.io/server/v2/pkg/core"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations"

	"go.keploy.io/server/v2/pkg"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations/grpc"
	postgresparser "go.keploy.io/server/v2/pkg/core/proxy/integrations/postgres"
	"go.keploy.io/server/v2/utils"

	"time"

	"github.com/cloudflare/cfssl/helpers"

	"github.com/miekg/dns"
	"go.keploy.io/server/v2/pkg/core/hooks"
	genericparser "go.keploy.io/server/v2/pkg/core/proxy/integrations/generic"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations/http"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mongo"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql"
	"go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

type Proxy struct {
	logger *zap.Logger

	IP4     uint32
	IP6     [4]uint32
	Port    uint32
	DnsPort uint32

	DestInfo  core.DestInfo
	connMutex *sync.Mutex

	clientConnections []net.Conn

	Listener net.Listener

	UdpDnsServer *dns.Server
	TcpDnsServer *dns.Server
}

func New(logger *zap.Logger) *Proxy {
	return &Proxy{
		logger:  logger,
		Port:    16789, // default
		DnsPort: 26789, // default
	}
}

func (p Proxy) InitIntegrations(opts core.ProxyOptions) error {
	// initialize the integrations
	for _, parser := range integrations.Registered {
		parser(p.logger, opts)
	}
	return nil
}

func (p Proxy) Record(ctx context.Context, mocks chan models.Frame, opts core.ProxyOptions) error {

	panic("implement me")
}

func (p Proxy) Mock(ctx context.Context, mocks []models.Frame, opts core.ProxyOptions) error {
	//TODO implement me
	panic("implement me")
}

func (p Proxy) SetMocks(ctx context.Context, mocks []models.Frame) error {
	//TODO implement me
	panic("implement me")
}

// BootProxy starts proxy server on the idle local port, Default:16789
func BootProxy(logger *zap.Logger, opt Option, appCmd, appContainer string, passThroughPorts []uint, h *hooks.Hook, ctx context.Context, delay uint64) *ProxySet {
	//Register all the parsers in the map.
	Register("grpc", grpc.NewGrpcParser(logger, h))
	Register("postgres", postgresparser.NewPostgresParser(logger, h))
	Register("mongo", mongo.NewMongoParser(logger, h, opt.MongoPassword))
	Register("http", http.NewHttpParser(logger, h))
	Register("mysql", mysql.NewMySqlParser(logger, h, delay))
	// assign default values if not provided

	if opt.Port == 0 {
		opt.Port = 16789
	}
	maxAttempts := 1000
	attemptsDone := 0

	if !isPortAvailable(opt.Port) {
		for i := 1024; i <= 65535 && attemptsDone < maxAttempts; i++ {
			if isPortAvailable(uint32(i)) {
				opt.Port = uint32(i)
				logger.Info("Found an available port to start proxy server", zap.Uint32("port", opt.Port))
				break
			}
			attemptsDone++
		}
	}

	if maxAttempts <= attemptsDone {
		logger.Error("Failed to find an available port to start proxy server")
		return nil
	}
	//IPv4
	localIp4, err := util.GetLocalIPv4()
	if err != nil {
		log.Fatalln("Failed to get the local Ip4 address", err)
	}

	proxyAddr4, ok := util.ConvertToIPV4(localIp4)
	if !ok {
		log.Fatalf("Failed to convert local Ip to IPV4")
	}

	//IPv6
	proxyAddr6 := [4]uint32{0000, 0000, 0000, 0001}

	//check if the user application is running inside docker container
	dCmd, _ := util.IsDockerRelatedCommand(appCmd)
	//check if the user application is running docker container using IDE
	dIDE := (appCmd == "" && len(appContainer) != 0)

	var proxySet = ProxySet{
		Port:              opt.Port,
		IP4:               proxyAddr4,
		IP6:               proxyAddr6,
		logger:            logger,
		clientConnections: []net.Conn{},
		connMutex:         &sync.Mutex{},
		dockerAppCmd:      (dCmd || dIDE),
		PassThroughPorts:  passThroughPorts,
		hook:              h,
		MongoPassword:     opt.MongoPassword,
	}

	//setting the proxy port field in hook
	proxySet.hook.SetProxyPort(opt.Port)

	if isPortAvailable(opt.Port) {
		go func() {
			defer h.Recover(pkg.GenerateRandomID())
			defer utils.HandlePanic()
			proxySet.startProxy(ctx)
		}()
		// Resolve DNS queries only in case of test mode.
		if models.GetMode() == models.MODE_TEST {
			proxySet.logger.Debug("Running Dns Server in Test mode...")
			proxySet.logger.Info("Keploy has hijacked the DNS resolution mechanism, your application may misbehave in keploy test mode if you have provided wrong domain name in your application code.")
			go func() {
				defer h.Recover(pkg.GenerateRandomID())
				defer utils.HandlePanic()
				proxySet.startDnsServer()
			}()
		}
	} else {
		// TODO: Release eBPF resources if failed abruptly
		log.Fatalf(Emoji+"Failed to start Proxy at [Port:%v]: %v", opt.Port, err)
	}

	proxySet.logger.Debug(fmt.Sprintf("Proxy IPv4:Port %v:%v", proxySet.IP4, proxySet.Port))
	proxySet.logger.Debug(fmt.Sprintf("Proxy IPV6:Port Addr %v:%v", proxySet.IP6, proxySet.Port))
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

// startProxy function initiates a proxy on the specified port to handle redirected outgoing network calls.
func (ps *ProxySet) startProxy(ctx context.Context) {

	port := ps.Port

	proxyAddress4 := util.ToIP4AddressStr(ps.IP4)
	ps.logger.Debug("", zap.Any("ProxyAddress4", proxyAddress4))

	proxyAddress6 := util.ToIPv6AddressStr(ps.IP6)
	ps.logger.Debug("", zap.Any("ProxyAddress6", proxyAddress6))

	listener, err := net.Listen("tcp", fmt.Sprintf(":%v", port))
	if err != nil {
		ps.logger.Error(fmt.Sprintf("failed to start proxy on port:%v", port), zap.Error(err))
		return
	}
	ps.Listener = listener

	ps.logger.Debug(fmt.Sprintf("Proxy server is listening on %s", fmt.Sprintf(":%v", listener.Addr())))
	ps.logger.Debug("Proxy will accept both ipv4 and ipv6 connections", zap.Any("Ipv4", proxyAddress4), zap.Any("Ipv6", proxyAddress6))

	// TODO: integerate method For TLS connections
	// config := &tls.Config{
	// 	GetCertificate: certForClient,
	// }
	// listener = tls.NewListener(listener, config)

	// retry := 0
	for {
		conn, err := listener.Accept()
		if err != nil {
			if strings.Contains(err.Error(), "use of closed network conn") {
				break
			}

			ps.logger.Error("failed to accept conn to the proxy", zap.Error(err))
			break
		}
		ps.connMutex.Lock()
		ps.clientConnections = append(ps.clientConnections, conn)
		ps.connMutex.Unlock()
		go func() {
			defer ps.hook.Recover(pkg.GenerateRandomID())
			defer utils.HandlePanic()
			ps.handleConnection(conn, port, ctx)
		}()
	}
}

func isTLSHandshake(data []byte) bool {
	if len(data) < 5 {
		return false
	}
	return data[0] == 0x16 && data[1] == 0x03 && (data[2] == 0x00 || data[2] == 0x01 || data[2] == 0x02 || data[2] == 0x03)
}

func (ps *Proxy) handleTLSConnection(conn net.Conn) (net.Conn, error) {
	//Load the CA certificate and private key

	var err error
	caPrivKey, err = helpers.ParsePrivateKeyPEM(caPKey)
	if err != nil {
		ps.logger.Error("Failed to parse CA private key: ", zap.Error(err))
		return nil, err
	}
	caCertParsed, err = helpers.ParseCertificatePEM(caCrt)
	if err != nil {
		ps.logger.Error("Failed to parse CA certificate: ", zap.Error(err))
		return nil, err
	}

	// Create a TLS configuration
	config := &tls.Config{
		GetCertificate: certForClient,
	}

	// Wrap the TCP conn with TLS
	tlsConn := tls.Server(conn, config)
	// Perform the handshake
	err = tlsConn.Handshake()

	if err != nil {
		ps.logger.Error("failed to complete TLS handshake with the client with error: ", zap.Error(err))
		return nil, err
	}
	// Use the tlsConn for further communication
	// For example, you can read and write data using tlsConn.Read() and tlsConn.Write()

	// Here, we simply close the conn
	return tlsConn, nil
}

// handleConnection function executes the actual outgoing network call and captures/forwards the request and response messages.
func (ps *Proxy) handleConnection(ctx context.Context, conn net.Conn) {

	//checking how much time proxy takes to execute the flow.
	start := time.Now()

	ps.logger.Debug("", zap.Any("PID in proxy:", os.Getpid()))

	remoteAddr := conn.RemoteAddr().(*net.TCPAddr)
	sourcePort := remoteAddr.Port

	ps.logger.Debug("Inside handleConnection of proxyServer", zap.Any("source port", sourcePort), zap.Any("Time", time.Now().Unix()))

	destInfo, err := ps.DestInfo.Get(ctx, uint16(sourcePort))
	if err != nil {
		ps.logger.Error("failed to fetch the destination info", zap.Any("Source port", sourcePort), zap.Any("err:", err))
		return
	}

	// releases the occupied source port when done fetching the destination info
	ps.DestInfo.Delete(ctx, uint16(sourcePort))

	if destInfo.Version == 4 {
		ps.logger.Debug("", zap.Any("DestIp4", destInfo.IPv4), zap.Any("DestPort", destInfo.Port))
	} else if destInfo.Version == 6 {
		ps.logger.Debug("", zap.Any("DestIp6", destInfo.IPv6), zap.Any("DestPort", destInfo.Port))
	}

	//checking for the destination port of mysql
	if destInfo.Port == 3306 {
		var dst net.Conn
		var actualAddress = ""
		if destInfo.IpVersion == 4 {
			actualAddress = fmt.Sprintf("%v:%v", util.ToIP4AddressStr(destInfo.DestIp4), destInfo.DestPort)
		} else if destInfo.IpVersion == 6 {
			actualAddress = fmt.Sprintf("[%v]:%v", util.ToIPv6AddressStr(destInfo.DestIp6), destInfo.DestPort)
		}
		if models.GetMode() != models.MODE_TEST {
			dst, err = net.Dial("tcp", actualAddress)
			if err != nil {
				ps.logger.Error("failed to dial the conn to destination server", zap.Error(err), zap.Any("proxy port", port), zap.Any("server address", actualAddress))
				conn.Close()
				return
				// }
			}
		}
		integrations.Registered["mysql"].ProcessOutgoing([]byte{}, conn, dst, ctx)

	} else {
		clientConnId := util.GetNextID()
		reader := bufio.NewReader(conn)
		initialData := make([]byte, 5)
		testBuffer, err := reader.Peek(len(initialData))
		if err != nil {
			if err == io.EOF && len(testBuffer) == 0 {
				ps.logger.Debug("received EOF, closing conn", zap.Error(err), zap.Any("connectionID", clientConnId))
				conn.Close()
				return
			}
			ps.logger.Error("failed to peek the request message in proxy", zap.Error(err), zap.Any("proxy port", port))
			return
		}

		isTLS := isTLSHandshake(testBuffer)
		multiReader := io.MultiReader(reader, conn)
		conn = &TLSConn{
			Conn:   conn,
			r:      multiReader,
			logger: ps.logger,
		}
		if isTLS {
			conn, err = ps.handleTLSConnection(conn)
			if err != nil {
				ps.logger.Error("failed to handle TLS conn", zap.Error(err))
				return
			}
		}
		// attempt to read the conn until buffer is either filled or conn is closed
		var buffer []byte
		buffer, err = util.ReadBytes(conn)
		if err != nil && err != io.EOF {
			ps.logger.Error("failed to read the request message in proxy", zap.Error(err), zap.Any("proxy port", port))
			return
		}

		if err == io.EOF && len(buffer) == 0 {
			ps.logger.Debug("received EOF, closing conn", zap.Error(err), zap.Any("connectionID", clientConnId))
			return
		}

		ps.logger.Debug("received buffer", zap.Any("size", len(buffer)), zap.Any("buffer", buffer), zap.Any("connectionID", clientConnId))
		ps.logger.Debug(fmt.Sprintf("the clientConnId: %v", clientConnId))
		if err != nil {
			ps.logger.Error("failed to read the request message in proxy", zap.Error(err), zap.Any("proxy port", port))
			return
		}

		// dst stores the conn with actual destination for the outgoing network call
		var dst net.Conn
		var actualAddress = ""
		if destInfo.IpVersion == 4 {
			actualAddress = fmt.Sprintf("%v:%v", util.ToIP4AddressStr(destInfo.DestIp4), destInfo.DestPort)
		} else if destInfo.IpVersion == 6 {
			actualAddress = fmt.Sprintf("[%v]:%v", util.ToIPv6AddressStr(destInfo.DestIp6), destInfo.DestPort)
		}

		//Dialing for tls conn
		destConnId := util.GetNextID()
		logger := ps.logger.With(zap.Any("Client IP Address", conn.RemoteAddr().String()), zap.Any("Client ConnectionID", clientConnId), zap.Any("Destination IP Address", actualAddress), zap.Any("Destination ConnectionID", destConnId))
		if isTLS {
			logger.Debug("", zap.Any("isTLS", isTLS))
			config := &tls.Config{
				InsecureSkipVerify: true,
				ServerName:         destinationUrl,
			}
			dst, err = tls.Dial("tcp", fmt.Sprintf("%v:%v", destinationUrl, destInfo.DestPort), config)
			if err != nil && models.GetMode() != models.MODE_TEST {
				logger.Error("failed to dial the conn to destination server", zap.Error(err), zap.Any("proxy port", port), zap.Any("server address", actualAddress))
				conn.Close()
				return
			}
		} else {
			dst, err = net.Dial("tcp", actualAddress)
			if err != nil && models.GetMode() != models.MODE_TEST {
				logger.Error("failed to dial the conn to destination server", zap.Error(err), zap.Any("proxy port", port), zap.Any("server address", actualAddress))
				conn.Close()
				return
			}
		}

		//for _, port := range ps.PassThroughPorts {
		//	if port == uint(destInfo.DestPort) {
		//		err = ps.callNext(buffer, conn, dst, logger)
		//		if err != nil {
		//			logger.Error("failed to pass through the outgoing call", zap.Error(err), zap.Any("for port", port))
		//			return
		//		}
		//	}
		//}

		genericCheck := true
		//Checking for all the parsers.
		for _, parser := range ParsersMap {
			if parser.OutgoingType(buffer) {
				parser.ProcessOutgoing(buffer, conn, dst, ctx)
				genericCheck = false
			}
		}
		if genericCheck {
			logger.Debug("The external dependency is not supported. Hence using generic parser")
			genericparser.ProcessGeneric(buffer, conn, dst, ps.hook, logger, ctx)
		}
	}

	// Closing the user client conn
	conn.Close()
	duration := time.Since(start)
	ps.logger.Debug("time taken by proxy to execute the flow", zap.Any("Duration(ms)", duration.Milliseconds()))
}

func (ps *Proxy) StopProxyServer(ctx context.Context) {
	<-ctx.Done()

	ps.logger.Info("stopping proxy server...")

	ps.connMutex.Lock()
	for _, clientConn := range ps.clientConnections {
		clientConn.Close()
	}
	ps.connMutex.Unlock()

	if ps.Listener != nil {
		err := ps.Listener.Close()
		if err != nil {
			ps.logger.Error("failed to stop proxy server", zap.Error(err))
		}
	}

	// stop dns servers
	ps.stopDnsServer(ctx)

	ps.logger.Info("proxy stopped...")
}

//func (ps *Proxy) callNext(ctx context.Context, requestBuffer []byte, clientConn, destConn net.Conn, logger *zap.Logger) error {
//	logger.Debug("trying to forward requests to target", zap.Any("Destination Addr", destConn.RemoteAddr().String()))
//
//	defer destConn.Close()
//	defer clientConn.Close()
//	// channels for writing messages from proxy to destination or client
//	destinationWriteChannel := make(chan []byte)
//	clientWriteChannel := make(chan []byte)
//
//	if requestBuffer != nil {
//		_, err := destConn.Write(requestBuffer)
//		if err != nil {
//			logger.Error("failed to write request message to the destination server", zap.Error(err), zap.Any("Destination Addr", destConn.RemoteAddr().String()))
//			return err
//		}
//	}
//
//	for {
//		// go routine to read from client
//		go func() {
//			defer ps.hook.Recover(pkg.GenerateRandomID())
//			defer utils.HandlePanic()
//			if clientConn != nil {
//				buffer, err := util.ReadBytes(clientConn)
//				if err != nil {
//					if err == io.EOF {
//						logger.Debug("received EOF, closing conn", zap.Error(err), zap.Any("connectionID", clientConn.RemoteAddr().String()))
//						return
//					}
//					logger.Error("failed to read the request from client in proxy", zap.Error(err), zap.Any("Client Addr", clientConn.RemoteAddr().String()))
//					return
//				}
//				destinationWriteChannel <- buffer
//			}
//		}()
//
//		// go routine to read from destination
//		go func() {
//			defer ps.hook.Recover(pkg.GenerateRandomID())
//			defer utils.HandlePanic()
//			if destConn != nil {
//				buffer, err := util.ReadBytes(destConn)
//				if err != nil {
//					if err == io.EOF {
//						logger.Debug("received EOF, closing conn", zap.Error(err), zap.Any("connectionID", destConn.RemoteAddr().String()))
//						return
//					}
//					logger.Error("failed to read the response from destination in proxy", zap.Error(err), zap.Any("Destination Addr", destConn.RemoteAddr().String()))
//					return
//				}
//				clientWriteChannel <- buffer
//			}
//		}()
//
//		select {
//		case requestBuffer := <-destinationWriteChannel:
//			// Write the request message to the actual destination server
//			_, err := destConn.Write(requestBuffer)
//			if err != nil {
//				logger.Error("failed to write request message to the destination server", zap.Error(err), zap.Any("Destination Addr", destConn.RemoteAddr().String()))
//				return err
//			}
//		case responseBuffer := <-clientWriteChannel:
//			// Write the response message to the client
//			_, err := clientConn.Write(responseBuffer)
//			if err != nil {
//				logger.Error("failed to write response to the client", zap.Error(err), zap.Any("Client Addr", clientConn.RemoteAddr().String()))
//				return err
//			}
//		}
//	}
//
//}
