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

func NewRecorder(logger *zap.Logger) Recorder {
	return &recorder{
		logger: logger,
	}
}

func (r *recorder) CaptureTraffic(tcsPath, mockPath string, appCmd, appContainer,appNetwork string, Delay uint64) {
	models.SetMode(models.MODE_RECORD)

	ys := yaml.NewYamlStore(tcsPath, mockPath, r.logger)
	// start the proxies
	ps := proxy.BootProxies(r.logger, proxy.Option{})
	// Initiate the hooks and update the vaccant ProxyPorts map
	loadedHooks := hooks.NewHook(ys, r.logger)
	if err := loadedHooks.LoadHooks(appCmd, appContainer); err != nil {
		return
	}

	//proxy fetches the destIp and destPort from the redirect proxy map
	ps.SetHook(loadedHooks)

	//Sending Proxy Ip & Port to the ebpf program
	if err := loadedHooks.SendProxyInfo(ps.IP, ps.Port); err != nil {
		return
	}

	// start user application
	if err := loadedHooks.LaunchUserApplication(appCmd, appContainer, appNetwork, Delay); err != nil {
		return
	}

	// Enable Pid Filtering
	loadedHooks.EnablePidFilter()
	ps.FilterPid = true

	// stop listening for the eBPF events
	loadedHooks.Stop(false)

	//stop listening for proxy server
	ps.StopProxyServer()
}
