package proxy

import (
	"strings"
	"sync/atomic"

	"go.keploy.io/server/v3/pkg/agent/containerkey"
	"go.keploy.io/server/v3/pkg/models"
)

// What the proxy learns of a test set's recorded destinations is read from
// the mocks it is staged with: the MySQL ports it routes to the MySQL parser
// (deriveMysqlPorts), and which destinations were recorded speaking HTTP/2
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
// bounded by its distinct destinations. (The loopback ports a recording
// reached, recorded.add, are read only by keploy mock, whose staging was
// never narrowed, so they are not seeded.)

// destinationKey identifies a recorded destination as the proxy's seeded
// rules read it: a MySQL mock's address (destAddr) with the recorded
// container it ran in and whether the test command started it, or an HTTP/2
// mock's scheme and authority, which the h2 preference matches on. "" for a
// mock of any other kind, or one that names no destination.
func destinationKey(m *models.Mock) string {
	if m == nil {
		return ""
	}
	switch m.Kind {
	case models.MySQL:
		addr := m.Spec.Metadata["destAddr"]
		if addr == "" {
			return ""
		}
		key, _ := containerkey.Recorded(m)
		return strings.Join([]string{
			string(m.Kind), addr,
			m.Spec.Metadata["startedByTests"],
			key, m.Spec.Metadata[containerkey.InstanceKey],
		}, "\x00")
	case models.HTTP2:
		if m.Spec.HTTP2Req == nil || m.Spec.HTTP2Req.Authority == "" {
			return ""
		}
		return string(m.Kind) + "\x00" + m.Spec.HTTP2Req.Scheme + "://" + m.Spec.HTTP2Req.Authority
	}
	return ""
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
// staging derives the MySQL ports from the seed beside what it stages
// (seededDestinations), so a session's reset of what was derived (Mock marks
// the MySQL ports stale) never loses it, whichever comes first. The HTTP/2
// destinations are read while the app starts (replayTargetHasHTTP2Mock).
// Satisfies the agent's optional RecordedDestinationsSeeder.
func (p *Proxy) SeedRecordedDestinations(mocks []*models.Mock) {
	// Kept is what a staging's derivation reads of each MySQL mock (its kind
	// and metadata), not the mock, so nothing recorded stays resident.
	var seeded []*models.Mock
	for _, mk := range mocks {
		if mk == nil || mk.Kind != models.MySQL {
			continue
		}
		seeded = append(seeded, &models.Mock{Kind: mk.Kind, Spec: models.MockSpec{Metadata: mk.Spec.Metadata}})
	}
	p.seededDests.Store(&seeded)
	p.recordedH2.seed(mocks)
}

// seededDestinations is the stored set's seeded recorded destinations
// (SeedRecordedDestinations), nil before any.
func (p *Proxy) seededDestinations() []*models.Mock {
	if s := p.seededDests.Load(); s != nil {
		return *s
	}
	return nil
}
