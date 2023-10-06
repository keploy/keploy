package serve

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/playground"
	"go.keploy.io/server/pkg"
	"go.keploy.io/server/pkg/hooks"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/platform/yaml"
	"go.keploy.io/server/pkg/proxy"
	"go.keploy.io/server/pkg/service/serve/graph"
	"go.keploy.io/server/pkg/service/test"
	"go.uber.org/zap"
	sentry "github.com/getsentry/sentry-go"
)

var Emoji = "\U0001F430" + " Keploy:"

type server struct {
	logger *zap.Logger
	mutex  sync.Mutex
}

func NewServer(logger *zap.Logger) Server {
	return &server{
		logger: logger,
		mutex:  sync.Mutex{},
	}
}

const defaultPort = 6789

// Serve is called by the serve command and is used to run a graphql server, to run tests separately via apis.
func (s *server) Serve(path, testReportPath string, Delay uint64, pid, port uint32, lang string, passThorughPorts []uint, apiTimeout uint64) {

	if port == 0 {
		port = defaultPort
	}

	models.SetMode(models.MODE_TEST)

	tester := test.NewTester(s.logger)
	testReportFS := yaml.NewTestReportFS(s.logger)
	ys := yaml.NewYamlStore("", "", "", "", s.logger)

	routineId := pkg.GenerateRandomID()
	// Initiate the hooks
	loadedHooks := hooks.NewHook(ys, routineId, s.logger)

	// Recover from panic and gracfully shutdown
	defer loadedHooks.Recover(routineId)

	ctx := context.Background()
	// load the ebpf hooks into the kernel
	if err := loadedHooks.LoadHooks("", "", pid, ctx); err != nil {
		return
	}

	//sending this graphql server port to be filterd in the eBPF program
	if err := loadedHooks.SendKeployServerPort(port); err != nil {
		return
	}

	// start the proxy
	ps := proxy.BootProxy(s.logger, proxy.Option{}, "", "", pid, lang, passThorughPorts, loadedHooks, ctx)

	// proxy update its state in the ProxyPorts map
	// ps.SetHook(loadedHooks)

	// Sending Proxy Ip & Port to the ebpf program
	if err := loadedHooks.SendProxyInfo(ps.IP4, ps.Port, ps.IP6); err != nil {
		return
	}

	srv := handler.NewDefaultServer(graph.NewExecutableSchema(graph.Config{
		Resolvers: &graph.Resolver{
			Tester:         tester,
			TestReportFS:   testReportFS,
			YS:             ys,
			LoadedHooks:    loadedHooks,
			Logger:         s.logger,
			Path:           path,
			TestReportPath: testReportPath,
			Delay:          Delay,
			AppPid:         pid,
			ApiTimeout:     apiTimeout,
		},
	}))

	http.Handle("/", playground.Handler("GraphQL playground", "/query"))
	http.Handle("/query", srv)

	// Create a new http.Server instance
	httpSrv := &http.Server{
		Addr:    ":" + strconv.Itoa(int(port)),
		Handler: nil, // Use the default http.DefaultServeMux
	}

	// Create a shutdown channel
	shutdown := make(chan struct{})

	// Start your server in a goroutine
	go func() {
		// Recover from panic and gracefully shutdown
		defer loadedHooks.Recover(pkg.GenerateRandomID())
		defer sentry.Recover()
		log.Printf(Emoji+"connect to http://localhost:%d/ for GraphQL playground", port)
		if err := httpSrv.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf(Emoji+"listen: %s\n", err)
		}
		s.logger.Debug("graphql server stopped")
	}()

	// Listen for the interrupt signal
	stopper := make(chan os.Signal, 1)
	// signal.Notify(stopper, os.Interrupt, os.Kill, syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)
	signal.Notify(stopper, syscall.SIGINT, syscall.SIGTERM)

	// Block until we receive one
	fmt.Printf(Emoji+"Received signal:%v\n", <-stopper)

	s.logger.Info("Received signal, initiating graceful shutdown...")

	// Gracefully shut down the HTTP server with a timeout
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(ctx); err != nil {
		s.logger.Error("Graphql server shutdown failed", zap.Error(err))
	}

	// Shutdown other resources
	loadedHooks.Stop(true)
	ps.StopProxyServer()

	close(shutdown) // If you have other goroutines that should listen for this, you can use this channel to notify them.
}
