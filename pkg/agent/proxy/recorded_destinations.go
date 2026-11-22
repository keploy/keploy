package proxy

import (
	"strconv"
	"strings"
	"sync/atomic"

	"go.keploy.io/server/v3/pkg/agent/containerkey"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// What the proxy learns of a test set's recorded destinations is read from
// the mocks it is staged with: the MySQL ports it routes to the MySQL parser
// (deriveMysqlPorts), whether a built-in default MySQL port held another
// dependency, which a replay then serves on its client's bytes
// (recordedNotMysql), and which destinations were recorded speaking HTTP/2
// (replayTargetHasHTTP2Mock, for the ALPN a TLS client is offered while the
// app starts). Since a set's staging reads only what was recorded before its
// first test (keploy #4819), a destination whose mocks are all per-test and
// recorded inside tests is unknown while the app starts.
//
// So the agent's on-disk store keeps, as it stores the set, the first mock it
// holds of each recorded destination those rules read (destinationKey), and
// the agent seeds them on the proxy when the store is published
// (SeedRecordedDestinations). The proxy keeps them, and every staging derives
// from them as from the mocks it stages: what the recording says of its
// destinations is known from the start, read from the whole recording,
// bounded by its distinct destinations.
//
// The ports a recording reached, which keploy mock's loopback rules read
// (recordedPorts), are seeded the same way under the proven switch, where
// keploy mock replay stages as keploy test does (keploy #4970): a local
// dependency the recording reached only inside tests is a recorded port while
// the app starts and while each test runs, whose own mocks reach the proxy
// through the exact view, never staged. The store keeps the first mock of
// each port (reachedPortKey) beside the destinations' (LoadDestinations).
// Without the switch a set's ports are, as before, what its stagings stage.

// destinationKey identifies a recorded destination as the proxy's seeded
// rules read it: a MySQL mock's address (destAddr) with the recorded
// container it ran in and whether the test command started it (one key for
// every MySQL mock that names no address, which may be any default port's),
// an HTTP/2 mock's scheme and authority, which the h2 preference matches on,
// or, for any other dependency reached at a built-in default MySQL port
// (3306, 4000), that port, which decides how a replay serves a connection
// there (mysqlPortRegistry.recordedNotMysql). "" for any other mock.
func destinationKey(m *models.Mock) string {
	if m == nil {
		return ""
	}
	switch m.Kind {
	case models.MySQL:
		addr := m.Spec.Metadata["destAddr"]
		if addr == "" {
			return string(m.Kind)
		}
		key, _ := containerkey.Recorded(m)
		return strings.Join([]string{
			string(m.Kind), addr,
			m.Spec.Metadata["startedByTests"],
			key, m.Spec.Metadata[containerkey.InstanceKey],
		}, "\x00")
	case models.HTTP2:
		if m.Spec.HTTP2Req != nil && m.Spec.HTTP2Req.Authority != "" {
			return string(m.Kind) + "\x00" + m.Spec.HTTP2Req.Scheme + "://" + m.Spec.HTTP2Req.Authority
		}
	}
	if port, ok := defaultPortReached(m); ok {
		return "port\x00" + strconv.FormatUint(uint64(port), 10)
	}
	return ""
}

// defaultPortReached is the built-in default MySQL port (3306, 4000) a
// recorded mock of a dependency other than MySQL reached (otherDependencyPort),
// if it reached one.
func defaultPortReached(m *models.Mock) (uint32, bool) {
	port, ok := otherDependencyPort(m)
	return port, ok && isMysqlPort(port, nil)
}

// recordedH2 is one destination a recording reached speaking HTTP/2: its
// authority's host (lower case) and port.
type recordedH2 struct {
	host string
	port int
}

// recordedHTTP2 holds the HTTP/2 destinations of the stored set's recording
// (SeedRecordedDestinations), replaced whole with each store.
type recordedHTTP2 struct {
	dests atomic.Pointer[[]recordedH2]
}

func (r *recordedHTTP2) seed(mocks []*models.Mock) {
	var out []recordedH2
	for _, mk := range mocks {
		if mk == nil || mk.Kind != models.HTTP2 || mk.Spec.HTTP2Req == nil {
			continue
		}
		h, pt := hostPortFromAuthority(mk.Spec.HTTP2Req.Authority, mk.Spec.HTTP2Req.Scheme)
		out = append(out, recordedH2{host: strings.ToLower(h), port: pt})
	}
	r.dests.Store(&out)
}

// has reports whether the stored set's recording reached sniHost:port
// speaking HTTP/2, by the rule replayTargetHasHTTP2Mock applies to the mocks
// it is staged with (http2DestMatches).
func (r *recordedHTTP2) has(sniHost string, port uint32) bool {
	d := r.dests.Load()
	if d == nil {
		return false
	}
	for _, h := range *d {
		if http2DestMatches(h.host, h.port, sniHost, port) {
			return true
		}
	}
	return false
}

// SeedRecordedDestinations tells the proxy the stored set's recorded
// destinations, the first mock of each that the agent's on-disk store holds
// (DiskMocks.LoadDestinations). Each store's seed replaces the previous
// store's; a set with no on-disk store seeds none (its per-test mocks are all
// resident, so its staging reaches them). Nothing is derived here: every
// staging derives the MySQL ports, and what the default ports held, from the
// seed beside what it stages
// (seededDestinations), so a session's reset of what was derived (Mock marks
// the MySQL ports stale) never loses it, whichever comes first. The HTTP/2
// destinations are read while the app starts (replayTargetHasHTTP2Mock).
// Satisfies the agent's optional RecordedDestinationsSeeder.
func (p *Proxy) SeedRecordedDestinations(mocks []*models.Mock) {
	// Kept is what a staging's derivation reads of each mock, not the mock,
	// so nothing recorded stays resident: a MySQL mock's kind and metadata,
	// and, of another dependency reached at a built-in default MySQL port,
	// its kind and the address it reached there (otherDependencyAddr), which
	// tells a replay that port held something other than MySQL. Under the
	// proven switch the store also keeps the first mock of each port reached
	// (reachedPortKey): one of no recorded destination (destinationKey "")
	// feeds none of these rules, being no MySQL mock, at no default MySQL
	// port, and of no HTTP/2 authority.
	var seeded []*models.Mock
	for _, mk := range mocks {
		switch {
		case mk == nil:
		case mk.Kind == models.MySQL:
			seeded = append(seeded, &models.Mock{Kind: mk.Kind, Spec: models.MockSpec{Metadata: mk.Spec.Metadata}})
		default:
			if _, ok := defaultPortReached(mk); ok {
				seeded = append(seeded, &models.Mock{Kind: mk.Kind, Spec: models.MockSpec{
					Metadata: map[string]string{"destAddr": otherDependencyAddr(mk)},
				}})
			}
		}
	}
	p.seededDests.Store(&seeded)
	p.recordedH2.seed(mocks)
	// The ports, of the mocks the store keeps of each (reachedPortKey), and
	// none without the proven switch, where a set's ports are what its
	// stagings stage.
	if provenswitch.On() {
		recorded.seed(mocks)
	} else {
		recorded.seed(nil)
	}
}

// seededDestinations is the stored set's seeded recorded destinations
// (SeedRecordedDestinations), nil before any.
func (p *Proxy) seededDestinations() []*models.Mock {
	if s := p.seededDests.Load(); s != nil {
		return *s
	}
	return nil
}
