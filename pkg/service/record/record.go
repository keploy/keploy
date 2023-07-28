package record

import (
	"go.keploy.io/server/pkg/hooks"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/platform/yaml"
	"go.keploy.io/server/pkg/proxy"
	"go.uber.org/zap"
)

var Emoji = "\U0001F430" + " Keploy:"

type recorder struct {
	logger *zap.Logger
}

func NewRecorder(logger *zap.Logger) Recorder {
	return &recorder{
		logger: logger,
	}
}

// func (r *recorder) CaptureTraffic(tcsPath, mockPath string, appCmd, appContainer, appNetwork string, Delay uint64) {
func (r *recorder) CaptureTraffic(path string, appCmd, appContainer, appNetwork string, Delay uint64) {
	models.SetMode(models.MODE_RECORD)

	ys := yaml.NewYamlStore(r.logger)
	// start the proxies
	// ps := proxy.BootProxies(r.logger, proxy.Option{}, appCmd)
	dirName, err := ys.NewSessionIndex(path)
	if err != nil {
		r.logger.Error("failed to find the directroy name for the session", zap.Error(err))
		return
	}
	path += "/" + dirName
	// Initiate the hooks and update the vaccant ProxyPorts map
	loadedHooks := hooks.NewHook(path, ys, r.logger)
	if err := loadedHooks.LoadHooks(appCmd, appContainer); err != nil {
		return
	}

	// start the proxies
	ps := proxy.BootProxies(r.logger, proxy.Option{}, appCmd)

	//proxy fetches the destIp and destPort from the redirect proxy map
	ps.SetHook(loadedHooks)

	//Sending Proxy Ip & Port to the ebpf program
	if err := loadedHooks.SendProxyInfo(ps.IP4, ps.Port, ps.IP6); err != nil {
		return
	}
	// time.

	// start user application
	if err := loadedHooks.LaunchUserApplication(appCmd, appContainer, appNetwork, Delay); err != nil {
		return
	}

	// Enable Pid Filtering
	// loadedHooks.EnablePidFilter()
	// ps.FilterPid = true

	// stop listening for the eBPF events
	loadedHooks.Stop(false)

	//stop listening for proxy server
	ps.StopProxyServer()
}
