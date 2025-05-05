//go:build linux

// Package proxy handles all the outgoing network calls and captures/forwards the request and response messages.
// It also handles the DNS resolution mechanism.
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
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/miekg/dns"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/core"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations"

	pTls "go.keploy.io/server/v2/pkg/core/proxy/tls"
	"go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

type ParserPriority struct {
	Priority   int
	ParserType integrations.IntegrationType
}

type Proxy struct {
	logger *zap.Logger

	IP4     string
	IP6     string
	Port    uint32
	DNSPort uint32

	DestInfo     core.DestInfo
	Integrations map[integrations.IntegrationType]integrations.Integrations

	MockManagers         sync.Map
	integrationsPriority []ParserPriority

	sessions *core.Sessions

	connMutex *sync.Mutex
	ipMutex   *sync.Mutex

	clientConnections []net.Conn

	Listener net.Listener

	//to store the nsswitch.conf file data
	nsswitchData []byte // in test mode we change the configuration of "hosts" in nsswitch.conf file to disable resolution over unix socket
	UDPDNSServer *dns.Server
	TCPDNSServer *dns.Server
}

func New(logger *zap.Logger, info core.DestInfo, opts *config.Config) *Proxy {
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
		Integrations: make(map[integrations.IntegrationType]integrations.Integrations),
	}
}

