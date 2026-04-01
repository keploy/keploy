// Package proxy handles all the outgoing network calls and captures/forwards the request and response messages.
// It also handles the DNS resolution mechanism.
package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	expirable "github.com/hashicorp/golang-lru/v2/expirable"
	"github.com/miekg/dns"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent"
	"golang.org/x/sync/errgroup"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	pTls "go.keploy.io/server/v3/pkg/agent/proxy/tls"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
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

	DestInfo     agent.DestInfo
	Integrations map[integrations.IntegrationType]integrations.Integrations

	integrationsPriority []ParserPriority
	errChannel           chan error
	// session holds the single active session for this proxy.
	// Previously this was a sync.Map keyed by appID (always 0), which is
	// no longer needed since the proxy serves a single client-app.
	sessionMu   sync.RWMutex
	session     *agent.Session
	mockManager *MockManager

	passThroughPortSet map[uint32]bool
	bypassedIPs        map[string]bool // IPs resolved from bypass host rules — skip TLS MITM

	// bypassedHosts is populated at DNS resolution time for hosts matching known
	// noise patterns (analytics, CDN, fonts, widgets). Connections to these hosts
	// skip TLS MITM during recording and are dropped during replay.
	bypassedHosts   map[string]bool
	bypassedHostsMu sync.RWMutex

	synchronous bool

	connMutex *sync.Mutex
	ipMutex   *sync.Mutex
	// channel to mark client connection as closed
	clientClose       chan bool
	clientConnections []net.Conn

	Listener net.Listener

	//to store the nsswitch.conf file data
	nsSwitchMutex     sync.Mutex
	nsswitchData      []byte // in test mode we change the configuration of "hosts" in nsswitch.conf file to disable resolution over unix socket
	UDPDNSServer      *dns.Server
	TCPDNSServer      *dns.Server
	GlobalPassthrough bool
	IsDocker          bool

	// dnsCache is a TTL-expiring, size-bounded LRU cache for DNS responses.
	dnsCache *expirable.LRU[string, dnsCacheEntry]

	// recordedDNSMocks tracks DNS queries that have already been recorded
	// to avoid recording duplicate mocks. Key format: "name:qtype:qclass"
	// Uses bounded LRU with TTL to prevent unbounded memory growth.
	recordedDNSMocks *expirable.LRU[string, bool]

	// isGracefulShutdown indicates the application is shutting down gracefully
	// When set, connection errors should be logged as debug instead of error
	isGracefulShutdown atomic.Bool

	auxiliaryHook agent.AuxiliaryProxyHook
}

// isLoopbackAddr returns true if the address (host:port) points to a loopback interface.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	// Handle IPv6 bracket notation
	host = strings.Trim(host, "[]")
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// isPassThroughPort returns true if the destination address targets a port
// that is in the passthrough set (proxy port, DNS port, bypass ports).
func (p *Proxy) isPassThroughPort(addr string) bool {
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	port, err := strconv.ParseUint(portStr, 10, 32)
	if err != nil {
		return false
	}
	return p.passThroughPortSet[uint32(port)]
}

// isBypassedDest checks if the destination IP:port belongs to a host that is
// in the bypass rules. Used to skip TLS MITM for third-party services.
func (p *Proxy) isBypassedDest(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	host = strings.Trim(host, "[]")
	return p.bypassedIPs[host]
}

// isNoiseHost checks if the hostname was classified as a noise host during DNS
// resolution (analytics, CDN, fonts, widgets). These hosts bypass TLS MITM.
func (p *Proxy) isNoiseHost(hostname string) bool {
	if hostname == "" {
		return false
	}
	p.bypassedHostsMu.RLock()
	defer p.bypassedHostsMu.RUnlock()
	return p.bypassedHosts[hostname]
}

// isNetworkClosedErr checks if the error is due to a closed network connection.
// This includes broken pipe, connection reset by peer, and use of closed network connection errors.
// These errors are expected during graceful shutdown and should not be logged as errors.
func isNetworkClosedErr(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return strings.Contains(errStr, "broken pipe") ||
		strings.Contains(errStr, "connection reset by peer") ||
		strings.Contains(errStr, "use of closed network connection") ||
		// Windows-specific error patterns
		strings.Contains(errStr, "wsarecv") ||
		strings.Contains(errStr, "wsasend") ||
		strings.Contains(errStr, "forcibly closed by the remote host")
}

