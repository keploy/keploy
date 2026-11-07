package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	expirable "github.com/hashicorp/golang-lru/v2/expirable"
	"github.com/miekg/dns"
	"go.keploy.io/server/v3/pkg/agent"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/plaintcp"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// dnsServer is one of the proxy's two DNS listeners, TCP or UDP, with the one
// rule for stopping it (stop).
type dnsServer struct {
	srv    *dns.Server
	logger *zap.Logger

	// settled is closed once srv serves on its socket, or once its serve call
	// has returned without serving. serving says which, and err why a serve
	// call that did not serve returned. Both are written before settled is
	// closed and read only after it is.
	settled chan struct{}
	settle  sync.Once
	serving bool
	err     error

	stopOnce sync.Once
	stopErr  error
}

// newDNSServer builds the DNS server for network ("tcp" or "udp") on
// p.DNSPort. It serves (serve) on the socket it is handed bound, in
// srv.Listener (TCP) or srv.PacketConn (UDP): startDNSServers binds both.
func (p *Proxy) newDNSServer(network string) *dnsServer {
	s := &dnsServer{logger: p.logger, settled: make(chan struct{})}
	s.srv = &dns.Server{
		Addr:    fmt.Sprintf(":%v", p.DNSPort),
		Net:     network,
		Handler: p,
		// miekg/dns calls it just before it serves on the socket, on the
		// goroutine that called ActivateAndServe. A serve call that fails
		// before it serves never calls it.
		NotifyStartedFunc: func() { s.settleAs(true, nil) },
	}
	return s
}

// name is s's network as its log lines and errors say it: TCP or UDP.
func (s *dnsServer) name() string { return strings.ToUpper(s.srv.Net) }

// settleAs settles s once: as serving, or as a serve call that returned with
// err without serving.
func (s *dnsServer) settleAs(serving bool, err error) {
	s.settle.Do(func() {
		s.serving, s.err = serving, err
		close(s.settled)
	})
}

// serve serves s on the socket it was handed bound until s is stopped, and
// returns: nil once stopped, else the error, which it also logs. Return it,
// never swallow it: a DNS listener that died silently looks healthy. The
// server closes its socket as it stops serving; serve closes it if the server
// never started to.
func (s *dnsServer) serve() (err error) {
	// A serve call that served has settled s already. This settles one that
	// returned, or panicked, before it served, so stop never waits on it.
	defer func() { s.settleAs(false, err) }()
	if sock := preBound(s.srv); sock != nil {
		defer func() { _ = sock.Close() }()
	}
	s.logger.Info(fmt.Sprintf("starting %s DNS server at addr %v", s.name(), s.srv.Addr))
	err = s.srv.ActivateAndServe()
	if err != nil {
		utils.LogError(s.logger, err, fmt.Sprintf("the %s DNS server failed to serve or stopped serving", s.name()), zap.String("addr", s.srv.Addr))
	}
	return err
}

// stop shuts s down. It is the one rule for that, followed by both of s's
// stoppers, serveDNS as its context ends and stopDNSServers: wait until s
// serves or its serve call has returned without serving, and shut it down
// only if it serves. miekg/dns refuses to shut down a server that has not
// started yet ("server not started"), and the serve call that came after would
// then hold the port for the life of the process. A serve call that returned
// without serving closed the socket (serve). Only the first call shuts s
// down; later ones return its result.
func (s *dnsServer) stop() error {
	s.stopOnce.Do(func() {
		<-s.settled
		if !s.serving {
			return
		}
		if err := s.srv.Shutdown(); err != nil {
			utils.LogError(s.logger, err, fmt.Sprintf("failed to stop the %s DNS server", s.name()))
			s.stopErr = err
			return
		}
		s.logger.Debug(fmt.Sprintf("%s DNS server stopped", s.name()))
	})
	return s.stopErr
}

// startDNSServers binds the TCP and UDP DNS servers' sockets, serves each on
// g until ctx ends, and returns once both serve: nothing gated on StartProxy,
// notably the agent's readiness that releases the depends_on'd app container
// in docker-compose replay, may see an unbound DNS socket. Without the wait a
// reconstructed replay app's eager boot-time resolve of a recorded name raced
// an unbound socket and died with UnknownHostException, tearing down the
// whole stack (intermittent cloud-replay flake, saas/selfhosted pipeline
// 9847). A failed bind fails the start at once with its error, which names
// the port and what holds it; a serve call that fails before it serves fails
// the wait with its error; dnsBindTimeout is only a backstop for a server
// that neither serves nor fails, one deadline for both servers.
//
// p.tcpDNS and p.udpDNS are set here, before the goroutines that serve and
// stop them start, and each goroutine is handed its server.
func (p *Proxy) startDNSServers(ctx context.Context, g *errgroup.Group) error {
	udpConn, tcpLn, err := p.bindDNS()
	if err != nil {
		return fmt.Errorf("DNS server failed to start: %w", err)
	}
	p.tcpDNS, p.udpDNS = p.newDNSServer("tcp"), p.newDNSServer("udp")
	p.tcpDNS.srv.Listener, p.udpDNS.srv.PacketConn = tcpLn, udpConn
	for _, s := range []*dnsServer{p.tcpDNS, p.udpDNS} {
		g.Go(func() error {
			defer utils.Recover(p.logger)
			return p.serveDNS(ctx, s)
		})
	}

	deadline := time.After(dnsBindTimeout)
	for _, s := range []*dnsServer{p.udpDNS, p.tcpDNS} {
		select {
		case <-s.settled:
			if !s.serving {
				return fmt.Errorf("DNS server failed to start: %w", s.err)
			}
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("%s DNS server did not report listening within %s", s.name(), dnsBindTimeout)
		}
	}
	return nil
}

// bindDNS binds the DNS servers' UDP and TCP sockets on one port: p.DNSPort,
// or, where another socket holds it and the port may move (SetPortMover), a
// free one. They bind it alone (no SO_REUSEPORT): with it, a second agent
// bound the same port beside the first, and the kernel then shared each one's
// DNS queries between the two. The bind is a stage of the agent's start of its
// own, on the calling goroutine (utils.BeginBootWork), so that a bind that
// stalls is named as the one in progress.
func (p *Proxy) bindDNS() (udpConn net.PacketConn, tcpLn net.Listener, err error) {
	defer beginBootWork("the DNS servers' bind")()
	err = p.bindOrMove("DNS server", &p.DNSPort, func(port uint32) error {
		addr := fmt.Sprintf(":%v", port)
		pc, err := net.ListenPacket("udp", addr)
		if err != nil {
			return err
		}
		ln, err := plaintcp.Listen(context.Background(), "tcp", addr)
		if err != nil {
			_ = pc.Close()
			return err
		}
		udpConn, tcpLn = pc, ln
		return nil
	}, func() { _ = udpConn.Close(); _ = tcpLn.Close() })
	return udpConn, tcpLn, err
}

// serveDNS serves s until ctx ends, and then stops it.
func (p *Proxy) serveDNS(ctx context.Context, s *dnsServer) error {
	served := make(chan error, 1)
	go func() {
		defer utils.Recover(p.logger)
		served <- s.serve()
	}()
	select {
	case <-ctx.Done():
		return s.stop()
	case err := <-served:
		return err
	}
}

type dnsCacheEntry struct {
	*dns.Msg

	// true if the response came from upstream resolution or a recorded mock.
	// false if we synthesized defaults (fallback).
	FromUpstream bool
}

