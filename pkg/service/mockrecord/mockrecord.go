package mockrecord

import (
	"context"
	"fmt"
	"os"
	"os/signal"
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

func (s *mockRecorder) MockRecord(path string, proxyPort uint32, pid uint32, mockName string) {

	models.SetMode(models.MODE_RECORD)
	teleFS := fs.NewTeleFS()
	tele := telemetry.NewTelemetry(true, false, teleFS, s.logger, "", nil)
	tele.Ping(false)
	ys := yaml.NewYamlStore(path, path, "", mockName, s.logger, tele)

	routineId := pkg.GenerateRandomID()

	mocksTotal := make(map[string]int)
	ctx := context.WithValue(context.Background(), "mocksTotal", &mocksTotal)
	//Add the name of the cmd to context.
	ctx = context.WithValue(ctx, "cmd", "mockrecord")
	// Initiate the hooks
	loadedHooks := hooks.NewHook(ys, routineId, s.logger)
	if err := loadedHooks.LoadHooks("", "", pid, ctx, nil); err != nil {
		return
	}
	// start the proxy
	ps := proxy.BootProxy(s.logger, proxy.Option{Port: proxyPort}, "", "", pid, "", []uint{}, loadedHooks, ctx)

	// proxy update its state in the ProxyPorts map
	// Sending Proxy Ip & Port to the ebpf program
	if err := loadedHooks.SendProxyInfo(ps.IP4, ps.Port, ps.IP6); err != nil {
		return
	}

	// Listen for the interrupt signal
	stopper := make(chan os.Signal, 1)
	signal.Notify(stopper, syscall.SIGINT, syscall.SIGTERM)

	fmt.Printf(Emoji+"Received signal:%v\n", <-stopper)

	s.logger.Info("Received signal, initiating graceful shutdown...")
	//Call the telemetry events.
	if len(mocksTotal) != 0 {
		tele.RecordedMocks(mocksTotal)
	}

	// Shutdown other resources
	loadedHooks.Stop(true)
	ps.StopProxyServer()
}
