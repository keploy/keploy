package graph

import (
	"context"
	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/playground"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/service/replay"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
	"log"
	"net/http"
	"strconv"
	"sync"
)

type Graph struct {
	logger *zap.Logger
	mutex  sync.Mutex
	replay replay.Service
	config config.Config
}

func NewGraph(logger *zap.Logger, replay replay.Service, config config.Config) *Graph {
	return &Graph{
		logger: logger,
		mutex:  sync.Mutex{},
		config: config,
		replay: replay,
	}
}

const defaultPort = 6789

func (g *Graph) Serve(ctx context.Context) error {

	if g.config.Port == 0 {
		g.config.Port = defaultPort
	}

	srv := handler.NewDefaultServer(NewExecutableSchema(Config{
		Resolvers: &Resolver{
			logger: g.logger,
			replay: g.replay,
		},
	}))

	http.Handle("/", playground.Handler("GraphQL playground", "/query"))
	http.Handle("/query", srv)

	// Create a new http.Server instance
	httpSrv := &http.Server{
		Addr:    ":" + strconv.Itoa(int(g.config.Port)),
		Handler: nil, // Use the default http.DefaultServeMux
	}

	go func() {
		defer utils.Recover(g.logger)
		<-ctx.Done()
		g.stopGraphqlServer(ctx, httpSrv)
	}()

	log.Printf(utils.Emoji+"connect to http://localhost:%d/ for GraphQL playground", g.config.Port)
	if err := httpSrv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf(utils.Emoji+"listen: %s\n", err)
	}
	g.logger.Debug("Graphql server stopped gracefully")
	return nil
}

// Gracefully shut down the HTTP server with a timeout
func (g *Graph) stopGraphqlServer(ctx context.Context, httpSrv *http.Server) {
	if err := httpSrv.Shutdown(ctx); err != nil {
		g.logger.Error("Graphql server shutdown failed", zap.Error(err))
	}
}
