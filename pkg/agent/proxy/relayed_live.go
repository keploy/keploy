package proxy

import (
	"net"
	"strings"
	"sync"

	"go.keploy.io/server/v3/pkg/models"
)

// relayedLive counts, for the agent's life, the connections the proxy relays
// live to a dependency instead of recording them or answering them from a
// recording, per reason (models.RelayedLive.Reason) and destination: the
// Redis skip's (an open-source keploy relays Redis to the real server), the
// agent's pass-through (its global pass-through, with a session or before
// one, a session with mocking off, and its opportunistic TLS intercept), and
// mock.real's. A connection the run's own process tree serves (a self-call)
// is no dependency, and is not counted: serveTreeListener answers it before
// any of those is reached.
// `keploy mock replay` reads the counts (Proxy.RelayedLive) before and after
// its test command, and claims the run isolated only when it relayed none.
type relayedLive struct {
	mu sync.Mutex
	n  map[relayedKey]int
}

type relayedKey struct{ reason, dest string }

// note counts one connection relayed live to dest, for reason.
func (r *relayedLive) note(reason, dest string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.n == nil {
		r.n = map[relayedKey]int{}
	}
	r.n[relayedKey{reason, dest}]++
}

// rename moves one connection counted for reason at from to dest to: the
// connection is counted as soon as it is matched, and named once its dial
// target is known.
func (r *relayedLive) rename(reason, from, to string) {
	if from == to {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	k := relayedKey{reason, from}
	if r.n[k] == 0 {
		return
	}
	r.n[k]--
	if r.n[k] == 0 {
		delete(r.n, k)
	}
	r.n[relayedKey{reason, to}]++
}

// rows is every count, sorted (models.RelayedLives.Sorted); empty, never
// nil, when nothing was relayed: an agent that counts and relayed nothing
// says "none".
func (r *relayedLive) rows() models.RelayedLives {
	r.mu.Lock()
	out := make(models.RelayedLives, 0, len(r.n))
	for k, n := range r.n {
		out = append(out, models.RelayedLive{Reason: k.reason, Destination: k.dest, Connections: n})
	}
	r.mu.Unlock()
	if sorted := out.Sorted(); len(sorted) > 0 {
		return sorted
	}
	return models.RelayedLives{}
}

// RelayedLive is every connection p relayed live to a dependency since it
// started, per reason and destination (relayedLive): the agent's answer to
// GET /agent/relayed (agent.RelayedLiveReporter).
func (p *Proxy) RelayedLive() models.RelayedLives {
	return p.relayed.rows()
}

// redisTarget is where a replay's Redis skip dials a connection the
// application made to dstAddr, which reaches addr (dstAddr, or over TLS the
// server's name it was dialled by), and the name the count gives it: by the
// name keploy's DNS answered for that address, at its port, when the dial
// goes to the name's address now (liveTarget), since the recorded address may
// no longer be the name's; by the TLS server's name when it is dialled by
// that; "" when the dial goes where the application dialled, and the count
// keeps dstAddr.
func (p *Proxy) redisTarget(dstAddr, addr string) (live, named string) {
	live, name := p.liveTarget(addr)
	switch {
	case name != "":
		// live is the name's address now, at addr's port.
		_, port, _ := net.SplitHostPort(live)
		return live, net.JoinHostPort(strings.TrimSuffix(name, "."), port)
	case live != dstAddr:
		return live, live
	}
	return live, ""
}
