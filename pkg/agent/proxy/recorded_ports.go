package proxy

import (
	"net"
	"net/url"
	"strconv"
	"sync"

	"go.keploy.io/server/v3/pkg/models"
)

var recorded recordedPorts

func RecordedPort(port uint32) bool { return recorded.has(port) && !recorded.child(port) }

// recordedPorts is the ports a recording reached, which keploy mock's
// loopback rules read (unowned, serveTreeListener): a loopback connect
// nothing listens at is refused unless the recording reached its port. It
// holds what the mocks staged since the session's start reached (add, reset
// by each Record and Mock) and what the stored set's whole recording reached,
// seeded when the agent publishes its on-disk store (seed, replaced by each
// store's seed and kept across a Mock, whichever comes first; forgotten by a
// Record, which replays nothing). A port is recorded if either has it.
type recordedPorts struct {
	mu     sync.RWMutex
	staged portSet
	seeded portSet
}

// portSet is the ports a set of mocks reached, kept apart by whether a
// process the test command started reached them (startedByTests).
type portSet struct {
	ports   map[uint32]struct{}
	kids    map[uint32]struct{}
	plain   map[uint32]struct{}
	unknown bool
}

// portReached is the port m reached (known), or that m reached a port that
// cannot be told (!known): either way counted. A DNS answer and a connection
// failure are not counted. A connection failure says the destination was NOT
// reached, so it is no evidence that the recording talked to that port. It
// carries no metadata.destAddr either, and a mock whose port is unknown would
// make every port "recorded", switching off the loopback refusal rules for
// the whole set.
func portReached(m *models.Mock) (port uint32, known, counted bool) {
	if m == nil || m.Kind == models.DNS || m.Kind == models.ConnectionFailure {
		return 0, false, false
	}
	addr := m.Spec.Metadata["destAddr"]
	if addr == "" {
		addr, _ = m.RecordedDestination()
	}
	port, ok := portFromAddr(addr)
	if !ok {
		port, ok = defaultPort(m, addr)
	}
	return port, ok, true
}

// reachedPortKey names, for the agent's on-disk store, the port m reached and
// whether a process the test command started reached it: the store keeps the
// first mock of each (DiskMocks.Add), so a seed (recordedPorts.seed) reads
// each port the whole recording reached, bounded by how many it reached. ""
// for a mock portReached does not count.
func reachedPortKey(m *models.Mock) string {
	port, known, counted := portReached(m)
	if !counted {
		return ""
	}
	key := "reached\x00?"
	if known {
		key = "reached\x00" + strconv.FormatUint(uint64(port), 10)
	}
	return key + "\x00" + strconv.FormatBool(m.Spec.Metadata["startedByTests"] == "true")
}

func (s *portSet) add(m *models.Mock) {
	port, known, counted := portReached(m)
	if !counted {
		return
	}
	if !known {
		s.unknown = true
		return
	}
	if s.ports == nil {
		s.ports = map[uint32]struct{}{}
	}
	s.ports[port] = struct{}{}
	if m.Spec.Metadata["startedByTests"] == "true" {
		if s.kids == nil {
			s.kids = map[uint32]struct{}{}
		}
		s.kids[port] = struct{}{}
	} else {
		if s.plain == nil {
			s.plain = map[uint32]struct{}{}
		}
		s.plain[port] = struct{}{}
	}
}

func (r *recordedPorts) add(sets ...[]*models.Mock) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, mocks := range sets {
		for _, m := range mocks {
			r.staged.add(m)
		}
	}
}

// seed replaces the seeded ports with those mocks reached: the stored set's
// whole recording, as its on-disk store keeps it (SeedRecordedDestinations).
func (r *recordedPorts) seed(mocks []*models.Mock) {
	var s portSet
	for _, m := range mocks {
		s.add(m)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seeded = s
}

// reset forgets the staged ports (a session's start). The seeded ports are
// the stored set's, published before or after its Mock, and stay.
func (r *recordedPorts) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.staged = portSet{}
}

// forget forgets the staged and the seeded ports (a recording's start, and a
// test's cleanup).
func (r *recordedPorts) forget() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.staged, r.seeded = portSet{}, portSet{}
}

func defaultPort(m *models.Mock, addr string) (uint32, bool) {
	if m.Kind != models.HTTP || m.Spec.HTTPReq == nil || addr == "" {
		return 0, false
	}
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return 0, false
	}
	if u, err := url.Parse(m.Spec.HTTPReq.URL); err == nil && u.Scheme == "https" {
		return 443, true
	}
	return 80, true
}

func (r *recordedPorts) has(port uint32) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.staged.unknown || r.seeded.unknown {
		return true
	}
	_, staged := r.staged.ports[port]
	_, seeded := r.seeded.ports[port]
	return staged || seeded
}

func (r *recordedPorts) child(port uint32) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, kid := r.staged.kids[port]
	_, seededKid := r.seeded.kids[port]
	_, plain := r.staged.plain[port]
	_, seededPlain := r.seeded.plain[port]
	return (kid || seededKid) && !plain && !seededPlain
}