func New(logger *zap.Logger, info agent.DestInfo, opts *config.Config) *Proxy {
	// Build the set of ports that should always be passed through.
	// Includes: keploy's own ports, configured bypass ports, and common
	// dev server ports that must not be intercepted.
	ptPorts := make(map[uint32]bool)
	ptPorts[opts.ProxyPort] = true
	ptPorts[opts.DNSPort] = true
	// Common frontend dev server ports — these serve the app under test
	// and must pass through the proxy untouched.
	ptPorts[3000] = true  // Next.js, React, Vue, Svelte default
	ptPorts[3001] = true  // Next.js HMR, alt dev port
	ptPorts[5173] = true  // Vite default
	ptPorts[5174] = true  // Vite alt
	ptPorts[4200] = true  // Angular default
	// Note: 8080 and 8000 are NOT passthrough'd because tests may use them
	// for the keploy agent health check endpoint or API servers.
	ptPorts[4173] = true  // Vite preview
	for _, rule := range config.GetByPassPorts(opts) {
		ptPorts[uint32(rule)] = true
	}

	// Resolve bypass hostnames to IPs at startup. Connections to these IPs
	// skip TLS MITM entirely — raw passthrough with zero overhead.
	bypassIPs := make(map[string]bool)
	for _, rule := range opts.BypassRules {
		if rule.Host != "" {
			ips, err := net.LookupHost(rule.Host)
			if err != nil {
				logger.Debug("could not resolve bypass host", zap.String("host", rule.Host), zap.Error(err))
				continue
			}
			for _, ip := range ips {
				bypassIPs[ip] = true
				logger.Debug("bypass IP resolved", zap.String("host", rule.Host), zap.String("ip", ip))
			}
		}
	}

	proxy := &Proxy{
		logger:             logger,
		Port:               opts.ProxyPort,
		DNSPort:            opts.DNSPort, // default: 26789
		synchronous:        opts.Agent.Synchronous,
		IP4:                "127.0.0.1", // default: "127.0.0.1" <-> (2130706433)
		IP6:                "::1",       //default: "::1" <-> ([4]uint32{0000, 0000, 0000, 0001})
		ipMutex:            &sync.Mutex{},
		connMutex:          &sync.Mutex{},
		DestInfo:           info,
		clientClose:        make(chan bool, 1),
		Integrations:       make(map[integrations.IntegrationType]integrations.Integrations),
		GlobalPassthrough:  opts.Agent.GlobalPassthrough,
		errChannel:         make(chan error, 100), // buffered channel to prevent blocking
		IsDocker:           opts.Agent.IsDocker,
		dnsCache:           newDNSCache(),
		recordedDNSMocks:   newRecordedDNSMocksCache(),
		passThroughPortSet: ptPorts,
		bypassedIPs:        bypassIPs,
		bypassedHosts:      make(map[string]bool),
	}

	return proxy
}

// getSession returns the current session in a thread-safe manner.
func (p *Proxy) getSession() *agent.Session {
	p.sessionMu.RLock()
	defer p.sessionMu.RUnlock()
	return p.session
}

// setSession replaces the current session in a thread-safe manner.
func (p *Proxy) setSession(s *agent.Session) {
	p.sessionMu.Lock()
	defer p.sessionMu.Unlock()
	p.session = s
}

// GetSession returns the current session in a thread-safe manner (exported for enterprise).
func (p *Proxy) GetSession() *agent.Session {
	return p.getSession()
}

func (p *Proxy) GetDestInfo() agent.DestInfo {
	return p.DestInfo
}

func (p *Proxy) GetIntegrations() map[integrations.IntegrationType]integrations.Integrations {
	result := make(map[integrations.IntegrationType]integrations.Integrations, len(p.Integrations))
	for k, v := range p.Integrations {
		result[k] = v
	}
	return result
}

// ResetRecordedDNSMocks clears the DNS mock deduplication tracker.
// This should be called when starting a new recording session to ensure
// DNS mocks are recorded fresh for the new session.
func (p *Proxy) ResetRecordedDNSMocks() {
	p.recordedDNSMocks = newRecordedDNSMocksCache()
	p.logger.Debug("DNS mock deduplication tracker reset")
}

// SetGracefulShutdown sets the graceful shutdown flag to indicate the application is shutting down
// When this flag is set, connection errors will be logged as debug instead of error
func (p *Proxy) SetGracefulShutdown(_ context.Context) error {
	p.isGracefulShutdown.Store(true)
	p.logger.Debug("Graceful shutdown flag set - connection errors will be logged as debug")
	return nil
}

// IsGracefulShutdown returns whether the graceful shutdown flag is set
func (p *Proxy) IsGracefulShutdown() bool {
	return p.isGracefulShutdown.Load()
}

