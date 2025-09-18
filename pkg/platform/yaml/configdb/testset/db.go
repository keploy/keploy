// Package testset provides functionality for working with keploy testset level configs like templates, post/pre script.
package testset

import (
	"context"
	"os"
	"path/filepath"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/platform/yaml"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

// Db is a generic struct to read and write testset config file
type Db[T any] struct {
	logger *zap.Logger
	path   string
	cfg    *config.Config // Added for secret fallback logic
}

func New[T any](logger *zap.Logger, path string) *Db[T] {
	return &Db[T]{
		logger: logger,
		path:   path,
		cfg:    nil,
	}
}

// NewWithConfig creates a new Db instance with config for secret fallback logic
func NewWithConfig[T any](logger *zap.Logger, path string, cfg *config.Config) *Db[T] {
	return &Db[T]{
		logger: logger,
		path:   path,
		cfg:    cfg,
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

	secretConfig, ok := any(config).(models.Secret)

	if !ok {
		return config, nil
	}

	secretValues, err := db.ReadSecret(ctx, testSetID)
	if err != nil {
		db.logger.Warn("Failed to read secret values, continuing without secrets", zap.String("testSet", testSetID), zap.Error(err))
		return config, err
	}

	secretConfig.SetSecrets(secretValues)

	return config, nil
}

func (db *Db[T]) Write(ctx context.Context, testSetID string, config T) error {
	filePath := filepath.Join(db.path, testSetID)
	data, err := yamlLib.Marshal(config)
	if err != nil {
		utils.LogError(db.logger, err, "failed to marshal test-set config file", zap.String("testSet", testSetID))
		return err
	}
	err = yaml.WriteFile(ctx, db.logger, filePath, "config", data, false)
	if err != nil {
		utils.LogError(db.logger, err, "failed to write test-set configuration in yaml file", zap.String("testSet", testSetID))
		return err
	}

	return nil
}

// ReadSecret reads the secret configuration for a test set with fallback logic:
// First, check if command-line secrets are provided; if not, fall back to secret.yaml file
func (db *Db[T]) ReadSecret(ctx context.Context, testSetID string) (map[string]interface{}, error) {
	// First, check if command-line secrets are provided
	if db.cfg != nil && len(db.cfg.Secrets) > 0 {
		db.logger.Debug("Using command-line provided secrets", zap.String("testSet", testSetID))
		return db.cfg.Secrets, nil
	}

	// Fall back to reading secret.yaml file
	filePath := filepath.Join(db.path, testSetID)
	secretPath := filepath.Join(filePath, "secret.yaml")
	if _, err := os.Stat(secretPath); os.IsNotExist(err) {
		db.logger.Debug("No secret.yaml file found, using empty secrets", zap.String("testSet", testSetID))
		return make(map[string]interface{}), nil
	}

	db.logger.Debug("Falling back to secret.yaml file", zap.String("testSet", testSetID))
	data, err := yaml.ReadFile(ctx, db.logger, filePath, "secret")
	if err != nil {
		return nil, err
	}

	var secretConfig map[string]interface{}
	if err := yamlLib.Unmarshal(data, &secretConfig); err != nil {
		utils.LogError(db.logger, err, "failed to unmarshal test-set secret file", zap.String("testSet", testSetID))
		return nil, err
	}

	return secretConfig, nil
}
