package server

import (
	"math/rand"
	"net/http"
	"time"
	// "log"
	// "fmt"
	// "context"
	
	// "go.mongodb.org/mongo-driver/mongo/options"
	// "go.mongodb.org/mongo-driver/mongo"
	
	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/playground"
	"github.com/go-chi/chi"
	"github.com/go-chi/cors"
	"github.com/kelseyhightower/envconfig"
	"go.keploy.io/server/graph"
	"go.keploy.io/server/graph/generated"
	"go.keploy.io/server/http/regression"
	"go.keploy.io/server/pkg/platform/mgo"
	regression2 "go.keploy.io/server/pkg/service/regression"
	"go.keploy.io/server/pkg/service/run"
	"go.keploy.io/server/web"
	"go.uber.org/zap"
	"github.com/keploy/go-sdk/integrations/kchi"
	"github.com/keploy/go-sdk/keploy"
	"github.com/keploy/go-sdk/integrations/kmongo"

)

// const defaultPort = "8080"

type config struct {
	MongoURI      string `envconfig:"MONGO_URI" default:"mongodb://localhost:27017"`
	DB            string `envconfig:"DB" default:"keploy-test"`
	TestCaseTable string `envconfig:"TEST_CASE_TABLE" default:"test-cases"`
	TestRunTable  string `envconfig:"TEST_RUN_TABLE" default:"test-runs"`
	TestTable     string `envconfig:"TEST_TABLE" default:"tests"`
	APIKey  	  string `envconfig:"API_KEY"`
}

func Server() *chi.Mux {
	rand.Seed(time.Now().UTC().UnixNano())

	logger, err := zap.NewProduction()
	if err != nil {
		panic(err)
	}
	defer logger.Sync() // flushes buffer, if any

	var conf config
	err = envconfig.Process("keploy", &conf)
	if err != nil {
		logger.Error("failed to read/process configuration", zap.Error(err))
	}

	cl, err := mgo.New(conf.MongoURI)
	if err != nil {
		logger.Fatal("failed to create mgo db client", zap.Error(err))
	}

	db := cl.Database(conf.DB)

	tdb := mgo.NewTestCase(kmongo.NewMongoCollection( db.Collection(conf.TestCaseTable) ), logger)
	
	rdb := mgo.NewRun(kmongo.NewMongoCollection( db.Collection(conf.TestRunTable) ), kmongo.NewMongoCollection( db.Collection(conf.TestTable) ), logger)

	regSrv := regression2.New(tdb, rdb, logger)
	runSrv := run.New(rdb, tdb, logger)

	srv := handler.NewDefaultServer(generated.NewExecutableSchema(generated.Config{Resolvers: graph.NewResolver(logger, runSrv, regSrv)}))

	// initialize the client serveri
	r := chi.NewRouter()
	port := "8082"
	kApp := keploy.New(keploy.Config{
		App: keploy.AppConfig{
			Name: "Test-Keploy-Client",
			Port: port,
		},
		Server: keploy.ServerConfig{
			LicenseKey:conf.APIKey,
			// URL: "http://localhost:8081/api",
			
		},
	})

	kchi.ChiV5(kApp, r)
	
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

	r.Handle("/*", web.Handler())

	// add api routes
	r.Route("/api", func(r chi.Router) {
		regression.New(r, logger, regSrv, runSrv)
		r.Handle("/", playground.Handler("johari backend", "/query"))
		r.Handle("/query", srv)
	})
	return r
}