func (p *Proxy) StartSandboxScope(ctx context.Context, scopeFilePath string) error {
	p.logger.Debug("Updating sandbox scope file path in proxy", zap.String("scopeFilePath", scopeFilePath))
	p.sessionMu.Lock()
	defer p.sessionMu.Unlock()
	if p.session == nil {
		return fmt.Errorf("no active session found")
	}

	// Update the session's Name (scope file path)
	p.session.OutgoingOptions.Name = scopeFilePath
	return nil
}

func (p *Proxy) GetCurrentScopeFilePath(_ context.Context) string {
	p.sessionMu.RLock()
	defer p.sessionMu.RUnlock()
	if p.session == nil {
		return ""
	}
	return strings.TrimSpace(p.session.OutgoingOptions.Name)
}

func (p *Proxy) SetAuxiliaryHook(h agent.AuxiliaryProxyHook) {
	p.auxiliaryHook = h
}

// getMockManager returns the current mock manager in a thread-safe manner.
func (p *Proxy) getMockManager() *MockManager {
	p.sessionMu.RLock()
	defer p.sessionMu.RUnlock()
	return p.mockManager
}

// setMockManager replaces the current mock manager in a thread-safe manner.
func (p *Proxy) setMockManager(m *MockManager) {
	p.sessionMu.Lock()
	defer p.sessionMu.Unlock()
	p.mockManager = m
}

func (p *Proxy) InitIntegrations(_ context.Context) error {
	// initialize the integrations
	for parserType, parser := range integrations.Registered {
		logger := p.logger.With(zap.Any("Type", parserType))
		prs := parser.Initializer(logger)
		p.Integrations[parserType] = prs
		logger.Debug("initialized the parser integration", zap.String("ParserType", string(parserType)))
		p.integrationsPriority = append(p.integrationsPriority, ParserPriority{Priority: parser.Priority, ParserType: parserType})
	}
	sort.Slice(p.integrationsPriority, func(i, j int) bool {
		return p.integrationsPriority[i].Priority > p.integrationsPriority[j].Priority
	})
	return nil
}

// In proxy.go
func (p *Proxy) StartProxy(ctx context.Context, opts agent.ProxyOptions) error {

	//first initialize the integrations
	err := p.InitIntegrations(ctx)
	if err != nil {
		utils.LogError(p.logger, err, "failed to initialize the integrations")
		return err
	}

	// set up the CA for tls connections
	err = pTls.SetupCA(ctx, p.logger, p.IsDocker)
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
			if p.TCPDNSServer != nil {
				err := p.TCPDNSServer.Shutdown()
				if err != nil {
					utils.LogError(p.logger, err, "failed to shutdown tcp dns server")
					return err
				}
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
			if p.UDPDNSServer != nil {
				err := p.UDPDNSServer.Shutdown()
				if err != nil {
					utils.LogError(p.logger, err, "failed to shutdown tcp dns server")
					return err
				}
			}
			return nil
		case err := <-errCh:
			return err
		}
	})

	// Wait for the proxy server to be ready or fail
	if err := <-readyChan; err != nil {
		return err
	}

	if p.auxiliaryHook != nil {
		err := p.auxiliaryHook.AfterStart(ctx, p)
		if err != nil {
			utils.LogError(p.logger, err, "failed to execute auxiliary proxy hook; verify auxiliary hook configuration or disable the hook if not required")
		}
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

		if err != nil && !isNetworkClosedErr(err) {
			p.logger.Error("failed to close the listener", zap.Error(err))
		}
		p.logger.Debug("proxy stopped...")
	}(listener)

	clientConnCtx, clientConnCancel := context.WithCancel(ctx)
	clientConnErrGrp, _ := errgroup.WithContext(clientConnCtx)
	defer func() {
		clientConnCancel()
		err := clientConnErrGrp.Wait()
		if err != nil {
			p.logger.Debug("failed to handle the client connection", zap.Error(err))
		}
		//closing the mock channel (if any in record mode)
		p.sessionMu.RLock()
		if p.session != nil && p.session.MC != nil {
			close(p.session.MC)
		}
		p.sessionMu.RUnlock()

		p.nsSwitchMutex.Lock()
		if string(p.nsswitchData) != "" {
			// reset the hosts config in nsswitch.conf of the system (in test mode)
			err = p.resetNsSwitchConfig()
			if err != nil {
				utils.LogError(p.logger, err, "failed to reset the nsswitch config")
			}
		}
		p.nsSwitchMutex.Unlock()
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
					// Network closed errors are expected when client closes connection (e.g., app shutdown)
					// Only log as error if it's not a shutdown/network closed error
					if isShutdownError(err) || isNetworkClosedErr(err) {
						p.logger.Debug("failed to handle the client connection (connection closed)", zap.Error(err))
					} else {
						utils.LogError(p.logger, err, "failed to handle the client connection")
					}
				}
				return nil
			})
		}
	}
}

