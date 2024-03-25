package graph

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/playground"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/service/replay"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
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

	graphGrp, graphCtx := errgroup.WithContext(ctx)

	resolver := &Resolver{
		logger: g.logger,
		replay: g.replay,
	}

	srv := handler.NewDefaultServer(NewExecutableSchema(Config{
		Resolvers: resolver,
	}))

	defer func() {
		// cancel the context of the hooks to stop proxy and ebpf hooks
		hookCtx, hookCancel := resolver.getHookCtxWithCancel()
		if hookCtx != nil && hookCancel != nil {
			hookCancel()
			hookErrGrp, ok := hookCtx.Value(models.ErrGroupKey).(*errgroup.Group)
			if ok {
				if err := hookErrGrp.Wait(); err != nil {
					utils.LogError(g.logger, err, "failed to stop the hooks gracefully")
				}
			}
		}

		// cancel the context of the app in case of sudden stop if the app was started
		appCtx, appCancel := resolver.getAppCtxWithCancel()
		if appCtx != nil && appCancel != nil {
			appCancel()
			appErrGrp, ok := appCtx.Value(models.ErrGroupKey).(*errgroup.Group)
			if ok {
				if err := appErrGrp.Wait(); err != nil {
					utils.LogError(g.logger, err, "failed to stop the application gracefully")
				}
			}
		}

		err := graphGrp.Wait()
		if err != nil {
			utils.LogError(g.logger, err, "failed to stop the graphql server gracefully")
		}
	}()

	http.Handle("/", playground.Handler("GraphQL playground", "/query"))
	http.Handle("/query", srv)

	// Create a new http.Server instance
	httpSrv := &http.Server{
		Addr:    ":" + strconv.Itoa(int(g.config.Port)),
		Handler: nil, // Use the default http.DefaultServeMux
	}

	graphGrp.Go(func() error {
		defer utils.Recover(g.logger)
		return g.stopGraphqlServer(graphCtx, httpSrv)
	})

	g.logger.Debug(fmt.Sprintf("connect to http://localhost:%d/ for GraphQL playground", int(g.config.Port)))
	g.logger.Info("Graphql server started", zap.Int("port", int(g.config.Port)))
	if err := httpSrv.ListenAndServe(); err != http.ErrServerClosed {
		stopErr := utils.Stop(g.logger, "Graphql server failed to start")
		if stopErr != nil {
			utils.LogError(g.logger, stopErr, "failed to stop Graphql server gracefully")
		}
		return err
	}
	g.logger.Info("Graphql server stopped gracefully")
	return nil
}

// Gracefully shut down the HTTP server
func (g *Graph) stopGraphqlServer(ctx context.Context, httpSrv *http.Server) error {
	<-ctx.Done()
	if err := httpSrv.Shutdown(ctx); err != nil {
		utils.LogError(g.logger, err, "Graphql server shutdown failed")
		return err
	}
	return nil
}
