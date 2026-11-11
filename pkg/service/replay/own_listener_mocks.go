package replay

import (
	"net"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/models"
)

// keploy record and keploy test pass a call the app makes to its own port
// through to the app (the proxy's ownListenerCall): such a call is the app's
// own business, not a dependency's. A recording a keploy before that made of
// one is never served now, so it must not fail a test or the set as a
// recorded call the app did not make, under either switch. The replay marks
// it as it reads the set's mocks (models.Mock.MarkOwnListenerCall), and every
// site that decides what a test or the worker scope requires leaves it out
// (models.Mock.OutsideTestScopes, excludedFromTestMockAssertion). The agent
// is sent it as before: a self-call the proxy cannot tell is the app's own is
// still answered by it.

// ownListenerPorts is the ports keploy knows the app listens on from a set's
// test cases: each one's AppPort, the port its request reached inside the
// app's network namespace when it was recorded. A test case recorded with
// none names none.
func ownListenerPorts(cases []*models.TestCase) map[uint16]bool {
	ports := map[uint16]bool{}
	for _, tc := range cases {
		if tc.AppPort != 0 {
			ports[tc.AppPort] = true
		}
	}
	return ports
}

// ownListenerMock reports whether m records a call the app made to its own
// listener: where it says it went, its destAddr (which decides when it has
// one) or else an HTTP mock's Host, is a loopback or unspecified address, or
// localhost, at one of the app's ports. A destination keploy cannot read, one
// that names no port, or another address of the app's (its container's own
// IP) is not recognized: the mock is judged as before.
func ownListenerMock(m *models.Mock, ports map[uint16]bool) bool {
	if m == nil {
		return false
	}
	addr := strings.TrimSpace(m.Spec.Metadata["destAddr"])
	if addr == "" {
		addr, _ = m.RecordedDestination()
	}
	// An address that does not split names no port, which ParseUint
	// refuses; a host that is no IP is neither loopback nor unspecified.
	host, port, _ := net.SplitHostPort(addr)
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil || !ports[uint16(n)] {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip.IsLoopback() || ip.IsUnspecified()
}

// maxNamedOwnListenerMocks is how many of the mocks it marked the replay's
// note names.
const maxNamedOwnListenerMocks = 10

// markOwnListenerMocks marks each of a set's mocks, from pools, that records
// a call the app made to its own listener (ownListenerMock), and says once
// for the set how many it marked and that they go unused. It returns how
// many it marked.
func (r *Replayer) markOwnListenerMocks(testSetID string, cases []*models.TestCase, pools ...[]*models.Mock) int {
	ports := ownListenerPorts(cases)
	seen := map[string]bool{}
	var names []string
	for _, pool := range pools {
		for _, m := range pool {
			if !ownListenerMock(m, ports) {
				continue
			}
			m.MarkOwnListenerCall()
			if !seen[m.Name] {
				seen[m.Name] = true
				names = append(names, m.Name)
			}
		}
	}
	if len(names) == 0 {
		return 0
	}
	named := names[:min(len(names), maxNamedOwnListenerMocks)]
	r.logger.Info("this test set holds recordings of calls the app made to its own port: keploy now passes such a call through to the app, so these go unused, and no test or worker scope requires them",
		zap.String("testSetID", testSetID), zap.Int("count", len(names)), zap.Strings("mocks", named),
		zap.String("note", "only a loopback, unspecified or localhost destination at a port the set's test cases were sent to is recognized; a call to the app's other addresses is judged as before"))
	return len(names)
}
