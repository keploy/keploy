package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"github.com/miekg/dns"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/core"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations"
	"go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

type Proxy struct {
	logger *zap.Logger

	IP4     uint32
	IP6     [4]uint32
	Port    uint32
	DnsPort uint32

	DestInfo     core.DestInfo
	Integrations map[string]integrations.Integrations
	sessions     *core.Sessions

	connMutex *sync.Mutex

	clientConnections []net.Conn

	Listener net.Listener

	UdpDnsServer *dns.Server
	TcpDnsServer *dns.Server
}

func New(logger *zap.Logger, info core.DestInfo, sess *core.Sessions, opt config.Config) *Proxy {
	return &Proxy{
		logger:    logger,
		Port:      opt.Port,   // default
		DnsPort:   26789,      // default
		IP4:       2130706433, // 127.0.0.1
		IP6:       [4]uint32{0000, 0000, 0000, 0001},
		connMutex: &sync.Mutex{},
		DestInfo:  info,
		sessions:  sess,
	}
}

func (p Proxy) InitIntegrations(ctx context.Context) error {
	// initialize the integrations
	for parserType, parser := range integrations.Registered {
		prs := parser(p.logger)
		p.Integrations[parserType] = prs
	}
	return nil
}

func (p Proxy) StartProxy(ctx context.Context, opts core.ProxyOptions) error {
	//first initialize the integrations
	err := p.InitIntegrations(ctx)
	if err != nil {
		p.logger.Error("failed to initialize the integrations", zap.Error(err))
		return err
	}

	// setup the CA for tls connections
	err = setupCA(p.logger)
	if err != nil {
		p.logger.Error("failed to setup CA", zap.Error(err))
		return err
	}

	// start the proxy server
	go func() {
		p.start(ctx)
	}()

	// start the TCP DNS server
	p.logger.Debug("Starting Tcp Dns Server for handling Dns queries over TCP")
	go func() {
		p.startTcpDnsServer()
	}()

	if models.GetMode() == models.MODE_TEST {
		p.logger.Info("Keploy has taken control of the DNS resolution mechanism, your application may misbehave in test mode if you have provided wrong domain name in your application code.")
		// start the UDP DNS server
		p.logger.Debug("Starting Udp Dns Server in Test mode...")
		go func() {
			p.startUdpDnsServer()
		}()
	}

	go func() {
		p.StopProxyServer(ctx)
	}()

	p.logger.Info(fmt.Sprintf("Proxy started at port:%v", p.Port))
	return nil
}

// start function starts the proxy server on the idle local port
func (p Proxy) start(ctx context.Context) {

	// It will listen on all the interfaces
	listener, err := net.Listen("tcp", fmt.Sprintf(":%v", p.Port))
	if err != nil {
		p.logger.Error(fmt.Sprintf("failed to start proxy on port:%v", p.Port), zap.Error(err))
		return
	}
	p.Listener = listener
	p.logger.Debug(fmt.Sprintf("Proxy server is listening on %s", fmt.Sprintf(":%v", listener.Addr())))

	for {
		conn, err := listener.Accept()
		if err != nil {
			if strings.Contains(err.Error(), "use of closed network connection") {
				break
			}
			p.logger.Error("failed to accept connection to the proxy", zap.Error(err))
			break
		}

		// collecting the client connections for cleanup
		p.connMutex.Lock()
		p.clientConnections = append(p.clientConnections, conn)
		p.connMutex.Unlock()

		go func() {
			p.handleConnection(ctx, conn)
		}()
	}
}

