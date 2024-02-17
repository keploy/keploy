package core

import (
	"context"
	"go.keploy.io/server/v2/pkg/models"
	"time"

	"github.com/cloudflare/cfssl/log"
	"go.keploy.io/server/v2/pkg/hooks"
	"go.keploy.io/server/v2/pkg/proxy"
	"go.uber.org/zap"
)

type Config struct {
	Port uint32
}

type Hooks interface {
	Load(ctx context.Context, tests chan models.Frame, opts HookOptions) error
}

type HookOptions struct {
	Pid uint32
}

type App interface {
	Run(ctx context.Context, cmd string, opts AppOptions) error
}

type AppOptions struct {
	// canExit disables any error returned if the app exits by itself.
	CanExit       bool
	BuildDelay    time.Duration
	Container     string
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

// Init will initialize: hooks and proxy
func Init(ctx context.Context, config Config) error {
	// disable init logs from the cfssl library
	log.Level = 0

	// Initiate the hooks
	loadedHooks, err := hooks.NewHook(ys, routineId, s.logger)
	if err != nil {
		s.logger.Error("error while creating hooks", zap.Error(err))
		return
	}

	if err := loadedHooks.LoadHooks("", "", pid, ctx, nil); err != nil {
		return
	}

	// start the proxy
	ps := proxy.Sart(s.logger, proxy.Option{Port: proxyPort}, "", "", pid, "", []uint{}, loadedHooks, ctx, 0)
}
