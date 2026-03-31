package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.keploy.io/server/v3/pkg/models"
	sandboxsvc "go.keploy.io/server/v3/pkg/service/sandbox"
	"go.uber.org/zap"
)

// MockRecordInput defines the input parameters for the mock record tool.
type MockRecordInput struct {
	// Command is the command to run. If empty, server attempts elicitation.
	Command string `json:"command,omitempty" jsonschema:"Command to run (prefer test commands like 'go test -v ./...'). If empty, server will elicit it from user."`
	// Path is the sandbox location directory (default: .).
	Path string `json:"path,omitempty" jsonschema:"Sandbox location directory (default: .)."`
	// Name is the sandbox file prefix (final file: <name>.sb.yaml).
	Name string `json:"name,omitempty" jsonschema:"Sandbox file prefix (default: keploy, final file: <name>.sb.yaml)."`
	// Tag is the semantic version tag for sandbox record workflows.
	Tag string `json:"tag" jsonschema:"Semantic version tag for sandbox record workflows (for example 'v1.0.0'). AI should generate this when not provided by user."`
}

// MockRecordOutput defines the output of the mock record tool.
type MockRecordOutput struct {
	// Success indicates whether recording was successful.
	Success bool `json:"success"`
	// MockFilePath is the path to the generated mock file.
	MockFilePath string `json:"mockFilePath"`
	// MockCount is the number of mocks recorded.
	MockCount int `json:"mockCount"`
	// Protocols lists the protocols detected in recorded mocks.
	Protocols []string `json:"protocols"`
	// Message is a human-readable status message.
	Message string `json:"message"`
	// Configuration shows the configuration that was used.
	Configuration *RecordConfiguration `json:"configuration,omitempty"`
}

// RecordConfiguration shows the configuration used for recording.
type RecordConfiguration struct {
	Command string `json:"command"`
	Path    string `json:"path"`
	Name    string `json:"name"`
	Tag     string `json:"tag,omitempty"`
	Ref     string `json:"ref,omitempty"`
}

// MockReplayInput defines the input parameters for the mock replay tool.
type MockReplayInput struct {
	// Command is the command to run with mocks.
	Command string `json:"command" jsonschema:"Command to run with sandbox replay (e.g. 'go test -v', 'npm test', or any other command)."`
	// Path is the sandbox location directory.
	Path string `json:"path,omitempty" jsonschema:"Sandbox location directory (default: .)."`
	// Name is the sandbox file prefix.
	Name string `json:"name,omitempty" jsonschema:"Sandbox file prefix (default: keploy, final file: <name>.sb.yaml)."`
	// FallBackOnMiss indicates whether to fall back to real calls when no mock matches (optional, default: false).
	FallBackOnMiss bool `json:"fallBackOnMiss,omitempty" jsonschema:"Whether to fall back to real calls when no sandbox entry matches (default: false)."`
	// Local indicates local-only sandbox replay mode.
	Local bool `json:"local,omitempty" jsonschema:"Local-only sandbox replay mode (skip cloud sync/upload, default: false)."`
}

// MockReplayOutput defines the output of the mock replay tool.
type MockReplayOutput struct {
	// Success indicates whether replay was successful.
	Success bool `json:"success"`
	// MocksReplayed is the number of mocks that were replayed.
	MocksReplayed int `json:"mocksReplayed"`
	// MocksMissed is the number of unmatched calls.
	MocksMissed int `json:"mocksMissed"`
	// AppExitCode is the application exit code.
	AppExitCode int `json:"appExitCode"`
	// Message is a human-readable status message.
	Message string `json:"message"`
	// Configuration shows the configuration that was used.
	Configuration *ReplayConfiguration `json:"configuration,omitempty"`
}

// PromptTestCommandInput defines input for keploy_prompt_test_command.
type PromptTestCommandInput struct {
	// TestCommand is optional context command for refinement/validation.
	TestCommand string `json:"testCommand,omitempty" jsonschema:"Optional existing test command context to refine/validate."`
}

// PromptDependencyStartInput defines input for keploy_prompt_dependency_start.
type PromptDependencyStartInput struct {
	// AppCommand is optional app/test command context for dependency discovery and startup checks.
	AppCommand string `json:"appCommand,omitempty" jsonschema:"Optional app/test command context for dependency detection and startup checks."`
	// ScopePath optionally narrows dependency discovery/startup to a subtree.
	ScopePath string `json:"scopePath,omitempty" jsonschema:"Optional path scope for dependency checks/startup."`
}

