// Package testset provides functionality for working with keploy testset level configs like templates, post/pre script.
package testset

import (
	"context"
	"path/filepath"

	"go.keploy.io/server/v2/pkg/platform/yaml"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

// Generic is a generic struct to read and write testset config file
type Generic[T any] struct {
	logger *zap.Logger
	path   string
}

func NewGeneric[T any](logger *zap.Logger, path string) *Generic[T] {
	return &Generic[T]{
		logger: logger,
		path:   path,
	}
}

func (g *Generic[T]) ReadConfig(ctx context.Context, testSetID string) (T, error) {
	filePath := filepath.Join(g.path, testSetID)

	var config T
	data, err := yaml.ReadFile(ctx, g.logger, filePath, "config")
	if err != nil {
		utils.LogError(g.logger, err, "failed to read the config from yaml")
		return config, err
	}

	if err := yamlLib.Unmarshal(data, &config); err != nil {
		g.logger.Info("failed to decode the configuration file", zap.Error(err))
		return config, err
	}

	return config, nil
}

func (g *Generic[T]) WriteConfig(ctx context.Context, testSetID string, config T) error {
	filePath := filepath.Join(g.path, testSetID)

	data, err := yamlLib.Marshal(config)
	if err != nil {
		g.logger.Error("failed to marshal test-set config file", zap.String("testSet", testSetID), zap.Error(err))
		return err
	}
	err = yaml.WriteFile(ctx, g.logger, filePath, "config", data, false)
	if err != nil {
		utils.LogError(g.logger, err, "failed to write test-set configuration in yaml file", zap.String("testSet", testSetID))
		return err
	}

	return nil
}
