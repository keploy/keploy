package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/golang-jwt/jwt/v4"
	"go.keploy.io/server/v3/config"
	platformAuth "go.keploy.io/server/v3/pkg/platform/auth"
	sandboxsvc "go.keploy.io/server/v3/pkg/service/sandbox"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

var (
	getSandboxJWTTokenFunc = getSandboxJWTToken
	newSandboxServiceFunc  = func(apiServerURL, jwtToken string, logger *zap.Logger) sandboxsvc.Service {
		cloudClient := sandboxsvc.NewCloudClient(apiServerURL, jwtToken, logger)
		return sandboxsvc.New(cloudClient, logger)
	}
)

func getSandboxJWTToken(ctx context.Context, logger *zap.Logger, cfg *config.Config) (string, error) {
	if cfg == nil {
		return "", fmt.Errorf("config is not available")
	}
	if strings.TrimSpace(cfg.APIServerURL) == "" {
		return "", fmt.Errorf("API server URL is not configured")
	}

	// If KEPLOY_API_KEY is set (e.g., in CI), authenticate via API key
	// instead of interactive GitHub OAuth.
	if apiKey := os.Getenv("KEPLOY_API_KEY"); apiKey != "" {
		logger.Info("KEPLOY_API_KEY detected, authenticating via API key")
		token, err := platformAuth.AuthenticateWithAPIKey(ctx, cfg.APIServerURL, cfg.InstallationID, apiKey, logger)
		if err != nil {
			return "", fmt.Errorf("API key authentication failed: %w", err)
		}
		token = strings.TrimSpace(token)
		if token == "" {
			return "", fmt.Errorf("received empty jwt token from API key auth")
		}
		return token, nil
	}

	// Fall back to GitHub OAuth (interactive login).
	authSvc := platformAuth.New(cfg.APIServerURL, cfg.InstallationID, logger, cfg.GitHubClientID)
	token, err := authSvc.GetToken(ctx)
	if err != nil {
		return "", fmt.Errorf("please login using `keploy login`: %w", err)
	}

	token = strings.TrimSpace(token)
	if token == "" {
		return "", fmt.Errorf("received empty jwt token")
	}

	return token, nil
}

func buildSandboxRefFromTag(logger *zap.Logger, cfg *config.Config, tag string, jwtToken string) (string, error) {
	if cfg == nil {
		return "", fmt.Errorf("config is not available")
	}

	username, err := extractUsernameFromJWT(jwtToken)
	if err != nil {
		return "", err
	}

	appName := strings.TrimSpace(cfg.AppName)
	if appName == "" {
		appName, err = utils.GetLastDirectory()
		if err != nil {
			return "", fmt.Errorf("failed to infer app name from current directory: %w", err)
		}
	}

	ref, err := sandboxsvc.BuildRef(username, appName, tag)
	if err != nil {
		return "", err
	}

	if logger != nil {
		logger.Debug("Inferred sandbox ref from tag",
			zap.String("tag", tag),
			zap.String("username", username),
			zap.String("appName", appName),
			zap.String("ref", ref),
		)
	}

	return ref, nil
}

func updateSandboxRefInConfig(cfg *config.Config, ref string) error {
	if cfg == nil {
		return fmt.Errorf("config is not available")
	}

	configPath := cfg.ConfigPath
	if configPath == "" {
		configPath = "."
	}
	configFilePath := filepath.Join(configPath, "keploy.yml")

	var configData map[string]interface{}
	data, err := os.ReadFile(configFilePath)
	if err != nil {
		if os.IsNotExist(err) {
			configData = make(map[string]interface{})
		} else {
			return fmt.Errorf("failed to read config file %q: %w", configFilePath, err)
		}
	} else {
		if err := yaml.Unmarshal(data, &configData); err != nil {
			return fmt.Errorf("failed to parse config file %q: %w", configFilePath, err)
		}
		if configData == nil {
			configData = make(map[string]interface{})
		}
	}

	sandboxSection, ok := configData["sandbox"].(map[string]interface{})
	if !ok {
		sandboxSection = make(map[string]interface{})
	}
	sandboxSection["ref"] = ref
	configData["sandbox"] = sandboxSection

	updatedData, err := yaml.Marshal(configData)
	if err != nil {
		return fmt.Errorf("failed to marshal updated config: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(configFilePath), 0o755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	if err := os.WriteFile(configFilePath, updatedData, 0o644); err != nil {
		return fmt.Errorf("failed to write config file %q: %w", configFilePath, err)
	}

	return nil
}

func extractUsernameFromJWT(tokenString string) (string, error) {
	token, _, err := new(jwt.Parser).ParseUnverified(tokenString, jwt.MapClaims{})
	if err != nil {
		return "", fmt.Errorf("failed to parse jwt token: %w", err)
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return "", fmt.Errorf("failed to parse jwt claims")
	}

	username, ok := claims["username"].(string)
	if !ok || strings.TrimSpace(username) == "" {
		return "", fmt.Errorf("username not found in jwt token")
	}

	return strings.TrimSpace(username), nil
}