const (
	// dnsCacheMaxSize is the maximum number of entries in the DNS cache.
	dnsCacheMaxSize = 256
	// dnsCacheTTL is the default time-to-live for cached DNS entries.
	// Entries expire automatically after this duration.
	dnsCacheTTL = 30 * time.Second

	// recordedDNSMocksMaxSize is the maximum number of questions with a
	// resolved answer in the DNS deduplication tracker.
	recordedDNSMocksMaxSize = 1024
	// recordedDNSNegativesMaxSize is the maximum number of negative answers,
	// NXDOMAIN or NODATA, in the DNS deduplication tracker. An app asks many
	// more names that do not exist than names that do: under k8s ndots:5 an
	// external name is asked under each of four search domains first, so its
	// A and AAAA questions add 8 NXDOMAINs, and its AAAA a NODATA, to its one
	// resolved answer. Four times the resolved bound holds those 9 negative
	// answers of 455 names, near the 512 names (A and AAAA) the one shared
	// tracker of 1024 held before negative answers were recorded.
	recordedDNSNegativesMaxSize = 4 * recordedDNSMocksMaxSize
	// recordedDNSMocksTTL is the TTL for recorded DNS mock entries.
	// Recording sessions typically don't last longer than this.
	recordedDNSMocksTTL = 30 * time.Minute

	// nodataRelayLogMaxSize / nodataRelayLogTTL bound the "relayed NODATA"
	// log-dedupe tracker. Deliberately the same shape and numbers as
	// recordedDNSMocks: identical key space (the generateCacheKey
	// (name, qtype) pair, which is app-controlled and therefore unbounded —
	// per-request hostnames and search-domain permutations mint a fresh key
	// every time) and identical lifetime expectations. Re-logging a line
	// after eviction or expiry is harmless — this tracker only suppresses
	// duplicate log output, it carries no replay state — so the bound has
	// no correctness cost.
	nodataRelayLogMaxSize = 1024
	nodataRelayLogTTL     = 30 * time.Minute
)

// newDNSCache creates a new thread-safe, size-bounded, TTL-expiring DNS cache.
func newDNSCache() *expirable.LRU[string, dnsCacheEntry] {
	return expirable.NewLRU[string, dnsCacheEntry](dnsCacheMaxSize, nil, dnsCacheTTL)
}

// shouldCacheDNSResponse reports whether a DNS resolution result is
// eligible for the per-proxy dnsCache.
//
// Only answers marked FromUpstream are cached, in both record and test
// modes: a resolver's answer, and at replay a recorded mock's answer
// (dnsMockAnswer). Synthetic/fallback responses are NOT cached: caching
// a fallback would pin it for 30 s after transient upstream failures.
//
// Record-mode caching is safe against duplicate DNS-mock emission
// because a cache hit short-circuits before resolveUncachedDNSResponse
// is called — which is the only site that invokes recordDNSMock. As a
// second line of defense recordDNSMock has its own (name, qtype)
// dedupe tracker (p.recordedDNSMocks), so even if the cache were
// bypassed the mock stream would still see each query at most once.
// A 30 s TTL is short enough to pick up DNS record changes during a
// long recording session yet long enough to amortise repeated
// resolutions from client libraries that open one TCP connection per
// request.
func shouldCacheDNSResponse(mode models.Mode, resp dnsCacheEntry) bool {
	if !resp.FromUpstream {
		return false
	}
	return mode == models.MODE_TEST || mode == models.MODE_RECORD
}

// dnsRecordDedupe holds the questions recordDNSMock has recorded an answer of
// each kind (dnsAnswerRank) for, so each is recorded once while it stays here.
// Resolved answers and negative ones, NXDOMAIN and NODATA, are kept in bounded,
// TTL-expiring trackers of their own: the many names an app asks that do not
// exist (search-list expansions) never evict a resolved answer, which would be
// recorded again each time its question leaves the dnsCache.
type dnsRecordDedupe struct {
	resolved *expirable.LRU[string, bool]
	negative *expirable.LRU[string, bool]
}

// newRecordedDNSMocksCache creates the bounded, TTL-expiring trackers for DNS
// mock deduplication.
func newRecordedDNSMocksCache() *dnsRecordDedupe {
	return &dnsRecordDedupe{
		resolved: expirable.NewLRU[string, bool](recordedDNSMocksMaxSize, nil, recordedDNSMocksTTL),
		negative: expirable.NewLRU[string, bool](recordedDNSNegativesMaxSize, nil, recordedDNSMocksTTL),
	}
}

// firstAnswer reports whether question's answer of kind rank is not held yet,
// holding it when so, with the key it is held under. Non-atomic Get-then-Add:
// two concurrent first answers can both record, as before.
func (d *dnsRecordDedupe) firstAnswer(question dns.Question, rank int) (string, bool) {
	tracker, key := d.resolved, generateDNSDedupeKey(question)
	if rank != dnsRankResolved {
		tracker, key = d.negative, key+dnsDedupeSuffix[rank]
	}
	if _, held := tracker.Get(key); held {
		return key, false
	}
	tracker.Add(key, true)
	return key, true
}

// Purge empties both trackers in place (see ResetRecordedDNSMocks).
func (d *dnsRecordDedupe) Purge() {
	d.resolved.Purge()
	d.negative.Purge()
}

// newNodataRelayLogCache creates the bounded, TTL-expiring tracker that dedupes
// the "relayed NODATA" Info log to once per (name, qtype). The unanswered
// forward WARN (logUnansweredForward) is deduped by another of the same shape:
// its key space is the same app-controlled (name, qtype).
func newNodataRelayLogCache() *expirable.LRU[string, bool] {
	return expirable.NewLRU[string, bool](nodataRelayLogMaxSize, nil, nodataRelayLogTTL)
}

// shouldLogNodataRelay reports whether the "relayed NODATA" Info line has not
// yet been emitted for this (name, qtype), recording it as emitted when it
// returns true.
//
// Non-atomic Get-then-Add on purpose: this is a log-dedupe, so the worst a
// concurrent race can do is print the same line twice. It mirrors the
// recordedDNSMocks dedupe a few hundred lines below.
//
// Nil-tolerant because the tracker is one optional field on a large struct: a
// construction path that forgets it should lose deduplication, not take the
// DNS server down mid-resolution. (It also keeps the bare &Proxy{} literals in
// this package's tests exercising the surrounding DNS logic.)
func (p *Proxy) shouldLogNodataRelay(key string) bool {
	return firstNotice(p.nodataRelayLogged, key)
}

// firstNotice reports whether key has not been seen by tracker yet, recording
// it as seen when it returns true: the once-per-key gate of a log line. A nil
// tracker sees nothing, so every sighting is a first one.
func firstNotice(tracker *expirable.LRU[string, bool], key string) bool {
	if tracker == nil {
		return true
	}
	if _, dup := tracker.Get(key); dup {
		return false
	}
	tracker.Add(key, true)
	return true
}

func generateCacheKey(name string, qtype uint16) string {
	return fmt.Sprintf("%s-%s", dns.Fqdn(name), dns.TypeToString[qtype])
}

func dnsTypeString(qtype uint16) string {
	if typeName, ok := dns.TypeToString[qtype]; ok {
		return typeName
	}
	return strconv.Itoa(int(qtype))
}

func dnsQuestionFields(question dns.Question) []zap.Field {
	return []zap.Field{
		zap.String("query", question.Name),
		zap.String("qtype", dnsTypeString(question.Qtype)),
		zap.Uint16("qclass", question.Qclass),
	}
}

func mergeRcode(cur, next int) int {
	// keep success unless we see a failure
	if next == dns.RcodeSuccess {
		return cur
	}
	if cur == dns.RcodeSuccess {
		return next
	}

	// severity preference (helpful for rare multi-question queries)
	if cur == dns.RcodeServerFailure || next == dns.RcodeServerFailure {
		return dns.RcodeServerFailure
	}
	if cur == dns.RcodeRefused || next == dns.RcodeRefused {
		return dns.RcodeRefused
	}
	if cur == dns.RcodeNameError || next == dns.RcodeNameError {
		return dns.RcodeNameError
	}
	return cur
}