func (p *Proxy) InitIntegrations(_ context.Context) error {
	// initialize the integrations
	for parserType, parser := range integrations.Registered {
		logger := p.logger.With(zap.Any("Type", parserType))
		prs := parser.Initializer(logger)
		p.Integrations[parserType] = prs
		p.integrationsPriority = append(p.integrationsPriority, ParserPriority{Priority: parser.Priority, ParserType: parserType})
	}
	sort.Slice(p.integrationsPriority, func(i, j int) bool {
		return p.integrationsPriority[i].Priority > p.integrationsPriority[j].Priority
	})
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
	err = pTls.SetupCA(ctx, p.logger)
	if err != nil {
		// log the error and continue
		p.logger.Warn("failed to setup CA", zap.Error(err))
	}
	g, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		return errors.New("failed to get the error group from the context")
	}
	// Create a channel to signal readiness of each server
	readyChan := make(chan error, 1)

	// start the proxy server
	g.Go(func() error {
		defer utils.Recover(p.logger)
		err := p.start(ctx, readyChan)
		readyChan <- err
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
		defer utils.Recover(p.logger)
		errCh := make(chan error, 1)
		go func(errCh chan error) {
			defer utils.Recover(p.logger)
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

	// start the UDP DNS server
	p.logger.Debug("Starting Udp Dns Server for handling Dns queries over UDP")
	g.Go(func() error {
		defer utils.Recover(p.logger)
		errCh := make(chan error, 1)
		go func(errCh chan error) {
			defer utils.Recover(p.logger)
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
	// Wait for the proxy server to be ready or fail
	err = <-readyChan
	if err != nil {
		return err
	}
	p.logger.Info("Keploy has taken control of the DNS resolution mechanism, your application may misbehave if you have provided wrong domain name in your application code.")

	p.logger.Info(fmt.Sprintf("Proxy started at port:%v", p.Port))
	return nil
}

// start function starts the proxy server on the idle local port
func (p *Proxy) start(ctx context.Context, readyChan chan<- error) error {

	// It will listen on all the interfaces
	listener, err := net.Listen("tcp", fmt.Sprintf(":%v", p.Port))
	if err != nil {
		utils.LogError(p.logger, err, fmt.Sprintf("failed to start proxy on port:%v", p.Port))
		// Notify failure
		readyChan <- err
		return err
	}
	p.Listener = listener
	p.logger.Debug(fmt.Sprintf("Proxy server is listening on %s", fmt.Sprintf(":%v", listener.Addr())))
	// Signal that the server is ready
	readyChan <- nil
	defer func(listener net.Listener) {
		err := listener.Close()

		if err != nil {
			p.logger.Error("failed to close the listener", zap.Error(err))
		}
		p.logger.Info("proxy stopped...")
	}(listener)

	clientConnCtx, clientConnCancel := context.WithCancel(ctx)
	clientConnErrGrp, _ := errgroup.WithContext(clientConnCtx)
	defer func() {
		clientConnCancel()
		err := clientConnErrGrp.Wait()
		if err != nil {
			p.logger.Debug("failed to handle the client connection", zap.Error(err))
		}
		//closing all the mock channels (if any in record mode)
		for _, mc := range p.sessions.GetAllMC() {
			if mc != nil {
				close(mc)
			}
		}

		if string(p.nsswitchData) != "" {
			// reset the hosts config in nsswitch.conf of the system (in test mode)
			err = p.resetNsSwitchConfig()
			if err != nil {
				utils.LogError(p.logger, err, "failed to reset the nsswitch config")
			}
		}
	}()

	for {
		clientConnCh := make(chan net.Conn, 1)
		errCh := make(chan error, 1)
		go func() {
			defer utils.Recover(p.logger)
			conn, err := listener.Accept()
			if err != nil {
				if strings.Contains(err.Error(), "use of closed network connection") {
					errCh <- nil
					return
				}
				utils.LogError(p.logger, err, "failed to accept connection to the proxy")
				errCh <- err
				return
			}
			clientConnCh <- conn
		}()
		select {
		case <-ctx.Done():
			return nil
		case err := <-errCh:
			return err
		// handle the client connection
		case clientConn := <-clientConnCh:
			clientConnErrGrp.Go(func() error {
				defer util.Recover(p.logger, clientConn, nil)
				err := p.handleConnection(clientConnCtx, clientConn)
				if err != nil && err != io.EOF {
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

	defer func(start time.Time) {
		duration := time.Since(start)
		p.logger.Debug("time taken by proxy to execute the flow", zap.Any("Duration(ms)", duration.Milliseconds()))
	}(start)

	// making a new client connection id for each client connection
	clientConnID := util.GetNextID()
	// dstConn stores conn with actual destination for the outgoing network call
	var dstConn net.Conn

	//Dialing for tls conn
	destConnID := util.GetNextID()

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

	//get the session rule
	rule, ok := p.sessions.Get(destInfo.AppID)
	if !ok {
		utils.LogError(p.logger, nil, "failed to fetch the session rule", zap.Any("AppID", destInfo.AppID))
		return err
	}

	var dstAddr string

	switch destInfo.Version {
	case 4:
		p.logger.Debug("the destination is ipv4")
		dstAddr = fmt.Sprintf("%v:%v", util.ToIP4AddressStr(destInfo.IPv4Addr), destInfo.Port)
		p.logger.Debug("", zap.Any("DestIp4", destInfo.IPv4Addr), zap.Any("DestPort", destInfo.Port))
	case 6:
		p.logger.Debug("the destination is ipv6")
		dstAddr = fmt.Sprintf("[%v]:%v", util.ToIPv6AddressStr(destInfo.IPv6Addr), destInfo.Port)
		p.logger.Debug("", zap.Any("DestIp6", destInfo.IPv6Addr), zap.Any("DestPort", destInfo.Port))
	}

	// This is used to handle the parser errors
	parserErrGrp, parserCtx := errgroup.WithContext(ctx)
	parserCtx = context.WithValue(parserCtx, models.ErrGroupKey, parserErrGrp)
	parserCtx = context.WithValue(parserCtx, models.ClientConnectionIDKey, fmt.Sprint(clientConnID))
	parserCtx = context.WithValue(parserCtx, models.DestConnectionIDKey, fmt.Sprint(destConnID))
	parserCtx, parserCtxCancel := context.WithCancel(parserCtx)
	defer func() {
		parserCtxCancel()

		if srcConn != nil {
			err := srcConn.Close()
			if err != nil {
				utils.LogError(p.logger, err, "failed to close the source connection", zap.Any("clientConnID", clientConnID))
				return
			}
		}

		if dstConn != nil {
			err = dstConn.Close()
			if err != nil {
				// Use string matching as a last resort to check for the specific error
				if !strings.Contains(err.Error(), "use of closed network connection") {
					// Log other errors
					utils.LogError(p.logger, err, "failed to close the destination connection")
				}
				return
			}
		}

		err = parserErrGrp.Wait()
		if err != nil {
			utils.LogError(p.logger, err, "failed to handle the parser cleanUp")
		}
	}()

	//check for global passthrough in test mode
	if !rule.Mocking && rule.Mode == models.MODE_TEST {

		dstConn, err = net.Dial("tcp", dstAddr)
		if err != nil {
			utils.LogError(p.logger, err, "failed to dial the conn to destination server", zap.Any("proxy port", p.Port), zap.Any("server address", dstAddr))
			return err
		}

		err = p.globalPassThrough(parserCtx, srcConn, dstConn)
		if err != nil {
			utils.LogError(p.logger, err, "failed to handle the global pass through")
			return err
		}
		return nil
	}

	//checking for the destination port of "mysql"
	if destInfo.Port == 3306 {
		if rule.Mode != models.MODE_TEST {
			dstConn, err = net.Dial("tcp", dstAddr)
			if err != nil {
				utils.LogError(p.logger, err, "failed to dial the conn to destination server", zap.Any("proxy port", p.Port), zap.Any("server address", dstAddr))
				return err
			}

			dstCfg := &models.ConditionalDstCfg{
				Port: uint(destInfo.Port),
			}
			rule.DstCfg = dstCfg

			// Record the outgoing message into a mock
			err := p.Integrations[integrations.MYSQL].RecordOutgoing(parserCtx, srcConn, dstConn, rule.MC, rule.OutgoingOptions)
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
		err := p.Integrations[integrations.MYSQL].MockOutgoing(parserCtx, srcConn, &models.ConditionalDstCfg{Addr: dstAddr}, m.(*MockManager), rule.OutgoingOptions)
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
	srcConn = &util.Conn{
		Conn:   srcConn,
		Reader: multiReader,
		Logger: p.logger,
	}

	isTLS := pTls.IsTLSHandshake(testBuffer)
	if isTLS {
		srcConn, err = pTls.HandleTLSConnection(ctx, p.logger, srcConn, rule.Backdate)
		if err != nil {
			utils.LogError(p.logger, err, "failed to handle TLS conn")
			return err
		}
	}

	clientID, ok := parserCtx.Value(models.ClientConnectionIDKey).(string)
	if !ok {
		utils.LogError(p.logger, err, "failed to fetch the client connection id")
		return err
	}
	destID, ok := parserCtx.Value(models.DestConnectionIDKey).(string)
	if !ok {
		utils.LogError(p.logger, err, "failed to fetch the destination connection id")
		return err
	}

	logger := p.logger.With(zap.Any("Client ConnectionID", clientID), zap.Any("Destination ConnectionID", destID), zap.Any("Destination IP Address", dstAddr), zap.Any("Client IP Address", srcConn.RemoteAddr().String()))

	var initialBuf []byte
	// attempt to read conn until buffer is either filled or conn is closed
	initialBuf, err = util.ReadInitialBuf(parserCtx, p.logger, srcConn)
	if err != nil {
		utils.LogError(logger, err, "failed to read the initial buffer")
		return err
	}

	if util.IsHTTPReq(initialBuf) && !util.HasCompleteHTTPHeaders(initialBuf) {
		// HTTP headers are never chunked according to the HTTP protocol,
		// but at the TCP layer, we cannot be sure if we have received the entire
		// header in the first buffer chunk. This is why we check if the headers are complete
		// and read more data if needed.

		// Some HTTP requests, like AWS SQS, may require special handling in Keploy Enterprise.
		// These cases may send partial headers in multiple chunks, so we need to read until
		// we get the complete headers.

		logger.Debug("Partial HTTP headers detected, reading more data to get complete headers")

		// Read more data from the TCP connection to get the complete HTTP headers.
		headerBuf, err := util.ReadHTTPHeadersUntilEnd(parserCtx, p.logger, srcConn)
		if err != nil {
			// Log the error if we fail to read the complete HTTP headers.
			utils.LogError(logger, err, "failed to read the complete HTTP headers from client")
			return err
		}
		// Append the additional data to the initial buffer.
		initialBuf = append(initialBuf, headerBuf...)
	}

	//update the src connection to have the initial buffer
	srcConn = &util.Conn{
		Conn:   srcConn,
		Reader: io.MultiReader(bytes.NewReader(initialBuf), srcConn),
		Logger: p.logger,
	}

	dstCfg := &models.ConditionalDstCfg{
		Port: uint(destInfo.Port),
	}

	//make new connection to the destination server
	if isTLS {

		// get the destinationUrl from the map for the tls connection
		url, ok := pTls.SrcPortToDstURL.Load(sourcePort)
		if !ok {
			utils.LogError(logger, err, "failed to fetch the destination url")
			return err
		}
		//type case the dstUrl to string
		dstURL, ok := url.(string)
		if !ok {
			utils.LogError(logger, err, "failed to type cast the destination url")
			return err
		}

		logger.Debug("the external call is tls-encrypted", zap.Any("isTLS", isTLS))
		cfg := &tls.Config{
			InsecureSkipVerify: true,
			ServerName:         dstURL,
		}

		addr := fmt.Sprintf("%v:%v", dstURL, destInfo.Port)
		if rule.Mode != models.MODE_TEST {
			dstConn, err = tls.Dial("tcp", addr, cfg)
			if err != nil {
				utils.LogError(logger, err, "failed to dial the conn to destination server", zap.Any("proxy port", p.Port), zap.Any("server address", dstAddr))
				return err
			}
		}

		dstCfg.TLSCfg = cfg
		dstCfg.Addr = addr

	} else {
		if rule.Mode != models.MODE_TEST {
			dstConn, err = net.Dial("tcp", dstAddr)
			if err != nil {
				utils.LogError(logger, err, "failed to dial the conn to destination server", zap.Any("proxy port", p.Port), zap.Any("server address", dstAddr))
				return err
			}
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

	var matchedParser integrations.Integrations
	var parserType integrations.IntegrationType

	//Checking for all the parsers according to their priority.
	for _, parserPair := range p.integrationsPriority { // Iterate over ordered priority list
		parser, exists := p.Integrations[parserPair.ParserType]
		if !exists {
			continue // Skip if parser not found
		}

		p.logger.Debug("Checking for the parser", zap.Any("ParserType", parserPair.ParserType))
		if parser.MatchType(parserCtx, initialBuf) {
			matchedParser = parser
			parserType = parserPair.ParserType
			generic = false
			break
		}
	}

	if !generic {
		p.logger.Debug("The external dependency is supported. Hence using the parser", zap.Any("ParserType", parserType))
		switch rule.Mode {
		case models.MODE_RECORD:
			err := matchedParser.RecordOutgoing(parserCtx, srcConn, dstConn, rule.MC, rule.OutgoingOptions)
			if err != nil {
				utils.LogError(logger, err, "failed to record the outgoing message")
				return err
			}
		case models.MODE_TEST:
			err := matchedParser.MockOutgoing(parserCtx, srcConn, dstCfg, m.(*MockManager), rule.OutgoingOptions)
			if err != nil && err != io.EOF {
				utils.LogError(logger, err, "failed to mock the outgoing message")
				return err
			}
		}
	}

	if generic {
		logger.Debug("The external dependency is not supported. Hence using generic parser")
		if rule.Mode == models.MODE_RECORD {
			err := p.Integrations[integrations.GENERIC].RecordOutgoing(parserCtx, srcConn, dstConn, rule.MC, rule.OutgoingOptions)
			if err != nil {
				utils.LogError(logger, err, "failed to record the outgoing message")
				return err
			}
		} else {
			err := p.Integrations[integrations.GENERIC].MockOutgoing(parserCtx, srcConn, dstCfg, m.(*MockManager), rule.OutgoingOptions)
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

func (p *Proxy) Record(_ context.Context, id uint64, mocks chan<- *models.Mock, opts models.OutgoingOptions) error {
	p.sessions.Set(id, &core.Session{
		ID:              id,
		Mode:            models.MODE_RECORD,
		MC:              mocks,
		OutgoingOptions: opts,
	})

	p.MockManagers.Store(id, NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), p.logger))

	////set the new proxy ip:port for a new session
	//err := p.setProxyIP(opts.DnsIPv4Addr, opts.DnsIPv6Addr)
	//if err != nil {
	//	return errors.New("failed to record the outgoing message")
	//}

	return nil
}

func (p *Proxy) Mock(_ context.Context, id uint64, opts models.OutgoingOptions) error {
	p.sessions.Set(id, &core.Session{
		ID:              id,
		Mode:            models.MODE_TEST,
		OutgoingOptions: opts,
	})
	p.MockManagers.Store(id, NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), p.logger))

	if !opts.Mocking {
		p.logger.Info("🔀 Mocking is disabled, the response will be fetched from the actual service")
	}

	if string(p.nsswitchData) == "" {
		// setup the nsswitch config to redirect the DNS queries to the proxy
		err := p.setupNsswitchConfig()
		if err != nil {
			utils.LogError(p.logger, err, "failed to setup nsswitch config")
			return errors.New("failed to mock the outgoing message")
		}
	}

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

// GetConsumedMocks returns the consumed filtered mocks for a given app id
func (p *Proxy) GetConsumedMocks(_ context.Context, id uint64) ([]models.MockState, error) {
	m, ok := p.MockManagers.Load(id)
	if !ok {
		return nil, fmt.Errorf("mock manager not found to get consumed filtered mocks")
	}
	return m.(*MockManager).GetConsumedMocks(), nil
}