// PromptTestIntegrationInput defines input for keploy_prompt_test_integration.
type PromptTestIntegrationInput struct {
	// Command provides optional command context to narrow test discovery scope.
	Command string `json:"command,omitempty" jsonschema:"Optional test command context (e.g., 'go test -v ./pkg/auth/...')."`
	// ScopePath optionally narrows edits to a subtree.
	ScopePath string `json:"scopePath,omitempty" jsonschema:"Optional path scope for test file edits."`
}

// PromptPipelineInput defines input for keploy_prompt_pipeline_creation.
type PromptPipelineInput struct {
	// AppCommand is the app/test command used in keploy sandbox replay.
	AppCommand string `json:"appCommand,omitempty" jsonschema:"Optional app/test command for pipeline prompt."`
}

// PromptOutput is raw text prompt output for prompt helper tools.
type PromptOutput struct {
	// Success indicates whether prompt generation was successful.
	Success bool `json:"success"`
	// Prompt is raw prompt text for client LLM execution.
	Prompt string `json:"prompt"`
	// Message is status text.
	Message string `json:"message"`
}

// ReplayConfiguration shows the configuration used for replay.
type ReplayConfiguration struct {
	Command        string `json:"command"`
	Path           string `json:"path"`
	Name           string `json:"name"`
	FallBackOnMiss bool   `json:"fallBackOnMiss"`
	Local          bool   `json:"local,omitempty"`
	Ref            string `json:"ref,omitempty"`
}

// handleMockRecord handles the keploy_mock_record tool invocation.
func (s *Server) handleMockRecord(ctx context.Context, req *sdkmcp.CallToolRequest, in MockRecordInput) (*sdkmcp.CallToolResult, MockRecordOutput, error) {
	s.logger.Info("Mock record tool invoked",
		zap.String("command", in.Command),
		zap.String("path", in.Path),
		zap.String("name", in.Name),
		zap.String("tag", in.Tag),
	)

	command := strings.TrimSpace(in.Command)
	if command == "" {
		elictedCommand, err := s.elicitRecordCommand(ctx)
		if err != nil {
			return nil, MockRecordOutput{
				Success:   false,
				Protocols: []string{}, // Must be non-nil for JSON schema validation
				Message:   fmt.Sprintf("Error: failed to get command via elicitation: %s", err.Error()),
			}, nil
		}
		command = strings.TrimSpace(elictedCommand)
		if command == "" {
			return nil, MockRecordOutput{
				Success:   false,
				Protocols: []string{}, // Must be non-nil for JSON schema validation
				Message:   "Mock recording cancelled: no command provided.",
			}, nil
		}
	}
	if s.cfg == nil {
		return nil, MockRecordOutput{
			Success:   false,
			Protocols: []string{}, // Must be non-nil for JSON schema validation
			Message:   "Recording failed: config is not available for sandbox record workflow.",
		}, nil
	}

	// Parse and validate configuration
	path := strings.TrimSpace(in.Path)
	if path == "" {
		path = "."
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = "keploy"
	}
	tag := strings.TrimSpace(in.Tag)
	if tag == "" {
		return nil, MockRecordOutput{
			Success:   false,
			Protocols: []string{}, // Must be non-nil for JSON schema validation
			Message:   "Recording failed: 'tag' is required (semantic version, e.g. v1.0.0).",
		}, nil
	}
	tag, err := sandboxsvc.ParseTag(tag)
	if err != nil {
		return nil, MockRecordOutput{
			Success:   false,
			Protocols: []string{}, // Must be non-nil for JSON schema validation
			Message:   fmt.Sprintf("Recording failed: invalid tag: %s", err.Error()),
		}, nil
	}

	jwtToken, err := getSandboxJWTTokenFunc(ctx, s.logger, s.cfg)
	if err != nil {
		return nil, MockRecordOutput{
			Success:   false,
			Protocols: []string{}, // Must be non-nil for JSON schema validation
			Message:   fmt.Sprintf("Recording failed: failed to authenticate user for sandbox record: %s", err.Error()),
		}, nil
	}

	ref, err := buildSandboxRefFromTag(s.logger, s.cfg, tag, jwtToken)
	if err != nil {
		return nil, MockRecordOutput{
			Success:   false,
			Protocols: []string{}, // Must be non-nil for JSON schema validation
			Message:   fmt.Sprintf("Recording failed: failed to infer sandbox ref from tag: %s", err.Error()),
		}, nil
	}

	config := &RecordConfiguration{
		Command: command,
		Path:    path,
		Name:    name,
		Tag:     tag,
		Ref:     ref,
	}

	// Check if mock recorder is available
	if s.mockRecorder == nil {
		return nil, MockRecordOutput{
			Success:       false,
			Protocols:     []string{}, // Must be non-nil for JSON schema validation
			Configuration: config,
			Message:       "Error: Mock recorder service is not available.",
		}, nil
	}

	// Show configuration and execute
	s.logger.Info("Starting mock recording with configuration",
		zap.String("command", command),
		zap.String("path", path),
		zap.String("name", name),
		zap.String("tag", tag),
	)

	// Execute recording
	result, err := s.mockRecorder.Record(ctx, models.RecordOptions{
		Command:   command,
		Path:      path,
		Name:      name,
		Duration:  s.cfg.Record.RecordTimer,
		ProxyPort: s.cfg.ProxyPort,
		DNSPort:   s.cfg.DNSPort,
	})
	if err != nil {
		s.logger.Error("Mock recording failed", zap.Error(err))
		return nil, MockRecordOutput{
			Success:       false,
			Protocols:     []string{}, // Must be non-nil for JSON schema validation
			Configuration: config,
			Message:       fmt.Sprintf("Recording failed: %s", err.Error()),
		}, nil
	}

	s.cfg.Sandbox.Ref = ref
	if err := updateSandboxRefInConfig(s.cfg, ref); err != nil {
		return nil, MockRecordOutput{
			Success:       false,
			Protocols:     []string{},
			Configuration: config,
			Message:       fmt.Sprintf("Recording failed: failed to update sandbox ref in config: %s", err.Error()),
		}, nil
	}
	s.logger.Info("Updated sandbox ref in config from MCP record flow", zap.String("ref", ref))

	// Ensure protocols is never nil for JSON schema validation (must be array, not null)
	protocols := []string{}
	if result.Metadata != nil && result.Metadata.Protocols != nil {
		protocols = result.Metadata.Protocols
	}

	s.logger.Info("Mock recording completed successfully",
		zap.String("mockFilePath", result.MockFilePath),
		zap.Int("mockCount", result.MockCount),
		zap.Strings("protocols", protocols),
	)

	return nil, MockRecordOutput{
		Success:       true,
		MockFilePath:  result.MockFilePath,
		MockCount:     result.MockCount,
		Protocols:     protocols,
		Configuration: config,
		Message:       fmt.Sprintf("Successfully recorded %d mock(s) to '%s'. Updated sandbox.ref to '%s'. Detected protocols: %v", result.MockCount, result.MockFilePath, ref, protocols),
	}, nil
}