func (p *Proxy) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {
	msg := new(dns.Msg)
	msg.SetReply(r)

	session := p.getSession()
	mode := models.GetMode()
	mockingEnabled := true
	if session != nil {
		mode = session.Mode
		mockingEnabled = session.Mocking
	}

	flagsSet := false
	// The run's mock.real (proven switch only): a name entry's questions are
	// answered live, and every answer is booked, so a connection to an
	// address can be named by the one name keploy's DNS gave it.
	rd := p.realList()

	for _, question := range r.Question {
		key := generateCacheKey(question.Name, question.Qtype)
		reqTimestamp := time.Now().UTC()

		var resp dnsCacheEntry
		found := false
		if isLocalhostName(question.Name) {
			// Before the mock.real list, the cache, a mock or an upstream:
			// keploy's DNS answers a localhost name itself, the same in a
			// recording and a replay (localhostDNSResponse).
			resp, found = localhostDNSResponse(question), true
		} else if _, listed := rd.listedNameOf(question.Name); listed {
			resp, found = p.realDNS(question), true
		} else {
			resp, found = p.dnsCache.Get(key)
		}
		if !found {
			// No per-cache-miss log: the dnsCache TTL is 30s, so any
			// unique query (including NXDOMAIN-heavy search-domain
			// expansion that we never record as a mock) cycles through
			// here every 30s and would fill the log on long sessions.
			// Diagnostic value is captured downstream — by upstream
			// failure logs in record mode, by mock-not-found SendError
			// in test mode, and by the deduped "Recording new DNS mock".
			resp = p.resolveUncachedDNSResponse(question, mode, mockingEnabled, reqTimestamp, session)

			if shouldCacheDNSResponse(mode, resp) {
				cached := dnsCacheEntry{Msg: resp.Msg.Copy(), FromUpstream: resp.FromUpstream}
				p.dnsCache.Add(key, cached)
			}
		}
		if rd != nil {
			rd.bookDNSAnswer(question, resp.Answer)
		}

		// Apply flags from the first response (DNS messages normally contain a single question)
		if !flagsSet {
			msg.Authoritative = resp.Authoritative
			msg.RecursionAvailable = resp.RecursionAvailable
			msg.Truncated = resp.Truncated
			flagsSet = true
		}

		// Merge rcode across questions (rare, but safe)
		msg.Rcode = mergeRcode(msg.Rcode, resp.Rcode)

		msg.Answer = append(msg.Answer, resp.Answer...)
		msg.Ns = append(msg.Ns, resp.Ns...)
		msg.Extra = append(msg.Extra, resp.Extra...)
	}

	// Over UDP an answer may be no larger than the client takes: 512 bytes,
	// or the size it advertised with EDNS0 (RFC 1035 4.2.1, RFC 6891 6.2.5).
	// Written whole, a larger one -- gmail.com's MX answer with its extra
	// records -- was cut off by the client's read and failed to parse ("dns:
	// overflowing header size"), while recording and when its mock was
	// replayed. Truncate drops what does not fit, extra records first, and
	// sets TC when it drops any record, so the client can ask over TCP.
	if addr := w.RemoteAddr(); addr != nil && addr.Network() == "udp" {
		msg.Truncate(udpAnswerSize(r))
	}

	err := w.WriteMsg(msg)
	if err != nil {
		utils.LogError(p.logger, err, "failed to write dns info back to the client")
	}
}

// udpAnswerSize is the most a UDP answer to r may be: the EDNS0 buffer size r
// advertises, or 512 bytes.
func udpAnswerSize(r *dns.Msg) int {
	if opt := r.IsEdns0(); opt != nil && int(opt.UDPSize()) > dns.MinMsgSize {
		return int(opt.UDPSize())
	}
	return dns.MinMsgSize
}

// isRedundantSearchExpansion reports whether name is a search-list expansion
// applied to a name that already carried that same suffix — the shape a
// resolver produces while walking its search list past an already-qualified
// name (ndots:5 is the Kubernetes default).
//
// Such a name cannot resolve. When the app asked it while keploy's DNS server
// recorded, its NXDOMAIN is a mock (recordDNSMock below) and the replay serves
// it. One with no mock was not asked then, or its recording holds no NXDOMAIN
// answers: one made before keploy recorded them, or by enterprise
// pkg/agent/proxy/dns_capture.go, which keeps NOERROR answers only. Its
// absence is a name that does not exist, not a gap in what the app depends
// on, so a test does not fail over it.
//
// Decided from the name alone on purpose. #4565 suppresses the same class of
// report but only on a definitive NXDOMAIN from upstream, which makes the
// verdict depend on the resolver being reachable within the forward deadline;
// a timeout or a SERVFAIL then reports a miss for a name that never could have
// had a mock. A name carrying only ONE search suffix is NOT redundant: it is
// a real name, and its miss is still worth reporting.
func (p *Proxy) isRedundantSearchExpansion(name string) bool {
	if len(p.dnsSearch) == 0 || name == "" {
		return false
	}
	q := strings.ToLower(dns.Fqdn(name))
	for _, s := range p.dnsSearch {
		s = strings.ToLower(strings.TrimSuffix(dns.Fqdn(s), "."))
		if s == "" {
			continue
		}
		suffix := "." + s + "."
		if !strings.HasSuffix(q, suffix) {
			continue
		}
		// Strip the trailing ".s", keeping the final dot, and ask whether
		// what remains still ends in the same suffix.
		base := q[:len(q)-len(s)-1]
		if strings.HasSuffix(base, suffix) {
			return true
		}
	}
	return false
}

