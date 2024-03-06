package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/core"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations"

	"go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

type Proxy struct {
	logger *zap.Logger

	IP4     string
	IP6     string
	Port    uint32
	DnsPort uint32

	DestInfo     core.DestInfo
	Integrations map[string]integrations.Integrations

	MockManagers sync.Map

	sessions *core.Sessions

	connMutex *sync.Mutex
	ipMutex   *sync.Mutex

	clientConnections []net.Conn

	Listener net.Listener

	UdpDnsServer *dns.Server
	TcpDnsServer *dns.Server
}

func New(logger *zap.Logger, info core.DestInfo, opts config.Config) *Proxy {
	return &Proxy{
		logger:       logger,
		Port:         opts.ProxyPort, // default: 16789
		DnsPort:      opts.DnsPort,   // default: 26789
		IP4:          "127.0.0.1",    // default: "127.0.0.1" <-> (2130706433)
		IP6:          "::1",          //default: "::1" <-> ([4]uint32{0000, 0000, 0000, 0001})
		ipMutex:      &sync.Mutex{},
		connMutex:    &sync.Mutex{},
		DestInfo:     info,
		sessions:     core.NewSessions(),
		MockManagers: sync.Map{},
		Integrations: make(map[string]integrations.Integrations),
	}
}

func (p *Proxy) InitIntegrations(ctx context.Context) error {
	// initialize the integrations
	for parserType, parser := range integrations.Registered {
		prs := parser(p.logger)
		p.Integrations[parserType] = prs
	}
	return nil
}

func (p *Proxy) StartProxy(ctx context.Context, opts core.ProxyOptions) error {

	//first initialize the integrations
	err := p.InitIntegrations(ctx)
	if err != nil {
		utils.LogError(p.logger, err, "failed to initialize the integrations")
		return err
	}

	// set up the CA for tls connections
	err = SetupCA(ctx, p.logger)
	if err != nil {
		if errors.Is(err, ctx.Err()) {
			println("context error in setup up CA")
			return err
		}
		utils.LogError(p.logger, err, "failed to setup CA")
		return err
	}

	// start the proxy server
	go func() {
		utils.Recover(p.logger)
		p.start(ctx)
	}()

	//change the ip4 and ip6 if provided in the opts in case of docker environment
	if len(opts.DnsIPv4Addr) != 0 {
		p.IP4 = opts.DnsIPv4Addr
	}

	if len(opts.DnsIPv6Addr) != 0 {
		p.IP6 = opts.DnsIPv6Addr
	}

	// start the TCP DNS server
	p.logger.Debug("Starting Tcp Dns Server for handling Dns queries over TCP")
	go func() {
		utils.Recover(p.logger)
		p.startTcpDnsServer()
	}()

	if models.GetMode() == models.MODE_TEST {
		p.logger.Info("Keploy has taken control of the DNS resolution mechanism, your application may misbehave in test mode if you have provided wrong domain name in your application code.")
		// start the UDP DNS server
		p.logger.Debug("Starting Udp Dns Server in Test mode...")
		go func() {
			utils.Recover(p.logger)
			p.startUdpDnsServer()
		}()
	}

	go func() {
		utils.Recover(p.logger)
		p.StopProxyServer(ctx)
	}()

	p.logger.Info(fmt.Sprintf("Proxy started at port:%v", p.Port))
	return nil
}

// start function starts the proxy server on the idle local port
func (p *Proxy) start(ctx context.Context) {

	// It will listen on all the interfaces
	listener, err := net.Listen("tcp", fmt.Sprintf(":%v", p.Port))
	if err != nil {
		utils.LogError(p.logger, err, fmt.Sprintf("failed to start proxy on port:%v", p.Port))
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
			utils.LogError(p.logger, err, "failed to accept connection to the proxy")
			break
		}

		// collecting the client connections for cleanup
		p.connMutex.Lock()
		p.clientConnections = append(p.clientConnections, conn)
		p.connMutex.Unlock()

		go func() {
			utils.Recover(p.logger)
			p.handleConnection(ctx, conn)
		}()
	}
}

