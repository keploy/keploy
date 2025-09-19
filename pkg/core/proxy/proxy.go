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
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/afpacket"
	"github.com/gopacket/gopacket/layers"
	"github.com/gopacket/gopacket/pcapgo"
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

var (
	ErrSrcConnTimeout = errors.New("timeout reading from source connection")
	ErrDstConnTimeout = errors.New("timeout reading from destination connection")
)

type ParserPriority struct {
	Priority   int
	ParserType integrations.IntegrationType
}

type Proxy struct {
	logger *zap.Logger

	IP4                   string
	IP6                   string
	Port                  uint32
	DNSPort               uint32
	Debug                 bool
	CaptureNetworkPackets bool
	GlobalPassthrough     bool

	DestInfo     core.DestInfo
	Integrations map[integrations.IntegrationType]integrations.Integrations

	MockManagers         sync.Map
	integrationsPriority []ParserPriority
	errChannel           chan error

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

func New(logger *zap.Logger, info core.DestInfo, opts *config.Config, session *core.Sessions) *Proxy {
	return &Proxy{
		logger:                logger,
		Port:                  opts.ProxyPort, // default: 16789
		DNSPort:               opts.DNSPort,   // default: 26789
		IP4:                   "127.0.0.1",    // default: "127.0.0.1" <-> (2130706433)
		IP6:                   "::1",          //default: "::1" <-> ([4]uint32{0000, 0000, 0000, 0001})
		Debug:                 opts.Debug,
		CaptureNetworkPackets: opts.CapturePackets,
		GlobalPassthrough:     opts.Record.GlobalPassthrough,
		ipMutex:               &sync.Mutex{},
		connMutex:             &sync.Mutex{},
		DestInfo:              info,
		sessions:              session,
		MockManagers:          sync.Map{},
		Integrations:          make(map[integrations.IntegrationType]integrations.Integrations),
		errChannel:            make(chan error, 100), // buffered channel to prevent blocking
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
	p.logger.Info("Starting Tcp Dns Server for handling Dns queries over TCP")
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
	p.logger.Info("Starting Udp Dns Server for handling Dns queries over UDP")
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
	p.logger.Info(fmt.Sprintf("Proxy server is listening on %s", fmt.Sprintf(":%v", listener.Addr())))
	// Signal that the server is ready
	readyChan <- nil

	if p.CaptureNetworkPackets {
		p.logger.Info("Debug mode is ON — starting packet capture on loopback for proxy port 16789 → traffic.pcap")
		go p.recordNetworkPacketsForProxy(ctx)
	}

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
			p.logger.Info("failed to handle the client connection", zap.Error(err))
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
	start := time.Now()
	clientConnID := util.GetNextID()

	defer func(start time.Time) {
		p.logger.Debug("time taken by proxy to execute the flow",
			zap.Any("Client ConnectionID", clientConnID),
			zap.Int64("Duration(ms)", time.Since(start).Milliseconds()))
	}(start)

	var dstConn net.Conn
	destConnID := util.GetNextID()

	remoteAddr := srcConn.RemoteAddr().(*net.TCPAddr)
	sourcePort := remoteAddr.Port
	p.logger.Debug("Inside handleConnection of proxyServer",
		zap.Int("source port", sourcePort),
		zap.Int64("Time", time.Now().Unix()))

	// Destination lookup
	destInfo, err := p.DestInfo.Get(ctx, uint16(sourcePort))
	if err != nil {
		utils.LogError(p.logger, err, "failed to fetch the destination info", zap.Int("Source port", sourcePort))
		return err
	}
	if err := p.DestInfo.Delete(ctx, uint16(sourcePort)); err != nil {
		utils.LogError(p.logger, err, "failed to delete the destination info", zap.Int("Source port", sourcePort))
		return err
	}

	// Session rule
	rule, ok := p.sessions.Get(destInfo.AppID)
	if !ok {
		utils.LogError(p.logger, nil, "failed to fetch the session rule", zap.Uint64("AppID", destInfo.AppID))
		return errors.New("session rule not found")
	}

	// Build dstAddr
	var dstAddr string
	switch destInfo.Version {
	case 4:
		p.logger.Info("the destination is ipv4")
		dstAddr = fmt.Sprintf("%v:%v", util.ToIP4AddressStr(destInfo.IPv4Addr), destInfo.Port)
		p.logger.Debug("", zap.Uint32("DestIp4", destInfo.IPv4Addr), zap.Uint32("DestPort", destInfo.Port))
	case 6:
		p.logger.Info("the destination is ipv6")
		dstAddr = fmt.Sprintf("[%v]:%v", util.ToIPv6AddressStr(destInfo.IPv6Addr), destInfo.Port)
		p.logger.Debug("", zap.Any("DestIp6", destInfo.IPv6Addr), zap.Uint32("DestPort", destInfo.Port))
	default:
		return fmt.Errorf("unsupported ip version: %d", destInfo.Version)
	}

	// Parser ctx / cleanup
	parserErrGrp, parserCtx := errgroup.WithContext(ctx)
	parserCtx = context.WithValue(parserCtx, models.ErrGroupKey, parserErrGrp)
	parserCtx = context.WithValue(parserCtx, models.ClientConnectionIDKey, fmt.Sprint(clientConnID))
	parserCtx = context.WithValue(parserCtx, models.DestConnectionIDKey, fmt.Sprint(destConnID))
	parserCtx, parserCtxCancel := context.WithCancel(parserCtx)
	defer func() {
		parserCtxCancel()

		if srcConn != nil {
			if err := srcConn.Close(); err != nil && !strings.Contains(err.Error(), "use of closed network connection") {
				utils.LogError(p.logger, err, "failed to close the source connection", zap.Any("clientConnID", clientConnID))
			}
		}
		if dstConn != nil {
			if err := dstConn.Close(); err != nil && !strings.Contains(err.Error(), "use of closed network connection") {
				utils.LogError(p.logger, err, "failed to close the destination connection")
			}
		}
		if err := parserErrGrp.Wait(); err != nil {
			utils.LogError(p.logger, err, "failed to handle the parser cleanUp")
		}
	}()

	//check for global passthrough in test mode
	if p.GlobalPassthrough || (!rule.Mocking && (rule.Mode == models.MODE_TEST)) {
		dstConn, err = net.Dial("tcp", dstAddr)
		if err != nil {
			utils.LogError(p.logger, err, "failed to dial the conn to destination server", zap.Uint32("proxy port", p.Port), zap.String("server address", dstAddr))
			return err
		}
		if err := p.globalPassThrough(parserCtx, srcConn, dstConn); err != nil {
			utils.LogError(p.logger, err, "failed to handle the global pass through")
			return err
		}
		return nil
	}

	// Establish destination upfront (except in MODE_TEST)
	if rule.Mode != models.MODE_TEST {
		dst, err := net.Dial("tcp", dstAddr)
		if err != nil {
			utils.LogError(p.logger, err, "failed to dial the conn to destination server", zap.Uint32("proxy port", p.Port), zap.String("server address", dstAddr))
			return err
		}
		dstConn = dst
	}

	// SINGLE readers per side (no one reads raw net.Conn elsewhere)
	srcBR := bufio.NewReader(srcConn)
	var dstBR *bufio.Reader
	if dstConn != nil {
		dstBR = bufio.NewReader(dstConn)
	}

	// Peek both sides concurrently; larger than 10ms because server greetings can be slow
	const sniffN = 5
	const sniffDeadline = 10 * time.Millisecond

	srcPeek, srcPeekErr, _, dstPeekErr, isServerFirst := peekNConcurrent(srcConn, srcBR, dstConn, dstBR, sniffN, sniffDeadline)

	if srcPeekErr != nil && srcPeekErr != io.EOF {
		p.logger.Debug("source peek error (non-fatal)", zap.Error(srcPeekErr))
	}
	if dstPeekErr != nil && dstPeekErr != io.EOF {
		p.logger.Debug("destination peek error (non-fatal)", zap.Error(dstPeekErr))
	}

	// In test mode, we replay the dest packet that's why we can judge only based on srcPeek
	if rule.Mode == models.MODE_TEST {
		isServerFirst = len(srcPeek) == 0
	}

	// Build util.Conn ONCE per side; prepend peeked bytes back into the stream
	srcUC := mkUtilConn(srcConn, srcBR, p.logger)
	var dstUC *util.Conn
	if dstConn != nil {
		dstUC = mkUtilConn(dstConn, dstBR, p.logger)
	}

	// TLS detection on client-first only (we have client hello)
	var isTLS bool
	if !isServerFirst && len(srcPeek) != 0 && pTls.IsTLSHandshake(srcPeek) {
		isTLS = true
		dec, err := pTls.HandleTLSConnection(parserCtx, p.logger, srcUC, rule.Backdate)
		if err != nil {
			utils.LogError(p.logger, err, "failed to handle TLS conn")
			return err
		}
		// Recreate single reader/conn for decrypted client side
		srcConn = dec
		srcBR = bufio.NewReader(dec)
		srcUC = &util.Conn{Conn: dec, Reader: srcBR, Logger: p.logger}
	}

	// Logger with IDs
	clientID, _ := parserCtx.Value(models.ClientConnectionIDKey).(string)
	destID, _ := parserCtx.Value(models.DestConnectionIDKey).(string)
	logger := p.logger.With(
		zap.String("Client ConnectionID", clientID),
		zap.String("Destination ConnectionID", destID),
		zap.String("Destination IP Address", dstAddr),
		zap.String("Client IP Address", srcConn.RemoteAddr().String()),
	)

	// If TLS, or for HTTP completeness, read initial bytes from SAME reader path
	var initial []byte
	if !isServerFirst {
		var err error
		initial, err = util.ReadInitialBuf(parserCtx, p.logger, srcUC)
		if err != nil && err != io.EOF {
			utils.LogError(logger, err, "failed to read the initial buffer")
			return err
		}
	}

	// HTTP header completion (still on the SAME srcUC)
	if util.IsHTTPReq(initial) && !util.HasCompleteHTTPHeaders(initial) {
		logger.Info("Partial HTTP headers detected, reading more data to get complete headers")
		more, err := util.ReadHTTPHeadersUntilEnd(parserCtx, p.logger, srcUC)
		if err != nil {
			utils.LogError(logger, err, "failed to read the complete HTTP headers from client")
			return err
		}
		initial = append(initial, more...)
	}
	// If we accumulated 'initial', prepend it back for parsers
	if len(initial) > 0 {
		srcUC = &util.Conn{
			Conn:   srcUC.Conn,
			Reader: io.MultiReader(bytes.NewReader(initial), srcBR),
			Logger: p.logger,
		}
	}

	// Prepare dst config (plain or TLS)
	dstCfg := &models.ConditionalDstCfg{Port: uint(destInfo.Port)}
	if isTLS {
		// map client sourcePort -> SNI/host
		urlVal, ok := pTls.SrcPortToDstURL.Load(sourcePort)
		if !ok {
			utils.LogError(logger, nil, "failed to fetch the destination url")
			return errors.New("tls dst url not found")
		}
		dstURL, ok := urlVal.(string)
		if !ok {
			utils.LogError(logger, nil, "failed to type cast the destination url")
			return errors.New("tls dst url wrong type")
		}
		cfg := &tls.Config{
			InsecureSkipVerify: true,
			ServerName:         dstURL,
		}
		addr := fmt.Sprintf("%v:%v", dstURL, destInfo.Port)

		if rule.Mode != models.MODE_TEST {
			tlsDst, err := tls.Dial("tcp", addr, cfg)
			if err != nil {
				utils.LogError(logger, err, "failed to dial the conn to destination server", zap.Uint32("proxy port", p.Port), zap.String("server address", dstAddr))
				return err
			}
			dstConn = tlsDst
			dstBR = bufio.NewReader(tlsDst)
			// rebuild dstUC preserving any earlier peek (usually empty under TLS)
			dstUC = &util.Conn{Conn: tlsDst, Reader: dstBR, Logger: p.logger}
		}
		dstCfg.TLSCfg = cfg
		dstCfg.Addr = addr
	} else {
		dstCfg.Addr = dstAddr
	}

	// Mock manager (if needed)
	m, ok := p.MockManagers.Load(destInfo.AppID)
	if !ok && rule.Mode == models.MODE_TEST {
		utils.LogError(logger, nil, "failed to fetch the mock manager", zap.Uint64("AppID", destInfo.AppID))
		return errors.New("mock manager not found")
	}

	// Parser selection:
	// - server-first: decide using dstPeek (e.g., MySQL greeting)
	// - client-first: decide using 'initial' (or srcPeek when initial empty)
	var (
		generic       = true
		matchedParser integrations.Integrations
		parserType    integrations.IntegrationType
	)

	for _, pr := range p.integrationsPriority {
		parser := p.Integrations[pr.ParserType]
		if parser == nil {
			continue
		}

		if pr.ParserType == integrations.MYSQL && isServerFirst {
			dstCfg := &models.ConditionalDstCfg{
				Port: uint(destInfo.Port),
			}
			rule.DstCfg = dstCfg
			matchedParser = parser
			parserType = pr.ParserType
			generic = false
			logger.Debug("Matched MySQL protocol based on server greeting")
			break
		}
		// client-first or other protocols
		cand := initial
		if len(cand) == 0 {
			cand = srcPeek
		}
		if parser.MatchType(parserCtx, cand) {
			matchedParser = parser
			parserType = pr.ParserType
			generic = false
			break
		}
	}

	// Dispatch
	if !generic {
		p.logger.Info("The external dependency is supported. Hence using the parser",
			zap.String("ParserType", string(parserType)),
			zap.Bool("isServerFirst", isServerFirst))

		switch rule.Mode {
		case models.MODE_RECORD:
			if err := matchedParser.RecordOutgoing(parserCtx, srcUC, dstUC, rule.MC, rule.OutgoingOptions); err != nil {
				utils.LogError(logger, err, "failed to record the outgoing message")
				return err
			}
		case models.MODE_TEST:
			if err := matchedParser.MockOutgoing(parserCtx, srcUC, dstCfg, m.(*MockManager), rule.OutgoingOptions); err != nil && err != io.EOF && !errors.Is(err, context.Canceled) {
				utils.LogError(logger, err, "failed to mock the outgoing message")
				// Send specific error type to error channel for external monitoring
				proxyErr := models.ParserError{
					ParserErrorType: models.ErrMockNotFound,
					Err:             err,
				}
				p.SendError(proxyErr)
				return err
			}
		}
		return nil
	}

	// Fallback generic
	logger.Info("The external dependency is not supported. Hence using generic parser")
	if rule.Mode == models.MODE_RECORD {
		if err := p.Integrations[integrations.GENERIC].RecordOutgoing(parserCtx, srcUC, dstUC, rule.MC, rule.OutgoingOptions); err != nil {
			utils.LogError(logger, err, "failed to record the outgoing message")
			return err
		}
	} else {
		if err := p.Integrations[integrations.GENERIC].MockOutgoing(parserCtx, srcUC, dstCfg, m.(*MockManager), rule.OutgoingOptions); err != nil && err != io.EOF && err != io.EOF && !errors.Is(err, context.Canceled) {
			utils.LogError(logger, err, "failed to mock the outgoing message")
			// Send specific error type to error channel for external monitoring
			proxyErr := models.ParserError{
				ParserErrorType: models.ErrMockNotFound,
				Err:             err,
			}
			p.SendError(proxyErr)
			return err
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

	// Close the error channel
	p.CloseErrorChannel()

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

func (p *Proxy) recordNetworkPacketsForProxy(ctx context.Context) {
	const (
		outPath      = "traffic.pcap"
		snaplen      = 65535
		matchPort    = uint16(16789)
		pollTO       = 300 * time.Millisecond
		blockTO      = 200 * time.Millisecond
		frameSizePow = 11 // 2^11 = 2048
		numBlocks    = 64
	)

	p.logger.Info("capturing packets", zap.String("outPath", outPath), zap.Uint16("port", matchPort))

	// Prepare output PCAP (pure Go writer)
	f, err := os.Create(outPath)
	if err != nil {
		p.logger.Error("creating output pcap", zap.Error(err))
		return
	}
	defer f.Close()

	w := pcapgo.NewWriter(f)
	if err := w.WriteFileHeader(uint32(snaplen), layers.LinkTypeEthernet); err != nil {
		p.logger.Error("writing pcap header", zap.Error(err))
		return
	}
	p.logger.Info("writing packets", zap.String("outPath", outPath))

	// AF_PACKET config
	frameSize := 1 << frameSizePow
	blockSize := 1 << 20 // 1 MiB per block
	if blockSize%frameSize != 0 {
		p.logger.Error("block size must be multiple of frame size", zap.Int("blockSize", blockSize), zap.Int("frameSize", frameSize))
		return
	}

	// Get list of interfaces
	interfaces, err := net.Interfaces()
	if err != nil {
		p.logger.Error("failed to get interfaces", zap.Error(err))
		return
	}

	// Initialize a wait group for concurrent packet capture on all interfaces
	var wg sync.WaitGroup

	// Capture packets from each interface
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 {
			// Skip down or loopback interfaces
			continue
		}

		wg.Add(1)
		go func(ifaceName string) {
			defer wg.Done()
			p.logger.Info("capturing packets", zap.String("iface", ifaceName))

			// Open a TPacket on the interface
			tp, err := afpacket.NewTPacket(
				afpacket.OptInterface(ifaceName),
				afpacket.OptFrameSize(frameSize),
				afpacket.OptBlockSize(blockSize),
				afpacket.OptNumBlocks(numBlocks),
				afpacket.OptBlockTimeout(blockTO),
				afpacket.OptPollTimeout(pollTO),
				afpacket.OptAddVLANHeader(false),
				afpacket.SocketRaw, // L2 frames
				afpacket.OptTPacketVersion(afpacket.TPacketVersion3),
			)
			if err != nil {
				p.logger.Error("open interface error", zap.String("iface", ifaceName), zap.Error(err))
				return
			}
			defer tp.Close()

			var writeMu sync.Mutex
			var seen uint64
			var total uint64
			start := time.Now()

			for ctx.Err() == nil {
				data, ci, err := tp.ZeroCopyReadPacketData()
				atomic.AddUint64(&seen, 1)

				if err != nil {
					if errors.Is(err, afpacket.ErrTimeout) || errors.Is(err, syscall.EAGAIN) {
						continue
					}
					select {
					case <-ctx.Done():
						return
					default:
						p.logger.Error("read error", zap.String("iface", ifaceName), zap.Error(err))
						continue
					}
				}

				pkt := gopacket.NewPacket(data, layers.LinkTypeEthernet, gopacket.NoCopy)
				tl := pkt.TransportLayer()
				if tl == nil {
					continue
				}

				keep := false
				var srcPort, dstPort uint16
				switch t := tl.(type) {
				case *layers.TCP:
					if t.RST {
						continue
					}
					srcPort = uint16(t.SrcPort)
					dstPort = uint16(t.DstPort)
					keep = (srcPort == matchPort || dstPort == matchPort)
				case *layers.UDP:
					srcPort = uint16(t.SrcPort)
					dstPort = uint16(t.DstPort)
					keep = (srcPort == matchPort || dstPort == matchPort)
				default:
					keep = false
				}
				if !keep {
					continue
				}

				if nl := pkt.NetworkLayer(); nl != nil {
					p.logger.Info("packet captured",
						zap.String("iface", ifaceName),
						zap.String("src", fmt.Sprintf("%v:%d", nl.NetworkFlow().Src(), srcPort)),
						zap.String("dst", fmt.Sprintf("%v:%d", nl.NetworkFlow().Dst(), dstPort)),
						zap.Int("len", len(data)),
					)
				} else {
					p.logger.Info("packet captured (no net layer)",
						zap.String("iface", ifaceName),
						zap.Int("len", len(data)),
					)
				}

				if len(data) > snaplen {
					data = data[:snaplen]
				}

				writeMu.Lock()
				if err := w.WritePacket(ci, data); err != nil {
					p.logger.Error("pcap write error", zap.Error(err))
				}
				writeMu.Unlock()

				if n := atomic.AddUint64(&total, 1); n%10000 == 0 {
					elapsed := time.Since(start).Truncate(time.Second)
					p.logger.Info("captured matching packets",
						zap.Uint64("matchingPackets", n),
						zap.Uint64("totalSeen", atomic.LoadUint64(&seen)),
						zap.Duration("elapsed", elapsed),
					)
				}
			}
		}(iface.Name)
	}

	// Wait for all captures to complete
	wg.Wait()

	p.logger.Info("capture completed")
}

// GetErrorChannel returns the error channel for external monitoring
func (p *Proxy) GetErrorChannel() <-chan error {
	return p.errChannel
}

// SendError sends an error to the error channel for external monitoring
func (p *Proxy) SendError(err error) {
	select {
	case p.errChannel <- err:
		// Error sent successfully
	default:
		// Channel is full, log the error instead
		p.logger.Warn("Error channel is full, dropping error", zap.Error(err))
	}
}

// CloseErrorChannel closes the error channel
func (p *Proxy) CloseErrorChannel() {
	close(p.errChannel)
}
