package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"golang.org/x/sync/errgroup"
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
	DNSPort uint32

	DestInfo     core.DestInfo
	Integrations map[string]integrations.Integrations

	MockManagers sync.Map

	sessions *core.Sessions

	connMutex *sync.Mutex
	ipMutex   *sync.Mutex

	clientConnections []net.Conn

	Listener net.Listener

	UDPDNSServer *dns.Server
	TCPDNSServer *dns.Server
}

func New(logger *zap.Logger, info core.DestInfo, opts config.Config) *Proxy {
	return &Proxy{
		logger:       logger,
		Port:         opts.ProxyPort, // default: 16789
		DNSPort:      opts.DNSPort,   // default: 26789
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

func (p *Proxy) InitIntegrations(_ context.Context) error {
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
		utils.LogError(p.logger, err, "failed to setup CA")
		return err
	}

	g := ctx.Value(models.ErrGroupKey).(*errgroup.Group)

	// start the proxy server
	g.Go(func() error {
		utils.Recover(p.logger)
		err := p.start(ctx)
		if err != nil {
			utils.LogError(p.logger, err, "error while running the proxy server")
			return err
		}
		return nil
	})

	//change the ip4 and ip6 if provided in the opts in case of docker environment
	if len(opts.DNSIPv4Addr) != 0 {
		p.IP4 = opts.DNSIPv4Addr
	}

	if len(opts.DNSIPv6Addr) != 0 {
		p.IP6 = opts.DNSIPv6Addr
	}

	// start the TCP DNS server
	p.logger.Debug("Starting Tcp Dns Server for handling Dns queries over TCP")
	g.Go(func() error {
		utils.Recover(p.logger)
		errCh := make(chan error, 1)
		go func(errCh chan error) {
			err := p.startTCPDNSServer(ctx)
			if err != nil {
				errCh <- err
			}
		}(errCh)

		select {
		case <-ctx.Done():
			err := p.TCPDNSServer.Shutdown()
			if err != nil {
				utils.LogError(p.logger, err, "failed to shutdown tcp dns server")
				return err
			}
			return nil
		case err := <-errCh:
			return err
		}
	})

	if models.GetMode() == models.MODE_TEST {
		p.logger.Info("Keploy has taken control of the DNS resolution mechanism, your application may misbehave in test mode if you have provided wrong domain name in your application code.")
		// start the UDP DNS server
		p.logger.Debug("Starting Udp Dns Server in Test mode...")
		g.Go(func() error {
			utils.Recover(p.logger)
			errCh := make(chan error, 1)
			go func(errCh chan error) {
				err := p.startUDPDNSServer(ctx)
				if err != nil {
					errCh <- err
				}
			}(errCh)

			select {
			case <-ctx.Done():
				err := p.UDPDNSServer.Shutdown()
				if err != nil {
					utils.LogError(p.logger, err, "failed to shutdown tcp dns server")
					return err
				}
				return nil
			case err := <-errCh:
				return err
			}
		})
	}

	p.logger.Info(fmt.Sprintf("Proxy started at port:%v", p.Port))
	return nil
}

// start function starts the proxy server on the idle local port
func (p *Proxy) start(ctx context.Context) error {

	// It will listen on all the interfaces
	listener, err := net.Listen("tcp", fmt.Sprintf(":%v", p.Port))
	if err != nil {
		utils.LogError(p.logger, err, fmt.Sprintf("failed to start proxy on port:%v", p.Port))
		return err
	}
	p.Listener = listener
	p.logger.Debug(fmt.Sprintf("Proxy server is listening on %s", fmt.Sprintf(":%v", listener.Addr())))

	defer func(listener net.Listener) {
		err := listener.Close()
		if err != nil {
			p.logger.Error("failed to close the listener", zap.Error(err))
		}
		p.logger.Info("proxy stopped...")
	}(listener)

	clientConnErrGrp, clientConnCtx := errgroup.WithContext(ctx)
	defer func() {
		err := clientConnErrGrp.Wait()
		if err != nil {
			//utils.LogError(p.logger, err, "failed to handle the client connection")
		}
	}()

	for {
		clientConnCh := make(chan net.Conn, 1)
		errCh := make(chan error, 1)
		go func() {
			conn, err := listener.Accept()
			if err != nil {
				if strings.Contains(err.Error(), "use of closed network connection") {
					errCh <- nil
				}
				utils.LogError(p.logger, err, "failed to accept connection to the proxy")
				errCh <- nil
			}
			clientConnCh <- conn

		}()
		select {
		case <-ctx.Done():
			return nil
		case <-errCh:
			return err
		// handle the client connection
		case clientConn := <-clientConnCh:
			clientConnErrGrp.Go(func() error {
				utils.Recover(p.logger)
				err := p.handleConnection(clientConnCtx, clientConn)
				if err != nil {
					utils.LogError(p.logger, err, "failed to handle the client connection")
				}
				return nil
			})
		}
	}
}

// handleConnection function executes the actual outgoing network call and captures/forwards the request and response messages.
func (p *Proxy) handleConnection(ctx context.Context, srcConn net.Conn) error {
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
	clientConnID := util.GetNextID()
	p.logger.Debug("New client connection", zap.Any("connectionID", clientConnID))

	remoteAddr := srcConn.RemoteAddr().(*net.TCPAddr)
	sourcePort := remoteAddr.Port

	p.logger.Debug("Inside handleConnection of proxyServer", zap.Any("source port", sourcePort), zap.Any("Time", time.Now().Unix()))

	destInfo, err := p.DestInfo.Get(ctx, uint16(sourcePort))
	if err != nil {
		utils.LogError(p.logger, err, "failed to fetch the destination info", zap.Any("Source port", sourcePort))
		return err
	}

	// releases the occupied source port when done fetching the destination info
	err = p.DestInfo.Delete(ctx, uint16(sourcePort))
	if err != nil {
		utils.LogError(p.logger, err, "failed to delete the destination info", zap.Any("Source port", sourcePort))
		return err
	}

	//TODO: how to close the mock channel here i mean before getting it if there is any error.
	//get the session rule
	rule, ok := p.sessions.Get(destInfo.AppID)
	if !ok {
		utils.LogError(p.logger, nil, "failed to fetch the session rule", zap.Any("AppID", destInfo.AppID))
		return err
	}

	var dstAddr string

	if destInfo.Version == 4 {
		dstAddr = fmt.Sprintf("%v:%v", util.ToIP4AddressStr(destInfo.IPv4Addr), destInfo.Port)
		p.logger.Debug("", zap.Any("DestIp4", destInfo.IPv4Addr), zap.Any("DestPort", destInfo.Port))
	} else if destInfo.Version == 6 {
		dstAddr = fmt.Sprintf("[%v]:%v", util.ToIPv6AddressStr(destInfo.IPv6Addr), destInfo.Port)
		p.logger.Debug("", zap.Any("DestIp6", destInfo.IPv6Addr), zap.Any("DestPort", destInfo.Port))
	}

	// This is used to handle the parser errors
	parserErrGrp, parserCtx := errgroup.WithContext(ctx)
	parserCtx = context.WithValue(parserCtx, models.ErrGroupKey, parserErrGrp)
	defer func() {
		err := parserErrGrp.Wait()
		if err != nil {
		}
	}()

	// To close the mock channel if the context is done
	//TODO: How can i close this if error occurs before this.
	parserErrGrp.Go(func() error {
		utils.Recover(p.logger)
		<-parserCtx.Done()
		close(rule.MC)
		return nil
	})

	//checking for the destination port of "mysql"
	if destInfo.Port == 3306 {
		var dstConn net.Conn
		if rule.Mode != models.MODE_TEST {
			dstConn, err = net.Dial("tcp", dstAddr)
			if err != nil {
				utils.LogError(p.logger, err, "failed to dial the conn to destination server", zap.Any("proxy port", p.Port), zap.Any("server address", dstAddr))
				return err
			}
			// Record the outgoing message into a mock
			err := p.Integrations["mysql"].RecordOutgoing(ctx, srcConn, dstConn, rule.MC, rule.OutgoingOptions)
			if err != nil {
				utils.LogError(p.logger, err, "failed to record the outgoing message")
				return err
			}
			return nil
		}

		m, ok := p.MockManagers.Load(destInfo.AppID)
		if !ok {
			utils.LogError(p.logger, nil, "failed to fetch the mock manager", zap.Any("AppID", destInfo.AppID))
			return err
		}

		//mock the outgoing message
		err := p.Integrations["mysql"].MockOutgoing(ctx, srcConn, &integrations.ConditionalDstCfg{Addr: dstAddr}, m.(*MockManager), rule.OutgoingOptions)
		if err != nil {
			utils.LogError(p.logger, err, "failed to mock the outgoing message")
			return err
		}
		return nil
	}

	reader := bufio.NewReader(srcConn)
	initialData := make([]byte, 5)
	// reading the initial data from the client connection to determine if the connection is a TLS handshake
	testBuffer, err := reader.Peek(len(initialData))
	if err != nil {
		if err == io.EOF && len(testBuffer) == 0 {
			p.logger.Debug("received EOF, closing conn", zap.Any("connectionID", clientConnID), zap.Error(err))
			return nil
		}
		utils.LogError(p.logger, err, "failed to peek the request message in proxy", zap.Any("proxy port", p.Port))
		return err
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
			return err
		}
	}

	// attempt to read conn until buffer is either filled or conn is closed
	initialBuf, err := util.ReadInitialBuf(ctx, p.logger, srcConn)
	if err != nil {
		utils.LogError(p.logger, err, "failed to read the initial buffer")
		return err
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
	destConnID := util.GetNextID()

	logger := p.logger.With(zap.Any("Client IP Address", srcConn.RemoteAddr().String()), zap.Any("Client ConnectionID", clientConnID), zap.Any("Destination IP Address", dstAddr), zap.Any("Destination ConnectionID", destConnID))

	dstCfg := &integrations.ConditionalDstCfg{
		Port: uint(destInfo.Port),
	}

	//make new connection to the destination server
	if isTLS {
		logger.Debug("", zap.Any("isTLS connection", isTLS))
		cfg := &tls.Config{
			InsecureSkipVerify: true,
			ServerName:         dstURL,
		}

		addr := fmt.Sprintf("%v:%v", dstURL, destInfo.Port)
		dstConn, err = tls.Dial("tcp", addr, cfg)
		if err != nil {
			utils.LogError(logger, err, "failed to dial the conn to destination server", zap.Any("proxy port", p.Port), zap.Any("server address", dstAddr))
			return err
		}

		dstCfg.TLSCfg = cfg
		dstCfg.Addr = addr

	} else {
		dstConn, err = net.Dial("tcp", dstAddr)
		if err != nil {
			utils.LogError(logger, err, "failed to dial the conn to destination server", zap.Any("proxy port", p.Port), zap.Any("server address", dstAddr))
			return err
		}
		dstCfg.Addr = dstAddr
	}

	// get the mock manager for the current app
	m, ok := p.MockManagers.Load(destInfo.AppID)
	if !ok {
		utils.LogError(logger, err, "failed to fetch the mock manager", zap.Any("AppID", destInfo.AppID))
		return err
	}

	generic := true
	//Checking for all the parsers.
	for _, parser := range p.Integrations {
		if parser.MatchType(ctx, initialBuf) {
			if rule.Mode == models.MODE_RECORD {
				err := parser.RecordOutgoing(ctx, srcConn, dstConn, rule.MC, rule.OutgoingOptions)
				if err != nil {
					utils.LogError(logger, err, "failed to record the outgoing message")
					return err
				}
			} else {
				err := parser.MockOutgoing(ctx, srcConn, dstCfg, m.(*MockManager), rule.OutgoingOptions)
				if err != nil {
					utils.LogError(logger, err, "failed to mock the outgoing message")
					return err
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
				utils.LogError(logger, err, "failed to record the outgoing message")
				return err
			}
		} else {
			err := p.Integrations["generic"].MockOutgoing(ctx, srcConn, dstCfg, m.(*MockManager), rule.OutgoingOptions)
			if err != nil {
				utils.LogError(logger, err, "failed to mock the outgoing message")
				return err
			}
		}
	}
	return nil
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
	err := p.stopDNSServers(ctx)
	if err != nil {
		utils.LogError(p.logger, err, "failed to stop the dns servers")
		return
	}

	p.logger.Info("proxy stopped...")
}

func (p *Proxy) Record(_ context.Context, id uint64, mocks chan<- *models.Mock, errChan chan error, opts models.OutgoingOptions) error {
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

func (p *Proxy) Mock(_ context.Context, id uint64, errChan chan error, opts models.OutgoingOptions) error {
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

func (p *Proxy) SetMocks(_ context.Context, id uint64, filtered []*models.Mock, unFiltered []*models.Mock) error {
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
