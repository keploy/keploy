package agent

import (
	"context"
	"sync"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
)

type Hooks interface {
	// AppInfo
	DestInfo
	Load(ctx context.Context, cfg HookCfg, setupOpts config.Agent) error
	WatchBindEvents(ctx context.Context) (<-chan models.IngressEvent, error)
}

type HookCfg struct {
	Pid      uint32
	IsDocker bool
	Mode     models.Mode
	Rules    []models.BypassRule
	Port     uint32
}

// Proxy listens on all available interfaces and forwards traffic to the destination
type Proxy interface {
	StartProxy(ctx context.Context, opts ProxyOptions) error
	Record(ctx context.Context, mocks chan<- *models.Mock, opts models.OutgoingOptions) error
	Mock(ctx context.Context, opts models.OutgoingOptions) error
	SetMocks(ctx context.Context, filtered []*models.Mock, unFiltered []*models.Mock) error
	GetConsumedMocks(ctx context.Context) ([]models.MockState, error)
	MakeClientDeRegisterd(ctx context.Context) error
	GetErrorChannel() <-chan error
	// SetGracefulShutdown sets a flag to indicate the application is shutting down gracefully.
	// When this flag is set, connection errors will be logged as debug instead of error.
	SetGracefulShutdown(ctx context.Context) error
	Mapping(ctx context.Context, mappingCh chan models.TestMockMapping)
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
