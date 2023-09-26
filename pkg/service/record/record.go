package record

import (
	"go.keploy.io/server/pkg"
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
func (r *recorder) CaptureTraffic(path string, appCmd, appContainer, appNetwork string, Delay uint64, ports []uint) {
	models.SetMode(models.MODE_RECORD)

	dirName, err := yaml.NewSessionIndex(path, r.logger)
	if err != nil {
		return
	}

	ys := yaml.NewYamlStore(path+"/"+dirName+"/tests", path+"/"+dirName, "", "", r.logger)

	routineId := pkg.GenerateRandomID()
	// Initiate the hooks and update the vaccant ProxyPorts map
	loadedHooks := hooks.NewHook(ys, routineId, r.logger)

	// Recover from panic and gracfully shutdown
	defer loadedHooks.Recover(routineId)

	// load the ebpf hooks into the kernel
	if err := loadedHooks.LoadHooks(appCmd, appContainer, 0); err != nil {
		return
	}

	// start the BootProxy
	ps := proxy.BootProxy(r.logger, proxy.Option{}, appCmd, appContainer, 0, "", ports, loadedHooks)

	//proxy fetches the destIp and destPort from the redirect proxy map
	// ps.SetHook(loadedHooks)

	//Sending Proxy Ip & Port to the ebpf program
	if err := loadedHooks.SendProxyInfo(ps.IP4, ps.Port, ps.IP6); err != nil {
		return
	}

	stopHooksAbort := make(chan bool)
	stoppedProxy := false

	// start user application
	go func() {
		if err := loadedHooks.LaunchUserApplication(appCmd, appContainer, appNetwork, Delay); err != nil {
			switch err {
			case hooks.ErrInterrupted:
				r.logger.Info("keploy terminated user application")
				return
			case hooks.ErrCommandError:
				r.logger.Error("failed to run user application hence stopping keploy", zap.Error(err))
			case hooks.ErrUnExpected:
				r.logger.Warn("user application terminated unexpectedly, please check application logs if this behaviour is expected")
			default:
				r.logger.Error("unknown error recieved from application")
			}
		}
		// stop listening for the eBPF events
		loadedHooks.Stop(true, nil)
		//stop listening for proxy server
		ps.StopProxyServer()
		stopHooksAbort <- true
		stoppedProxy = true
	}()

	loadedHooks.Stop(false, stopHooksAbort)
	if !stoppedProxy {
		ps.StopProxyServer()
	}
}