func (p *Proxy) resolveUncachedDNSResponse(question dns.Question, mode models.Mode, mockingEnabled bool, reqTime time.Time, session *agent.Session) dnsCacheEntry {
	switch mode {
	case models.MODE_TEST:
		// Use recorded DNS mocks only.
		if mockingEnabled {
			if resp, mocked := p.getMockedDNSResponse(question); mocked {
				return p.replayDNSAnswer(question, resp)
			}
		}
		// Mock miss (or mocking disabled). Before falling back to a
		// synthetic/NXDOMAIN response, try forwarding to the cluster's
		// real resolver (typically CoreDNS). This is the fix for the
		// sap-demo / mysql.svc.cluster.local case: cluster-internal
		// hostnames are not — and should not be — recorded as mocks, so
		// a miss in replay mode is the expected shape for in-cluster
		// DB / cache / queue connections. Faking them with the proxy's
		// 127.0.0.1 (the legacy default) steers the app at the wrong
		// IP and crashes the DB driver.
		//
		// When mocking is explicitly disabled the operator's intent is
		// "use real traffic", so forwarding is even more clearly the
		// right behaviour than returning a synthetic proxy IP.
		//
		// If the forward succeeds we return the real answer as
		// FromUpstream so the outer cache layer retains it for the
		// usual 30 s — subsequent queries don't hit the network.
		// If the forward fails we fall through to the existing
		// "mock not found" path: same error, same log line, same
		// NXDOMAIN / synthetic fallback. Forwarding is strictly
		// additive.
		//
		// A replay is isolated from the real network by default, so the
		// miss goes only to a resolver inside the replay's own network
		// (replayMissServers: the cluster's DNS, docker's embedded
		// resolver), only for a name that network owns (networkOwnsName),
		// and only for a query type its resolver answers for such a name
		// itself (inNetworkAnswersItself): those resolvers recurse to the
		// host's and public resolvers for any other name, and docker's
		// for every type but A, AAAA and MX of its own names. The host's and public
		// resolvers are asked only when a named knob opens real egress:
		// test.allowEgress "dns", --global-passthrough, or mocking off. A
		// native replay has no in-network resolver, so its misses take the
		// fallback below, as does every name a container or cluster
		// network does not own.
		// Set when upstream answers authoritatively that the name does not
		// exist. Such a name has no mock to miss: a recording that asked it
		// holds its NXDOMAIN, served above, so one that reaches here was not
		// asked while recording or comes from a recording that holds no
		// NXDOMAIN answers. See the mock-miss report below.
		upstreamSaysNameDoesNotExist := false
		// keploy never reports the gRPC service-config probe or a redundant
		// search-list expansion as a miss (both below), so none of the
		// forward's lines (answered, NODATA, negative, failed) calls them one.
		redundantExpansion := mockingEnabled && p.isRedundantSearchExpansion(question.Name)
		reportable := mockingEnabled && !redundantExpansion && !isGrpcServiceConfigProbe(question)
		if fwdResp, fwdErr := p.forwardDNS(question, p.replayMissServers(mockingEnabled, question.Name, question.Qtype)); fwdErr == nil && fwdResp != nil {
			// A RcodeSuccess response splits into two cases by answer count:
			// resolved (>=1 RR) vs NODATA (0 RRs). Both are relayed as-is; only
			// a non-success rcode falls through to the #2006 steering below.
			if fwdResp.Rcode == dns.RcodeSuccess {
				if len(fwdResp.Answer) > 0 {
					// Resolved — relay the real answer. With mocking off no
					// mock was asked, and keploy does not report the queries
					// reportable leaves out, so neither line calls it a miss.
					fields := []zap.Field{
						zap.String("query", question.Name),
						zap.String("qtype", dns.TypeToString[question.Qtype]),
						zap.Int("rcode", fwdResp.Rcode),
						zap.Int("answers", len(fwdResp.Answer)),
					}
					switch {
					case reportable:
						p.logger.Debug("DNS mock miss resolved via upstream forward", fields...)
					case mockingEnabled:
						p.logger.Debug("DNS upstream forward answered a query keploy does not report as a miss; relaying upstream answer", fields...)
					default:
						p.logger.Debug("DNS query resolved via upstream forward; mocking disabled", fields...)
					}
					return dnsCacheEntry{Msg: fwdResp, FromUpstream: true}
				}
				// NODATA — NOERROR + zero answer RRs: a *valid* negative answer
				// ("the name exists but has no record of this type"), distinct
				// from NXDOMAIN. Relay it as-is even with mocking enabled. Only
				// NXDOMAIN (name absent) falls through to #2006 proxy-IP steering
				// below; fabricating an A→proxy-IP for a name that authoritatively
				// has no such record would be less correct than the honest empty
				// answer. Kept qtype-agnostic on purpose (the IPv6-only empty-A
				// case is symmetric). See issue #2006 and the regression tests
				// TestResolveUncachedDNSResponse_TestMode_UpstreamNODATA_* for the
				// deliberately-loosened bare-name-A case.
				//
				// Caveat (resolver-config-dependent): the "genuinely-needed
				// unmocked names return NXDOMAIN, not NODATA" assumption holds for
				// bare names on most resolvers, but a search-domain / ndots setup
				// (systemd-resolved, k8s pod DNS) can qualify a name into a zone
				// that answers NODATA — such a name would now get an empty answer
				// instead of being steered. Narrow; validate against the target
				// cluster resolver config if steering unmocked in-cluster names is
				// relied upon.
				//
				// Logging: this branch is on the HOT path — glibc fires A+AAAA for
				// essentially every hostname and every IPv4-only cluster service
				// yields an AAAA NODATA here — so an unconditional Info would spam.
				//   - mocking disabled: not a "mock miss" at all (the operator
				//     wants real DNS); log at Debug.
				//   - mocking enabled: keep it discoverable (a genuinely unrecorded
				//     record of this type also relays empty here) but dedupe to
				//     once per (name,qtype) so the signal survives without the spam.
				//   - mocking enabled for a query keploy does not report (the
				//     gRPC service-config probe, whose empty answer is the normal
				//     one; a redundant search-list expansion): not a miss, Debug.
				switch {
				case reportable:
					if p.shouldLogNodataRelay(generateCacheKey(question.Name, question.Qtype)) {
						p.logger.Info("DNS mock miss: upstream NODATA (empty NOERROR), relaying as valid negative answer (a genuinely unrecorded record of this type would also relay empty here)",
							zap.String("query", question.Name),
							zap.String("qtype", dns.TypeToString[question.Qtype]))
					} else {
						p.logger.Debug("DNS mock miss: upstream NODATA, relaying (already logged for this name/qtype)",
							zap.String("query", question.Name),
							zap.String("qtype", dns.TypeToString[question.Qtype]))
					}
				case mockingEnabled:
					p.logger.Debug("DNS NODATA (empty NOERROR) for a query keploy does not report as a miss; relaying upstream answer",
						zap.String("query", question.Name),
						zap.String("qtype", dns.TypeToString[question.Qtype]))
				default:
					p.logger.Debug("DNS NODATA (empty NOERROR); mocking disabled, relaying real upstream answer",
						zap.String("query", question.Name),
						zap.String("qtype", dns.TypeToString[question.Qtype]))
				}
				return dnsCacheEntry{Msg: fwdResp, FromUpstream: true}
			}
			// Upstream returned a genuinely negative answer: NXDOMAIN,
			// SERVFAIL, or any other non-success rcode (REFUSED, NOTIMP,
			// FORMERR, …) — everything except the RcodeSuccess cases handled
			// above.
			// When mocking is disabled the operator wants real DNS
			// behaviour, so honour the upstream negative answer as-is.
			// When mocking is enabled, a negative answer must NOT reach
			// the app: any unknown name should resolve to the proxy IP
			// so eBPF can intercept it and match against recorded mocks
			// (relaying NXDOMAIN crashes apps using bare service names
			// like "localstack" or "postgres" — issue #2006).
			if !mockingEnabled {
				p.logger.Debug("DNS upstream returned negative answer; mocking disabled, returning upstream response",
					zap.String("query", question.Name),
					zap.String("qtype", dns.TypeToString[question.Qtype]),
					zap.Int("rcode", fwdResp.Rcode))
				return dnsCacheEntry{Msg: fwdResp, FromUpstream: true}
			}
			// ONLY NXDOMAIN. This branch also covers SERVFAIL, REFUSED,
			// NOTIMP and FORMERR, which mean the resolver could not or would
			// not answer -- they say nothing about whether the name exists,
			// so they stay reportable like an unreachable upstream.
			fields := []zap.Field{
				zap.String("query", question.Name),
				zap.String("qtype", dns.TypeToString[question.Qtype]),
				zap.Int("rcode", fwdResp.Rcode),
			}
			if fwdResp.Rcode == dns.RcodeNameError {
				upstreamSaysNameDoesNotExist = true
				// Not a miss: no mock could exist for the name (see the
				// report below), so the line does not call it one.
				p.logger.Debug("DNS name does not exist upstream (NXDOMAIN), so no mock could answer it; falling back to synthetic DNS response", fields...)
			} else if reportable {
				p.logger.Debug("DNS mock miss: upstream returned negative answer; falling back to synthetic DNS response", fields...)
			} else {
				p.logger.Debug("DNS upstream returned negative answer for a query keploy does not report as a miss; falling back to synthetic DNS response", fields...)
			}
		} else if fwdErr != nil {
			fields := []zap.Field{
				zap.String("query", question.Name),
				zap.String("qtype", dns.TypeToString[question.Qtype]),
				zap.Error(fwdErr),
			}
			switch {
			case reportable:
				p.logger.Debug("DNS mock miss + upstream forward failed; falling back to synthetic response", fields...)
			case mockingEnabled:
				p.logger.Debug("DNS upstream forward failed for a query keploy does not report as a miss; falling back to synthetic response", fields...)
			default:
				p.logger.Debug("DNS upstream forward failed; mocking disabled, falling back to synthetic response", fields...)
			}
		}
		// grpc-go probes _grpc_config.<target> for a service-config TXT record on
		// EVERY target, and the absence of that record is the normal answer —
		// it means "no service config, use defaults". defaultDNSResponse below
		// already returns exactly that, so for this query the synthetic answer
		// is COMPLETE, not a degraded fallback.
		//
		// Reporting it as a missing mock therefore fails a replay that is
		// behaving correctly: every gRPC test case in a stock grpc-go
		// application reports two DNS mismatches it can do nothing about. That
		// is what the first run of the integrations gRPC e2e lane hit — every
		// test case failed on "Mock mismatch: [DNS] TXT _grpc_config.upstream"
		// without a single gRPC assertion being reached.
		//
		// Narrow on purpose: only the _grpc_config probe. A missing TXT mock for
		// any other name is still a real miss, because an application that reads
		// its own TXT records would be silently served an empty answer.
		if isGrpcServiceConfigProbe(question) {
			p.logger.Debug("no mock for the gRPC service-config TXT probe; "+
				"an empty TXT answer is the correct, complete response",
				zap.String("query", question.Name))
			return p.defaultDNSResponse(question)
		}
		// Skipped when upstream said NXDOMAIN: there is no mock to be missing.
		// A recording that asked the name holds its NXDOMAIN and the replay
		// served it above, so a name that reaches here does not exist and the
		// app did not ask it while recording, or its recording holds no
		// NXDOMAIN answers. Kubernetes resolvers reach here routinely: with
		// ndots:5 a name like appdb.ns.svc.cluster.local is first tried as
		// appdb.ns.svc.cluster.local.ns.svc.cluster.local, which NXDOMAINs by
		// design.
		//
		// Everything else that reaches this point is still reported: upstream
		// unreachable, no upstream configured, a non-forwardable qtype, and
		// SERVFAIL/REFUSED/NOTIMP/FORMERR -- none of which tell us the name is
		// absent. (A name that DOES resolve never gets here; it returns
		// upstream's answer above. So after this change DNS-miss reporting
		// fires only when the forwarder itself could not reach a verdict.)
		//
		// The app is unaffected either way -- it still receives the synthetic
		// NOERROR steer below (issue #2006). Only the spurious verdict goes.
		// A redundant search expansion cannot resolve by construction, so it
		// is filtered here rather than on upstream's verdict — that keeps the
		// report from depending on whether the resolver answered at all.
		if redundantExpansion {
			// The only field-visible trace that the name-shape gate fired.
			// Without it a suppressed report and a report that never reached
			// the gate look identical in the agent log.
			p.logger.Debug("DNS query not reported as a miss: redundant search-list expansion, a name that cannot resolve",
				zap.String("query", question.Name),
				zap.String("qtype", dns.TypeToString[question.Qtype]),
				zap.Strings("search", p.dnsSearch))
		}
		if mockingEnabled && !upstreamSaysNameDoesNotExist && !redundantExpansion {
			// Send mock not found error if we couldn't match any DNS
			// mock and upstream forwarding also failed.
			p.logger.Debug("mock miss",
				zap.String("protocol", "DNS"),
				zap.String("query", question.Name),
				zap.String("qtype", dns.TypeToString[question.Qtype]),
				zap.String("hint", "DNS mocks may be missing. Re-record to capture DNS queries."),
			)
			missed := time.Now()
			proxyErr := models.ParserError{
				ParserErrorType: models.ErrMockNotFound,
				Err:             fmt.Errorf("DNS mock not found for query: %s (%s)", question.Name, dns.TypeToString[question.Qtype]),
				At:              missed,
				MismatchReport: &models.MockMismatchReport{
					At:            missed,
					Protocol:      "DNS",
					ActualSummary: fmt.Sprintf("%s %s", dns.TypeToString[question.Qtype], question.Name),
					NextSteps:     "DNS mocks may be missing. Re-record to capture DNS queries.",
				},
			}
			p.sendDNSMiss(proxyErr, question)
		}
		return p.defaultDNSResponse(question)

	case models.MODE_RECORD:
		resp, err := p.recordDNSMock(question, reqTime, session)
		if err != nil {
			// The failure's level says whose it is, as realDNS's does.
			// recordDNSMock marks its error errNoUpstreamAnswered when it
			// asked the app's own upstream resolvers and none answered: the
			// SERVFAIL is the one the app's resolver gives, so it is
			// logUnansweredForward's WARN. Any other failure is keploy's own
			// and stays an ERROR, such as public resolvers keploy picked
			// (recordDNSFallback) not answering when it captured none of the
			// app's.
			if errors.Is(err, errNoUpstreamAnswered) {
				p.logUnansweredForward(question, "DNS resolution failed in record mode",
					append(dnsQuestionFields(question), zap.Error(err),
						zap.String("app_answer", "SERVFAIL, as a recursive resolver answers when the servers it asks do not; nothing is recorded for the name"))...,
				)
			} else {
				utils.LogError(p.logger, err, "DNS resolution failed in record mode",
					dnsQuestionFields(question)...,
				)
			}
			return dnsCacheEntry{
				Msg: &dns.Msg{
					MsgHdr: dns.MsgHdr{Rcode: dns.RcodeServerFailure},
				},
				FromUpstream: false,
			}
		}
		return resp
	default:
		// any other mode -> best-effort defaults
		return p.defaultDNSResponse(question)
	}
}