func (s *Server) elicitRecordCommand(ctx context.Context) (string, error) {
	session := s.getActiveSession()
	if session == nil {
		return "", fmt.Errorf("no active session for elicitation")
	}

	s.logger.Info("Eliciting mock record command from user")
	result, err := session.Elicit(ctx, &sdkmcp.ElicitParams{
		Mode: "form",
		Message: "Please provide the command for `keploy sandbox record`.\n\n" +
			"Policy:\n" +
			"- Prefer test commands over run commands.\n" +
			"- For Go projects, prefer `go test` commands (for example `go test -v -run \"TestA|TestB\"` or `go test -v ./...`).\n" +
			"- Do not default to `go run main.go` when tests exist.\n" +
			"- Avoid long-running/watch/interactive commands.\n",
		RequestedSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "Command to execute for mock recording",
				},
			},
			"required":             []string{"command"},
			"additionalProperties": false,
		},
	})
	if err != nil {
		return "", fmt.Errorf("elicitation failed: %w", err)
	}

	if result == nil {
		return "", nil
	}
	if result.Action != "accept" {
		return "", nil
	}

	rawCommand, ok := result.Content["command"]
	if !ok {
		return "", nil
	}
	command, _ := rawCommand.(string)
	return strings.TrimSpace(command), nil
}