// handleConnection function executes the actual outgoing network call and captures/forwards the request and response messages.
func (p *Proxy) handleConnection(ctx context.Context, srcConn net.Conn) {
	//checking how much time proxy takes to execute the flow.
	start := time.Now()

	defer func(start time.Time, srcConn net.Conn) {
		srcConn.Close()
		duration := time.Since(start)
		p.logger.Debug("time taken by proxy to execute the flow", zap.Any("Duration(ms)", duration.Milliseconds()))
	}(start, srcConn)

	// making a new client connection id for each client connection
	clientConnId := util.GetNextID()
	p.logger.Debug("New client connection", zap.Any("connectionID", clientConnId))

	remoteAddr := srcConn.RemoteAddr().(*net.TCPAddr)
	sourcePort := remoteAddr.Port

	p.logger.Debug("Inside handleConnection of proxyServer", zap.Any("source port", sourcePort), zap.Any("Time", time.Now().Unix()))

	destInfo, err := p.DestInfo.Get(ctx, uint16(sourcePort))
	if err != nil {
		p.logger.Error("failed to fetch the destination info", zap.Any("Source port", sourcePort), zap.Any("err:", err))
		return
	}

	// releases the occupied source port when done fetching the destination info
	err = p.DestInfo.Delete(ctx, uint16(sourcePort))
	if err != nil {
		p.logger.Error("failed to delete the destination info", zap.Any("Source port", sourcePort), zap.Any("err:", err))
		return
	}

	//get the session rule
	rule, ok := p.sessions.Get(destInfo.AppID)
	if !ok {
		p.logger.Error("failed to fetch the session rule", zap.Any("AppID", destInfo.AppID))
		return
	}

	var dstAddr string

	if destInfo.Version == 4 {
		dstAddr = fmt.Sprintf("%v:%v", util.ToIP4AddressStr(destInfo.IPv4Addr), destInfo.Port)
		p.logger.Debug("", zap.Any("DestIp4", destInfo.IPv4Addr), zap.Any("DestPort", destInfo.Port))
	} else if destInfo.Version == 6 {
		dstAddr = fmt.Sprintf("[%v]:%v", util.ToIPv6AddressStr(destInfo.IPv6Addr), destInfo.Port)
		p.logger.Debug("", zap.Any("DestIp6", destInfo.IPv6Addr), zap.Any("DestPort", destInfo.Port))
	}

	//checking for the destination port of mysql
	if destInfo.Port == 3306 {
		var dstConn net.Conn
		if rule.Mode != models.MODE_TEST {
			dstConn, err = net.Dial("tcp", dstAddr)
			if err != nil {
				p.logger.Error("failed to dial the conn to destination server", zap.Error(err), zap.Any("proxy port", p.Port), zap.Any("server address", dstAddr))
				return
			}
			// Record the outgoing message into a mock
			p.Integrations["mysql"].RecordOutgoing(ctx, []byte{}, srcConn, dstConn, rule.MC, rule.OutgoingOptions)
			return
		}

		//mock the outgoing message
		p.Integrations["mysql"].MockOutgoing(ctx, []byte{}, srcConn, &integrations.ConditionalDstCfg{Addr: dstAddr}, rule.Mocks, rule.OutgoingOptions)
		return
	}

	reader := bufio.NewReader(srcConn)
	initialData := make([]byte, 5)
	// reading the initial data from the client connection to determine if the connection is a TLS handshake
	testBuffer, err := reader.Peek(len(initialData))
	if err != nil {
		if err == io.EOF && len(testBuffer) == 0 {
			p.logger.Debug("received EOF, closing conn", zap.Any("connectionID", clientConnId), zap.Error(err))
			return
		}
		p.logger.Error("failed to peek the request message in proxy", zap.Any("proxy port", p.Port), zap.Error(err))
		return
	}

	isTLS := isTLSHandshake(testBuffer)
	multiReader := io.MultiReader(reader, srcConn)
	srcConn = &TLSConn{
		Conn:   srcConn,
		r:      multiReader,
		logger: p.logger,
	}

	if isTLS {
		srcConn, err = p.handleTLSConnection(srcConn)
		if err != nil {
			p.logger.Error("failed to handle TLS conn", zap.Error(err))
			return
		}
	}

	// attempt to read the conn until buffer is either filled or conn is closed
	var initialBuf []byte
	initialBuf, err = util.ReadBytes(srcConn)
	if err != nil && err != io.EOF {
		p.logger.Error("failed to read the request message in proxy", zap.Error(err), zap.Any("proxy port", p.Port))
		return
	}

	if err == io.EOF && len(initialBuf) == 0 {
		p.logger.Debug("received EOF, closing conn", zap.Error(err), zap.Any("connectionID", clientConnId))
		return
	}

	p.logger.Debug("received initial buffer", zap.Any("size", len(initialBuf)), zap.Any("initial buffer", initialBuf), zap.Any("connectionID", clientConnId))
	if err != nil {
		p.logger.Error("failed to read the request message in proxy", zap.Error(err), zap.Any("proxy port", p.Port))
		return
	}

	// dstConn stores the conn with actual destination for the outgoing network call
	var dstConn net.Conn

	//Dialing for tls conn
	destConnId := util.GetNextID()

	logger := p.logger.With(zap.Any("Client IP Address", srcConn.RemoteAddr().String()), zap.Any("Client ConnectionID", clientConnId), zap.Any("Destination IP Address", dstAddr), zap.Any("Destination ConnectionID", destConnId))

	dstCfg := &integrations.ConditionalDstCfg{
		Port: uint(destInfo.Port),
	}

	//make new connection to the destination server
	if isTLS {
		logger.Debug("", zap.Any("isTLS connection", isTLS))
		config := &tls.Config{
			InsecureSkipVerify: true,
			ServerName:         dstUrl,
		}

		addr := fmt.Sprintf("%v:%v", dstUrl, destInfo.Port)
		dstConn, err = tls.Dial("tcp", addr, config)
		if err != nil {
			logger.Error("failed to dial the conn to destination server", zap.Error(err), zap.Any("proxy port", p.Port), zap.Any("server address", dstAddr))
			return
		}

		dstCfg.TlsCfg = config
		dstCfg.Addr = addr

	} else {
		dstConn, err = net.Dial("tcp", dstAddr)
		if err != nil {
			logger.Error("failed to dial the conn to destination server", zap.Error(err), zap.Any("proxy port", p.Port), zap.Any("server address", dstAddr))
			return
		}
		dstCfg.Addr = dstAddr
	}

	generic := true

	//Checking for all the parsers.
	for _, parser := range p.Integrations {
		if parser.OutgoingType(ctx, initialBuf) {
			if rule.Mode == models.MODE_RECORD {
				err := parser.RecordOutgoing(ctx, initialBuf, srcConn, dstConn, rule.MC, rule.OutgoingOptions)
				if err != nil {
					logger.Error("failed to record the outgoing message", zap.Error(err))
					return
				}
			} else {
				err := parser.MockOutgoing(ctx, initialBuf, srcConn, dstCfg, rule.Mocks, rule.OutgoingOptions)
				if err != nil {
					logger.Error("failed to mock the outgoing message", zap.Error(err))
					return
				}
			}
			generic = false
		}
	}

	if generic {
		logger.Debug("The external dependency is not supported. Hence using generic parser")
		if rule.Mode == models.MODE_RECORD {
			err := p.Integrations["generic"].RecordOutgoing(ctx, initialBuf, srcConn, dstConn, rule.MC, rule.OutgoingOptions)
			if err != nil {
				logger.Error("failed to record the outgoing message", zap.Error(err))
				return
			}
		} else {
			err := p.Integrations["generic"].MockOutgoing(ctx, initialBuf, srcConn, dstCfg, rule.Mocks, rule.OutgoingOptions)
			if err != nil {
				logger.Error("failed to mock the outgoing message", zap.Error(err))
				return
			}
		}
	}
	return
}