// isLocalhostName reports whether name is a localhost name (RFC 6761 6.3):
// localhost. itself or any name under it, in any letter case.
func isLocalhostName(name string) bool {
	n := strings.ToLower(dns.Fqdn(name))
	return n == "localhost." || strings.HasSuffix(n, ".localhost.")
}

// localhostDNSResponse is keploy's own answer to a localhost name, the one RFC
// 6761 6.3 asks of a resolver: the loopback address for an address query
// (127.0.0.1 for A, ::1 for AAAA) and, for any other type, NOERROR with no
// record (NODATA: the name exists, it has addresses, but no record of that
// type). It never asks another server, so it is the same in a recording and in
// a replay, needs no mock and is never recorded.
//
// The names never leave the machine they are asked on, so no upstream can
// answer them better, and asking one only costs: grpc-go asks TXT
// _grpc_config.<host> before a client's first connection, and for a localhost
// target that went upstream, where a resolver that did not answer held the
// client back for its timeout. The addresses are the ones an app's own
// resolver gives (a hosts file's localhost entries; systemd-resolved for every
// name under localhost.); ::1 is the loopback address whether or not keploy
// redirects IPv6, as it is in a hosts file.
func localhostDNSResponse(question dns.Question) dnsCacheEntry {
	resp := dnsCacheEntry{Msg: &dns.Msg{MsgHdr: dns.MsgHdr{
		Rcode:              dns.RcodeSuccess,
		Authoritative:      true,
		RecursionAvailable: true,
	}}}
	hdr := dns.RR_Header{Name: question.Name, Rrtype: question.Qtype, Class: dns.ClassINET, Ttl: 3600}
	switch question.Qtype {
	case dns.TypeA:
		resp.Answer = []dns.RR{&dns.A{Hdr: hdr, A: net.IPv4(127, 0, 0, 1)}}
	case dns.TypeAAAA:
		resp.Answer = []dns.RR{&dns.AAAA{Hdr: hdr, AAAA: net.IPv6loopback}}
	}
	return resp
}

// logUnansweredForward logs msg for a query keploy forwarded for the app that
// no upstream resolver answered (a timeout, a refused or a failed exchange),
// and that keploy then answered SERVFAIL.
//
// At WARN, not ERROR, because keploy did not fail: it answered as a recursive
// resolver answers when the servers it asks do not (SERVFAIL), so the app got
// the failure its own resolver would have given it, and nothing keploy keeps
// is lost: a failed answer is never a mock (recordDNSMock records NOERROR
// answers only), and a mock.real name is never recorded. The app did see a
// DNS failure, which can shape what it did while it was recorded, so the line
// is said at the level an operator reads. It is a WARN once per (name, qtype)
// every 30 minutes and Debug in between: a failed answer is not cached, so
// every retry of an app comes back here, and an upstream that stays down would
// otherwise repeat the line for as long. The tracker (unansweredForwardLogged)
// holds the last 1024 questions for 30 minutes each, and a new recording
// session clears it (ResetRecordedDNSMocks).
func (p *Proxy) logUnansweredForward(question dns.Question, msg string, fields ...zap.Field) {
	if firstNotice(p.unansweredForwardLogged, generateCacheKey(question.Name, question.Qtype)) {
		p.logger.Warn(msg, fields...)
		return
	}
	p.logger.Debug(msg, fields...)
}

// isGrpcServiceConfigProbe reports whether q is grpc-go's service-config
// lookup: a TXT query for _grpc_config.<target>.
//
// grpc-go's default (dns) resolver issues one per target. The record has no
// bearing on the RPCs keploy captures, and its absence is the normal answer.
func isGrpcServiceConfigProbe(q dns.Question) bool {
	return q.Qtype == dns.TypeTXT && strings.HasPrefix(strings.ToLower(q.Name), "_grpc_config.")
}

