package record

import (
	"go.keploy.io/server/pkg/hooks"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/platform/yaml"
	"go.keploy.io/server/pkg/proxy"
	"go.uber.org/zap"
)

type recorder struct {
	logger *zap.Logger
}

func NewRecorder (logger *zap.Logger) Recorder {
	return &recorder{
		logger: logger,
	}
}

func (r *recorder) CaptureTraffic(tcsPath, mockPath string)  {
	models.SetMode(models.MODE_RECORD)

	ys := yaml.NewYamlStore(tcsPath, mockPath, r.logger)
	// start the proxies
	ps := proxy.BootProxies(r.logger, proxy.Option{})
	// Initiate the hooks and update the vaccant ProxyPorts map
	loadedHooks := hooks.NewHook(ps.PortList, ys, r.logger)
	loadedHooks.LoadHooks()
	// proxy update its state in the ProxyPorts map
	ps.SetHook(loadedHooks)

	// stop listening for the eBPF events
	loadedHooks.Stop()
}