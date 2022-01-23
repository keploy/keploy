package server

import (
	"math/rand"
	"net/http"
	"os"
	"time"
	"github.com/99designs/gqlgen/graphql/playground"
	"github.com/go-chi/chi"
	"github.com/go-chi/cors"
	"go.keploy.io/server/graph"
	"go.keploy.io/server/graph/generated"
	"go.keploy.io/server/http/regression"
	"go.keploy.io/server/pkg/platform/mgo"
	regression2 "go.keploy.io/server/pkg/service/regression"
	"go.keploy.io/server/pkg/service/run"
	"github.com/99designs/gqlgen/graphql/handler"
	"go.uber.org/zap"
)

// const defaultPort = "8080"

func Server() *chi.Mux {
	rand.Seed(time.Now().UTC().UnixNano())

	mongoHost := os.Getenv("MONGO_HOST")
	mongoUser := os.Getenv("MONGO_USER")
	mongoPassword := os.Getenv("MONGO_PASSWORD")
	mongoDB := os.Getenv("MONGO_DB")
	testCaseTable := os.Getenv("TESTCASE_TABLE")

	testRunTable := os.Getenv("TEST_RUN_TABLE")
	testTable := os.Getenv("TEST_TABLE")


	logger, err := zap.NewProduction()
	if err != nil {
		panic(err)
	}
	defer logger.Sync() // flushes buffer, if any

	client, err := mgo.New(mongoUser, mongoPassword, mongoHost, mongoDB)
	if err != nil {
		logger.Fatal("failed to create mgo db client", zap.Error(err))
	}

	db := client.Database(mongoDB)

	tdb := mgo.NewTestCase(db.Collection(testCaseTable), logger)

	rdb := mgo.NewRun(db.Collection(testRunTable), db.Collection(testTable), logger)

	regSrv := regression2.New(tdb, rdb, logger)
	runSrv := run.New(rdb, logger)

	srv := handler.NewDefaultServer(generated.NewExecutableSchema(generated.Config{Resolvers: graph.NewResolver(logger, runSrv, regSrv)}))

	// initialize the serveri
	r := chi.NewRouter()

	r.Use(cors.Handler(cors.Options{

		AllowedOrigins:   []string{"*"},
		AllowCredentials: true,
		ExposedHeaders:   []string{"Link"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-CSRF-Token"},
	}))

	regression.New(r, logger, regSrv, runSrv)

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})

	r.Handle("/", playground.Handler("johari backend", "/query"))	

	r.Handle("/query", srv)
	
	return r
}