func (p *Proxy) defaultDNSResponse(question dns.Question) dnsCacheEntry {
	// Default synthesized responses should be NOERROR.
	resp := dnsCacheEntry{
		Msg: &dns.Msg{
			MsgHdr: dns.MsgHdr{
				Rcode:              dns.RcodeSuccess,
				Authoritative:      true,
				RecursionAvailable: true,
			},
		},
		FromUpstream: false,
	}

	switch question.Qtype {
	case dns.TypeA:
		p.logger.Debug("using synthesized A fallback for DNS query",
			append(dnsQuestionFields(question), zap.String("proxy_ip4", p.IP4))...,
		)
		resp.Answer = []dns.RR{&dns.A{
			Hdr: dns.RR_Header{Name: question.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 3600},
			A:   net.ParseIP(p.IP4),
		}}
		return resp

	case dns.TypeAAAA:
		// When EnableIPv6Redirect is set, the BPF cgroup program rewrites
		// v6 destinations (including ::1) to the v4-mapped proxy address,
		// so it is safe — and in fact required on modern Linux distros
		// where glibc prefers ::1 over 127.0.0.1 — to answer AAAA with
		// the proxy's v6 address. Mirror the TypeA path symmetrically.
		//
		// With the flag disabled we preserve the legacy behaviour of
		// returning an empty AAAA answer: synthesising ::1 in a v4-only
		// environment (no v6 redirect) would point clients at an
		// unreachable destination.
		if p.EnableIPv6Redirect {
			proxyIP6 := net.ParseIP(p.IP6)
			if proxyIP6 == nil {
				// Fallback to v4-mapped ::ffff:<IP4> if p.IP6 is not a
				// valid address (defensive — p.IP6 is hard-coded to ::1
				// in New() today but could be overridden in docker mode).
				if v4 := net.ParseIP(p.IP4); v4 != nil {
					proxyIP6 = v4.To16()
				}
			}
			if proxyIP6 != nil {
				resp.Answer = []dns.RR{&dns.AAAA{
					Hdr:  dns.RR_Header{Name: question.Name, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: 3600},
					AAAA: proxyIP6,
				}}
				return resp
			}
			// fall through to empty AAAA response if we could not build an answer
		}
		return resp

	case dns.TypeSRV:
		// Special handling for MongoDB SRV queries
		if strings.HasPrefix(question.Name, "_mongodb._tcp.") {
			baseDomain := strings.TrimPrefix(question.Name, "_mongodb._tcp.")
			resp.Answer = []dns.RR{&dns.SRV{
				Hdr:      dns.RR_Header{Name: dns.Fqdn(question.Name), Rrtype: dns.TypeSRV, Class: dns.ClassINET, Ttl: 3600},
				Priority: 0,
				Weight:   0,
				Port:     27017,
				Target:   dns.Fqdn("mongodb." + baseDomain),
			}}
			return resp
		}
		p.logger.Debug("sending default SRV record response")
		resp.Answer = []dns.RR{&dns.SRV{
			Hdr:      dns.RR_Header{Name: question.Name, Rrtype: dns.TypeSRV, Class: dns.ClassINET, Ttl: 3600},
			Priority: 0,
			Weight:   0,
			Port:     8080,
			Target:   dns.Fqdn("keploy.proxy"),
		}}
		return resp

	case dns.TypeTXT:
		// Always return no TXT records (empty answer). Avoid bogus TXT payloads.
		p.logger.Debug("skipping TXT answer (configured to always return empty TXT)")
		return resp

	case dns.TypeMX:
		p.logger.Debug("sending default MX record response")
		resp.Answer = []dns.RR{&dns.MX{
			Hdr:        dns.RR_Header{Name: dns.Fqdn(question.Name), Rrtype: dns.TypeMX, Class: dns.ClassINET, Ttl: 3600},
			Preference: 10,
			Mx:         dns.Fqdn("mail." + question.Name),
		}}
		return resp

	default:
		p.logger.Debug("Ignoring unsupported DNS query type", zap.Int("query type", int(question.Qtype)))
		return resp
	}
}

func (p *Proxy) getMockedDNSResponse(question dns.Question) (dnsCacheEntry, bool) {
	mgr := p.getMockManager()
	if mgr == nil {
		return dnsCacheEntry{}, false
	}

	// DNS mocks are stateless config mocks — no need to consume or
	// bump SortOrder. Just find and return the matching response.
	filteredMocks, unfilteredMocks := mgr.GetStatelessMocks(models.DNS, question.Name)
	if len(filteredMocks) == 0 && len(unfilteredMocks) == 0 {
		return dnsCacheEntry{}, false
	}

	// A question may hold up to three recorded answers, one of each kind
	// (recordDNSMock): resolved, NODATA and NXDOMAIN. The replay serves the
	// best (dnsMockRank), the first recorded of that kind, filtered mocks
	// before unfiltered ones. Only that one's records are decoded whole.
	var best *models.Mock
	bestRank := -1
	for _, mocks := range [][]*models.Mock{filteredMocks, unfilteredMocks} {
		for _, mock := range mocks {
			if mock == nil || !dnsRequestMatches(mock.Spec.DNSReq, question) {
				continue
			}
			if rank := dnsMockRank(mock.Spec.DNSResp); rank > bestRank {
				best, bestRank = mock, rank
			}
		}
	}
	if best == nil {
		return dnsCacheEntry{}, false
	}
	return dnsMockAnswer(best, p.logger), true
}

// replayDNSAnswer is a replay's answer to question, whose best recorded answer
// is recorded (getMockedDNSResponse). A negative one, NODATA or NXDOMAIN, says
// what the name was while recording, as for a container or service not up
// yet. So for a name the replay's own network owns, its resolver is asked
// first (inNetworkServers), and its answer is served when it ranks above the
// recorded one: a container that exists at replay resolves, as it did before
// keploy recorded negative answers. Every other name, and every name of a
// native replay, which has no in-network resolver, gets the recorded answer,
// as does an owned name the network does not resolve better or whose resolver
// does not answer.
func (p *Proxy) replayDNSAnswer(question dns.Question, recorded dnsCacheEntry) dnsCacheEntry {
	rank := dnsAnswerRank(recorded.Msg)
	if rank == dnsRankResolved {
		return recorded
	}
	live, err := p.forwardDNS(question, p.inNetworkServers(question.Name, question.Qtype))
	if err != nil || dnsAnswerRank(live) <= rank {
		return recorded
	}
	p.logger.Debug("DNS answer recorded as negative; the replay's own network answers the name better, serving its answer",
		zap.String("query", question.Name),
		zap.String("qtype", dns.TypeToString[question.Qtype]),
		zap.String("recorded_rcode", dns.RcodeToString[recorded.Rcode]),
		zap.Int("answers", len(live.Answer)))
	return dnsCacheEntry{Msg: live, FromUpstream: true}
}

// dnsMockAnswer is the answer recorded in mock, a DNS mock.
func dnsMockAnswer(mock *models.Mock, logger *zap.Logger) dnsCacheEntry {
	resp := dnsCacheEntry{
		Msg: &dns.Msg{
			MsgHdr: dns.MsgHdr{
				Rcode:              dns.RcodeSuccess,
				Authoritative:      true,
				RecursionAvailable: true,
			},
		},
		FromUpstream: true,
	}

	if mock.Spec.DNSResp != nil {
		resp.Rcode = mock.Spec.DNSResp.Rcode
		resp.Authoritative = mock.Spec.DNSResp.Authoritative
		resp.RecursionAvailable = mock.Spec.DNSResp.RecursionAvailable
		resp.Truncated = mock.Spec.DNSResp.Truncated

		resp.Answer = decodeDNSRRs(logger, mock.Spec.DNSResp.Answers)
		resp.Ns = decodeDNSRRs(logger, mock.Spec.DNSResp.Ns)
		resp.Extra = decodeDNSRRs(logger, mock.Spec.DNSResp.Extra)
	}

	return resp
}

func dnsRequestMatches(recorded *models.DNSReq, question dns.Question) bool {
	if recorded == nil {
		return false
	}
	if recorded.Qtype != 0 && recorded.Qtype != question.Qtype {
		return false
	}
	if recorded.Qclass != 0 && recorded.Qclass != question.Qclass {
		return false
	}
	return strings.EqualFold(dns.Fqdn(recorded.Name), dns.Fqdn(question.Name))
}

