package agent

import (
	"net"
	"slices"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
)

// recordedDests is, by port, the destinations a recording's mocks were
// recorded to (their destAddr), as the recording spells them. Evidence
// names a call's destination the same way (spelling), so a reader who greps
// mocks.yaml for the destination a miss names finds the mocks.
type recordedDests map[string][]string

// add notes m's recorded destination, when it has one.
func (d recordedDests) add(m *models.Mock) {
	if m == nil {
		return
	}
	addr := strings.TrimSpace(m.Spec.Metadata["destAddr"])
	if _, port, err := net.SplitHostPort(addr); err == nil && port != "" && !slices.Contains(d[port], addr) {
		d[port] = append(d[port], addr)
	}
}

// spelling is live as the recording spells it, and live itself when that
// differs ("" when it does not): live when the recording names it exactly;
// for a loopback IP address (127.0.0.0/8, ::1), the one loopback IP address
// the recording names on live's port. live is kept as it is when it has no
// port, when it names its host by name (localhost: the name the app gave,
// which an HTTP recording keeps in its Host header, so a search finds it as
// it is), when the recording names no loopback address on its port, or names
// several (127.0.0.1 and [::1] both), since keploy cannot tell which one the
// call is.
func (d recordedDests) spelling(live string) (string, string) {
	host, port, err := net.SplitHostPort(live)
	if err != nil || port == "" || slices.Contains(d[port], live) || !loopback(host) {
		return live, ""
	}
	var found []string
	for _, rec := range d[port] {
		if h, _, err := net.SplitHostPort(rec); err == nil && loopback(h) {
			found = append(found, rec)
		}
	}
	if len(found) != 1 {
		return live, ""
	}
	return found[0], live
}

// loopback reports whether host is a loopback IP address. A name is never
// one: keploy keeps the name the app gave.
func loopback(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// recordedDests is the loaded recording's destinations (StoreMocks and
// StoreMocksStream note them as they read the mocks, before any is parked on
// disk); a store built another way has them read from its resident mocks.
func (s *ClientMockStorage) recordedDests() recordedDests {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dests == nil {
		s.dests = recordedDests{}
		for _, pool := range [][]*models.Mock{s.filtered, s.unfiltered} {
			for _, m := range pool {
				s.dests.add(m)
			}
		}
	}
	return s.dests
}

// asRecorded is m with its destination named as the loaded recording names
// it, and the live address kept as LiveDestination when the two differ
// (recordedDests.spelling). With no recording loaded, m is as it was.
func (a *Agent) asRecorded(m models.UnmatchedCall) models.UnmatchedCall {
	if m.Destination == "" || m.LiveDestination != "" {
		return m
	}
	v, ok := a.clientMocks.Load(uint64(0))
	s, _ := v.(*ClientMockStorage)
	if !ok || s == nil {
		return m
	}
	if rec, live := s.recordedDests().spelling(m.Destination); live != "" {
		m.Destination, m.LiveDestination = rec, live
	}
	return m
}