// handleConnection function executes the actual outgoing network call and captures/forwards the request and response messages.
func (p *Proxy) handleConnection(ctx context.Context, srcConn net.Conn) {
	//checking how much time proxy takes to execute the flow.
	start := time.Now()

	defer func(start time.Time, srcConn net.Conn) {
		err := srcConn.Close()
		if err != nil {
			utils.LogError(p.logger, err, "failed to close the source connection")
			return
		}
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
		utils.LogError(p.logger, err, "failed to fetch the destination info", zap.Any("Source port", sourcePort))
		return
	}

	// releases the occupied source port when done fetching the destination info
	err = p.DestInfo.Delete(ctx, uint16(sourcePort))
	if err != nil {
		utils.LogError(p.logger, err, "failed to delete the destination info", zap.Any("Source port", sourcePort))
		return
	}

	//get the session rule
	rule, ok := p.sessions.Get(destInfo.AppID)
	if !ok {
		utils.LogError(p.logger, nil, "failed to fetch the session rule", zap.Any("AppID", destInfo.AppID))
		return
	}

	//TODO: where to close the mock channel and this depsErrChan
	depsErrChan := rule.DepsErrChan
	var dstAddr string

	if destInfo.Version == 4 {
		dstAddr = fmt.Sprintf("%v:%v", util.ToIP4AddressStr(destInfo.IPv4Addr), destInfo.Port)
		p.logger.Debug("", zap.Any("DestIp4", destInfo.IPv4Addr), zap.Any("DestPort", destInfo.Port))
	} else if destInfo.Version == 6 {
		dstAddr = fmt.Sprintf("[%v]:%v", util.ToIPv6AddressStr(destInfo.IPv6Addr), destInfo.Port)
		p.logger.Debug("", zap.Any("DestIp6", destInfo.IPv6Addr), zap.Any("DestPort", destInfo.Port))
	}

	//checking for the destination port of "mysql"
	if destInfo.Port == 3306 {
		var dstConn net.Conn
		if rule.Mode != models.MODE_TEST {
			dstConn, err = net.Dial("tcp", dstAddr)
			if err != nil {
				depsErrChan <- err
				utils.LogError(p.logger, err, "failed to dial the conn to destination server", zap.Any("proxy port", p.Port), zap.Any("server address", dstAddr))
				return
			}
			// Record the outgoing message into a mock
			err := p.Integrations["mysql"].RecordOutgoing(ctx, srcConn, dstConn, rule.MC, rule.OutgoingOptions)
			if err != nil {
				depsErrChan <- err
				utils.LogError(p.logger, err, "failed to record the outgoing message")
				return
			}
			return
		}

		m, ok := p.MockManagers.Load(destInfo.AppID)
		if !ok {
			depsErrChan <- err
			utils.LogError(p.logger, nil, "failed to fetch the mock manager", zap.Any("AppID", destInfo.AppID))
			return
		}

		//mock the outgoing message
		err := p.Integrations["mysql"].MockOutgoing(ctx, srcConn, &integrations.ConditionalDstCfg{Addr: dstAddr}, m.(*MockManager), rule.OutgoingOptions)
		if err != nil {
			depsErrChan <- err
			utils.LogError(p.logger, err, "failed to mock the outgoing message")
			return
		}
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
		depsErrChan <- err
		utils.LogError(p.logger, err, "failed to peek the request message in proxy", zap.Any("proxy port", p.Port))
		return
	}

	multiReader := io.MultiReader(reader, srcConn)
	srcConn = &Conn{
		Conn:   srcConn,
		r:      multiReader,
		logger: p.logger,
	}

	isTLS := isTLSHandshake(testBuffer)
	if isTLS {
		srcConn, err = p.handleTLSConnection(srcConn)
		if err != nil {
			utils.LogError(p.logger, err, "failed to handle TLS conn")
			depsErrChan <- err
			return
		}
	}

	// attempt to read conn until buffer is either filled or conn is closed
	initialBuf, err := util.ReadInitialBuf(ctx, p.logger, srcConn)
	if err != nil {
		depsErrChan <- err
		utils.LogError(p.logger, err, "failed to read the initial buffer")
		return
	}

	//update the src connection to have the initial buffer
	srcConn = &Conn{
		Conn:   srcConn,
		r:      io.MultiReader(bytes.NewReader(initialBuf), srcConn),
		logger: p.logger,
	}

	// dstConn stores conn with actual destination for the outgoing network call
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
		cfg := &tls.Config{
			InsecureSkipVerify: true,
			ServerName:         dstUrl,
		}

		addr := fmt.Sprintf("%v:%v", dstUrl, destInfo.Port)
		dstConn, err = tls.Dial("tcp", addr, cfg)
		if err != nil {
			depsErrChan <- err
			utils.LogError(logger, err, "failed to dial the conn to destination server", zap.Any("proxy port", p.Port), zap.Any("server address", dstAddr))
			return
		}

		dstCfg.TlsCfg = cfg
		dstCfg.Addr = addr

	} else {
		dstConn, err = net.Dial("tcp", dstAddr)
		if err != nil {
			depsErrChan <- err
			utils.LogError(logger, err, "failed to dial the conn to destination server", zap.Any("proxy port", p.Port), zap.Any("server address", dstAddr))
			return
		}
		dstCfg.Addr = dstAddr
	}

	// get the mock manager for the current app
	m, ok := p.MockManagers.Load(destInfo.AppID)
	if !ok {
		depsErrChan <- errors.New(fmt.Sprintf("failed to fetch the mock manager for the app:%v", destInfo.AppID))
		utils.LogError(logger, nil, "failed to fetch the mock manager", zap.Any("AppID", destInfo.AppID))
		return
	}

	generic := true
	//Checking for all the parsers.
	for _, parser := range p.Integrations {
		if parser.MatchType(ctx, initialBuf) {
			if rule.Mode == models.MODE_RECORD {
				err := parser.RecordOutgoing(ctx, srcConn, dstConn, rule.MC, rule.OutgoingOptions)
				if err != nil {
					depsErrChan <- err
					utils.LogError(logger, err, "failed to record the outgoing message")
					return
				}
			} else {
				err := parser.MockOutgoing(ctx, srcConn, dstCfg, m.(*MockManager), rule.OutgoingOptions)
				if err != nil {
					depsErrChan <- err
					utils.LogError(logger, err, "failed to mock the outgoing message")
					return
				}
			}
			generic = false
		}
	}

	if generic {
		logger.Debug("The external dependency is not supported. Hence using generic parser")
		if rule.Mode == models.MODE_RECORD {
			err := p.Integrations["generic"].RecordOutgoing(ctx, srcConn, dstConn, rule.MC, rule.OutgoingOptions)
			if err != nil {
				depsErrChan <- err
				utils.LogError(logger, err, "failed to record the outgoing message")
				return
			}
		} else {
			err := p.Integrations["generic"].MockOutgoing(ctx, srcConn, dstCfg, m.(*MockManager), rule.OutgoingOptions)
			if err != nil {
				depsErrChan <- err
				utils.LogError(logger, err, "failed to mock the outgoing message")
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
		err := clientConn.Close()
		if err != nil {
			return
		}
	}
	p.connMutex.Unlock()

	if p.Listener != nil {
		err := p.Listener.Close()
		if err != nil {
			utils.LogError(p.logger, err, "failed to stop proxy server")
		}
	}

	// stop dns servers
	p.stopDnsServer(ctx)

	p.logger.Info("proxy stopped...")
}

func (p *Proxy) Record(ctx context.Context, id uint64, mocks chan<- *models.Mock, errChan chan error, opts models.OutgoingOptions) error {
	p.sessions.Set(id, &core.Session{
		ID:              id,
		Mode:            models.MODE_RECORD,
		MC:              mocks,
		DepsErrChan:     errChan,
		OutgoingOptions: opts,
	})

	p.MockManagers.Store(id, NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator)))

	////set the new proxy ip:port for a new session
	//err := p.setProxyIP(opts.DnsIPv4Addr, opts.DnsIPv6Addr)
	//if err != nil {
	//	return errors.New("failed to record the outgoing message")
	//}

	return nil
}