func decodeDNSRRs(logger *zap.Logger, rrs []string) []dns.RR {
	if len(rrs) == 0 {
		return nil
	}
	decoded := make([]dns.RR, 0, len(rrs))
	for _, raw := range rrs {
		if rr, ok := decodeDNSRR(logger, raw); ok {
			decoded = append(decoded, rr)
		}
	}
	return decoded
}

// decodeDNSRR is raw, a record recorded in zone format (encodeDNSRRs),
// decoded; ok is false for a string that does not parse or that holds no
// record (an empty line or a comment, which dns.NewRR returns as a nil
// record), as a hand edit can leave a mock. decodeDNSRRs leaves such a string
// out of the answer served. Served, its nil record crashed keploy's agent: a
// nil pointer dereference when the replay copied the answer into its
// dnsCache.
func decodeDNSRR(logger *zap.Logger, raw string) (rr dns.RR, ok bool) {
	rr, err := dns.NewRR(raw)
	if err != nil || rr == nil {
		if logger != nil {
			logger.Debug("failed to parse dns rr", zap.String("rr", raw), zap.Error(err))
		}
		return nil, false
	}
	return rr, true
}

func encodeDNSRRs(rrs []dns.RR) []string {
	if len(rrs) == 0 {
		return nil
	}
	encoded := make([]string, 0, len(rrs))
	for _, rr := range rrs {
		if rr == nil {
			continue
		}
		encoded = append(encoded, rr.String())
	}
	return encoded
}

// recordableDNSRcode reports whether a resolver's answer with rcode is
// recorded as a DNS mock: NOERROR, with its records or with none (NODATA: the
// name has no record of the type), and NXDOMAIN (the name does not exist).
// Each is what the app got, with the SOA whose TTL tells its resolver how long
// to keep a negative verdict, and an isolated replay can only serve what was
// recorded: an app that asks for AAAA of a host /etc/hosts holds as IPv4 only
// gets NXDOMAIN at every request, and each was a mock miss at replay.
//
// SERVFAIL, REFUSED, NOTIMP, FORMERR and the rest are not recorded: they say
// the resolver could not or would not answer at that moment, nothing about
// the name, so serving one at replay would fail the app on a failure that was
// the resolver's.
func recordableDNSRcode(rcode int) bool {
	return rcode == dns.RcodeSuccess || rcode == dns.RcodeNameError
}

// The kinds of a resolver's answer to a question, in the order a replay
// prefers them (getMockedDNSResponse): NXDOMAIN, the name does not exist;
// NODATA, NOERROR with no answer, the name exists with no record of the type;
// resolved, NOERROR with records. A name's answers can change while recording
// (a service not up yet), and the better kind is what the app got once the
// name existed: musl's getaddrinfo fails a name whose AAAA question answers
// NXDOMAIN even when its A question resolved, where NODATA lets it resolve.
const (
	dnsRankNXDOMAIN = iota
	dnsRankNODATA
	dnsRankResolved
)

// dnsAnswerRank is m's kind.
func dnsAnswerRank(m *dns.Msg) int {
	return dnsRank(m.Rcode, len(m.Answer))
}

// dnsMockRank is the kind of the answer recorded in resp as dnsMockAnswer
// serves it: its rcode, and whether one of its answer records decodes, since
// decodeDNSRRs leaves out the ones that do not. So a mock whose answer
// records all fail to decode, which keploy never writes but a hand edit can,
// ranks as the NODATA it is served as, under a valid resolved answer recorded
// after it. Answer records are decoded up to the first that decodes; the
// served mock's records are decoded whole by dnsMockAnswer. A mock with no
// response is served as NOERROR with no answer: NODATA.
func dnsMockRank(resp *models.DNSResp) int {
	if resp == nil {
		return dnsRankNODATA
	}
	answers := 0
	if slices.ContainsFunc(resp.Answers, func(raw string) bool {
		_, ok := decodeDNSRR(nil, raw)
		return ok
	}) {
		answers = 1
	}
	return dnsRank(resp.Rcode, answers)
}

// dnsRank is the kind of an answer with rcode and that many answer records.
// NXDOMAIN is negative even with records (a CNAME to a name that does not
// exist). Another rcode, which keploy does not record, ranks with NXDOMAIN.
func dnsRank(rcode, answers int) int {
	if rcode != dns.RcodeSuccess {
		return dnsRankNXDOMAIN
	}
	if answers == 0 {
		return dnsRankNODATA
	}
	return dnsRankResolved
}

// dnsDedupeSuffix keeps a question's negative answers of each kind apart in
// recordedDNSMocks, so each kind is recorded once.
var dnsDedupeSuffix = [...]string{dnsRankNXDOMAIN: ":nxdomain", dnsRankNODATA: ":nodata", dnsRankResolved: ""}

// generateDNSDedupeKey creates a unique key for DNS mock deduplication.
// The key is based solely on the DNS question (name + type + class).
// Response data (IP addresses, etc.) is intentionally excluded because services
// like AWS SQS return different IPs on every query via round-robin / load balancing,
// which would defeat deduplication and produce thousands of duplicate mocks.
// For test replay, a single recorded response is sufficient.
func generateDNSDedupeKey(question dns.Question) string {
	return fmt.Sprintf("%s:%d:%d",
		strings.ToLower(dns.Fqdn(question.Name)),
		question.Qtype,
		question.Qclass,
	)
}

// recordDNSFallback is what a recording's lookup asks when keploy captured
// no upstream resolver of the app's at startup (captureDNSUpstream): public
// resolvers keploy picked, as host:port. recordDNSTimeout caps each exchange
// of the lookup. Both are vars only so a test can point the fallback at a
// resolver that does not answer, and need not wait 5 s for one.
var (
	recordDNSFallback = []string{"8.8.8.8:53", "1.1.1.1:53"}
	recordDNSTimeout  = 5 * time.Second
)

