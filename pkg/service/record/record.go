package record

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

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

type recorder struct {
	Logger *zap.Logger
}

func NewRecorder(logger *zap.Logger) Recorder {
	return &recorder{
		Logger: logger,
	}
}

func (r *recorder) CaptureTraffic(path string, proxyPort uint32, appCmd, appContainer, appNetwork string, Delay uint64, buildDelay time.Duration, ports []uint, filters *models.Filters, disableTele bool) {

	var ps *proxy.ProxySet
	stopper := make(chan os.Signal, 1)
	signal.Notify(stopper, os.Interrupt, os.Kill, syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGKILL)

	models.SetMode(models.MODE_RECORD)
	teleFS := fs.NewTeleFS(r.Logger)
	tele := telemetry.NewTelemetry(!disableTele, false, teleFS, r.Logger, "", nil)
	tele.Ping(false)

	dirName, err := yaml.NewSessionIndex(path, r.Logger)
	if err != nil {
		r.Logger.Error("Failed to create the session index file", zap.Error(err))
		return
	}

	ys := yaml.NewYamlStore(path+"/"+dirName+"/tests", path+"/"+dirName, "", "", r.Logger, tele)
	routineId := pkg.GenerateRandomID()
	// Initiate the hooks and update the vaccant ProxyPorts map
	loadedHooks := hooks.NewHook(ys, routineId, r.Logger)

	// Recover from panic and gracfully shutdown
	defer loadedHooks.Recover(routineId)

	mocksTotal := make(map[string]int)
	testsTotal := 0
	ctx := context.WithValue(context.Background(), "mocksTotal", &mocksTotal)
	ctx = context.WithValue(ctx, "testsTotal", &testsTotal)

	select {
	case <-stopper:
		return
	default:
		// load the ebpf hooks into the kernel
		if err := loadedHooks.LoadHooks(appCmd, appContainer, 0, ctx, filters); err != nil {
			return
		}
	}

	select {
	case <-stopper:
		loadedHooks.Stop(true)
		return
	default:
		// start the BootProxy
		ps = proxy.BootProxy(r.Logger, proxy.Option{Port: proxyPort}, appCmd, appContainer, 0, "", ports, loadedHooks, ctx)
	}

	//proxy fetches the destIp and destPort from the redirect proxy map
	//Sending Proxy Ip & Port to the ebpf program
	if err := loadedHooks.SendProxyInfo(ps.IP4, ps.Port, ps.IP6); err != nil {
		return
	}

	// Channels to communicate between different types of closing keploy
	abortStopHooksInterrupt := make(chan bool) // channel to stop closing of keploy via interrupt
	exitCmd := make(chan bool)                 // channel to exit this command
	abortStopHooksForcefully := false          // boolen to stop closing of keploy via user app error

	select {
	case <-stopper:
		loadedHooks.Stop(true)
		ps.StopProxyServer()
		return
	default:
		// start user application
		go func() {
			stopApplication := false
			if err := loadedHooks.LaunchUserApplication(appCmd, appContainer, appNetwork, Delay, buildDelay, false); err != nil {
				switch err {
				case hooks.ErrInterrupted:
					r.Logger.Info("keploy terminated user application")
					return
				case hooks.ErrCommandError:
				case hooks.ErrUnExpected:
					r.Logger.Warn("user application terminated unexpectedly hence stopping keploy, please check application logs if this behaviour is not expected")
				case hooks.ErrDockerError:
					stopApplication = true
				default:
					r.Logger.Error("unknown error recieved from application", zap.Error(err))
				}
			}
			if !abortStopHooksForcefully {
				abortStopHooksInterrupt <- true
				// stop listening for the eBPF events
				loadedHooks.Stop(!stopApplication)
				//stop listening for proxy server
				ps.StopProxyServer()
				exitCmd <- true
			} else {
				return
			}
		}()
	}

	select {
	case <-stopper:
		abortStopHooksForcefully = true
		loadedHooks.Stop(false)
		if testsTotal != 0 {
			tele.RecordedTestSuite(dirName, testsTotal, mocksTotal)
		}
		ps.StopProxyServer()
		return
	case <-abortStopHooksInterrupt:
		if testsTotal != 0 {
			tele.RecordedTestSuite(path, testsTotal, mocksTotal)
		}

	}

	<-exitCmd
}
