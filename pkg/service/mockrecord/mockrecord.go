package mockrecord

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
	"go.keploy.io/server/pkg/proxy"
	"go.keploy.io/server/pkg/platform/telemetry"
	"go.keploy.io/server/pkg/platform/fs"
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

func (s *mockRecorder) MockRecord(path string, pid uint32, mockName string) {

	models.SetMode(models.MODE_RECORD)
	ys := yaml.NewYamlStore(path, path, "", mockName, s.logger)

	//Initiate the telemetry.
	store := fs.NewTeleFS()
	tele := telemetry.NewTelemetry(true, false, store, s.logger, "", nil)

	routineId := pkg.GenerateRandomID()

	mocksTotal := make(map[string]int)
	ctx := context.WithValue(context.Background(), "mocksTotal", &mocksTotal)
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

	// Listen for the interrupt signal
	stopper := make(chan os.Signal, 1)
	signal.Notify(stopper, syscall.SIGINT, syscall.SIGTERM)

	fmt.Printf(Emoji+"Received signal:%v\n", <-stopper)
	mocksRecorded := make(map[string]int)
	tcsMocks, configMocks, err := ys.ReadMocks(path)
	if err != nil{
		s.logger.Debug("Failed to read mocks")
	}
	tcsMocks = append(tcsMocks, configMocks...)
	for _, mock := range tcsMocks {
		mocksRecorded[string(mock.Kind)] ++
	}

	s.logger.Info("Received signal, initiating graceful shutdown...")

	// Shutdown other resources
	loadedHooks.Stop(true, nil)
	//Call the telemetry events.
	tele.RecordedMock("mockrecord", mocksRecorded)
	ps.StopProxyServer()
}
