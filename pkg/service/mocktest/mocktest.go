package mocktest

import (
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"context"

	"go.keploy.io/server/pkg"
	"go.keploy.io/server/pkg/hooks"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/platform/yaml"
	"go.keploy.io/server/pkg/platform/fs"
	"go.keploy.io/server/pkg/platform/telemetry"
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

	teleFS := fs.NewTeleFS()
	tele := telemetry.NewTelemetry(true, false, teleFS, s.logger, "", nil)
	s.logger.Debug("path of mocks : " + path)

	routineId := pkg.GenerateRandomID()
	ctx := context.Background()
	// Initiate the hooks
	loadedHooks := hooks.NewHook(ys, routineId, s.logger)
	if err := loadedHooks.LoadHooks("", "", pid, ctx); err != nil {
		return
	}

	// start the proxy
	ps := proxy.BootProxy(s.logger, proxy.Option{}, "", "", pid, "", []uint{}, loadedHooks, ctx)

	// proxy update its state in the ProxyPorts map
	// ps.SetHook(loadedHooks)

	// Sending Proxy Ip & Port to the ebpf program
	if err := loadedHooks.SendProxyInfo(ps.IP4, ps.Port, ps.IP6); err != nil {
		return
	}

	configMocks, tcsMocks, err := ys.ReadMocks("")

	if err != nil {
		loadedHooks.Stop(true, nil)
		ps.StopProxyServer()
		return
	}

	loadedHooks.SetConfigMocks(configMocks)
	loadedHooks.SetTcsMocks(tcsMocks)

	mocksBefore := len(configMocks) + len(tcsMocks)

	// Listen for the interrupt signal
	stopper := make(chan os.Signal, 1)
	signal.Notify(stopper, syscall.SIGINT, syscall.SIGTERM)

	fmt.Printf(Emoji+"Received signal:%v\n", <-stopper)

	s.logger.Info("Received signal, initiating graceful shutdown...")
	usedMocks := mocksBefore - ( len(loadedHooks.GetConfigMocks()) + len(loadedHooks.GetTcsMocks()) )
	//Call the telemetry events.
	tele.MockTestRun("mocktest", usedMocks)
	// Shutdown other resources
	loadedHooks.Stop(true, nil)
	ps.StopProxyServer()
}
