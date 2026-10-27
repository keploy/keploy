package proxy

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/miekg/dns"
	"go.uber.org/zap"
)

// dnsStubPolicy is how keploy asks the upstream resolvers: the resolv.conf
// options a stub resolver (glibc, musl) asks them by. Keploy sits between
// the app's stub resolver and the upstream, so it must ask the upstream at
// least as patiently as the app would have without keploy: a lost UDP packet
// is resent, never turned into a failure the app sees while the upstream
// still answers.
type dnsStubPolicy struct {
	// timeout is the first round's wait for an answer after each send
	// (resolv.conf "options timeout:", default 5 s).
	timeout time.Duration
	// attempts is how many rounds the servers are asked in
	// (resolv.conf "options attempts:", default 2).
	attempts int
	// rotate starts each query at the next server in turn, spreading the
	// queries over the servers (resolv.conf "options rotate"); without it
	// every query starts at the first server.
	rotate bool
}

// The defaults and caps are glibc's (RES_TIMEOUT, RES_DFLRETRY,
// RES_MAXRETRANS, RES_MAXRETRY): miekg/dns parses the options but caps
// neither, and the schedule below doubles per round.
const (
	dnsStubDefaultTimeout  = 5 * time.Second
	dnsStubDefaultAttempts = 2
	dnsStubMaxTimeout      = 30 * time.Second
	dnsStubMaxAttempts     = 5
)

func stubPolicyFromConfig(cfg *dns.ClientConfig) dnsStubPolicy {
	if cfg == nil {
		return dnsStubPolicy{}
	}
	return dnsStubPolicy{timeout: time.Duration(cfg.Timeout) * time.Second, attempts: cfg.Attempts}
}

// resolvConfRotates reports whether the resolv.conf at path sets "options
// rotate", which miekg/dns does not parse. An unreadable file does not.
func resolvConfRotates(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	rotate := false
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "options" {
			continue
		}
		for _, opt := range fields[1:] {
			if opt == "rotate" {
				rotate = true
			}
		}
	}
	return rotate
}

func (pol dnsStubPolicy) normalized() dnsStubPolicy {
	if pol.timeout <= 0 {
		pol.timeout = dnsStubDefaultTimeout
	}
	if pol.timeout > dnsStubMaxTimeout {
		pol.timeout = dnsStubMaxTimeout
	}
	if pol.attempts <= 0 {
		pol.attempts = dnsStubDefaultAttempts
	}
	if pol.attempts > dnsStubMaxAttempts {
		pol.attempts = dnsStubMaxAttempts
	}
	return pol
}

// waits is the classic resolver's (BIND res_send's) schedule for n servers:
// round r sends to each server in turn and listens timeout<<r after each
// send, shared among the servers after the first round. Entry k belongs to
// server k%n.
func (pol dnsStubPolicy) waits(n int) []time.Duration {
	pol = pol.normalized()
	if n <= 0 {
		return nil
	}
	out := make([]time.Duration, 0, n*pol.attempts)
	for r := 0; r < pol.attempts; r++ {
		w := pol.timeout << r
		if r > 0 {
			w /= time.Duration(n)
		}
		for i := 0; i < n; i++ {
			out = append(out, w)
		}
	}
	return out
}

// waitsWithin is the same schedule compressed to last budget in all: the
// test-mode forward on a mock miss answers the app within dnsForwardTimeout
// (so the app's resolver hears keploy's fallback before it gives up), and
// still resends inside that budget.
func (pol dnsStubPolicy) waitsWithin(n int, budget time.Duration) []time.Duration {
	out := pol.waits(n)
	var sum time.Duration
	for _, w := range out {
		sum += w
	}
	if sum <= 0 {
		return out
	}
	for k, w := range out {
		out[k] = time.Duration(math.Round(float64(w) / float64(sum) * float64(budget)))
	}
	return out
}

// dnsStub is the proxy's upstream policy, the defaults when resolv.conf was
// not read.
func (p *Proxy) dnsStub() dnsStubPolicy {
	return p.dnsUpstreamPolicy.normalized()
}

