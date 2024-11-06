//go:build linux

package agent

import (
	"context"
	"fmt"
	"sync"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/agent/hooks/structs"
	"go.keploy.io/server/v2/pkg/client/app"
	"go.keploy.io/server/v2/utils"

	"go.keploy.io/server/v2/pkg/models"
)

type Hooks interface {
	DestInfo
	OutgoingInfo
	Load(ctx context.Context, id uint64, cfg HookCfg) error
	Record(ctx context.Context, id uint64, opts models.IncomingOptions) (<-chan *models.TestCase, error)
	// send KeployClient Pid
	SendKeployClientInfo(clientID uint64, clientInfo structs.ClientInfo) error
	DeleteKeployClientInfo(clientID uint64) error
	SendClientProxyInfo(clientID uint64, proxyInfo structs.ProxyInfo) error
}

type HookCfg struct {
	ClientID   uint64
	Pid        uint32
	IsDocker   bool
	KeployIPV4 string
	Mode       models.Mode
	Rules      []config.BypassRule
}

type App interface {
	Setup(ctx context.Context, opts app.Options) error
	Run(ctx context.Context, opts app.Options) error
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
	MakeClientDeRegisterd(ctx context.Context) error
}

type ProxyOptions struct {
	ProxyPort  uint32
	// DNSIPv4Addr is the proxy IP returned by the DNS server. default is loopback address
	DNSIPv4Addr string
	// DNSIPv6Addr is the proxy IP returned by the DNS server. default is loopback address
	DNSIPv6Addr string
}

type DestInfo interface {
	Get(ctx context.Context, srcPort uint16) (*NetworkAddress, error)
	Delete(ctx context.Context, srcPort uint16) error
}

// For keploy test bench

type Tester interface {
	Setup(ctx context.Context, opts models.TestingOptions) error
}
type TestBenchInfo interface {
	// SendKeployPids(key models.ModeKey, pid uint32) error
	// SendKeployPorts(key models.ModeKey, port uint32) error
}

// ----------------------

type OutgoingInfo interface {
}

type NetworkAddress struct {
	ClientID uint64
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
	fmt.Println("Inside Get of Sessions !!", id)
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
