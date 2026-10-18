package replay

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"sync"

	"go.keploy.io/server/v3/pkg"
	"go.uber.org/zap"
)

// publishedPorts is, for an instrumentation that can tell it
// (pkg.AppPortPublishing, Docker mode), which host port each app port's tests
// go to from the host when host port appPort does not reach the app: the one
// host port the app's port is published on (pkg.PublishedTarget). It asks the
// instrumentation once a run of a test set for each address and app port, as
// a docker-compose replay starts the agent container, which publishes the
// app's ports, again for each run of a set (each attempt too: a port Docker
// picks may differ when the agent starts again), and again whenever the
// instrumentation could not tell. It says each redirect once, at Info.
type publishedPorts struct {
	pub    pkg.AppPortPublishing
	logger *zap.Logger

	mu    sync.Mutex
	set   string
	gen   uint64 // the run of set the answers in ports are for (newRun)
	ports map[string]uint16
	said  map[[2]uint16]bool
}

// newRun begins a run of test set testSetID (RunTestSet, each attempt): what
// an earlier run of it was told is forgotten, and an answer to a question the
// earlier run asked is not kept.
func (p *publishedPorts) newRun(testSetID string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.set, p.ports = testSetID, map[string]uint16{}
	p.gen++
}

// publishedPortsByInstrumentation holds one publishedPorts for each
// instrumentation, so that the replayer's readiness probes and its hooks'
// tests, which each hold the instrumentation, ask once and say once.
var publishedPortsByInstrumentation sync.Map

// publishedPortsOf is instrumentation's publishedPorts: nil when it cannot
// tell where the app's ports are published, and then tests go where they were
// recorded, as without it.
func publishedPortsOf(logger *zap.Logger, instrumentation Instrumentation) *publishedPorts {
	pub, ok := instrumentation.(pkg.AppPortPublishing)
	if !ok || pub == nil {
		return nil
	}
	fresh := &publishedPorts{pub: pub, logger: logger, ports: map[string]uint16{}, said: map[[2]uint16]bool{}}
	if !reflect.TypeOf(instrumentation).Comparable() {
		return fresh // no key to share it by
	}
	p, _ := publishedPortsByInstrumentation.LoadOrStore(instrumentation, fresh)
	return p.(*publishedPorts)
}

// of is test set testSetID's pkg.SimulationConfig.PublishedPort: nil when there
// is nothing to ask.
func (p *publishedPorts) of(testSetID string) func(ctx context.Context, host string, appPort uint16) uint16 {
	if p == nil {
		return nil
	}
	return func(ctx context.Context, host string, appPort uint16) uint16 {
		key := host + " " + strconv.Itoa(int(appPort))
		p.mu.Lock()
		if p.set != testSetID {
			p.set, p.ports = testSetID, map[string]uint16{}
			p.gen++
		}
		port, asked := p.ports[key]
		gen := p.gen
		p.mu.Unlock()
		if asked {
			return port
		}
		port, known := p.pub.PublishedHostPort(ctx, host, appPort)
		p.mu.Lock()
		defer p.mu.Unlock()
		if known && p.set == testSetID && p.gen == gen {
			p.ports[key] = port
		}
		if port != 0 && !p.said[[2]uint16{appPort, port}] {
			p.said[[2]uint16{appPort, port}] = true
			p.logger.Info(fmt.Sprintf("app port %d is published on host port %d; sending these tests there", appPort, port),
				zap.String("testSetID", testSetID))
		}
		return port
	}
}