func (p *Proxy) StopProxyServer(ctx context.Context) {
	<-ctx.Done()

	p.logger.Info("stopping proxy server...")

	p.connMutex.Lock()
	for _, clientConn := range p.clientConnections {
		clientConn.Close()
	}
	p.connMutex.Unlock()

	if p.Listener != nil {
		err := p.Listener.Close()
		if err != nil {
			p.logger.Error("failed to stop proxy server", zap.Error(err))
		}
	}

	// stop dns servers
	p.stopDnsServer(ctx)

	p.logger.Info("proxy stopped...")
}

func (p *Proxy) Record(ctx context.Context, id uint64, mocks chan<- *models.Mock, opts models.OutgoingOptions) error {
	p.sessions.Set(id, &core.Session{
		ID:              id,
		Mode:            models.MODE_RECORD,
		MC:              mocks,
		OutgoingOptions: opts,
	})
	return nil
}

func (p *Proxy) Mock(ctx context.Context, id uint64, mocks []*models.Mock, opts models.OutgoingOptions) error {
	p.sessions.Set(id, &core.Session{
		ID:              id,
		Mode:            models.MODE_TEST,
		Mocks:           mocks,
		OutgoingOptions: opts,
	})
	return nil
}

func (p *Proxy) SetMocks(ctx context.Context, id uint64, mocks []*models.Mock) error {
	session, ok := p.sessions.Get(id)
	if !ok {
		return fmt.Errorf("session not found")
	}
	session.Mocks = mocks
	return nil
}