func (p *Proxy) recordDNSMock(question dns.Question, reqTime time.Time, session *agent.Session) (dnsCacheEntry, error) {
	// Prefer the upstream resolver list captured once at proxy
	// startup (see captureDNSUpstream). This is critical in sidecar
	// deployments where the app-container resolv.conf has been
	// rewritten so the `nameserver` entry is 127.0.0.1 and DNS
	// traffic is redirected to the agent's port 26789 via iptables/
	// eBPF (resolv.conf entries themselves are IP-only and carry no
	// port). Re-reading the file here would either (a) discover the
	// loopback entry and forward queries back at ourselves, or
	// (b) discover nothing at all. Capturing once at startup pins
	// the REAL cluster resolvers.
	//
	// The public-DNS fallback (recordDNSFallback: 8.8.8.8 / 1.1.1.1) is
	// asked when keploy captured no upstream of the app's: resolv.conf
	// is missing or unreadable, or names no resolver keploy can use (no
	// nameserver given as a plain IP, or only keploy's own listener),
	// and always on Windows, where keploy reads none. The forwarder
	// treats this as "no upstream" and we would otherwise be stuck.
	// Those servers are keploy's pick, not the app's, so a failure to
	// reach them is not marked errNoUpstreamAnswered, and it stays an
	// ERROR.
	var addrs []string
	appUpstream := p.hasDNSUpstream()
	if appUpstream {
		for _, server := range p.dnsUpstreamServers {
			addrs = append(addrs, net.JoinHostPort(server, p.dnsUpstreamPort))
		}
	} else {
		// captureDNSUpstream already logs once at startup if it cannot
		// snapshot a real resolver, so we don't re-log per-query here
		// (that path runs on every cache miss and would burst the log).
		addrs = recordDNSFallback
	}

	c := new(dns.Client)
	c.Timeout = recordDNSTimeout

	m := new(dns.Msg)
	m.SetQuestion(question.Name, question.Qtype)
	m.RecursionDesired = true

	var (
		in         *dns.Msg
		resolveErr error
	)

	for _, addr := range addrs {
		// Per-server-attempt logs fire on every cache miss (worst case, multiple
		// servers per miss) so we keep only the diagnostic ones — silent success
		// path, log the rare failure / truncation / TCP-retry transitions.
		c.Net = "udp"
		resp, _, err := c.Exchange(m, addr)
		if err != nil {
			resolveErr = err
			p.logger.Debug("upstream DNS exchange failed",
				append(dnsQuestionFields(question),
					zap.String("upstream_addr", addr),
					zap.String("transport", c.Net),
					zap.Error(err),
				)...,
			)
			continue
		}

		if resp != nil && resp.Truncated {
			p.logger.Debug("upstream DNS response truncated; retrying over TCP",
				append(dnsQuestionFields(question), zap.String("upstream_addr", addr))...,
			)
			c.Net = "tcp"
			resp, _, err = c.Exchange(m, addr)
			if err != nil {
				resolveErr = err
				p.logger.Debug("upstream DNS TCP retry failed",
					append(dnsQuestionFields(question),
						zap.String("upstream_addr", addr),
						zap.String("transport", c.Net),
						zap.Error(err),
					)...,
				)
				continue
			}
		}

		if resp != nil {
			in = resp
			resolveErr = nil
			break
		}
	}

	if resolveErr != nil {
		err := fmt.Errorf("failed to resolve DNS query for %s: %w", question.Name, resolveErr)
		if appUpstream {
			// It asked the app's own upstream resolvers and none answered.
			err = fmt.Errorf("%w: %w", errNoUpstreamAnswered, err)
		}
		return dnsCacheEntry{}, err
	}
	if in == nil {
		return dnsCacheEntry{}, fmt.Errorf("nil DNS response for %s", question.Name)
	}

	resp := dnsCacheEntry{
		Msg:          in,
		FromUpstream: true,
	}

	// If no session, we still return resolved response but skip recording.
	if session == nil || session.MC == nil {
		return resp, nil
	}

	// Only the resolver's answers about the name are recorded (see
	// recordableDNSRcode). A failure to answer is not: it re-enters
	// recordDNSMock every 30s once the dnsCache TTL elapses, so this path is
	// silent, or it would fill the agent log for a whole recording against a
	// chatty client.
	if !recordableDNSRcode(in.Rcode) {
		return resp, nil
	}

	// ========== DNS MOCK DEDUPLICATION ==========
	// Generate a unique key based on the DNS query (ignoring response IPs).
	// If we've already recorded a mock for this query, skip recording.
	// Silent on the dedup-hit path: this fires on every cache miss after
	// the first record (every 30s once the dnsCache TTL elapses) for the
	// duration of the dedup tracker TTL — a debug log here would burst
	// just like the resolver-failure path above.
	//
	// Each kind of answer (dnsAnswerRank) is recorded once: a name that did
	// not resolve at first (a service not up yet) and did later keeps its
	// later answer, which a replay serves before the earlier one
	// (getMockedDNSResponse).
	dedupeKey, first := p.recordedDNSMocks.firstAnswer(question, dnsAnswerRank(in))
	if !first {
		return resp, nil
	}
	// ============================================

	resTime := time.Now().UTC()
	mock := &models.Mock{
		Version: models.GetVersion(),
		Name:    "mocks",
		Kind:    models.DNS,
		Spec: models.MockSpec{
			Metadata: map[string]string{
				"name":  "DNS",
				"qtype": dns.TypeToString[question.Qtype],
				"type":  "config",
			},
			DNSReq: &models.DNSReq{
				Name:   dns.Fqdn(question.Name),
				Qtype:  question.Qtype,
				Qclass: question.Qclass,
			},
			DNSResp: &models.DNSResp{
				Rcode:              in.Rcode,
				Authoritative:      in.Authoritative,
				RecursionAvailable: in.RecursionAvailable,
				Truncated:          in.Truncated,
				Answers:            encodeDNSRRs(in.Answer),
				Ns:                 encodeDNSRRs(in.Ns),
				Extra:              encodeDNSRRs(in.Extra),
			},
			ReqTimestampMock: reqTime,
			ResTimestampMock: resTime,
		},
	}

	// One log per UNIQUE (name, qtype, kind) — bounded by the dedupe
	// trackers' sizes so this is naturally rate-limited. Keep counts only; full
	// answer/ns/extra string dumps would balloon log size for chatty clients.
	p.logger.Debug("Recording new DNS mock",
		append(dnsQuestionFields(question),
			zap.String("dedupe_key", dedupeKey),
			zap.Int("rcode", in.Rcode),
			zap.Int("answer_count", len(in.Answer)),
			zap.Int("ns_count", len(in.Ns)),
			zap.Int("extra_count", len(in.Extra)),
		)...,
	)

	// DNS is a separate key-value map, not a streaming parser; each
	// (name, qtype) unique query is captured exactly once via the
	// p.recordedDNSMocks dedupe above, independent of whether the
	// first app request has been seen. Route every DNS mock through
	// SyncMockManager so its outChanMu also guards the send against
	// a concurrent CloseOutChan during proxy shutdown — a bare
	// `session.MC <- mock` here would race the close.
	//
	// SendConfigMock is the bypass-buffering path: unlike AddMock
	// it does not observe firstReqSeen, matching the original async
	// behavior of always forwarding immediately. The Synchronous
	// path keeps AddMock so its session-level ordering (startup
	// buffer then flush) stays intact.
	//
	// SetOutputChannel is safe to call on every DNS mock because it
	// is now idempotent: a same-pointer re-bind (this hot path) is
	// a no-op, and a post-Close same-pointer call deliberately does
	// NOT reset outChanClosed — see SetOutputChannel's doc. DNS
	// needs to be able to bind the channel the first time a query
	// arrives, which may precede the first TCP connection that
	// otherwise triggers proxy.buildRecordSession's SetOutputChannel.
	if mgr := syncMock.Get(); mgr != nil {
		mgr.SetOutputChannel(session.MC)
		if session.Synchronous {
			mgr.AddMock(mock)
		} else {
			mgr.SendConfigMock(mock)
		}
	}
	return resp, nil
}

// stopDNSServers stops both DNS servers by dnsServer.stop, the rule serveDNS
// stops each by as its context ends: whichever comes first shuts a server
// down, and the other gets its result. Before StartProxy built them there is
// nothing to stop.
func (p *Proxy) stopDNSServers() error {
	var errs []error
	for _, s := range []*dnsServer{p.tcpDNS, p.udpDNS} {
		if s != nil {
			errs = append(errs, s.stop())
		}
	}
	return errors.Join(errs...)
}

const (
	nsSwitchConfig = "/etc/nsswitch.conf"
	nsSwitchPerm   = 0644
)

// setting up the dns routing for the linux system
func (p *Proxy) setupNsswitchConfig() error {
	if _, err := os.Stat(nsSwitchConfig); err == nil {
		data, err := os.ReadFile(nsSwitchConfig)
		if err != nil {
			utils.LogError(p.logger, err, "failed to read the nsswitch.conf file from system")
			return errors.New("failed to setup the nsswitch.conf file to redirect the DNS queries to proxy")
		}

		p.nsswitchData = data

		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			if strings.HasPrefix(line, "hosts:") {
				lines[i] = "hosts: files dns"
			}
		}

		data = []byte(strings.Join(lines, "\n"))

		err = writeNsswitchConfig(p.logger, nsSwitchConfig, data, nsSwitchPerm)
		if err != nil {
			return errors.New("failed to setup the nsswitch.conf file to redirect the DNS queries to proxy")
		}

		p.logger.Debug("Successfully written to nsswitch config of linux")
	}
	return nil
}

// resetNsSwitchConfig resets the hosts config of nsswitch of the system
func (p *Proxy) resetNsSwitchConfig() error {
	data := p.nsswitchData

	err := writeNsswitchConfig(p.logger, nsSwitchConfig, data, nsSwitchPerm)
	if err != nil {
		return errors.New("failed to reset the nsswitch.conf back to the original state")
	}

	p.logger.Debug("Successfully reset the nsswitch config of linux")
	return nil
}

// preBound is the socket srv was handed bound (startDNSServers), or nil.
func preBound(srv *dns.Server) io.Closer {
	if srv.Listener != nil {
		return srv.Listener
	}
	if srv.PacketConn != nil {
		return srv.PacketConn
	}
	return nil
}
