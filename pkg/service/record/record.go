package record

import (
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

type recorder struct {
	logger *zap.Logger
}

func NewRecorder(logger *zap.Logger) Recorder {
	return &recorder{
		logger: logger,
	}
}

// func (r *recorder) CaptureTraffic(tcsPath, mockPath string, appCmd, appContainer, appNetwork string, Delay uint64) {
func (r *recorder) CaptureTraffic(path string, appCmd, appContainer, appNetwork string, Delay uint64, ports []uint) {
	models.SetMode(models.MODE_RECORD)

	dirName, err := yaml.NewSessionIndex(path, r.logger)
	if err != nil {
		r.logger.Error("Failed to create the session index file", zap.Error(err))
		return
	}


	//Initiate the telemetry.
	store := fs.NewTeleFS()
	tele := telemetry.NewTelemetry(true, false, store, r.logger, "", nil)



	ys := yaml.NewYamlStore(path+"/"+dirName+"/tests", path+"/"+dirName, "", "", r.logger)

	routineId := pkg.GenerateRandomID()
	// Initiate the hooks and update the vaccant ProxyPorts map
	loadedHooks := hooks.NewHook(ys, routineId, r.logger)

	// Recover from panic and gracfully shutdown
	defer loadedHooks.Recover(routineId)

	testsTotal := 0
	// load the ebpf hooks into the kernel
	if err := loadedHooks.LoadHooks(appCmd, appContainer, 0, &testsTotal); err != nil {
		return
	}

	// start the BootProxy
	mockTotal := make(map[string]int)
	ps := proxy.BootProxy(r.logger, proxy.Option{}, appCmd, appContainer, 0, "", ports, loadedHooks, mockTotal)

	//proxy fetches the destIp and destPort from the redirect proxy map
	// ps.SetHook(loadedHooks)

	//Sending Proxy Ip & Port to the ebpf program
	if err := loadedHooks.SendProxyInfo(ps.IP4, ps.Port, ps.IP6); err != nil {
		return
	}
	// time.

	// start user application
	if err := loadedHooks.LaunchUserApplication(appCmd, appContainer, appNetwork, Delay); err != nil {
		r.logger.Error("failed to process user application hence stopping keploy", zap.Error(err))
		loadedHooks.Stop(true)
		ps.StopProxyServer()
		return
	}

	// Enable Pid Filtering
	// loadedHooks.EnablePidFilter()
	// ps.FilterPid = true

	// stop listening for the eBPF events
	loadedHooks.Stop(false)
	//Call the telemetry events.
	tele.RecordedMock(mockTotal)
	tele.RecordedTest(dirName, testsTotal)

	//stop listening for proxy server
	ps.StopProxyServer()
}
