package proxy

import (
	"errors"
	"fmt"
	"math"
	"net"
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

// dnsUpstreamReply is what a server's socket reader hands the exchange.
type dnsUpstreamReply struct {
	server int
	msg    *dns.Msg
	err    error
}

// exchangeDNSUpstream asks servers (each at port) for m as a stub resolver
// does, and returns the first answer, whatever its rcode.
//
//   - It walks waits in order: entry k sends m to server k%len(servers) and
//     listens up to waits[k] before the next send.
//   - Each server gets one UDP socket for the whole exchange and every send
//     to it is the same query (same ID), so a late answer to an earlier send
//     still counts, from whichever server it comes.
//   - A server that refuses (ICMP port unreachable) ends its wait at once.
//   - A truncated answer is asked again over TCP of the server that sent it.
//
// The exchange lasts at most the sum of waits.
func exchangeDNSUpstream(logger *zap.Logger, m *dns.Msg, servers []string, port string, waits []time.Duration) (*dns.Msg, error) {
	if len(servers) == 0 {
		return nil, errors.New("no upstream DNS server to ask")
	}
	wire, err := m.Pack()
	if err != nil {
		return nil, fmt.Errorf("pack DNS query: %w", err)
	}
	// The schedule is absolute: send k is due at the sum of the waits before
	// it, so time spent on a TCP retry comes out of the later waits and the
	// exchange ends at end whatever happened on the way.
	start := time.Now()
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
		timer := time.NewTimer(time.Until(due))
	listen:
		for {
			select {
			case rep := <-replies:
				from := net.JoinHostPort(servers[rep.server], port)
				if rep.err == nil && rep.msg.Truncated {
					logger.Debug("upstream DNS answer truncated; asking again over TCP",
						zap.String("upstream_addr", from), zap.String("question", m.Question[0].Name))
					rep.msg, rep.err = exchangeDNSOverTCP(m, from, time.Until(end))
				}
				if rep.err == nil {
					timer.Stop()
					return rep.msg, nil
				}
				lastErr = rep.err
				logger.Debug("upstream DNS server failed",
					zap.String("upstream_addr", from), zap.String("question", m.Question[0].Name), zap.Error(rep.err))
				if rep.server == i {
					break listen
				}
			case <-timer.C:
				lastErr = fmt.Errorf("no answer from %s within %s", addr, wait)
				logger.Debug("no upstream DNS answer yet; asking the next server",
					zap.String("upstream_addr", addr), zap.String("question", m.Question[0].Name), zap.Duration("waited", wait))
				break listen
			}
		}
		timer.Stop()
	}
	if lastErr == nil {
		lastErr = errors.New("no upstream DNS server answered")
	}
	return nil, lastErr
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

func exchangeDNSOverTCP(m *dns.Msg, addr string, budget time.Duration) (*dns.Msg, error) {
	if budget <= 0 {
		return nil, fmt.Errorf("no time left to ask %s over TCP", addr)
	}
	c := &dns.Client{Net: "tcp", Timeout: budget}
	resp, _, err := c.Exchange(m, addr)
	if err != nil {
		return nil, err
	}
	return resp, nil
}
