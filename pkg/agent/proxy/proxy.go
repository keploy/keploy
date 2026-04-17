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

	// activeTestErrors accumulates mock-not-found errors during active test execution.
	// The continuous error drain goroutine routes errors here when non-nil,
	// and discards errors when nil (no test running). This prevents the
	// errChannel from filling up with background noise (OTel, health checks).
	activeTestErrors atomic.Pointer[testErrorAccumulator]
	errDrainOnce     sync.Once
	// session holds the single active session for this proxy.
	// Previously this was a sync.Map keyed by appID (always 0), which is
	// no longer needed since the proxy serves a single client-app.
	sessionMu   sync.RWMutex
	session     *agent.Session
	mockManager *MockManager

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

	// skipListener disables the TCP accept loop. When true, the proxy
	// does not bind a port. DNS, parser init, and session state still run.
	skipListener bool
}

// SetSkipListener disables the TCP accept loop.
func (p *Proxy) SetSkipListener(skip bool) {
	p.skipListener = skip
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

// isPostgresSSLRequest reports whether the first 8 bytes of a connection
// are a Postgres SSLRequest packet:
//
//	int32 length = 8
//	int32 code   = 80877103 (0x04D2162F)
//
// This is sent by libpq / pgx / pq when `sslmode` is prefer/require/
// verify-ca/verify-full, BEFORE the TLS handshake begins. The server is
// expected to reply with a single byte: 'S' (accept TLS) or 'N' (refuse).
func isPostgresSSLRequest(b []byte) bool {
	if len(b) < 8 {
		return false
	}
	return b[0] == 0x00 && b[1] == 0x00 && b[2] == 0x00 && b[3] == 0x08 &&
		b[4] == 0x04 && b[5] == 0xd2 && b[6] == 0x16 && b[7] == 0x2f
}

// isPostgresSSLRequestPrefix reports whether the first 5 bytes of a
// connection match the Postgres SSLRequest packet's prefix (length=8
// + first byte of the 80877103 code). This is the widest prefix we
// can detect after a 5-byte Peek — used to decide whether it's worth
// blocking on a second Peek(8) to verify the full signature. The
// prefix 00 00 00 08 04 does not collide with TLS (0x16 ...), any
// printable-ASCII protocol greeting, or any PG protocol message
// other than SSLRequest, so this check is effectively exact.
func isPostgresSSLRequestPrefix(b []byte) bool {
	if len(b) < 5 {
		return false
	}
	return b[0] == 0x00 && b[1] == 0x00 && b[2] == 0x00 && b[3] == 0x08 && b[4] == 0x04
}

func New(logger *zap.Logger, info agent.DestInfo, opts *config.Config) *Proxy {
	proxy := &Proxy{
		logger:            logger,
		Port:              opts.ProxyPort,
		DNSPort:           opts.DNSPort, // default: 26789
		synchronous:       opts.Agent.Synchronous,
		IP4:               "127.0.0.1", // default: "127.0.0.1" <-> (2130706433)
		IP6:               "::1",       //default: "::1" <-> ([4]uint32{0000, 0000, 0000, 0001})
		ipMutex:           &sync.Mutex{},
		connMutex:         &sync.Mutex{},
		DestInfo:          info,
		clientClose:       make(chan bool, 1),
		Integrations:      make(map[integrations.IntegrationType]integrations.Integrations),
		GlobalPassthrough: opts.Agent.GlobalPassthrough,
		errChannel:        make(chan error, 100), // buffered channel to prevent blocking
		IsDocker:          opts.Agent.IsDocker,
		dnsCache:          newDNSCache(),
		recordedDNSMocks:  newRecordedDNSMocksCache(),
	}

	return proxy
}

// buildRecordSession constructs a RecordSession for a parser in record mode.
// It wraps src/dst in SafeConn. The caller (handleConnection) is responsible
// for creating the TLSUpgrader using pointers to its own srcConn/dstConn
// variables, so the upgrader updates the correct references on TLS upgrade.
func (p *Proxy) buildRecordSession(
	srcConn, dstConn net.Conn,
	mocks chan<- *models.Mock,
	errGrp *errgroup.Group,
	logger *zap.Logger,
	clientConnID, destConnID int64,
	opts models.OutgoingOptions,
	tlsUpgrader models.TLSUpgrader,
) *integrations.RecordSession {
	return &integrations.RecordSession{
		Ingress:      util.NewSafeConnWithReader(srcConn, srcConn, logger),
		Egress:       util.NewSafeConn(dstConn, logger),
		Mocks:        mocks,
		ErrGroup:     errGrp,
		TLSUpgrader:  tlsUpgrader,
		Logger:       logger,
		ClientConnID: fmt.Sprint(clientConnID),
		DestConnID:   fmt.Sprint(destConnID),
		Opts:         opts,
	}
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
//
// Swaps the new manager in while holding sessionMu, then closes the
// PREVIOUS manager after releasing the lock — Close() must not run under
// sessionMu because Close() synchronises with its own internal workers.
// Closing the outgoing manager stops its background idle-sweeper
// goroutine; without this, every Record / Mock session would leak a
// goroutine since MockManager owns a per-instance ticker that only
// stops on Close().
func (p *Proxy) setMockManager(m *MockManager) {
	p.sessionMu.Lock()
	prev := p.mockManager
	p.mockManager = m
	p.sessionMu.Unlock()
	if prev != nil {
		prev.Close()
	}
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

	// Skip the TCP listener if configured. DNS + parsers + session still run.
	if agent.SkipProxyListener {
		p.skipListener = true
	}

	//first initialize the integrations
	err := p.InitIntegrations(ctx)
	if err != nil {
		utils.LogError(p.logger, err, "failed to initialize the integrations")
		return err
	}

	// Start the continuous error drain so the error channel never fills up.
	// This must happen before any connections are handled.
	p.StartErrorDrain(ctx)

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

	if p.skipListener {
		p.logger.Info("Proxy TCP listener intentionally not bound (SkipProxyListener is set); DNS and session services are live but no port is accepting connections")
	} else {
		p.logger.Info(fmt.Sprintf("Proxy started at port:%v", p.Port))
	}
	return nil
}

// start function starts the proxy server on the idle local port.
// When skipListener is true, no TCP listener is created.
// The function blocks on ctx.Done and handles cleanup on shutdown.
func (p *Proxy) start(ctx context.Context, readyChan chan<- error) error {

	if p.skipListener {
		// Info-level notice is emitted once in StartProxy (near the
		// DNS-control log, which is the conventional "startup banner"
		// spot). Here we only want a debug trace for the start()
		// internal entry point so logs aren't duplicated.
		p.logger.Debug("Proxy TCP listener skipped; DNS and session services active")
		readyChan <- nil
		// Block until context is cancelled, then run cleanup.
		<-ctx.Done()
		p.sessionMu.RLock()
		if p.session != nil && p.session.MC != nil {
			close(p.session.MC)
		}
		p.sessionMu.RUnlock()
		p.nsSwitchMutex.Lock()
		if string(p.nsswitchData) != "" {
			if err := p.resetNsSwitchConfig(); err != nil {
				utils.LogError(p.logger, err, "failed to reset the nsswitch config")
			}
		}
		p.nsSwitchMutex.Unlock()
		p.logger.Debug("proxy (skipListener) stopped")
		return nil
	}

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

		// Close the listener synchronously BEFORE waiting for connection
		// handlers. Defers are LIFO, so without this the listener-close
		// defer (func1) runs AFTER this defer (func2). The accept
		// goroutine blocks on listener.Accept() — if we wait for
		// clientConnErrGrp before closing the listener, the accept
		// goroutine never exits, and on 2-CPU CI runners the scheduler
		// may not run other unblocking goroutines promptly, causing a hang.
		if listener != nil {
			listener.Close()
		}

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

	// This is used to handle the parser errors
	parserErrGrp, parserCtx := errgroup.WithContext(ctx)
	parserCtx = context.WithValue(parserCtx, models.ErrGroupKey, parserErrGrp)
	parserCtx = context.WithValue(parserCtx, models.ClientConnectionIDKey, fmt.Sprint(clientConnID))
	parserCtx = context.WithValue(parserCtx, models.DestConnectionIDKey, fmt.Sprint(destConnID))
	parserCtx, parserCtxCancel := context.WithCancel(parserCtx)

	// Capture the initial srcConn before it gets reassigned (TLS upgrade, wrapping).
	initialSrcConn := srcConn

	// connCloser is started later (after dstConn is established) to close
	// both connections on context cancellation, unblocking any goroutine
	// stuck in a blocking read (ReadBytes → reader.Read).
	var connCloserStarted bool
	startConnCloser := func() {
		if connCloserStarted {
			return
		}
		connCloserStarted = true
		// Capture dstConn NOW (after dial) so there's no data race.
		closeDst := dstConn
		go func() {
			<-parserCtx.Done()
			initialSrcConn.Close()
			if closeDst != nil {
				closeDst.Close()
			}
		}()
	}

	defer func() {
		parserCtxCancel()

		if srcConn != nil {
			err := srcConn.Close()
			if err != nil {
				if !strings.Contains(err.Error(), "use of closed network connection") {
					utils.LogError(p.logger, err, "failed to close the source connection", zap.Any("clientConnID", clientConnID))
				}
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

			mysqlLogger := p.logger.With(
				zap.String("Client ConnectionID", fmt.Sprint(clientConnID)),
				zap.String("Destination ConnectionID", fmt.Sprint(destConnID)),
				zap.String("Destination Address", dstAddr),
			)
			mysqlSession := &integrations.RecordSession{
				Ingress:      util.NewSafeConn(srcConn, mysqlLogger),
				Egress:       util.NewSafeConn(dstConn, mysqlLogger),
				Mocks:        rule.MC,
				ErrGroup:     parserErrGrp,
				TLSUpgrader:  util.NewConnTLSUpgrader(&srcConn, &dstConn, p.logger, pTls.HandleTLSConnection),
				Logger:       mysqlLogger,
				ClientConnID: fmt.Sprint(clientConnID),
				DestConnID:   fmt.Sprint(destConnID),
				Opts:         outgoingOpts,
			}

			// Record the outgoing message into a mock
			err := p.Integrations[integrations.MYSQL].RecordOutgoing(parserCtx, mysqlSession)
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
	// Peek 5 bytes first — exactly what the TLS record header needs,
	// and the widest Peek we can do without risking a deadlock on
	// short plaintext greetings (e.g. Redis 'PING\r\n' is 6 bytes and
	// some protocols send <8 bytes then wait for a server reply).
	testBuffer, err := reader.Peek(5)
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

	// ── Postgres SSLRequest handshake (opt-in) ──
	// When agent.InterceptPostgresSSLRequest is enabled, the proxy itself
	// replies 'S' and upgrades to TLS. Disabled by default because the
	// default build registers a Postgres parser (via extraparsers.go)
	// that already handles SSLRequest through the TLSUpgrader interface;
	// double-handling breaks the parser-driven flow. Downstream builds
	// that ship without a Postgres parser (pure proxy-mode) flip the
	// flag on via agent.SetInterceptPostgresSSLRequest.
	//
	// Client-side only: this block terminates TLS with the client but
	// does NOT forward an SSLRequest to the upstream Postgres server.
	// See the runtime_hooks.go docstring on InterceptPostgresSSLRequest
	// for the supported deployment shapes (downstream builds that do
	// not re-originate the upstream connection, or upstreams accepting
	// direct TLS). End-to-end MITM against a vanilla Postgres still
	// goes through the parser-driven TLSUpgrader path.
	//
	// We only extend the Peek to 8 bytes when the first 5 bytes match
	// the SSLRequest prefix (`00 00 00 08 04`). That prefix is unique
	// enough to rule out TLS (0x16 ...), CONNECT ("CONN"), and other
	// common greetings, so the extra 3-byte wait is deterministic for
	// exactly the one client that's actually speaking SSLRequest.
	// This keeps the short-greeting deadlock (Redis PING, memcached
	// 'quit', etc.) off the default code path.
	if agent.InterceptPostgresSSLRequest && isPostgresSSLRequestPrefix(testBuffer) {
		sslBuf, perr := reader.Peek(8)
		if perr != nil {
			utils.LogError(p.logger, perr, "client started with a Postgres SSLRequest prefix (00 00 00 08 04) but did not deliver the full 8-byte packet; the connection likely dropped mid-handshake — verify the client is a standard libpq/pgx/pq and check for a mismatched sslmode or an intermediary terminating the TCP stream",
				zap.Uint32("sourcePort", uint32(sourcePort)),
				zap.String("dstAddr", dstAddr))
			return perr
		}
		testBuffer = sslBuf
	}
	if agent.InterceptPostgresSSLRequest && isPostgresSSLRequest(testBuffer) {
		p.logger.Debug("Postgres SSLRequest detected, accepting and upgrading to TLS",
			zap.Int("sourcePort", sourcePort),
			zap.String("dstAddr", dstAddr),
		)
		// Consume the 8-byte SSLRequest from the buffered reader so the
		// downstream parser never sees it.
		if _, derr := reader.Discard(8); derr != nil {
			utils.LogError(p.logger, derr, "failed to discard Postgres SSLRequest bytes; the 8-byte SSLRequest was peeked but the bufio reader could not be advanced. This usually means the underlying TCP connection was reset between peek and discard — retry the client connection, and if it persists capture a packet trace between the client and the proxy listener",
				zap.Uint32("sourcePort", uint32(sourcePort)),
				zap.String("dstAddr", dstAddr))
			return derr
		}
		// Reply 'S' (TLS accepted). Write straight to the underlying TCP
		// connection; writes bypass the buffered reader.
		if _, werr := srcConn.Write([]byte{'S'}); werr != nil {
			utils.LogError(p.logger, werr, "failed to write 'S' SSLResponse to Postgres client; the proxy accepted the SSLRequest but could not send the one-byte acknowledgment. Verify the client is still connected (not a half-closed stream), check for client-side read timeouts shorter than the proxy's accept latency, and confirm no firewall is dropping 1-byte segments",
				zap.Uint32("sourcePort", uint32(sourcePort)),
				zap.String("dstAddr", dstAddr))
			return werr
		}
		// Re-peek for the TLS ClientHello. The client app sends it as soon
		// as it gets 'S'. 5 bytes is enough for IsTLSHandshake().
		testBuffer, err = reader.Peek(5)
		if err != nil {
			if err == io.EOF && len(testBuffer) == 0 {
				p.logger.Debug("Postgres client closed conn after SSLRequest reply")
				return nil
			}
			// Any non-nil error here means we could not read a complete
			// 5-byte TLS record header — either the client sent fewer
			// than 5 bytes before closing (EOF with partial buffer) or
			// the network errored out. Falling through would leave the
			// downstream TLS detection running against a truncated
			// prefix and misclassify the stream as plaintext, so bail.
			utils.LogError(p.logger, err, "failed to read a complete 5-byte TLS record header after replying 'S' to the Postgres SSLRequest; the client did not deliver enough bytes to identify a TLS ClientHello. Check the client's sslmode (require/verify-* should always follow with ClientHello after 'S'), confirm InterceptPostgresSSLRequest is only enabled in pure-proxy builds without a Postgres parser, and capture the bytes sent immediately after 'S' to verify a full TLS record header arrives (starting with 0x16)",
				zap.Uint32("sourcePort", uint32(sourcePort)),
				zap.String("dstAddr", dstAddr),
				zap.Int("bytesRead", len(testBuffer)))
			return err
		}
	}

	srcConn = &util.Conn{
		Conn:   srcConn,
		Reader: reader,
		Logger: p.logger,
	}

	// ── CONNECT tunnel handling ──
	// If the first bytes are an HTTP CONNECT request (app is using a corporate
	// HTTP proxy), we handle the CONNECT handshake here, then re-enter the
	// normal TLS MITM + parser flow on the unwrapped tunnel.
	//
	// Record mode: forward CONNECT to the real proxy, relay the 200, then MITM.
	// Test mode:   respond with 200 directly (no external proxy needed).
	var connectResult *connectTunnelResult
	if isConnectRequest(testBuffer) {
		p.logger.Debug("Detected HTTP CONNECT request, handling tunnel",
			zap.Int("sourcePort", sourcePort),
			zap.String("dstAddr", dstAddr),
		)

		isTestMode := rule.Mode == models.MODE_TEST

		// In record mode, we need a connection to the corporate proxy.
		var proxyConn net.Conn
		if !isTestMode {
			proxyConn, err = net.Dial("tcp", dstAddr)
			if err != nil {
				utils.LogError(p.logger, err, "failed to dial corporate proxy for CONNECT; verify the proxy address is correct, DNS/network is reachable, and HTTP_PROXY/HTTPS_PROXY settings are configured correctly",
					zap.String("proxy_addr", dstAddr))
				return err
			}
		}

		connectResult, err = handleConnectTunnel(p.logger, srcConn, proxyConn, isTestMode)
		if err != nil {
			// Close proxyConn on error to avoid leaking the TCP connection,
			// since it won't be assigned to dstConn for deferred cleanup.
			if proxyConn != nil {
				proxyConn.Close()
			}
			utils.LogError(p.logger, err, "failed to handle CONNECT tunnel; check HTTP_PROXY/HTTPS_PROXY settings, proxy authentication (407), DNS/network reachability to the proxy, and egress firewall rules")
			return err
		}

		// The CONNECT handshake is now complete. The app will send the next
		// bytes (typically a TLS ClientHello) over the same srcConn.
		// We need to re-peek to detect TLS on the inner connection.
		//
		// Also update dstAddr and destInfo to reflect the real target
		// (e.g., api.example.com:443) instead of the corporate proxy.
		dstAddr = connectResult.TargetAddr
		destInfo.Port = 443 // CONNECT targets are almost always TLS on 443
		if connectResult.TargetPort != "" {
			if portNum, err := strconv.ParseUint(connectResult.TargetPort, 10, 16); err == nil && portNum >= 1 && portNum <= 65535 {
				destInfo.Port = uint32(portNum)
			}
		}

		// Re-peek the next bytes to detect TLS on the inner tunnel.
		// Use the BufferedReader from handleConnectTunnel to preserve any
		// bytes it read ahead (e.g., TLS ClientHello pipelined by the client).
		innerReader := connectResult.BufferedReader
		testBuffer, err = innerReader.Peek(5)
		if err != nil {
			if err == io.EOF && len(testBuffer) == 0 {
				p.logger.Debug("CONNECT tunnel closed immediately after handshake")
				return nil
			}
			if err != io.EOF {
				utils.LogError(p.logger, err, "failed to peek inner tunnel data after CONNECT")
				return err
			}
			// Partial read with EOF — proceed with what we have.
		}

		// Wrap the raw TCP connection with innerReader for subsequent reads.
		// innerReader is the bufio.Reader from handleConnectTunnel that may
		// hold pipelined bytes (TLS ClientHello) in its internal buffer.
		srcConn = &util.Conn{
			Conn:   stripUtilConn(srcConn),
			Reader: innerReader,
			Logger: p.logger,
		}

		// In record mode, dstConn is now the tunneled connection through the
		// corporate proxy (the CONNECT tunnel is established, raw bytes flow).
		// We'll set dstConn = proxyConn so the TLS dial below wraps it.
		if !isTestMode {
			dstConn = proxyConn
		}

		p.logger.Debug("CONNECT tunnel established, proceeding with inner connection",
			zap.String("target", connectResult.TargetAddr),
			zap.Bool("innerTLS", pTls.IsTLSHandshake(testBuffer)),
		)
	}

	var clientPeerCert *x509.Certificate
	var isMTLS bool
	isTLS := pTls.IsTLSHandshake(testBuffer)
	if isTLS {
		srcConn, isMTLS, err = pTls.HandleTLSConnection(ctx, p.logger, srcConn, rule.Backdate)
		if err != nil {
			utils.LogError(p.logger, err, "failed to handle TLS conn")
			return err
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

	logger := p.logger.With(zap.String("Client ConnectionID", clientID), zap.String("Destination ConnectionID", destID), zap.String("Destination Address", dstAddr), zap.String("Client IP Address", srcConn.RemoteAddr().String()))

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
		Reader: util.NewPrefixReader(initialBuf, srcConn),
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

		serverName := dstURL
		// If SNI was not captured (e.g., client omitted it after CONNECT),
		// fall back to the CONNECT target hostname for the TLS handshake.
		// Skip IP literals — Go's TLS uses IP SANs, not ServerName for those.
		if serverName == "" && connectResult != nil && net.ParseIP(connectResult.TargetHost) == nil {
			serverName = connectResult.TargetHost
		}

		cfg := &tls.Config{
			InsecureSkipVerify: true,
			ServerName:         serverName,
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
			addr = net.JoinHostPort(dstURL, fmt.Sprint(destInfo.Port))
		}

		if rule.Mode != models.MODE_TEST {
			if connectResult != nil && dstConn != nil {
				// CONNECT tunnel: dstConn is already connected through the
				// corporate proxy tunnel. If the proxyReader has buffered
				// bytes beyond the 200 response, wrap dstConn to preserve them.
				var tlsTransport net.Conn = dstConn
				if connectResult.DstReader != nil && connectResult.DstReader.Buffered() > 0 {
					tlsTransport = &util.Conn{
						Conn:   dstConn,
						Reader: connectResult.DstReader,
						Logger: p.logger,
					}
				}
				tlsConn := tls.Client(tlsTransport, cfg)
				if err := tlsConn.Handshake(); err != nil {
					utils.LogError(logger, err, "failed TLS handshake over CONNECT tunnel; verify the corporate proxy allows CONNECT to this target, check proxy auth/egress rules, and confirm the target hostname is reachable",
						zap.String("target", addr))
					return err
				}
				dstConn = tlsConn
				logger.Debug("TLS over CONNECT tunnel established",
					zap.String("protocol", tlsConn.ConnectionState().NegotiatedProtocol),
					zap.String("target", addr))
			} else {
				dstConn, err = tls.Dial("tcp", addr, cfg)
				if err != nil {
					utils.LogError(logger, err, "failed to dial the conn to destination server", zap.Uint32("proxy port", p.Port), zap.String("server address", addr))
					return err
				}

				conn := dstConn.(*tls.Conn)
				state := conn.ConnectionState()

				p.logger.Debug("Negotiated protocol:", zap.String("protocol", state.NegotiatedProtocol))
			}
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

	// Start the shutdown goroutine now that dstConn is established.
	startConnCloser()

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
			// Create TLSUpgrader in handleConnection scope so it holds pointers to
			// the real srcConn/dstConn variables (not copies in buildRecordSession).
			var upgrader models.TLSUpgrader
			if parserType == integrations.MYSQL || parserType == integrations.POSTGRES_V2 {
				upgrader = util.NewConnTLSUpgrader(&srcConn, &dstConn, p.logger, pTls.HandleTLSConnection)
			}
			session := p.buildRecordSession(srcConn, dstConn, rule.MC, parserErrGrp, logger, clientConnID, destConnID, outgoingOpts, upgrader)
			err := matchedParser.RecordOutgoing(parserCtx, session)
			if err != nil {
				if isNetworkClosedErr(err) {
					logger.Debug("failed to record the outgoing message (connection closed)", zap.Error(err))
				} else {
					utils.LogError(logger, err, "failed to record the outgoing message")
				}
				return err
			}
			logger.Debug("successfully recorded outgoing message", zap.String("ParserType", string(parserType)))
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
			genericSession := p.buildRecordSession(srcConn, dstConn, rule.MC, parserErrGrp, logger, clientConnID, destConnID, outgoingOpts, nil)
			err := p.Integrations[integrations.GENERIC].RecordOutgoing(parserCtx, genericSession)
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

// SetMocksWithWindow atomically updates mocks AND the test window in a
// single call so concurrent readers cannot observe a torn (newMocks,
// oldWindow) view. Used to satisfy the WindowedProxy extension interface.
func (p *Proxy) SetMocksWithWindow(_ context.Context, filtered, unFiltered []*models.Mock, start, end time.Time) error {
	if m := p.getMockManager(); m != nil {
		m.SetMocksWithWindow(filtered, unFiltered, start, end)
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

// testErrorAccumulator collects errors during an active test case.
// It is goroutine-safe via an internal mutex.
type testErrorAccumulator struct {
	mu   sync.Mutex
	errs []error
}

func (a *testErrorAccumulator) add(err error) {
	a.mu.Lock()
	a.errs = append(a.errs, err)
	a.mu.Unlock()
}

func (a *testErrorAccumulator) drain() []error {
	a.mu.Lock()
	e := a.errs
	a.errs = nil
	a.mu.Unlock()
	return e
}

// StartErrorDrain launches a background goroutine that continuously reads from
// errChannel so it never fills up. Errors are routed to the activeTestErrors
// accumulator when a test is running, and discarded otherwise.
// This prevents background services (OTel, health checks) from saturating the
// 100-slot error channel and blocking test coordination.
func (p *Proxy) StartErrorDrain(ctx context.Context) {
	p.errDrainOnce.Do(func() {
		var discarded atomic.Int64
		go func() {
			defer utils.Recover(p.logger)
			for {
				select {
				case <-ctx.Done():
					return
				case err, ok := <-p.errChannel:
					if !ok {
						return
					}
					if acc := p.activeTestErrors.Load(); acc != nil {
						acc.add(err)
					} else {
						// Log only the first discard and then every 100th to reduce noise.
						n := discarded.Add(1)
						if n == 1 || n%100 == 0 {
							p.logger.Debug("discarding mock error outside active test",
								zap.Error(err), zap.Int64("totalDiscarded", n))
						}
					}
				}
			}
		}()
	})
}

// BeginTestErrorCapture starts collecting errors for the current test case.
// Call EndTestErrorCapture when the test finishes to retrieve collected errors.
func (p *Proxy) BeginTestErrorCapture() {
	p.activeTestErrors.Store(&testErrorAccumulator{})
}

// EndTestErrorCapture stops collecting errors and returns all accumulated errors.
func (p *Proxy) EndTestErrorCapture() []error {
	if acc := p.activeTestErrors.Swap(nil); acc != nil {
		return acc.drain()
	}
	return nil
}

// GetErrorChannel returns the error channel for external monitoring.
// When StartErrorDrain is active, this channel is continuously drained by the
// background goroutine. Direct consumers (monitorProxyErrors) will compete
// for reads. Prefer using BeginTestErrorCapture/EndTestErrorCapture instead.
func (p *Proxy) GetErrorChannel() <-chan error {
	return p.errChannel
}

// GetMockErrors drains all mock-not-found errors and returns them.
// When StartErrorDrain is active, it reads from the test accumulator instead
// of the channel (which is drained by the background goroutine).
func (p *Proxy) GetMockErrors(_ context.Context) ([]models.UnmatchedCall, error) {
	var rawErrs []error

	// Prefer test accumulator if active (StartErrorDrain is running)
	if acc := p.activeTestErrors.Load(); acc != nil {
		rawErrs = acc.drain()
	} else {
		// Fallback: drain from channel directly (legacy path)
	drainLoop:
		for {
			select {
			case err, ok := <-p.errChannel:
				if !ok {
					break drainLoop
				}
				rawErrs = append(rawErrs, err)
			default:
				break drainLoop
			}
		}
	}

	var errs []models.UnmatchedCall
	for _, err := range rawErrs {
		if parserErr, ok := err.(models.ParserError); ok && parserErr.ParserErrorType == models.ErrMockNotFound {
			if parserErr.MismatchReport != nil {
				errs = append(errs, models.UnmatchedCall{
					Protocol:      parserErr.MismatchReport.Protocol,
					ActualSummary: parserErr.MismatchReport.ActualSummary,
					ClosestMock:   parserErr.MismatchReport.ClosestMock,
					Diff:          parserErr.MismatchReport.Diff,
					NextSteps:     parserErr.MismatchReport.NextSteps,
				})
			}
		}
	}
	return errs, nil
}

// SendError sends an error to the error channel for external monitoring.
func (p *Proxy) SendError(err error) {
	select {
	case p.errChannel <- err:
	default:
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
