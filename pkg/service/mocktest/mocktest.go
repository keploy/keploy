package mocktest

import (
	"sync"

	"go.keploy.io/server/pkg"
	"go.keploy.io/server/pkg/hooks"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/platform/yaml"
	"go.keploy.io/server/pkg/proxy"
	"go.uber.org/zap"
)

var Emoji = "\U0001F430" + " Keploy:"

type mockTester struct {
	logger *zap.Logger
	mutex  sync.Mutex
}

func NewMockTester(logger *zap.Logger) MockTester {
	return &mockTester{
		logger: logger,
		mutex:  sync.Mutex{},
	}
}

func (s *mockTester) MockTest(path string, pid uint32, mockName string) {

	models.SetMode(models.MODE_TEST)
	ys := yaml.NewYamlStore(path, path, "", mockName, s.logger)

	s.logger.Debug("path of mocks : " + path)

	routineId := pkg.GenerateRandomID()

	// Initiate the hooks
	loadedHooks := hooks.NewHook(ys, routineId, s.logger)
	if err := loadedHooks.LoadHooks("", "", pid); err != nil {
		return
	}

	// start the proxy
	ps := proxy.BootProxy(s.logger, proxy.Option{}, "", "", pid, "", []uint{}, loadedHooks)

	// proxy update its state in the ProxyPorts map
	// ps.SetHook(loadedHooks)

	// Sending Proxy Ip & Port to the ebpf program
	if err := loadedHooks.SendProxyInfo(ps.IP4, ps.Port, ps.IP6); err != nil {
		return
	}

	configMocks, tcsMocks, err := ys.ReadMocks("")

	if err != nil {
		loadedHooks.Stop(true)
		ps.StopProxyServer()
		return
	}

	loadedHooks.SetConfigMocks(configMocks)
	loadedHooks.SetTcsMocks(tcsMocks)

	// Shutdown other resources
	loadedHooks.Stop(false)
	ps.StopProxyServer()
}
