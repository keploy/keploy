package mockrecord

import (
	"sync"

	"go.keploy.io/server/pkg/hooks"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/platform/yaml"
	"go.keploy.io/server/pkg/proxy"
	"go.uber.org/zap"
)

var Emoji = "\U0001F430" + " Keploy:"

type mockRecorder struct {
	logger *zap.Logger
	mutex  sync.Mutex
}

func NewMockRecorder(logger *zap.Logger) MockRecorder {
	return &mockRecorder{
		logger: logger,
		mutex:  sync.Mutex{},
	}
}

func (s *mockRecorder) MockRecord(path string, Delay uint64, pid uint32, dirName string) {

	models.SetMode(models.MODE_RECORD)
	ys := yaml.NewYamlStore(s.logger)

	path += "/" + dirName

	// Initiate the hooks
	loadedHooks := hooks.NewHook(path, ys, s.logger)
	if err := loadedHooks.LoadHooks("", "", pid); err != nil {
		return
	}

	// start the proxy
	ps := proxy.BootProxy(s.logger, proxy.Option{}, "", "", pid, "")

	// proxy update its state in the ProxyPorts map
	ps.SetHook(loadedHooks)

	// Sending Proxy Ip & Port to the ebpf program
	if err := loadedHooks.SendProxyInfo(ps.IP4, ps.Port, ps.IP6); err != nil {
		return
	}

	// Shutdown other resources
	loadedHooks.Stop(false)
	ps.StopProxyServer()
}