func (p *Proxy) MakeClientDeRegisterd(_ context.Context) error {
	p.logger.Info("Inside MakeClientDeregisterd of proxyServer")
	p.clientClose <- true
	return nil
}

// handleConnection function executes the actual outgoing network call and captures/forwards the request and response messages.
func (p *Proxy) handleConnection(ctx context.Context, srcConn net.Conn) error {
	//checking how much time proxy takes to execute the flow.
	start := time.Now()

	// making a new client connection id for each client connection
	clientConnID := util.GetNextID()
	defer func(start time.Time) {
		duration := time.Since(start)
		p.logger.Debug("time taken by proxy to execute the flow", zap.Any("Client ConnectionID", clientConnID), zap.Int64("Duration(ms)", duration.Milliseconds()))
	}(start)

	// dstConn stores conn with actual destination for the outgoing network call
	var dstConn net.Conn

	//Dialing for tls conn
	destConnID := util.GetNextID()

	remoteAddr := srcConn.RemoteAddr().(*net.TCPAddr)
	sourcePort := remoteAddr.Port

	p.logger.Debug("Inside handleConnection of proxyServer", zap.Int("source port", sourcePort), zap.Int64("Time", time.Now().Unix()))
	destInfo, err := p.DestInfo.Get(ctx, uint16(sourcePort))
	if err != nil {
		// Gracefully handle untracked connections (eBPF lookup failed)
		// This can happen when:
		// 1. A new connection was opened after test completion (e.g., driver health check)
		// 2. The eBPF hook didn't fire for this connection (race condition, bypass rule)
		// 3. The entry was already deleted before this lookup
		//
		// Instead of failing with an error (which causes the app to see a broken connection),
		// we close the connection gracefully. The client will see "connection refused" or EOF,
		// which is cleaner than a partial/broken connection that causes "already closed" errors.
		p.logger.Warn("Untracked connection (eBPF lookup failed), closing gracefully",
			zap.Int("Source port", sourcePort), zap.Error(err))
		if srcConn != nil {
			srcConn.Close()
		}
		return nil
	}

	p.logger.Debug("Handling outgoing connection to destination port", zap.Uint32("Destination port", destInfo.Port))

	// releases the occupied source port when done fetching the destination info
	err = p.DestInfo.Delete(ctx, uint16(sourcePort))
	if err != nil {
		utils.LogError(p.logger, err, "failed to delete the destination info", zap.Int("Source port", sourcePort))
		return err
	}

	//get the session rule
	rule := p.getSession()
	if rule == nil {
		utils.LogError(p.logger, nil, "failed to fetch the session rule")
		return err
	}

	// Create a local copy of OutgoingOptions to avoid data race when multiple
	// goroutines handle connections concurrently and modify DstCfg/Synchronous
	outgoingOpts := rule.OutgoingOptions

	mgr := syncMock.Get()
	mgr.SetOutputChannel(rule.MC)
	var dstAddr string

	switch destInfo.Version {
	case 4:
		p.logger.Debug("the destination is ipv4")
		dstAddr = fmt.Sprintf("%v:%v", util.ToIP4AddressStr(destInfo.IPv4Addr), destInfo.Port)
		p.logger.Debug("", zap.Uint32("DestIp4", destInfo.IPv4Addr), zap.Uint32("DestPort", destInfo.Port))
	case 6:
		p.logger.Debug("the destination is ipv6")
		dstAddr = fmt.Sprintf("[%v]:%v", util.ToIPv6AddressStr(destInfo.IPv6Addr), destInfo.Port)
		p.logger.Debug("", zap.Any("DestIp6", destInfo.IPv6Addr), zap.Uint32("DestPort", destInfo.Port))
	}

	// Loopback handling: passthrough known infrastructure ports (dev server,
	// HMR, keploy ports) and intercept everything else (API server).
	// Non-loopback connections on port 443 bypass via eBPF passthrough.
	if isLoopbackAddr(dstAddr) {
		if p.isPassThroughPort(dstAddr) {
			_, err = util.PassThrough(ctx, p.logger, srcConn, &models.ConditionalDstCfg{Addr: dstAddr}, nil)
			if err != nil && !isNetworkClosedErr(err) {
				p.logger.Debug("loopback passthrough (infra port)", zap.String("dstAddr", dstAddr), zap.Error(err))
			}
			return nil
		}
		// Non-passthrough loopback port (e.g. API server on :8083) — fall through
		// to normal record/replay path.
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
				if !isNetworkClosedErr(err) {
					utils.LogError(p.logger, err, "failed to close the source connection", zap.Any("clientConnID", clientConnID))
				}
			}
		}

		if dstConn != nil {
			err = dstConn.Close()
			if err != nil {
				if !isNetworkClosedErr(err) {
					utils.LogError(p.logger, err, "failed to close the destination connection")
				}
			}
		}

		err = parserErrGrp.Wait()
		if err != nil {
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

		err = p.globalPassThrough(parserCtx, srcConn, dstConn)
		if err != nil {
			utils.LogError(p.logger, err, "failed to handle the global pass through")
			return err
		}
		return nil
	}
	if p.synchronous {
		outgoingOpts.Synchronous = true
	}

	//checking for the destination port of "mysql"
	if destInfo.Port == 3306 {
		if rule.Mode != models.MODE_TEST {
			dstConn, err = net.Dial("tcp", dstAddr)
			if err != nil {
				utils.LogError(p.logger, err, "failed to dial the conn to destination server", zap.Uint32("proxy port", p.Port), zap.String("server address", dstAddr))
				return err
			}
			dstCfg := &models.ConditionalDstCfg{
				Addr: dstAddr,
				Port: uint(destInfo.Port),
			}
			outgoingOpts.DstCfg = dstCfg

			// Record the outgoing message into a mock
			err := p.Integrations[integrations.MYSQL].RecordOutgoing(parserCtx, srcConn, dstConn, rule.MC, outgoingOpts)
			if err != nil {
				utils.LogError(p.logger, err, "failed to record the outgoing message")
				return err
			}
			return nil
		}

		m := p.getMockManager()
		if m == nil {
			utils.LogError(p.logger, nil, "failed to fetch the mock manager")
			return err
		}

		//mock the outgoing message
		err := p.Integrations[integrations.MYSQL].MockOutgoing(parserCtx, srcConn, &models.ConditionalDstCfg{Addr: dstAddr}, m, outgoingOpts)
		if err != nil && err != io.EOF && !errors.Is(err, context.Canceled) && !isNetworkClosedErr(err) {
			p.logger.Debug("mysql mock outgoing finished with error", zap.Error(err))
			p.sendMockNotFoundError(err)
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
		// Network closed errors are expected when client closes connection (e.g., app shutdown)
		if isShutdownError(err) || isNetworkClosedErr(err) {
			p.logger.Debug("failed to peek the request message in proxy (connection closed)", zap.Uint32("proxy port", p.Port), zap.Error(err))
		} else {
			utils.LogError(p.logger, err, "failed to peek the request message in proxy", zap.Uint32("proxy port", p.Port))
		}
		return err
	}

	var clientPeerCert *x509.Certificate
	var isMTLS bool
	isTLS := pTls.IsTLSHandshake(testBuffer)

	// Extract SNI from TLS ClientHello for noise-host bypass classification.
	var sniHostname string
	if isTLS {
		if helloData, peekErr := reader.Peek(4096); peekErr == nil || len(helloData) > 5 {
			sniHostname = pTls.ExtractSNI(helloData)
		}
	}

	multiReader := io.MultiReader(reader, srcConn)
	srcConn = &util.Conn{
		Conn:   srcConn,
		Reader: multiReader,
		Logger: p.logger,
	}

	// Fast-path: if the destination is a TLS connection to a bypassed host
	// (either by IP from bypass rules, or by hostname from DNS noise classification),
	// skip the expensive TLS MITM entirely.
	if isTLS && (p.isBypassedDest(dstAddr) || p.isNoiseHost(sniHostname) || matchesNoisePattern(sniHostname)) {
		if rule.Mode == models.MODE_TEST {
			// In replay mode there is no real server — just drop the connection.
			// Chrome handles failed connections to analytics/widget endpoints gracefully.
			p.logger.Debug("noise host dropped in test mode", zap.String("sni", sniHostname), zap.String("dstAddr", dstAddr))
			return nil
		}
		// Record mode: raw TCP passthrough to real destination with short timeout.
		p.logger.Debug("TLS bypass (noise/bypassed host)", zap.String("sni", sniHostname), zap.String("dstAddr", dstAddr))
		dstConn, dialErr := net.DialTimeout("tcp", dstAddr, 2*time.Second)
		if dialErr != nil {
			// Destination unreachable — drop silently, Chrome handles gracefully
			return nil
		}
		// Raw bidirectional relay — no TLS MITM, no HTTP parsing
		go func() {
			defer dstConn.Close()
			_, _ = io.Copy(dstConn, srcConn)
		}()
		_, _ = io.Copy(srcConn, dstConn)
		return nil
	}

	if isTLS {
		tlsStart := time.Now()
		srcConn, isMTLS, err = pTls.HandleTLSConnection(ctx, p.logger, srcConn, rule.Backdate)
		tlsElapsed := time.Since(tlsStart)
		if tlsElapsed > 100*time.Millisecond {
			p.logger.Warn("slow TLS MITM handshake", zap.Duration("elapsed", tlsElapsed), zap.String("sni", sniHostname), zap.String("dstAddr", dstAddr))
		}
		if err != nil {
			p.logger.Debug("TLS handshake failed, dropping connection (non-fatal)",
				zap.String("dstAddr", dstAddr), zap.Error(err))
			return nil
		}

		if isMTLS {
			if tlsConn, ok := srcConn.(*tls.Conn); ok {
				state := tlsConn.ConnectionState()
				if len(state.PeerCertificates) > 0 {
					clientPeerCert = state.PeerCertificates[0]
				}
			}
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

	logger := p.logger.With(zap.String("Client ConnectionID", clientID), zap.String("Destination ConnectionID", destID), zap.String("Destination IP Address", dstAddr), zap.String("Client IP Address", srcConn.RemoteAddr().String()))

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

	// For HTTP/2 connections, the initial buffer may only contain the client
	// preface and SETTINGS frames. Protocol matchers (gRPC vs h2c) need to
	// inspect the HEADERS frame's content-type to decide.
	//
	// We use a short timeout read instead of a blocking read because:
	//  - h2c clients typically send preface+SETTINGS+HEADERS in quick succession
	//  - gRPC clients send preface+SETTINGS, then WAIT for server SETTINGS_ACK
	//    before sending HEADERS — a blocking read would deadlock.
	//
	// If HEADERS arrives within the timeout → matchers can inspect content-type.
	// If it doesn't (gRPC pattern) → matchers treat missing HEADERS as gRPC.
	if util.IsHTTP2Preface(initialBuf) && !util.HasHTTP2HeadersFrame(initialBuf) {
		logger.Debug("HTTP/2 preface detected but no HEADERS frame yet, attempting short timeout read")

		const http2HeadersReadTimeout = 150 * time.Millisecond
		moreBuf, err := util.ReadWithTimeout(srcConn, http2HeadersReadTimeout)
		if err != nil {
			utils.LogError(logger, err, "failed to read HTTP/2 HEADERS frame from client")
			return err
		}
		if len(moreBuf) > 0 {
			initialBuf = append(initialBuf, moreBuf...)
		}
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

		logger.Debug("the external call is tls-encrypted", zap.Bool("isTLS", isTLS))

		nextProtos := []string{"http/1.1"} // default safe

		isHTTP := util.IsHTTPReq(initialBuf)
		isCONNECT := bytes.HasPrefix(initialBuf, []byte("CONNECT "))
		logger.Debug("ALPN decision debug",
			zap.Bool("isHTTPReq", isHTTP),
			zap.Bool("isCONNECT", isCONNECT),
			zap.Int("initialBufLen", len(initialBuf)),
			zap.String("initialBufPrefix", string(initialBuf[:min(20, len(initialBuf))])),
		)

		// Allow H2 if:
		// 1. It's not an HTTP/1.x request (could be gRPC/HTTP2 frames), OR
		// 2. It's a CONNECT request (used by gRPC parser for tunneling, ALB requires H2)
		if !isHTTP || isCONNECT {
			// not an HTTP/1.x request line; could be HTTP/2 (gRPC) frames
			nextProtos = []string{"h2", "http/1.1"}
			logger.Debug("Offering H2 for ALPN", zap.Strings("nextProtos", nextProtos))
		} else {
			logger.Debug("NOT offering H2 (HTTP/1.x detected)", zap.Strings("nextProtos", nextProtos))
		}

		cfg := &tls.Config{
			InsecureSkipVerify: true,
			ServerName:         dstURL,
			NextProtos:         nextProtos,
		}

		if isMTLS && rule.Mode != models.MODE_TEST && clientPeerCert != nil {
			err = p.applyMTLSClientCert(cfg, clientPeerCert, outgoingOpts.TLSPrivateKey)
			if err != nil {
				return err
			}
		}

		addr := dstAddr
		if dstURL != "" {
			addr = fmt.Sprintf("%v:%v", dstURL, destInfo.Port)
		}

		if rule.Mode != models.MODE_TEST {
			dstConn, err = tls.Dial("tcp", addr, cfg)
			if err != nil {
				utils.LogError(logger, err, "failed to dial the conn to destination server", zap.Uint32("proxy port", p.Port), zap.String("server address", dstAddr))
				return err
			}

			conn := dstConn.(*tls.Conn)
			state := conn.ConnectionState()

			p.logger.Debug("Negotiated protocol:", zap.String("protocol", state.NegotiatedProtocol))
		}

		dstCfg.TLSCfg = cfg
		dstCfg.Addr = addr
	} else {
		// Only dial if dstConn not already set (e.g., from PostgreSQL SSL handling)
		if rule.Mode != models.MODE_TEST && dstConn == nil {
			dstConn, err = net.Dial("tcp", dstAddr)
			if err != nil {
				utils.LogError(logger, err, "failed to dial the conn to destination server", zap.Uint32("proxy port", p.Port), zap.String("server address", dstAddr))
				return err
			}
		}
		dstCfg.Addr = dstAddr
	}

	outgoingOpts.DstCfg = dstCfg
	// get the mock manager for the current app
	m := p.getMockManager()
	if m == nil {
		utils.LogError(logger, err, "failed to fetch the mock manager")
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

		p.logger.Debug("Checking for the parser", zap.String("ParserType", string(parserPair.ParserType)))
		if parser.MatchType(parserCtx, initialBuf) {
			matchedParser = parser
			parserType = parserPair.ParserType
			generic = false
			break
		}
	}

	if !generic {
		p.logger.Debug("The external dependency is supported. Hence using the parser", zap.String("ParserType", string(parserType)))
		switch rule.Mode {
		case models.MODE_RECORD:
			err := matchedParser.RecordOutgoing(parserCtx, srcConn, dstConn, rule.MC, outgoingOpts)
			if err != nil {
				if isNetworkClosedErr(err) {
					p.logger.Debug("failed to record the outgoing message (connection closed)", zap.Error(err))
				} else {
					utils.LogError(logger, err, "failed to record the outgoing message")
				}
				return err
			}
			p.logger.Debug("successfully recorded outgoing message", zap.String("ParserType", string(parserType)))
		case models.MODE_TEST:
			err := matchedParser.MockOutgoing(parserCtx, srcConn, dstCfg, m, outgoingOpts)
			if err != nil && err != io.EOF && !errors.Is(err, context.Canceled) && !isNetworkClosedErr(err) {
				utils.LogError(logger, err, "failed to mock the outgoing message")
				// Send specific error type to error channel for external monitoring
				p.sendMockNotFoundError(err)
				return err
			}
		}
	}

	if generic {
		logger.Debug("The external dependency is not supported. Hence using generic parser")
		if rule.Mode == models.MODE_RECORD {
			err := p.Integrations[integrations.GENERIC].RecordOutgoing(parserCtx, srcConn, dstConn, rule.MC, outgoingOpts)
			if err != nil && err != io.EOF && !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "tls: user canceled") {
				utils.LogError(logger, err, "failed to record the outgoing message")
				return err
			}
		} else {
			err := p.Integrations[integrations.GENERIC].MockOutgoing(parserCtx, srcConn, dstCfg, m, outgoingOpts)
			if err != nil && err != io.EOF && !errors.Is(err, context.Canceled) && !isNetworkClosedErr(err) {
				utils.LogError(logger, err, "failed to mock the outgoing message")
				// Send specific error type to error channel for external monitoring
				p.sendMockNotFoundError(err)
				return err
			}
		}
	}
	return nil
}

func (p *Proxy) StopProxyServer(ctx context.Context) {
	<-ctx.Done()

	p.logger.Info("stopping proxy server...")
	var cleanupErrors []error
	p.connMutex.Lock()
	for _, clientConn := range p.clientConnections {
		if err := clientConn.Close(); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("failed to close client connection: %w", err))

		}
	}
	p.clientConnections = nil
	p.connMutex.Unlock()
	if p.Listener != nil {
		if err := p.Listener.Close(); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("failed to close listener: %w", err))
		}
		p.Listener = nil
	}
	if p.UDPDNSServer != nil || p.TCPDNSServer != nil {
		if err := p.stopDNSServers(ctx); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("failed to stop DNS servers: %w", err))

		}
	}

	p.CloseErrorChannel()
	if len(cleanupErrors) > 0 {
		for _, err := range cleanupErrors {
			utils.LogError(p.logger, err, "cleanup error in StopProxyServer")
		}
		p.logger.Warn("proxy stopped with cleanup errors", zap.Int("error_count", len(cleanupErrors)))
	} else {
		p.logger.Debug("proxy stopped cleanly...")
	}
}