// handleMockReplay handles the keploy_mock_test tool invocation.
func (s *Server) handleMockReplay(ctx context.Context, req *sdkmcp.CallToolRequest, in MockReplayInput) (*sdkmcp.CallToolResult, MockReplayOutput, error) {
	s.logger.Info("Mock replay tool invoked",
		zap.String("command", in.Command),
		zap.String("path", in.Path),
		zap.String("name", in.Name),
		zap.Bool("fallBackOnMiss", in.FallBackOnMiss),
	)

	// Validate input
	command := strings.TrimSpace(in.Command)
	if command == "" {
		return nil, MockReplayOutput{
			Success: false,
			Message: "Error: 'command' is required. Please provide the test command to run (e.g., 'go test -v', 'npm test').",
		}, nil
	}
	path := strings.TrimSpace(in.Path)
	if req != nil && !hasArgument(req, "path") {
		path = "."
	}
	if path == "" {
		path = "."
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = "keploy"
	}
	if s.cfg == nil {
		return nil, MockReplayOutput{
			Success: false,
			Message: "Error: config is not available for sandbox replay workflow.",
		}, nil
	}

	// Check if mock replayer is available
	if s.mockReplayer == nil {
		return nil, MockReplayOutput{
			Success: false,
			Message: "Error: Mock replayer service is not available.",
		}, nil
	}

	refInConfig := strings.TrimSpace(s.cfg.Sandbox.Ref)
	if refInConfig == "" {
		return nil, MockReplayOutput{
			Success: false,
			Message: "Error: sandbox ref not found in config (keploy.yml). Run 'keploy sandbox record --tag <tag>' first.",
		}, nil
	}

	ref := refInConfig
	if _, _, _, err := sandboxsvc.ParseRef(ref); err != nil {
		return nil, MockReplayOutput{
			Success: false,
			Message: fmt.Sprintf("Error: invalid sandbox ref in config (expected <company>/<service>:<tag>): %s", err.Error()),
		}, nil
	}

	config := &ReplayConfiguration{
		Command:        command,
		Path:           path,
		Name:           name,
		FallBackOnMiss: in.FallBackOnMiss,
		Local:          in.Local,
		Ref:            ref,
	}

	shouldUploadAfterReplay := false
	var sbSvc sandboxsvc.Service
	if in.Local {
		s.logger.Info("Local-only sandbox replay enabled for MCP tool, skipping cloud sync")
	} else if strings.TrimSpace(s.cfg.APIServerURL) != "" {
		jwtToken, err := getSandboxJWTTokenFunc(ctx, s.logger, s.cfg)
		if err != nil {
			return nil, MockReplayOutput{
				Success:       false,
				Configuration: config,
				Message:       fmt.Sprintf("Replay failed: failed to authenticate user for sandbox replay: %s", err.Error()),
			}, nil
		}

		sbSvc = newSandboxServiceFunc(s.cfg.APIServerURL, jwtToken, s.logger)
		err = sbSvc.Sync(ctx, ref, path)
		if err != nil {
			if errors.Is(err, sandboxsvc.ErrManifestNotFound) {
				shouldUploadAfterReplay = true
				s.logger.Info("sandbox manifest not found in cloud, proceeding with local sandbox replay",
					zap.String("ref", ref),
				)
			} else {
				return nil, MockReplayOutput{
					Success:       false,
					Configuration: config,
					Message:       fmt.Sprintf("Replay failed: sandbox sync failed: %s", err.Error()),
				}, nil
			}
		} else {
			s.logger.Info("Sandbox synced from cloud for MCP replay", zap.String("ref", ref))
		}
	} else {
		s.logger.Info("No API server URL configured, using local sandbox files only for MCP replay")
	}

	s.logger.Info("Starting mock replay with configuration",
		zap.String("command", command),
		zap.String("path", path),
		zap.String("name", name),
		zap.Bool("fallBackOnMiss", in.FallBackOnMiss),
	)

	// Execute replay
	result, err := s.mockReplayer.Replay(ctx, models.ReplayOptions{
		Command:        command,
		Path:           path,
		Name:           name,
		ProxyPort:      s.cfg.ProxyPort,
		DNSPort:        s.cfg.DNSPort,
		FallBackOnMiss: in.FallBackOnMiss,
	})
	if err != nil {
		s.logger.Error("Mock replay failed", zap.Error(err))
		return nil, MockReplayOutput{
			Success:       false,
			Configuration: config,
			Message:       fmt.Sprintf("Replay failed: %s", err.Error()),
		}, nil
	}

	// Build result message
	var messageParts []string
	mocksLoaded := result.MocksReplayed + result.MocksMissed
	mocksUnused := result.MocksMissed
	messageParts = append(messageParts, fmt.Sprintf("Loaded %d mock(s)", mocksLoaded))
	messageParts = append(messageParts, fmt.Sprintf("Replayed %d mock(s)", result.MocksReplayed))
	if mocksUnused > 0 {
		messageParts = append(messageParts, fmt.Sprintf("%d mock(s) unused", mocksUnused))
	}

	if result.AppExitCode != 0 {
		messageParts = append(messageParts, fmt.Sprintf("app exited with code %d", result.AppExitCode))
	} else {
		messageParts = append(messageParts, "app exited successfully")
	}

	message := strings.Join(messageParts, ", ")
	if result.Success {
		message = "Test passed! " + message
	} else {
		message = "Test completed with issues. " + message
	}

	if result.Success && shouldUploadAfterReplay {
		if sbSvc == nil {
			return nil, MockReplayOutput{
				Success:       false,
				Configuration: config,
				Message:       "Replay failed: internal error, sandbox service unavailable for post-replay upload.",
			}, nil
		}
		s.logger.Info("Uploading sandbox to cloud after successful local replay from MCP flow",
			zap.String("ref", ref),
		)
		if err := sbSvc.Upload(ctx, ref, path); err != nil {
			return nil, MockReplayOutput{
				Success:       false,
				Configuration: config,
				Message:       fmt.Sprintf("Replay failed: sandbox upload after replay failed: %s", err.Error()),
			}, nil
		}
		s.logger.Info("Sandbox uploaded to cloud successfully after MCP replay",
			zap.String("ref", ref),
		)
		message = message + ", sandbox uploaded to cloud"
	}

	s.logger.Info("Mock replay completed",
		zap.Bool("success", result.Success),
		zap.Int("mocksReplayed", result.MocksReplayed),
		zap.Int("mocksLoaded", mocksLoaded),
		zap.Int("mocksUnused", mocksUnused),
		zap.Int("exitCode", result.AppExitCode),
	)

	return nil, MockReplayOutput{
		Success:       result.Success,
		MocksReplayed: result.MocksReplayed,
		MocksMissed:   result.MocksMissed,
		AppExitCode:   result.AppExitCode,
		Configuration: config,
		Message:       message,
	}, nil
}

