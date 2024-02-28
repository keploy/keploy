package core

import (
	"context"
	"go.keploy.io/server/v2/pkg/core/app"
	"go.keploy.io/server/v2/utils"
	"sync"

	"go.keploy.io/server/v2/pkg/models"
)

type Hooks interface {
	DestInfo
	AppInfo
	Load(ctx context.Context, id uint64, cfg HookCfg) error
	Record(ctx context.Context, id uint64) (<-chan *models.TestCase, <-chan error)
}

type HookCfg struct {
	AppID      uint64
	Pid        uint32
	IsDocker   bool
	KeployIPV4 string
}

type App interface {
	Setup(ctx context.Context, opts app.AppOptions) error
	Run(ctx context.Context, opts app.AppOptions) error
	Kind(ctx context.Context) utils.CmdType
	KeployIPv4Addr() string
}

// Proxy listens on all available interfaces and forwards traffic to the destination
type Proxy interface {
	StartProxy(ctx context.Context, opts ProxyOptions) error
	Record(ctx context.Context, id uint64, mocks chan<- *models.Mock, opts models.OutgoingOptions) error
	Mock(ctx context.Context, id uint64, opts models.OutgoingOptions) error
	SetMocks(ctx context.Context, id uint64, filtered []*models.Mock, unFiltered []*models.Mock) error
}

type ProxyOptions struct {
	// DnsIPv4Addr is the proxy IP returned by the DNS server. default is loopback address
	DnsIPv4Addr string
	// DnsIPv6Addr is the proxy IP returned by the DNS server. default is loopback address
	DnsIPv6Addr string
}

type DestInfo interface {
	Get(ctx context.Context, srcPort uint16) (*NetworkAddress, error)
	Delete(ctx context.Context, srcPort uint16) error
}

type AppInfo interface {
	SendInode(ctx context.Context, id uint64, inode uint64) error
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
	ID   uint64
	Mode models.Mode
	TC   chan<- *models.TestCase
	MC   chan<- *models.Mock
	models.OutgoingOptions
}
