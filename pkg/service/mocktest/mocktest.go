package mocktest

import (
	"fmt"
	"path/filepath"
	"sync"

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

func (s *mockTester) MockTest(path string, Delay uint64, pid uint32, dirName string) {

	models.SetMode(models.MODE_TEST)
	ys := yaml.NewYamlStore(s.logger)

	s.logger.Debug("path of mocks : " + path)

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

	configMocks, tcsMocks, err := ys.ReadMocks(filepath.Join(path, dirName))
	if err != nil {
		loadedHooks.Stop(true)
		ps.StopProxyServer()
		return
	}

	s.logger.Debug(fmt.Sprintf("the config mocks for %s are: %v\nthe testcase mocks are: %v", dirName, configMocks, tcsMocks))
	loadedHooks.SetConfigMocks(configMocks)
	loadedHooks.SetTcsMocks(tcsMocks)

	// Shutdown other resources
	loadedHooks.Stop(false)
	ps.StopProxyServer()
}
