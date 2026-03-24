package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/golang-jwt/jwt/v4"
	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	platformAuth "go.keploy.io/server/v3/pkg/platform/auth"
	"go.keploy.io/server/v3/pkg/service/mockrecord"
	"go.keploy.io/server/v3/pkg/service/mockreplay"
	"go.keploy.io/server/v3/pkg/service/sandbox"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

var (
	getSandboxJWTTokenFunc = getSandboxJWTToken
	newSandboxServiceFunc  = func(apiServerURL, jwtToken string, logger *zap.Logger) sandbox.Service {
		cloudClient := sandbox.NewCloudClient(apiServerURL, jwtToken, logger)
		return sandbox.New(cloudClient, logger)
	}
	newMockReplayServiceFunc = func(logger *zap.Logger, cfg *config.Config, runtime mockreplay.Runtime) mockreplay.Service {
		return mockreplay.New(logger, cfg, runtime)
	}
)

func init() {
	Register("sandbox", Sandbox)
}

func Sandbox(ctx context.Context, logger *zap.Logger, cfg *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:   "sandbox",
		Short: "Managing sandbox",
	}

	cmd.AddCommand(SandboxRecord(ctx, logger, cfg, serviceFactory, cmdConfigurator))
	cmd.AddCommand(SandboxReplay(ctx, logger, cfg, serviceFactory, cmdConfigurator))
	for _, subCmd := range cmd.Commands() {
		err := cmdConfigurator.AddFlags(subCmd)
		if err != nil {
			utils.LogError(logger, err, "failed to add flags to command", zap.String("command", subCmd.Name()))
		}
	}
	return cmd
}

func SandboxRecord(ctx context.Context, logger *zap.Logger, cfg *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:   "record",
		Short: "record outgoing calls as sandboxes",
		Example: `  # Local-only recording (no cloud, no auth required):
  keploy sandbox record -c "npx playwright test" --name "e2e"

  # With cloud sync (requires KEPLOY_API_KEY or login):
  keploy sandbox record -c "go test -v" --tag "v3.3.0" --name "main_test"`,
		SilenceUsage: true,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Read command directly from flag — cfg.Command may be cleared by
			// viper unmarshalling between PreRunE and RunE.
			appCommand, _ := cmd.Flags().GetString("command")
			if appCommand == "" {
				appCommand = cfg.Command
			}
			if appCommand == "" {
				return errors.New("missing required -c flag: specify the command to run")
			}

			tagInput, err := cmd.Flags().GetString("tag")
			if err != nil {
				utils.LogError(logger, err, "failed to get tag flag")
				return errors.New("failed to get tag flag")
			}
			tagInput = strings.TrimSpace(tagInput)

			// When --tag is provided, authenticate and build cloud ref.
			// When --tag is omitted, record locally without cloud integration.
			var ref string
			if tagInput != "" {
				tag, err := sandbox.ParseTag(tagInput)
				if err != nil {
					utils.LogError(logger, err, "invalid --tag value", zap.String("tag", tagInput))
					return fmt.Errorf("invalid --tag value: %w", err)
				}

				jwtToken, err := getSandboxJWTTokenFunc(ctx, logger, cfg)
				if err != nil {
					utils.LogError(logger, err, "failed to authenticate user for sandbox record")
					return fmt.Errorf("failed to authenticate user for sandbox record: %w", err)
				}

				ref, err = buildSandboxRefFromTag(logger, cfg, tag, jwtToken)
				if err != nil {
					return fmt.Errorf("failed to infer sandbox ref from --tag: %w", err)
				}
			} else {
				logger.Info("No --tag provided, recording locally without cloud sync")
			}

			// Restore command into cfg before GetService — needed by downstream services.
			cfg.Command = appCommand

			recordSvc, err := serviceFactory.GetService(ctx, "record")
			if err != nil {
				utils.LogError(logger, err, "failed to get record service")
				return nil
			}

			// Re-set after GetService in case it was cleared again.
			cfg.Command = appCommand

			runner, ok := recordSvc.(mockrecord.RecordRunner)
			if !ok {
				utils.LogError(logger, nil, "service doesn't satisfy record runner interface")
				return nil
			}

			recorder := mockrecord.New(logger, cfg, runner, nil)

			name, err := cmd.Flags().GetString("name")
			if err != nil {
				utils.LogError(logger, err, "failed to get name flag")
				return errors.New("failed to get name flag")
			}

			logger.Debug("sandbox record: invoking recorder.Record",
				zap.String("cfg.Command", cfg.Command),
				zap.String("cfg.Path", cfg.Path),
				zap.String("name", name),
			)

			result, err := recorder.Record(ctx, models.RecordOptions{
				Command:   cfg.Command,
				Path:      cfg.Path,
				Name:      name,
				Duration:  cfg.Record.RecordTimer,
				ProxyPort: cfg.ProxyPort,
				DNSPort:   cfg.DNSPort,
			})
			if err != nil {
				utils.LogError(logger, err, "failed to record mocks")
				return nil
			}

			if output := strings.TrimSpace(result.Output); output != "" {
				fmt.Fprintln(cmd.OutOrStdout(), output)
			}
			if result.AppExitCode != 0 {
				logger.Warn("sandbox record command exited with non-zero code",
					zap.Int("exitCode", result.AppExitCode),
				)
			}

			logger.Info("Sandbox recording completed",
				zap.Int("sandboxCount", result.MockCount),
				zap.Strings("protocols", result.Metadata.Protocols),
				zap.String("sandboxFilePath", result.MockFilePath),
				zap.Int("exitCode", result.AppExitCode),
			)

			// Only write sandbox ref to keploy.yml when --tag was provided (cloud mode).
			if ref != "" {
				cfg.Sandbox.Ref = ref
				if err := updateSandboxRefInConfig(cfg, ref); err != nil {
					utils.LogError(logger, err, "failed to update sandbox ref in config")
					return nil
				}
				logger.Info("Updated sandbox ref in config",
					zap.String("ref", ref),
				)
			}

			return nil
		},
	}

	return cmd
}