// askOrder is the order this query asks servers in: as listed, or, when
// resolv.conf says rotate, starting at the next server in turn as glibc does.
// The returned slice is the query's own.
func (p *Proxy) askOrder(servers []string) []string {
	if !p.dnsUpstreamPolicy.rotate || len(servers) < 2 {
		return append([]string(nil), servers...)
	}
	first := int((p.dnsRotateNext.Add(1) - 1) % uint32(len(servers)))
	out := make([]string, 0, len(servers))
	out = append(out, servers[first:]...)
	return append(out, servers[:first]...)
}

// dnsUpstreamReply is what a server's socket reader hands the exchange.
type dnsUpstreamReply struct {
	server int
	msg    *dns.Msg
	err    error
}

// dnsClock is the time an upstream exchange keeps. Production asks the wall
// clock; a row brings its own, so it decides by events which wait or budget
// the exchange asked for and when that wait ends.
type dnsClock interface {
	Now() time.Time
	// After fires once d has passed; stop releases it early.
	After(d time.Duration) (fire <-chan time.Time, stop func())
}

type wallDNSClock struct{}

func (wallDNSClock) Now() time.Time { return time.Now() }

func (wallDNSClock) After(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTimer(d)
	return t.C, func() { t.Stop() }
}

// exchangeDNSUpstream asks servers (each at port) for m as a stub resolver
// does, and returns the first answer, whatever its rcode, with the address
// (host:port) of the server that gave it: the server whose socket the answer
// came in on (for a truncated one, the server asked again over TCP), which is
// not always the server asked last.
//
//   - It walks waits in order: entry k sends m to server k%len(servers) and
//     listens until the sum of waits[0..k] has passed since the start before
//     the next send.
//   - Each server gets one UDP socket for the whole exchange and every send
//     to it is the same query (same ID), so a late answer to an earlier send
//     still counts, from whichever server it comes.
//   - A server that refuses (ICMP port unreachable) ends its wait at once.
//   - A truncated answer is asked again over TCP of the server that sent it.
//   - It ends when ctx does: the proxy's stop never waits out its schedule.
//
// The exchange lasts at most the sum of waits.
func exchangeDNSUpstream(ctx context.Context, logger *zap.Logger, m *dns.Msg, servers []string, port string, waits []time.Duration) (*dns.Msg, string, error) {
	return exchangeDNSUpstreamOn(ctx, wallDNSClock{}, logger, m, servers, port, waits)
}