func (p *Proxy) applyMTLSClientCert(cfg *tls.Config, clientPeerCert *x509.Certificate, tlsPrivateKey string) error {
	if cfg == nil || clientPeerCert == nil {
		return nil
	}

	if tlsPrivateKey == "" {
		err := errors.New("failed to read private key from outgoing options")
		utils.LogError(p.logger, err, "failed to apply mTLS client cert")
		return err
	}

	keyBytes := []byte(tlsPrivateKey)
	block, _ := pem.Decode(keyBytes)
	if block == nil {
		err := errors.New("failed to decode PEM block containing private key")
		utils.LogError(p.logger, err, "failed to apply mTLS client cert")
		return err
	}

	var privKey any
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		privKey = key
	} else if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		privKey = key
	} else if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		privKey = key
	}

	if privKey == nil {
		err := errors.New("failed to parse private key from PEM")
		utils.LogError(p.logger, err, "failed to apply mTLS client cert")
		return err
	}

	cfg.Certificates = []tls.Certificate{{
		Certificate: [][]byte{clientPeerCert.Raw},
		PrivateKey:  privKey,
	}}
	p.logger.Debug("Successfully constructed mTLS certificate using client peer cert and local key")
	return nil
}

func (p *Proxy) Record(_ context.Context, mocks chan<- *models.Mock, opts models.OutgoingOptions) error {
	// Reset graceful shutdown flag for a new recording session.
	p.isGracefulShutdown.Store(false)
	// Reset DNS mock deduplication tracker for fresh recording
	p.ResetRecordedDNSMocks()
	p.setSession(&agent.Session{
		Mode:            models.MODE_RECORD,
		MC:              mocks,
		OutgoingOptions: opts,
	})
	p.setMockManager(NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), p.logger))

	return nil
}