func SandboxReplay(ctx context.Context, logger *zap.Logger, cfg *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:   "replay",
		Short: "replay recorded sandboxes during testing",
		Example: `  # Local-only replay (no cloud sync):
  keploy sandbox replay -c "npx playwright test" --local --name "e2e"

  # With cloud sync (reads ref from keploy.yml):
  keploy sandbox replay -c "go test -v" --name "main_test"`,
		SilenceUsage: true,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Read command directly from flag — cfg.Command may be cleared.
			appCommand, _ := cmd.Flags().GetString("command")
			if appCommand == "" {
				appCommand = cfg.Command
			}
			if appCommand == "" {
				return errors.New("missing required -c flag: specify the command to run")
			}
			cfg.Command = appCommand

			localOnly, err := cmd.Flags().GetBool("local")
			if err != nil {
				utils.LogError(logger, err, "failed to get local flag")
				return errors.New("failed to get local flag")
			}

			// Step 1: Determine sandbox ref.
			// In local-only mode, ref is optional (just replay whatever .sb.yaml files exist).
			// In cloud mode, ref is required for sync.
			refInConfig := strings.TrimSpace(cfg.Sandbox.Ref)
			ref := refInConfig

			shouldUploadAfterReplay := false
			var sbSvc sandbox.Service
			basePath := cfg.Path
			if basePath == "" {
				basePath = "."
			}

			// Step 2: Sync from cloud if not local-only and ref + API server are configured.
			if localOnly {
				logger.Info("Local-only sandbox replay, skipping cloud sync")
			} else if ref == "" {
				logger.Info("No sandbox ref in config, using local sandbox files only")
			} else {
				if _, _, _, err := sandbox.ParseRef(ref); err != nil {
					return fmt.Errorf("invalid sandbox ref in config (expected <company>/<service>:<tag>): %w", err)
				}
				logger.Info("Found sandbox reference in config", zap.String("value", refInConfig))

				if cfg.APIServerURL != "" {
					jwtToken, err := getSandboxJWTTokenFunc(ctx, logger, cfg)
					if err != nil {
						utils.LogError(logger, err, "failed to authenticate user for sandbox replay")
						return fmt.Errorf("failed to authenticate user for sandbox replay: %w", err)
					}

					sbSvc = newSandboxServiceFunc(cfg.APIServerURL, jwtToken, logger)

					err = sbSvc.Sync(ctx, ref, basePath)
					if err != nil {
						if errors.Is(err, sandbox.ErrManifestNotFound) {
							shouldUploadAfterReplay = true
							logger.Info("sandbox manifest not found in cloud, proceeding with local sandbox replay",
								zap.String("ref", ref),
							)
						} else {
							utils.LogError(logger, err, "failed to sync sandbox from cloud")
							return fmt.Errorf("sandbox sync failed: %w", err)
						}
					} else {
						logger.Info("Sandbox synced from cloud", zap.String("ref", ref))
					}
				} else {
					logger.Info("No API server URL configured, using local sandbox files only")
				}
			}

			// Step 3: Proceed with normal mock replay.
			cfg.Command = appCommand

			replaySvc, err := serviceFactory.GetService(ctx, "test")
			if err != nil {
				utils.LogError(logger, err, "failed to get replay service")
				return nil
			}

			cfg.Command = appCommand

			runtime, ok := replaySvc.(mockreplay.Runtime)
			if !ok {
				utils.LogError(logger, nil, "service doesn't satisfy replay runtime interface")
				return nil
			}

			name, err := cmd.Flags().GetString("name")
			if err != nil {
				utils.LogError(logger, err, "failed to get name flag")
				return errors.New("failed to get name flag")
			}

			replayer := newMockReplayServiceFunc(logger, cfg, runtime)
			result, err := replayer.Replay(ctx, models.ReplayOptions{
				Command:   cfg.Command,
				Path:      cfg.Path,
				Name:      name,
				ProxyPort: cfg.ProxyPort,
				DNSPort:   cfg.DNSPort,
			})
			if err != nil {
				utils.LogError(logger, err, "failed to replay mocks")
				return fmt.Errorf("sandbox replay failed: %w", err)
			}

			if output := strings.TrimSpace(result.Output); output != "" {
				fmt.Fprintln(cmd.OutOrStdout(), output)
			}

			mocksLoaded := result.MocksReplayed + result.MocksMissed
			mocksUnused := result.MocksMissed
			logger.Info("Sandbox replay completed",
				zap.Bool("success", result.Success),
				zap.Int("sandboxesReplayed", result.MocksReplayed),
				zap.Int("sandboxesLoaded", mocksLoaded),
				zap.Int("sandboxesUnused", mocksUnused),
				zap.Int("exitCode", result.AppExitCode),
			)

			if !result.Success {
				return errors.New("sandbox replay failed: tests did not pass")
			}

			if shouldUploadAfterReplay && sbSvc != nil && ref != "" {
				logger.Info("Uploading sandbox to cloud after successful local replay",
					zap.String("ref", ref),
				)
				err = sbSvc.Upload(ctx, ref, basePath)
				if err != nil {
					utils.LogError(logger, err, "failed to upload sandbox to cloud after replay")
					return fmt.Errorf("sandbox upload after replay failed: %w", err)
				}
				logger.Info("Sandbox uploaded to cloud successfully after replay",
					zap.String("ref", ref),
				)
			}

			return nil
		},
	}

	return cmd
}