func (p *Proxy) Mock(ctx context.Context, id uint64, errChan chan error, opts models.OutgoingOptions) error {
	p.sessions.Set(id, &core.Session{
		ID:              id,
		Mode:            models.MODE_TEST,
		DepsErrChan:     errChan,
		OutgoingOptions: opts,
	})
	p.MockManagers.Store(id, NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator)))

	////set the new proxy ip:port for a new session
	//err := p.setProxyIP(opts.DnsIPv4Addr, opts.DnsIPv6Addr)
	//if err != nil {
	//	return errors.New("failed to mock the outgoing message")
	//}

	return nil
}

func (p *Proxy) SetMocks(ctx context.Context, id uint64, filtered []*models.Mock, unFiltered []*models.Mock) error {
	//session, ok := p.sessions.Get(id)
	//if !ok {
	//	return fmt.Errorf("session not found")
	//}
	m, ok := p.MockManagers.Load(id)
	if ok {
		m.(*MockManager).SetFilteredMocks(filtered)
		m.(*MockManager).SetUnFilteredMocks(unFiltered)
	}

	return nil
}

func (p *Proxy) setProxyIP(ip4, ip6 string) error {
	p.ipMutex.Lock()
	defer p.ipMutex.Unlock()

	if len(ip4) == 0 {
		return errors.New("ip4 is empty")
	}

	p.IP4 = ip4
	//set the ip6 if provided
	if len(ip6) != 0 {
		p.IP6 = ip6
	}
	return nil
}
