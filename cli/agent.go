package cli
import (
    "context"
    "fmt"
    "github.com/go-chi/chi/v5"
    "github.com/spf13/cobra"
    "go.keploy.io/server/v3/config"
    "go.keploy.io/server/v3/pkg/agent/routes"
    "go.keploy.io/server/v3/pkg/service/agent"
    "go.keploy.io/server/v3/utils"
    "go.uber.org/zap"
)
func init() {
    Register("agent", Agent)
}
func Agent(ctx context.Context, logger *zap.Logger, conf *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
    var cmd = &cobra.Command{
        Use:    "agent",
        Short:  "starts keploy agent for hooking and starting proxy",
        PreRunE: func(cmd *cobra.Command, _ []string) error {
            return cmdConfigurator.Validate(ctx, cmd)
        },
        RunE: func(cmd *cobra.Command, _ []string) error {
            return runAgent(ctx, logger, cmd, serviceFactory)
        },
    }
    if err := cmdConfigurator.AddFlags(cmd); err != nil {
        utils.LogError(logger, err, "failed to add agent flags")
        return nil
    }
    return cmd
}
func runAgent(ctx context.Context, logger *zap.Logger, cmd *cobra.Command, serviceFactory ServiceFactory) error {
    // Get service
    svc, err := serviceFactory.GetService(ctx, cmd.Name())
    if err != nil {
        utils.LogError(logger, err, "failed to get service")
        return fmt.Errorf("failed to get service: %w", err)
    }
    // Type assertion with better error message
    agentSvc, ok := svc.(agent.Service)
    if !ok {
        err := fmt.Errorf("service type %T doesn't implement agent.Service interface", svc)
        utils.LogError(logger, err, "invalid service type")
        return err
    }
    // Setup channels with proper sizing
    startAgentCh := make(chan int, 1)
    serverErrCh := make(chan error, 1)
    defer close(startAgentCh)
    // Setup router
    router := chi.NewRouter()
    routes.ActiveHooks.New(router, agentSvc, logger)
    // Start server goroutine with proper error handling
    go startAgentServerAsync(ctx, logger, startAgentCh, router, serverErrCh)
    // Setup agent
    if err := agentSvc.Setup(ctx, startAgentCh); err != nil {
        utils.LogError(logger, err, "failed to setup agent")
        return fmt.Errorf("failed to setup agent: %w", err)
    }
    // Wait for server errors or context cancellation
    select {
    case err := <-serverErrCh:
        if err != nil {
            utils.LogError(logger, err, "agent server error")
            return fmt.Errorf("agent server error: %w", err)
        }
    case <-ctx.Done():
        logger.Info("agent context cancelled", zap.Error(ctx.Err()))
        return ctx.Err()
    }
    return nil
}
func startAgentServerAsync(ctx context.Context, logger *zap.Logger, startAgentCh <-chan int, router *chi.Mux, errCh chan<- error) {
    defer close(errCh)
    select {
    case <-ctx.Done():
        logger.Info("context cancelled before agent http server could start")
        errCh <- ctx.Err()
        return
    case port := <-startAgentCh:
        // Execute pre-server hooks
        if err := agent.SetupAgentHook.AfterSetup(ctx); err != nil {
            utils.LogError(logger, err, "failed to execute pre-server startup hooks")
            errCh <- fmt.Errorf("pre-server hooks failed: %w", err)
            return
        }
        // Start server (assuming StartAgentServer blocks or returns error)
        if err := routes.StartAgentServer(logger, port, router); err != nil {
            utils.LogError(logger, err, "agent server failed")
            errCh <- fmt.Errorf("server failed: %w", err)
            return
        }
    }
}
