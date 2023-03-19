package server

import (
	"fmt"
	"go.keploy.io/server/pkg/models"
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
	"github.com/kelseyhightower/envconfig"
	"github.com/keploy/go-sdk/integrations/kchi"
	"github.com/keploy/go-sdk/integrations/khttpclient"
	"github.com/keploy/go-sdk/integrations/kmongo"
	"github.com/keploy/go-sdk/keploy"
	"github.com/soheilhy/cmux"
	"go.keploy.io/server/graph"
	"go.keploy.io/server/graph/generated"
	"go.keploy.io/server/grpc/grpcserver"
	"go.keploy.io/server/http/browserMock"
	"go.keploy.io/server/http/regression"
	mockPlatform "go.keploy.io/server/pkg/platform/fs"
	"go.keploy.io/server/pkg/platform/mgo"
	"go.keploy.io/server/pkg/platform/telemetry"
	mock2 "go.keploy.io/server/pkg/service/browserMock"
	"go.keploy.io/server/pkg/service/mock"
	regression2 "go.keploy.io/server/pkg/service/regression"
	"go.keploy.io/server/pkg/service/testCase"
	"go.keploy.io/server/web"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// const defaultPort = "8080"

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

type config struct {
	MongoURI         string `envconfig:"MONGO_URI" default:"mongodb://localhost:27017"`
	DB               string `envconfig:"DB" default:"keploy"`
	TestCaseTable    string `envconfig:"TEST_CASE_TABLE" default:"test-cases"`
	TestRunTable     string `envconfig:"TEST_RUN_TABLE" default:"test-runs"`
	TestTable        string `envconfig:"TEST_TABLE" default:"tests"`
	TelemetryTable   string `envconfig:"TELEMETRY_TABLE" default:"telemetry"`
	APIKey           string `envconfig:"API_KEY"`
	EnableDeDup      bool   `envconfig:"ENABLE_DEDUP" default:"false"`
	EnableTelemetry  bool   `envconfig:"ENABLE_TELEMETRY" default:"true"`
	EnableDebugger   bool   `envconfig:"ENABLE_DEBUG" default:"false"`
	EnableTestExport bool   `envconfig:"ENABLE_TEST_EXPORT" default:"true"`
	KeployApp        string `envconfig:"APP_NAME" default:"Keploy-Test-App"`
	Port             string `envconfig:"PORT" default:"6789"`
	ReportPath       string `envconfig:"REPORT_PATH" default:""`
	PathPrefix       string `envconfig:"KEPLOY_PATH_PREFIX" default:"/"`

	// Use this option to tune the behaviour of the path where the logs would be written.
	LogLocation models.LogLocation `envconfig:"LOG_LOCATION" default:"console"`

	// The absolute or relative filepath of the files to store the logs in.
	// This is platform agnostic, so forward and backward slashes both are fine.
	// If the file doesn't exists, one will be created for you.
	LogFileLocation string `envconfig:"LOG_FILE_LOCATION" default:"/tmp/keploy.log"`
}

func Server(ver string) *chi.Mux {
	rand.Seed(time.Now().UTC().UnixNano())

	// Parse the config before creating the logger to figure out whether we need
	// to pipe the logs or not.
	var conf config
	err := envconfig.Process("keploy", &conf)
	if err != nil {
		log.Fatalf("failed to read/process configuration with error %v", err)
	}

	// Convert the separators in the filepath to a platform agnostic version.
	// For example, if user passed in "/C/Downloads", it'd be converted to
	// "\C\Downloads" in Windows.
	conf.LogFileLocation = filepath.FromSlash(conf.LogFileLocation)

	// If we need to pipe or redirect the logs, create the relevant directories.
	if conf.LogLocation != models.Console {
		err := CreateNestedDirectoriesIfNotExists(conf.LogFileLocation)
		if err != nil {
			log.Fatalf("Could not create nested directories for filepath due to error %v", err)
		}
	}

	logConf := zap.NewDevelopmentConfig()
	logConf.Level = zap.NewAtomicLevelAt(zap.InfoLevel)

	// If the user has requested to the pipe the logs in a file,
	// modify the internal config for zap which supports multiple syncers.
	switch conf.LogLocation {
	case models.Console:
		logConf.OutputPaths = []string{"stderr"}
	case models.File:
		logConf.OutputPaths = []string{conf.LogFileLocation}
	case models.Pipe:
		logConf.OutputPaths = []string{"stderr", conf.LogFileLocation}
	}

	logger, err := logConf.Build()
	if err != nil {
		panic(err)
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

	if conf.EnableDebugger {
		logConf.Level.SetLevel(zap.DebugLevel)
	}

	cl, err := mgo.New(conf.MongoURI)
	if err != nil {
		logger.Fatal("failed to create mgo db client", zap.Error(err))
	}

	db := cl.Database(conf.DB)

	tdb := mgo.NewTestCase(kmongo.NewCollection(db.Collection(conf.TestCaseTable)), logger)

	rdb := mgo.NewRun(kmongo.NewCollection(db.Collection(conf.TestRunTable)), kmongo.NewCollection(db.Collection(conf.TestTable)), logger)

	mockFS := mockPlatform.NewMockExportFS(keploy.GetMode() == keploy.MODE_TEST)
	testReportFS := mockPlatform.NewTestReportFS(keploy.GetMode() == keploy.MODE_TEST)
	teleFS := mockPlatform.NewTeleFS()
	mdb := mgo.NewBrowserMockDB(kmongo.NewCollection(db.Collection("test-browser-mocks")), logger)
	browserMockSrv := mock2.NewBrMockService(mdb, logger)
	enabled := conf.EnableTelemetry
	analyticsConfig := telemetry.NewTelemetry(mgo.NewTelemetryDB(db, conf.TelemetryTable, enabled, logger), enabled, keploy.GetMode() == keploy.MODE_OFF, conf.EnableTestExport, teleFS, logger, ver)

	client := http.Client{
		Transport: khttpclient.NewInterceptor(http.DefaultTransport),
	}

	tcSvc := testCase.New(tdb, logger, conf.EnableDeDup, analyticsConfig, client, conf.EnableTestExport, mockFS)
	// runSrv := run.New(rdb, tdb, logger, analyticsConfig, client, testReportFS)
	regSrv, err := regression2.New(tdb, rdb, testReportFS, analyticsConfig, client, logger, conf.EnableTestExport, mockFS, conf.LogLocation, conf.LogFileLocation)
	if err != nil {
		logger.Fatal("Could not create regression service due to error %s" + err.Error())
	}
	mockSrv := mock.NewMockService(mockFS, logger)

	srv := handler.NewDefaultServer(generated.NewExecutableSchema(generated.Config{Resolvers: graph.NewResolver(logger, regSrv, tcSvc)}))

	// initialize the client serveri
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
		regression.New(r, logger, regSrv, tcSvc, conf.EnableTestExport, conf.ReportPath)
		browserMock.New(r, logger, browserMockSrv)

		r.Handle("/", playground.Handler("keploy graphql backend", "/api/query"))
		r.Handle("/query", srv)
	})

	analyticsConfig.Ping(keploy.GetMode() == keploy.MODE_TEST)

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
		return grpcserver.New(k, logger, regSrv, mockSrv, tcSvc, grpcListener, conf.EnableTestExport, conf.ReportPath, analyticsConfig, client)
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

func CreateNestedDirectoriesIfNotExists(filePath string) error {
	// Remove the filename to get the path for the directory
	directoryPath := filepath.Dir(filePath)

	// Create the nested directories if they don't exist.
	return os.MkdirAll(directoryPath, os.ModePerm)
}