func exchangeDNSUpstreamOn(ctx context.Context, clock dnsClock, logger *zap.Logger, m *dns.Msg, servers []string, port string, waits []time.Duration) (*dns.Msg, string, error) {
	if len(servers) == 0 {
		return nil, "", errors.New("no upstream DNS server to ask")
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	wire, err := m.Pack()
	if err != nil {
		return nil, "", fmt.Errorf("pack DNS query: %w", err)
	}
	// The schedule is absolute: send k is due at the sum of the waits before
	// it, so time spent on a TCP retry or a late wake-up comes out of the
	// later waits and the exchange ends at end whatever happened on the way.
	start := clock.Now()
	end := start
	for _, w := range waits {
		end = end.Add(w)
	}
	due := start

	replies := make(chan dnsUpstreamReply)
	done := make(chan struct{})
	conns := make([]net.Conn, len(servers))
	var readers sync.WaitGroup
	defer func() {
		close(done)
		for _, c := range conns {
			if c != nil {
				_ = c.Close()
			}
		}
		readers.Wait()
	}()

	var lastErr error
	for k, wait := range waits {
		due = due.Add(wait)
		i := k % len(servers)
		addr := net.JoinHostPort(servers[i], port)
		if conns[i] == nil {
			c, err := net.Dial("udp", addr)
			if err != nil {
				lastErr = err
				continue
			}
			conns[i] = c
			readers.Add(1)
			go readDNSUpstreamReplies(c, i, m.Id, replies, done, &readers)
		}
		if _, err := conns[i].Write(wire); err != nil {
			lastErr = err
			continue
		}
		fire, stop := clock.After(due.Sub(clock.Now()))
	listen:
		for {
			select {
			case <-ctx.Done():
				stop()
				return nil, "", ctx.Err()
			case rep := <-replies:
				from := net.JoinHostPort(servers[rep.server], port)
				if rep.err == nil && rep.msg.Truncated {
					logger.Debug("upstream DNS answer truncated; asking again over TCP",
						zap.String("upstream_addr", from), zap.String("question", m.Question[0].Name))
					rep.msg, rep.err = exchangeDNSOverTCP(ctx, clock, m, from, end.Sub(clock.Now()))
				}
				if rep.err == nil {
					stop()
					return rep.msg, from, nil
				}
				if ctx.Err() != nil {
					stop()
					return nil, "", ctx.Err()
				}
				lastErr = rep.err
				logger.Debug("upstream DNS server failed",
					zap.String("upstream_addr", from), zap.String("question", m.Question[0].Name), zap.Error(rep.err))
				if rep.server == i {
					break listen
				}
			case <-fire:
				lastErr = fmt.Errorf("no answer from %s within %s", addr, wait)
				logger.Debug("no upstream DNS answer yet; asking the next server",
					zap.String("upstream_addr", addr), zap.String("question", m.Question[0].Name), zap.Duration("waited", wait))
				break listen
			}
		}
		stop()
	}
	if lastErr == nil {
		lastErr = errors.New("no upstream DNS server answered")
	}
	return nil, "", lastErr
}

// readDNSUpstreamReplies hands the exchange every answer to id that arrives
// on c, and each refusal, until the exchange is done or c fails otherwise.
func readDNSUpstreamReplies(c net.Conn, server int, id uint16, replies chan<- dnsUpstreamReply, done <-chan struct{}, wg *sync.WaitGroup) {
	defer wg.Done()
	buf := make([]byte, dns.MaxMsgSize)
	for {
		n, err := c.Read(buf)
		var rep dnsUpstreamReply
		if err != nil {
			select {
			case <-done:
				return
			default:
			}
			rep = dnsUpstreamReply{server: server, err: err}
		} else {
			msg := new(dns.Msg)
			if msg.Unpack(buf[:n]) != nil || msg.Id != id || !msg.Response {
				continue
			}
			rep = dnsUpstreamReply{server: server, msg: msg}
		}
		select {
		case replies <- rep:
		case <-done:
			return
		}
		// A refusal is one ICMP message: the socket reads on. Any other
		// read error leaves it unusable.
		if err != nil && !errors.Is(err, syscall.ECONNREFUSED) {
			return
		}
	}
}

// exchangeDNSOverTCP asks addr for m over TCP, for at most budget on clock,
// and no longer than ctx lasts.
func exchangeDNSOverTCP(ctx context.Context, clock dnsClock, m *dns.Msg, addr string, budget time.Duration) (*dns.Msg, error) {
	if budget <= 0 {
		return nil, fmt.Errorf("no time left to ask %s over TCP", addr)
	}
	tctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	fire, stop := clock.After(budget)
	defer stop()
	go func() {
		select {
		case <-fire:
			cancel(fmt.Errorf("no TCP answer from %s within %s", addr, budget))
		case <-tctx.Done():
		}
	}()

	var d net.Dialer
	nc, err := d.DialContext(tctx, "tcp", addr)
	if err != nil {
		return nil, tcpExchangeErr(tctx, err)
	}
	defer func() { _ = nc.Close() }()
	// A past deadline ends a blocked write or read at once.
	unblock := context.AfterFunc(tctx, func() { _ = nc.SetDeadline(time.Unix(1, 0)) })
	defer unblock()
	co := &dns.Conn{Conn: nc}
	if err := co.WriteMsg(m); err != nil {
		return nil, tcpExchangeErr(tctx, err)
	}
	resp, err := co.ReadMsg()
	if err != nil {
		return nil, tcpExchangeErr(tctx, err)
	}
	if resp.Id != m.Id {
		return nil, fmt.Errorf("the TCP answer from %s is to another query (id %d, asked %d)", addr, resp.Id, m.Id)
	}
	return resp, nil
}

// tcpExchangeErr names why a TCP exchange stopped: the proxy's stop, the
// exchange's budget, or err itself.
func tcpExchangeErr(tctx context.Context, err error) error {
	if tctx.Err() != nil {
		return context.Cause(tctx)
	}
	return err
}
