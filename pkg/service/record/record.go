package record

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"go.keploy.io/server/pkg"
	"go.keploy.io/server/pkg/hooks"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/platform"
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

func (r *recorder) StartCaptureTraffic(options models.RecordOptions) {
	teleFS := fs.NewTeleFS(r.Logger)
	tele := telemetry.NewTelemetry(options.EnableTele, false, teleFS, r.Logger, "", nil)
	tele.Ping(false)
	dirName, err := yaml.NewSessionIndex(options.Path, r.Logger)
	if err != nil {
		r.Logger.Error("Failed to create the session index file", zap.Error(err))
		return
	}
	tcDB := yaml.NewYamlStore(options.Path+"/"+dirName+"/tests", options.Path+"/"+dirName, "", "", r.Logger, tele)
	r.CaptureTraffic(options, dirName, tcDB, tele)
}

func (r *recorder) CaptureTraffic(options models.RecordOptions, dirName string, ys platform.TestCaseDB, tele *telemetry.Telemetry) {

	var ps *proxy.ProxySet
	stopper := make(chan os.Signal, 1)
	signal.Notify(stopper, os.Interrupt, os.Kill, syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGKILL)

	models.SetMode(models.MODE_RECORD)
	tele.Ping(false)
	routineId := pkg.GenerateRandomID()
	// Initiate the hooks and update the vaccant ProxyPorts map
	loadedHooks, err := hooks.NewHook(ys, routineId, r.Logger)
	loadedHooks.SetPassThroughHosts(options.PassThroughHosts)
	if err != nil {
		r.Logger.Error("error while creating hooks", zap.Error(err))
		return
	}

	// Recover from panic and gracefully shutdown
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
		if err := loadedHooks.LoadHooks(options.AppCmd, options.AppContainer, 0, ctx, options.Filters); err != nil {
			return
		}
	}

	select {
	case <-stopper:
		loadedHooks.Stop(true)
		return
	default:
		// start the BootProxy
		ps = proxy.BootProxy(r.Logger, proxy.Option{Port: options.ProxyPort}, options.AppCmd, options.AppContainer, 0, "", options.Ports, loadedHooks, ctx, 0)
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
			if err := loadedHooks.LaunchUserApplication(options.AppCmd, options.AppContainer, options.AppNetwork, options.Delay, options.BuildDelay, false); err != nil {
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
			tele.RecordedTestSuite(options.Path, testsTotal, mocksTotal)
		}

	}

	<-exitCmd
}
