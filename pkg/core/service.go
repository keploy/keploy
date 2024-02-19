package core

import (
	"context"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"time"
)

type Config struct {
	Port uint32
}

type Hooks interface {
	Load(ctx context.Context, tests chan models.Frame, opts HookOptions) error
}

type HookOptions struct {
	Pid      uint32
	IsDocker bool
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
	Record(ctx context.Context, mocks chan models.Frame, opts ProxyOptions) error
	Mock(ctx context.Context, mocks []models.Frame, opts ProxyOptions) error
	SetMocks(ctx context.Context, mocks []models.Frame) error
}

type ProxyOptions struct {
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

type IPVersion int

const (
	IPv4 IPVersion = iota // 0
	IPv6                  // 1
)

type NetworkAddress struct {
	Version  IPVersion
	IPv4Addr string
	IPv6Addr string
	Port     uint32
}
