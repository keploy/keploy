package core

import (
	"context"
	"sync"

	"go.keploy.io/server/v2/pkg/core/app"
	"go.keploy.io/server/v2/utils"

	"go.keploy.io/server/v2/pkg/models"
)

type Hooks interface {
	AppInfo
	DestInfo
	OutgoingInfo
	TestBenchInfo
	Load(ctx context.Context, id uint64, cfg HookCfg) error
	Record(ctx context.Context, id uint64, opts models.IncomingOptions) (<-chan *models.TestCase, error)
}

type HookCfg struct {
	AppID      uint64
	Pid        uint32
	IsDocker   bool
	KeployIPV4 string
	Mode       models.Mode
}

type App interface {
	Setup(ctx context.Context, opts app.Options) error
	Run(ctx context.Context, inodeChan chan uint64, opts app.Options) error
	Kind(ctx context.Context) utils.CmdType
	KeployIPv4Addr() string
}

// Proxy listens on all available interfaces and forwards traffic to the destination
type Proxy interface {
	StartProxy(ctx context.Context, opts ProxyOptions) error
	Record(ctx context.Context, id uint64, mocks chan<- *models.Mock, opts models.OutgoingOptions) error
	Mock(ctx context.Context, id uint64, opts models.OutgoingOptions) error
	SetMocks(ctx context.Context, id uint64, filtered []*models.Mock, unFiltered []*models.Mock) error
	GetConsumedMocks(ctx context.Context, id uint64) ([]string, error)
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

type AppInfo interface {
	SendInode(ctx context.Context, id uint64, inode uint64) error
}

// For keploy test bench

type Tester interface {
	Setup(ctx context.Context, opts models.TestingOptions) error
}
type TestBenchInfo interface {
	SendKeployPids(key models.ModeKey, pid uint32) error
	SendKeployPorts(key models.ModeKey, port uint32) error
}

// ----------------------

type OutgoingInfo interface {
	PassThroughPortsInKernel(ctx context.Context, id uint64, ports []uint) error
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

func (s *Sessions) getAll() map[uint64]*Session {
	sessions := map[uint64]*Session{}
	s.sessions.Range(func(k, v interface{}) bool {
		sessions[k.(uint64)] = v.(*Session)
		return true
	})
	return sessions
}

func (s *Sessions) GetAllMC() []chan<- *models.Mock {
	sessions := s.getAll()
	var mc []chan<- *models.Mock
	for _, session := range sessions {
		mc = append(mc, session.MC)
	}
	return mc
}

type Session struct {
	ID   uint64
	Mode models.Mode
	TC   chan<- *models.TestCase
	MC   chan<- *models.Mock
	models.OutgoingOptions
}
