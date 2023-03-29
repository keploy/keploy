package server

import (
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/playground"
	"github.com/go-chi/chi"
	"github.com/go-chi/cors"
	// "github.com/kelseyhightower/envconfig"
	"github.com/keploy/go-sdk/integrations/kchi"
	// "github.com/keploy/go-sdk/integrations/khttpclient"
	// "github.com/keploy/go-sdk/integrations/kmongo"
	"github.com/keploy/go-sdk/keploy"
	"github.com/soheilhy/cmux"
	"go.keploy.io/server/config"
	"go.keploy.io/server/graph"
	"go.keploy.io/server/graph/generated"
	"go.keploy.io/server/grpc/grpcserver"
	"go.keploy.io/server/http/browserMock"
	"go.keploy.io/server/http/regression"
	"go.keploy.io/server/pkg/service"
	// mockPlatform "go.keploy.io/server/pkg/platform/fs"
	// "go.keploy.io/server/pkg/platform/mgo"
	// "go.keploy.io/server/pkg/platform/telemetry"
	// mock2 "go.keploy.io/server/pkg/service/browserMock"
	// "go.keploy.io/server/pkg/service/mock"
	// regression2 "go.keploy.io/server/pkg/service/regression"
	// "go.keploy.io/server/pkg/service/testCase"
	"go.keploy.io/server/web"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

const logo string = `
       ▓██▓▄
    ▓▓▓▓██▓█▓▄
     ████████▓▒
          ▀▓▓███▄      ▄▄   ▄               ▌
         ▄▌▌▓▓████▄    ██ ▓█▀  ▄▌▀▄  ▓▓▌▄   ▓█  ▄▌▓▓▌▄ ▌▌   ▓
       ▓█████████▌▓▓   ██▓█▄  ▓█▄▓▓ ▐█▌  ██ ▓█  █▌  ██  █▌ █▓
      ▓▓▓▓▀▀▀▀▓▓▓▓▓▓▌  ██  █▓  ▓▌▄▄ ▐█▓▄▓█▀ █▓█ ▀█▄▄█▀   █▓█
       ▓▌                           ▐█▌                   █▌
        ▓
`

// starts the keploy API server for Http and gRPC calls
func Server(args ...interface{}) *chi.Mux {
	rand.Seed(time.Now().UTC().UnixNano())

	var (
		conf      *config.Config
		ver       string
		logger    *zap.Logger
		kServices *service.KServices
		err       error
	)
	// check if conf/logger is passed as arguments.
	for _, arg := range args {
		switch v := arg.(type) {
		case *config.Config:
			conf = v
		case string:
			ver = v
		case *zap.Logger:
			logger = v
		case *service.KServices:
			kServices = v
		}
	}

	// If config is not provided, use default values or create a default config
	if conf == nil {
		conf = config.NewConfig()
	}

	// If logger is not provided, use default values or create a default logger
	if logger == nil {
		logConf := zap.NewDevelopmentConfig()
		logConf.Level = zap.NewAtomicLevelAt(zap.InfoLevel)
		logger, err = logConf.Build()
		if err != nil {
			log.Fatalf("failed to initialize logger. error: %v", err)
		}

		if conf.EnableDebugger {
			logConf.Level.SetLevel(zap.DebugLevel)
		}
	}
	defer logger.Sync() // flushes buffer, if any

	// default resultPath is current directory from which keploy binary is running
	if conf.ReportPath == "" {
		curr, err := os.Getwd()
		if err != nil {
			logger.Error("failed to get path of current directory from which keploy binary is running", zap.Error(err))
		}
		conf.ReportPath = curr
	} else if conf.ReportPath[0] != '/' {
		path, err := filepath.Abs(conf.ReportPath)
		if err != nil {
			logger.Error("Failed to get the absolute path from relative conf.path", zap.Error(err))
		}
		conf.ReportPath = path
	}
	conf.ReportPath += "/test-reports"

	if kServices == nil {
		kServices = service.NewServices(ver, conf, logger)
	}

	srv := handler.NewDefaultServer(generated.NewExecutableSchema(generated.Config{Resolvers: graph.NewResolver(logger, kServices.RegressionSrv, kServices.TestcaseSrv)}))

	// initialize the routers servers
	r := chi.NewRouter()

	port := conf.Port

	k := keploy.New(keploy.Config{
		App: keploy.AppConfig{
			Name: conf.KeployApp,
			Port: port,
			Filter: keploy.Filter{
				AcceptUrlRegex: "^/api",
			},
			TestPath: "./cmd/server/keploy/tests",
			MockPath: "./cmd/server/keploy/mocks",
			Timeout:  80 * time.Second,
		},

		Server: keploy.ServerConfig{
			// LicenseKey: conf.APIKey,
			// URL:        "https://api.keploy.io",
			URL: "http://localhost:6790/api",
		},
	})

	r.Use(kchi.ChiMiddlewareV5(k))

	r.Use(cors.Handler(cors.Options{

		AllowedOrigins:   []string{"*"},
		AllowCredentials: true,
		ExposedHeaders:   []string{"Link"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-CSRF-Token"},
	}))

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})

	r.Handle("/*", http.StripPrefix(conf.PathPrefix, web.Handler()))

	// add api routes
	r.Route("/api", func(r chi.Router) {
		regression.New(r, logger, kServices.RegressionSrv, kServices.TestcaseSrv, conf.EnableTestExport, conf.ReportPath)
		browserMock.New(r, logger, kServices.BrowserMockSrv)

		r.Handle("/", playground.Handler("keploy graphql backend", "/api/query"))
		r.Handle("/query", srv)
	})
	fmt.Println(kServices)
	kServices.TelemetrySrv.Ping(keploy.GetMode() == keploy.MODE_TEST)

	listener, err := net.Listen("tcp", ":"+port)

	if err != nil {
		panic(err)
	}

	m := cmux.New(listener)
	grpcListener := m.MatchWithWriters(cmux.HTTP2MatchHeaderFieldSendSettings("content-type", "application/grpc"))

	httpListener := m.Match(cmux.HTTP1Fast())

	//log.Printf("👍 connect to http://localhost:%s for GraphQL playground\n ", port)

	g := new(errgroup.Group)
	g.Go(func() error {
		return grpcserver.New(k, logger, kServices.RegressionSrv, kServices.MockSrv, kServices.TestcaseSrv, grpcListener, conf.EnableTestExport, conf.ReportPath, kServices.TelemetrySrv, kServices.TelemetryClient)
	})

	g.Go(func() error {
		srv := http.Server{Handler: r}
		err := srv.Serve(httpListener)
		return err
	})
	g.Go(func() error { return m.Serve() })
	fmt.Println(logo, " ")
	fmt.Printf("keploy %v\n\n.", ver)
	logger.Info("keploy started at port " + port)
	g.Wait()

	return r
}
