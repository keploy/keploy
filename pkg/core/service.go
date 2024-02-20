package core

import (
	"context"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"sync"
	"time"
)

type Config struct {
	Port uint32
}

type Hooks interface {
	DestInfo
	Load(ctx context.Context, id uint64, opts HookOptions) error
	Record(ctx context.Context, id uint64) (<-chan *models.TestCase, <-chan error)
}

type HookOptions struct {
	AppID      uint64
	Pid        uint32
	IsDocker   bool
	KeployIPV4 string
}

type App interface {
	Setup(ctx context.Context, opts AppOptions) error
	Run(ctx context.Context) error
	Kind(ctx context.Context) utils.CmdType
	KeployIPv4Addr() string
}

type AppOptions struct {
	// canExit disables any error returned if the app exits by itself.
	//CanExit       bool
	Type          utils.CmdType
	DockerDelay   time.Duration
	DockerNetwork string
}

// Proxy listens on all available interfaces and forwards traffic to the destination
type Proxy interface {
	Record(ctx context.Context, id uint64, mocks chan<- *models.Mock, opts models.OutgoingOptions) error
	Mock(ctx context.Context, id uint64, mocks []*models.Mock, opts models.OutgoingOptions) error
	SetMocks(ctx context.Context, id uint64, mocks []*models.Mock) error
}

type ProxyOptions struct {
	appID uint64
	models.OutgoingOptions
	// DnsIPv4Addr is the proxy IP returned by the DNS server. default is loopback address
	DnsIPv4Addr string
	// DnsIPv6Addr is the proxy IP returned by the DNS server. default is loopback address
	DnsIPv6Addr string
}

type DestInfo interface {
	Get(ctx context.Context, srcPort uint16) (*NetworkAddress, error)
	Delete(ctx context.Context, srcPort uint16) error
}

type NetworkAddress struct {
	AppID    uint64
	Version  uint32
	IPv4Addr uint32
	IPv6Addr [4]uint32
	Port     uint32
}

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

type Session struct {
	ID    uint64
	Mode  models.Mode
	TC    chan<- *models.TestCase
	MC    chan<- *models.Mock
	Mocks []*models.Mock
	//TODO: replace mocks with unfilteredMocks and filteredMocks
	models.OutgoingOptions
}
