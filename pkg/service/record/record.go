package record

import (
	"go.uber.org/zap"

	"go.keploy.io/server/pkg/hooks"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/persistence"
	"go.keploy.io/server/pkg/platform/yaml"
	"go.keploy.io/server/pkg/proxy"
)

type recorder struct {
	fileSystem persistence.FileSystem
	logger     *zap.Logger
}

func NewRecorder(fileSystem persistence.FileSystem, logger *zap.Logger) Recorder {
	return &recorder{
		fileSystem: fileSystem,
		logger:     logger,
	}
}

func (r *recorder) CaptureTraffic(tcsPath, mockPath string, pid uint32) {
	models.SetMode(models.MODE_RECORD)

	ys := yaml.NewYamlStore(tcsPath, mockPath, r.fileSystem, r.logger)
	// start the proxies
	ps := proxy.BootProxies(r.logger, proxy.Option{})
	// Initiate the hooks and update the vaccant ProxyPorts map
	loadedHooks := hooks.NewHook(ps.PortList, ys, r.logger)
	if err := loadedHooks.LoadHooks(pid); err != nil {
		return
	}
	// proxy update its state in the ProxyPorts map
	ps.SetHook(loadedHooks)

	// stop listening for the eBPF events
	loadedHooks.Stop(false)
}