func hasArgument(req *sdkmcp.CallToolRequest, key string) bool {
	if req == nil || req.Params == nil || len(req.Params.Arguments) == 0 {
		return false
	}

	args := map[string]json.RawMessage{}
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return false
	}

	_, ok := args[key]
	return ok
}

// handlePromptTestCommand returns a raw prompt for deriving the best serialized app test command.
func (s *Server) handlePromptTestCommand(_ context.Context, _ *sdkmcp.CallToolRequest, in PromptTestCommandInput) (*sdkmcp.CallToolResult, PromptOutput, error) {
	prompt := buildTestCommandPrompt(in.TestCommand)
	return nil, PromptOutput{
		Success: true,
		Prompt:  prompt,
		Message: "Generated test command prompt. Client LLM should execute this prompt as a direct user task.",
	}, nil
}

// handlePromptDependencyStart returns a raw prompt for dependency discovery, health checks, and startup.
func (s *Server) handlePromptDependencyStart(_ context.Context, _ *sdkmcp.CallToolRequest, in PromptDependencyStartInput) (*sdkmcp.CallToolResult, PromptOutput, error) {
	prompt := buildDependencyStartPrompt(in.AppCommand, in.ScopePath)
	return nil, PromptOutput{
		Success: true,
		Prompt:  prompt,
		Message: "Generated dependency startup prompt. Client LLM should execute this prompt as a direct user task before mock recording.",
	}, nil
}

// handlePromptTestIntegration returns a raw prompt for automatic sandbox scope integration in test files.
func (s *Server) handlePromptTestIntegration(_ context.Context, _ *sdkmcp.CallToolRequest, in PromptTestIntegrationInput) (*sdkmcp.CallToolResult, PromptOutput, error) {
	prompt := buildTestIntegrationPrompt(in.Command, in.ScopePath)
	return nil, PromptOutput{
		Success: true,
		Prompt:  prompt,
		Message: "Generated test integration prompt. Client LLM should execute this prompt as a direct user task.",
	}, nil
}

// handlePromptPipelineCreation returns a raw prompt for CI/CD pipeline generation.
func (s *Server) handlePromptPipelineCreation(_ context.Context, _ *sdkmcp.CallToolRequest, in PromptPipelineInput) (*sdkmcp.CallToolResult, PromptOutput, error) {
	prompt := buildPipelineCreationPrompt(in.AppCommand)
	return nil, PromptOutput{
		Success: true,
		Prompt:  prompt,
		Message: "Generated pipeline creation prompt. Client LLM should execute this prompt as a direct user task.",
	}, nil
}