// updateSandboxRefInConfig writes the sandbox ref to the keploy.yml config file.
// It always overwrites the sandbox.ref value.
func updateSandboxRefInConfig(cfg *config.Config, ref string) error {
	configPath := cfg.ConfigPath
	if configPath == "" {
		configPath = "."
	}
	configFilePath := filepath.Join(configPath, "keploy.yml")

	// Read existing config file if it exists.
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

	// Set sandbox.ref (always overwrite).
	sandboxSection, ok := configData["sandbox"].(map[string]interface{})
	if !ok {
		sandboxSection = make(map[string]interface{})
	}
	sandboxSection["ref"] = ref
	configData["sandbox"] = sandboxSection

	// Write back the config.
	updatedData, err := yaml.Marshal(configData)
	if err != nil {
		return fmt.Errorf("failed to marshal updated config: %w", err)
	}

	// Ensure directory exists.
	if err := os.MkdirAll(filepath.Dir(configFilePath), 0o755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	if err := os.WriteFile(configFilePath, updatedData, 0o644); err != nil {
		return fmt.Errorf("failed to write config file %q: %w", configFilePath, err)
	}

	return nil
}

func buildSandboxRefFromTag(logger *zap.Logger, cfg *config.Config, tag string, jwtToken string) (string, error) {
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

	ref, err := sandbox.BuildRef(username, appName, tag)
	if err != nil {
		return "", err
	}

	logger.Debug("Inferred sandbox ref from tag",
		zap.String("tag", tag),
		zap.String("username", username),
		zap.String("appName", appName),
		zap.String("ref", ref),
	)

	return ref, nil
}

func getSandboxJWTToken(ctx context.Context, logger *zap.Logger, cfg *config.Config) (string, error) {
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