func (p *Proxy) Mock(_ context.Context, opts models.OutgoingOptions) error {
	// Reset graceful shutdown flag for a new mocking session.
	p.isGracefulShutdown.Store(false)
	p.setSession(&agent.Session{
		Mode:            models.MODE_TEST,
		OutgoingOptions: opts,
	})
	p.setMockManager(NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), p.logger))

	if !opts.Mocking {
		p.logger.Info("🔀 Mocking is disabled, the response will be fetched from the actual service")
	}

	p.nsSwitchMutex.Lock()
	defer p.nsSwitchMutex.Unlock()
	if string(p.nsswitchData) == "" {
		// setup the nsswitch config to redirect the DNS queries to the proxy
		err := p.setupNsswitchConfig()
		if err != nil {
			utils.LogError(p.logger, err, "failed to setup nsswitch config")
			return errors.New("failed to mock the outgoing message")
		}
	}
	return nil
}

func (p *Proxy) SetMocks(_ context.Context, filtered []*models.Mock, unFiltered []*models.Mock) error {
	if m := p.getMockManager(); m != nil {
		m.SetFilteredMocks(filtered)
		m.SetUnFilteredMocks(unFiltered)
		p.dnsCache.Purge()
	}

	return nil
}

// GetConsumedMocks returns the consumed filtered mocks.
func (p *Proxy) GetConsumedMocks(_ context.Context) ([]models.MockState, error) {
	m := p.getMockManager()
	if m == nil {
		return nil, fmt.Errorf("mock manager not found to get consumed filtered mocks")
	}
	return m.GetConsumedMocks(), nil
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

// sendMockNotFoundError builds a ParserError from a mock-miss error,
// extracting the MismatchReport if the error carries one.
func (p *Proxy) sendMockNotFoundError(err error) {
	proxyErr := models.ParserError{
		ParserErrorType: models.ErrMockNotFound,
		Err:             err,
	}
	// Extract diff report from the error chain if available.
	// Use errors.As to traverse wrapped errors.
	type mismatchReporter interface {
		MismatchReport() *models.MockMismatchReport
	}
	var reporter mismatchReporter
	if errors.As(err, &reporter) && reporter != nil {
		proxyErr.MismatchReport = reporter.MismatchReport()
	}
	p.SendError(proxyErr)
}

// CloseErrorChannel closes the error channel
func (p *Proxy) CloseErrorChannel() {
	close(p.errChannel)
}

func (p *Proxy) Mapping(ctx context.Context, mappingCh chan models.TestMockMapping) {
	mgr := syncMock.Get()
	if mgr != nil {
		mgr.SetMappingChannel(mappingCh)
	}
}

func isShutdownError(err error) bool {
	if errors.Is(err, context.Canceled) {
		return true
	}
	if errors.Is(err, net.ErrClosed) {
		return true
	}
	errStr := err.Error()
	if strings.Contains(errStr, "use of closed network connection") {
		return true
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) {
		return true
	}
	if strings.Contains(errStr, "connection reset by peer") {
		return true
	}
	// Windows-specific error patterns for connection close during shutdown
	if strings.Contains(errStr, "wsarecv") || strings.Contains(errStr, "wsasend") ||
		strings.Contains(errStr, "forcibly closed by the remote host") {
		return true
	}
	return false
}
