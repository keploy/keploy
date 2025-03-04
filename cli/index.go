package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/service/embedding"
	"go.keploy.io/server/v2/pkg/service/vectorstore"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func init() {
	Register("index", IndexCodebase)
}

func IndexCodebase(ctx context.Context, logger *zap.Logger, cfg *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:     "index",
		Short:   "Index your codebase for semantic search",
		Example: `keploy index --dir="." --ignore=".git,.idea,node_modules,vendor"`,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !cfg.VectorStore.Enabled {
				logger.Info("Vector store is disabled in configuration. Enable it to use this feature.")
				return nil
			}

			// Get directory from flags
			dir, _ := cmd.Flags().GetString("dir")
			ignoreStr, _ := cmd.Flags().GetString("ignore")
			ignorePatterns := strings.Split(ignoreStr, ",")

			// Initialize embedding service
			embeddingService := embedding.NewEmbeddingService(logger, cfg.Embedding.ApiKey)
			if embeddingService.GetDimension() == 0 {
				return fmt.Errorf("failed to initialize embedding service, please check your API key")
			}

			// Initialize vector store
			milvusConfig := &vectorstore.MilvusConfig{
				Host:           cfg.VectorStore.Host,
				Port:           cfg.VectorStore.Port,
				CollectionName: cfg.VectorStore.CollectionName,
				Dimension:      embeddingService.GetDimension(),
				IndexType:      cfg.VectorStore.IndexType,
				MetricType:     cfg.VectorStore.MetricType,
			}

			logger.Info("Connecting to Milvus...",
				zap.String("host", milvusConfig.Host),
				zap.Int("port", milvusConfig.Port),
				zap.String("collection", milvusConfig.CollectionName))

			milvusStore, err := vectorstore.NewMilvusStore(ctx, logger, milvusConfig)
			if err != nil {
				utils.LogError(logger, err, "failed to initialize vector store")
				return err
			}

			// Create scanner
			scanner := vectorstore.NewCodeScanner(logger, milvusStore, embeddingService)

			logger.Info("Starting indexing of codebase...",
				zap.String("directory", dir),
				zap.Strings("ignorePatterns", ignorePatterns))

			// Scan directory
			err = scanner.ScanDirectory(ctx, dir, ignorePatterns)
			if err != nil {
				utils.LogError(logger, err, "failed to scan directory")
				return err
			}

			// Start file watcher if requested
			watch, _ := cmd.Flags().GetBool("watch")
			if watch {
				fileWatcher, err := vectorstore.NewFileWatcher(logger, milvusStore, embeddingService, ignorePatterns)
				if err != nil {
					utils.LogError(logger, err, "failed to initialize file watcher")
					return err
				}

				err = fileWatcher.WatchDirectory(ctx, dir)
				if err != nil {
					utils.LogError(logger, err, "failed to start file watcher")
					return err
				}

				logger.Info("File watcher started. Press Ctrl+C to stop.")

				// Keep running until signal
				c := make(chan os.Signal, 1)
				signal.Notify(c, os.Interrupt, syscall.SIGTERM)
				<-c

				fileWatcher.Stop()
			}

			logger.Info("Indexing completed successfully")
			return nil
		},
	}

	cmd.Flags().String("dir", ".", "Directory to scan")
	cmd.Flags().String("ignore", ".git,.idea,node_modules,vendor", "Comma-separated list of patterns to ignore")
	cmd.Flags().Bool("watch", false, "Start a file watcher to continuously index changes")

	return cmd
}
