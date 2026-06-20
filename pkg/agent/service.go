package agent

import (
	"context"
	"io"
	"sync"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

type Hooks interface {
	// AppInfo
	DestInfo
	Load(ctx context.Context, cfg HookCfg, setupOpts config.Agent) error
	WatchBindEvents(ctx context.Context) (<-chan models.IngressEvent, error)
}

type HookCfg struct {
	Pid        uint32
	IsDocker   bool
	Mode       models.Mode
	Rules      []models.BypassRule
	Port       uint32
	CgroupPath string // optional: explicit cgroupv2 path override (used by DaemonSet agent)
}

type AuxiliaryProxyHook interface {
	AfterStart(ctx context.Context, proxy Proxy) error
}

// Proxy listens on all available interfaces and forwards traffic to the destination.
//
// Proxy is the stable contract; window-aware methods live on the optional
// WindowedProxy interface below so adding them does not break third-party
// implementers. Callers should type-assert when they need windowing —
// see agent.go's UpdateMockParams for the canonical pattern.
type Proxy interface {
	StartProxy(ctx context.Context, opts ProxyOptions) error
	Record(ctx context.Context, mocks chan<- *models.Mock, opts models.OutgoingOptions) error
	Mock(ctx context.Context, opts models.OutgoingOptions) error
	SetMocks(ctx context.Context, filtered []*models.Mock, unFiltered []*models.Mock) error
	GetConsumedMocks(ctx context.Context) ([]models.MockState, error)
	GetMockErrors(ctx context.Context) ([]models.UnmatchedCall, error)
	MakeClientDeRegisterd(ctx context.Context) error
	GetErrorChannel() <-chan error
	// SetGracefulShutdown sets a flag to indicate the application is shutting down gracefully.
	// When this flag is set, connection errors will be logged as debug instead of error.
	SetGracefulShutdown(ctx context.Context) error
	Mapping(ctx context.Context, mappingCh chan models.TestMockMapping)
	GetDestInfo() DestInfo
	GetIntegrations() map[integrations.IntegrationType]integrations.Integrations
	GetSession() *Session
	SetAuxiliaryHook(h AuxiliaryProxyHook)
}

// PcapStreamer is the optional extension implemented by proxies
// that broadcast captured packets to dynamic subscribers. Callers
// MUST type-assert from Proxy and gracefully fall back when the
// assertion fails — this keeps third-party Proxy implementations
// compiling without forcing them to implement packet capture.
//
// SubscribePcap registers w to receive every captured frame as a
// pcap byte stream (file header followed by one record per frame).
// flush, when non-nil, is invoked after each frame so chunked
// transports (HTTP) can push bytes immediately. The unsubscribe
// func MUST be called when the consumer is done — typically on the
// HTTP request context's cancellation.
//
// Why streaming, not pulling: the cluster live-recording use case
// has no defined "stop" — the recorder is always connected. A
// fetch-on-stop model would never deliver any bytes. Subscribers
// always receive a fresh pcap header and frames from then on, so
// disconnects do not corrupt the stream of any other consumer.
type PcapStreamer interface {
	SubscribePcap(w io.Writer, flush func()) (func(), error)
}

// WindowedProxy is the optional extension implemented by proxies that
// support per-test [req,res] window enforcement (Option-1 strict
// containment). Callers MUST type-assert from Proxy and gracefully
// fall back when the assertion fails — this keeps third-party Proxy
// implementations compiling without the extra methods.
//
//	if wp, ok := p.(WindowedProxy); ok {
//	    _ = wp.SetMocksWithWindow(ctx, f, u, start, end)
//	} else {
//	    _ = p.SetMocks(ctx, f, u)
//	}
type WindowedProxy interface {
	// SetMocksWithWindow atomically replaces mocks AND publishes the active
	// outer-test [req,res] window.
	SetMocksWithWindow(ctx context.Context, filtered, unFiltered []*models.Mock, start, end time.Time) error
}

// FirstWindowStartReader is an optional extension implemented by proxies
// that can report the earliest test window start observed by their
// underlying MockManager. Consumed by the agent's tier-aware
// strictMockWindow filter to distinguish startup-init mocks (req_ts <
// firstWindowStart) from stale previous-test mocks (firstWindowStart <=
// req_ts < currentStart). Returns the zero time before the first real
// test window has landed.
//
// Callers MUST type-assert from Proxy and gracefully fall back (zero
// time = "no cutoff known, keep legacy behaviour") when the assertion
// fails — keeps third-party Proxy implementations compiling without
// the extra method.
type FirstWindowStartReader interface {
	FirstTestWindowStart() time.Time
}

type IncomingProxy interface {
	Start(ctx context.Context, opts models.IncomingOptions) chan *models.TestCase
}

type ProxyOptions struct {
	// DNSIPv4Addr is the proxy IP returned by the DNS server. default is loopback address
	DNSIPv4Addr string
	// DNSIPv6Addr is the proxy IP returned by the DNS server. default is loopback address
	DNSIPv6Addr string
}

type DestInfo interface {
	Get(ctx context.Context, srcPort uint16) (*NetworkAddress, error)
	Delete(ctx context.Context, srcPort uint16) error
}

type TestBenchInfo interface {
	// SendKeployPids(key models.ModeKey, pid uint32) error
	// SendKeployPorts(key models.ModeKey, port uint32) error
}

type NetworkAddress struct {
	Version  uint32
	IPv4Addr uint32
	IPv6Addr [4]uint32
	Port     uint32
}

// Sessions provides a thread-safe store for Session objects, keyed by ID.
// Used by the hooks packages (linux, windows, others) to track client sessions.
type Sessions struct {
	sessions sync.Map
}

func NewSessions() *Sessions {
	return &Sessions{
		sessions: sync.Map{},
	}
}

func (s *Sessions) Get(id uint64) (*Session, bool) {
	v, ok := s.sessions.Load(id)
	if !ok {
		return nil, false
	}
	return v.(*Session), true
}

func (s *Sessions) Set(id uint64, session *Session) {
	s.sessions.Store(id, session)
}

func (s *Sessions) Delete(id uint64) {
	s.sessions.Delete(id)
}

type Session struct {
	ID   uint64
	Mode models.Mode
	TC   chan<- *models.TestCase
	MC   chan<- *models.Mock
	models.OutgoingOptions
}
