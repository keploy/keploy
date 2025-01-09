// Package testset provides functionality for working with keploy testset level configs like templates, post/pre script.
package testset

import (
	"context"
	"fmt"
	"path/filepath"

	"go.keploy.io/server/v2/pkg/platform/yaml"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

// Db is a generic struct to read and write testset config file
type Db[T any] struct {
	logger *zap.Logger
	path   string
}

func New[T any](logger *zap.Logger, path string) *Db[T] {
	return &Db[T]{
		logger: logger,
		path:   path,
	}
}

func (db *Db[T]) Read(ctx context.Context, testSetID string) (T, error) {
	filePath := filepath.Join(db.path, testSetID)

	var config T
	data, err := yaml.ReadFile(ctx, db.logger, filePath, "config")
	if err != nil {
		return config, err
	}
	if err := yamlLib.Unmarshal(data, &config); err != nil {
		utils.LogError(db.logger, err, "failed to unmarshal test-set config file", zap.String("testSet", testSetID))
		return config, err
	}

	return config, nil
}

func (db *Db[T]) Write(ctx context.Context, testSetID string, config T) error {
	filePath := filepath.Join(db.path, testSetID)
	data, err := yamlLib.Marshal(config)
	if err != nil {
		utils.LogError(db.logger, err, "failed to marshal test-set config file", zap.String("testSet", testSetID))
		return err
	}
	fmt.Println("Writing test-set configuration to file", filePath)
	err = yaml.WriteFile(ctx, db.logger, filePath, "config", data, false)
	if err != nil {
		utils.LogError(db.logger, err, "failed to write test-set configuration in yaml file", zap.String("testSet", testSetID))
		return err
	}

	return nil
}
