package proxy

import (
	"net"
	"net/url"
	"sync"

	"go.keploy.io/server/v3/pkg/models"
)

var recorded recordedPorts

func RecordedPort(port uint32) bool { return recorded.has(port) && !recorded.child(port) }

type recordedPorts struct {
	mu      sync.RWMutex
	ports   map[uint32]struct{}
	kids    map[uint32]struct{}
	plain   map[uint32]struct{}
	unknown bool
}

func (r *recordedPorts) add(sets ...[]*models.Mock) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, mocks := range sets {
		for _, m := range mocks {
			if m == nil || m.Kind == models.DNS {
				continue
			}
			addr := m.Spec.Metadata["destAddr"]
			if addr == "" {
				addr, _ = m.RecordedDestination()
			}
			port, ok := portFromAddr(addr)
			if !ok {
				port, ok = defaultPort(m, addr)
			}
			if !ok {
				r.unknown = true
				continue
			}
			if r.ports == nil {
				r.ports = map[uint32]struct{}{}
			}
			r.ports[port] = struct{}{}
			if m.Spec.Metadata["startedByTests"] == "true" {
				if r.kids == nil {
					r.kids = map[uint32]struct{}{}
				}
				r.kids[port] = struct{}{}
			} else {
				if r.plain == nil {
					r.plain = map[uint32]struct{}{}
				}
				r.plain[port] = struct{}{}
			}
		}
	}
}

func (r *recordedPorts) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ports, r.kids, r.plain, r.unknown = nil, nil, nil, false
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
	if r.unknown {
		return true
	}
	_, ok := r.ports[port]
	return ok
}

func (r *recordedPorts) child(port uint32) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, kid := r.kids[port]
	_, plain := r.plain[port]
	return kid && !plain
}
