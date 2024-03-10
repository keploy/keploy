package mocktest

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"sync"
	"syscall"

	"go.keploy.io/server/pkg"
	"go.keploy.io/server/pkg/hooks"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/platform/fs"
	"go.keploy.io/server/pkg/platform/telemetry"
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

func (s *mockTester) MockTest(path string, proxyPort, pid uint32, mockName string, enableTele bool) {

	models.SetMode(models.MODE_TEST)
	teleFS := fs.NewTeleFS(s.logger)
	tele := telemetry.NewTelemetry(enableTele, false, teleFS, s.logger, "", nil)
	tele.Ping(false)
	ys := yaml.NewYamlStore(path, path, "", mockName, s.logger, tele)
	s.logger.Debug("path of mocks : " + path)

	routineId := pkg.GenerateRandomID()
	ctx := context.Background()
	// Initiate the hooks
	loadedHooks, err := hooks.NewHook(ys, routineId, s.logger)
	if err != nil {
		s.logger.Error("error while creating hooks", zap.Error(err))
		return
	}

	if err := loadedHooks.LoadHooks("", "", pid, ctx, nil, false); err != nil {
		return
	}

	// start the proxy
	ps := proxy.BootProxy(s.logger, proxy.Option{Port: proxyPort}, "", "", pid, "", []uint{}, loadedHooks, ctx, 0)

	// proxy update its state in the ProxyPorts map
	// Sending Proxy Ip & Port to the ebpf program
	if err := loadedHooks.SendProxyInfo(ps.IP4, ps.Port, ps.IP6); err != nil {
		return
	}

	// Sending the Dns Port to the ebpf program
	if err := loadedHooks.SendDnsPort(ps.DnsPort); err != nil {
		return
	}

	tcsMocks, err := ys.ReadTcsMocks(&models.TestCase{}, "")
	if err != nil {
		loadedHooks.Stop(true)
		ps.StopProxyServer()
		return
	}
	readTcsMocks := []*models.Mock{}

	for _, mock := range tcsMocks {
		tcsmock, ok := mock.(*models.Mock)
		if !ok {
			continue
		}
		readTcsMocks = append(readTcsMocks, tcsmock)
	}

	sort.SliceStable(readTcsMocks, func(i, j int) bool {
		return readTcsMocks[i].Spec.ReqTimestampMock.Before(readTcsMocks[j].Spec.ReqTimestampMock)
	})

	configMocks, err := ys.ReadConfigMocks("")

	if err != nil {
		loadedHooks.Stop(true)
		ps.StopProxyServer()
		return
	}

	readConfigMocks := []*models.Mock{}
	for _, mock := range configMocks {
		configmock, ok := mock.(*models.Mock)
		if !ok {
			continue
		}
		readConfigMocks = append(readConfigMocks, configmock)
	}

	loadedHooks.SetConfigMocks(readConfigMocks)
	loadedHooks.SetTcsMocks(readTcsMocks)

	mocksBefore := len(configMocks) + len(tcsMocks)

	// Listen for the interrupt signal
	stopper := make(chan os.Signal, 1)
	signal.Notify(stopper, syscall.SIGINT, syscall.SIGTERM)

	fmt.Printf(Emoji+"Received signal:%v\n", <-stopper)

	s.logger.Info("Received signal, initiating graceful shutdown...")

	currentConfigMocks, err := loadedHooks.GetConfigMocks()
	if err != nil {
		s.logger.Error("Failed to get config-mocks", zap.Error(err))
	}
	currentMocks, err := loadedHooks.GetTcsMocks()
	if err != nil {
		s.logger.Error("Failed to get mocks", zap.Error(err))
	}

	usedMocks := mocksBefore - (len(currentConfigMocks) + len(currentMocks))
	//Call the telemetry events.
	if usedMocks != 0 {
		tele.MockTestRun(usedMocks)
	}
	// Shutdown other resources
	loadedHooks.Stop(true)
	ps.StopProxyServer()
}
