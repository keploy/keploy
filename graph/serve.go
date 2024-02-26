package graph

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/playground"
	"go.keploy.io/server/pkg/service/replay"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

type graph struct {
	logger *zap.Logger
	mutex  sync.Mutex
}

func New(logger *zap.Logger) graphInterface {
	return &graph{
		logger: logger,
		mutex:  sync.Mutex{},
	}
}

// defaultPort is the default port for the graphql server
const defaultPort = 6789

// Serve is called by the serve command and is used to run a graphql server, to run tests separately via apis.
func (g *graph) Serve(ctx context.Context) {
	if port == 0 {
		port = defaultPort
	}
	replayer := replay.NewReplayer(g.logger)

	// BootReplay will be called in the unit test.

	// Starting the test command
	srv := handler.NewDefaultServer(NewExecutableSchema(Config{
		Resolvers: &Resolver{
			Tester:    replayer,
			Logger:    g.logger,
			ServeTest: true,
		},
	}))

	http.Handle("/", playground.Handler("GraphQL playground", "/query"))
	http.Handle("/query", srv)

	// Create a new http.Server instance
	httpSrv := &http.Server{
		Addr:    ":" + strconv.Itoa(int(port)),
		Handler: nil, // Use the default http.DefaultServeMux
	}

	// Start the graphql server
	go func() {
		// Recover from panic and gracefully shutdown
		defer utils.HandlePanic(g.logger)
		log.Printf(Emoji+"connect to http://localhost:%d/ for GraphQL playground", port)
		if err := httpSrv.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf(Emoji+"listen: %s\n", err)
		}
		g.logger.Debug("graphql server stopped")
	}()

	defer g.stopGraphqlServer(httpSrv)

	// Start the testing framework. This will run until all the testsets are complete.
	go func() {
		defer utils.HandlePanic(g.logger)
		replayer.RunApplication(ctx, pid, models.RunOptions{})
	}()
}

// Gracefully shut down the HTTP server with a timeout
func (g *graph) stopGraphqlServer(httpSrv *http.Server) {
	shutdown := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(ctx); err != nil {
		g.logger.Error("Graphql server shutdown failed", zap.Error(err))
	}
	// If you have other goroutines that should listen for this, you can use this channel to notify them.
	close(shutdown)
}
