package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"
)

// handlePipeline handles the pipeline action for CI/CD generation.
// This implements the workflow described in the UNIFIED_TOOL_PROPOSAL.md:
// 1. Gather app command via elicitation if not provided
// 2. Detect CI/CD platform via sampling or file detection
// 3. Generate pipeline using sampling with tools
// 4. Write pipeline file to the project
func (s *Server) handlePipeline(ctx context.Context, in PipelineInput) (*PipelineOutput, error) {
	s.logger.Info("Pipeline action started",
		zap.String("appCommand", in.AppCommand),
		zap.String("defaultBranch", in.DefaultBranch),
		zap.String("mockPath", in.MockPath),
		zap.String("cicdTool", in.CICDTool),
	)

	// Build pipeline configuration with defaults
	config := PipelineConfig{
		AppCommand:    strings.TrimSpace(in.AppCommand),
		DefaultBranch: strings.TrimSpace(in.DefaultBranch),
		MockPath:      strings.TrimSpace(in.MockPath),
		CICDTool:      strings.TrimSpace(in.CICDTool),
	}

	// Set defaults
	if config.DefaultBranch == "" {
		config.DefaultBranch = "main"
	}
	if config.MockPath == "" {
		config.MockPath = "./keploy"
	}

	// Step 1: Check if appCommand is provided, if not try elicitation
	if config.AppCommand == "" {
		// Try to get app command via elicitation
		appCommand, err := s.elicitAppCommand(ctx)
		if err != nil {
			s.logger.Warn("Failed to elicit app command", zap.Error(err))
			return &PipelineOutput{
				Success: false,
				Message: "Error: 'appCommand' is required for pipeline generation. Please provide the command to run your application (e.g., 'go run main.go', 'npm start').",
			}, nil
		}
		if appCommand == "" {
			return &PipelineOutput{
				Success: false,
				Message: "Pipeline creation cancelled: No application command provided.",
			}, nil
		}
		config.AppCommand = appCommand
	}

	// Step 2: Detect or validate CI/CD platform
	if config.CICDTool == "" {
		// Try to auto-detect CI/CD platform
		detectedPlatform, err := s.detectCICDPlatform(ctx)
		if err != nil {
			s.logger.Warn("CI/CD auto-detection failed", zap.Error(err))
		}

		if detectedPlatform != "" && detectedPlatform != "unknown" {
			config.CICDTool = detectedPlatform
			s.logger.Info("Auto-detected CI/CD platform", zap.String("platform", detectedPlatform))
		} else {
			// Try to elicit CI/CD platform from user
			platform, err := s.elicitCICDPlatform(ctx)
			if err != nil {
				s.logger.Warn("Failed to elicit CI/CD platform, defaulting to GitHub Actions", zap.Error(err))
				config.CICDTool = CICDGitHubActions
			} else if platform != "" {
				config.CICDTool = platform
			} else {
				// Default to GitHub Actions
				config.CICDTool = CICDGitHubActions
			}
		}
	}

	// Validate CI/CD platform
	if !isValidCICDPlatform(config.CICDTool) {
		config.CICDTool = CICDGitHubActions
		s.logger.Warn("Invalid CI/CD platform, defaulting to GitHub Actions")
	}

	// Step 3: Generate pipeline content
	pipelineContent, filePath := s.generatePipelineContent(config)

	// Step 4: Write pipeline file
	err := s.writePipelineFile(filePath, pipelineContent)
	if err != nil {
		s.logger.Error("Failed to write pipeline file", zap.Error(err), zap.String("filePath", filePath))
		return &PipelineOutput{
			Success:  false,
			CICDTool: config.CICDTool,
			FilePath: filePath,
			Content:  pipelineContent,
			Message:  fmt.Sprintf("Failed to write pipeline file: %s. You can manually create the file with the content provided.", err.Error()),
			Configuration: &PipelineConfiguration{
				AppCommand:    config.AppCommand,
				DefaultBranch: config.DefaultBranch,
				MockPath:      config.MockPath,
				CICDTool:      config.CICDTool,
			},
		}, nil
	}

	s.logger.Info("Pipeline file created successfully",
		zap.String("filePath", filePath),
		zap.String("cicdTool", config.CICDTool),
	)

	platformName := getPlatformDisplayName(config.CICDTool)
	return &PipelineOutput{
		Success:  true,
		CICDTool: config.CICDTool,
		FilePath: filePath,
		Content:  pipelineContent,
		Message:  fmt.Sprintf("Successfully created %s pipeline at '%s'. The pipeline will run Keploy mock tests on PRs and merges to '%s'.", platformName, filePath, config.DefaultBranch),
		Configuration: &PipelineConfiguration{
			AppCommand:    config.AppCommand,
			DefaultBranch: config.DefaultBranch,
			MockPath:      config.MockPath,
			CICDTool:      config.CICDTool,
		},
	}, nil
}

// elicitAppCommand uses MCP elicitation to request the application command from the user.
func (s *Server) elicitAppCommand(ctx context.Context) (string, error) {
	session := s.getActiveSession()
	if session == nil {
		return "", fmt.Errorf("no active session for elicitation")
	}

	s.logger.Info("Eliciting application command from user")

	// Create elicitation request using MCP Elicitation (Form Mode)
	result, err := session.CreateMessage(ctx, &sdkmcp.CreateMessageParams{
		MaxTokens: 200,
		Messages: []*sdkmcp.SamplingMessage{
			{
				Role: "user",
				Content: &sdkmcp.TextContent{
					Text: `Please provide the application command for Keploy mock testing in your CI/CD pipeline.

I need the following information:
1. Application Command (required): The command to run your application (e.g., 'go run main.go', 'npm start', 'python app.py')
2. Default Branch (optional, default: main): The branch name for merge triggers
3. Mock Path (optional, default: ./keploy): Path where Keploy mocks are stored

Please respond with the application command, or say "cancel" to abort.`,
				},
			},
		},
		SystemPrompt: "You are helping configure a Keploy CI/CD pipeline. Extract the application command from the user's response. If the user provides a command, respond with just the command. If the user wants to cancel, respond with 'CANCELLED'.",
		ModelPreferences: &sdkmcp.ModelPreferences{
			SpeedPriority:        0.9,
			IntelligencePriority: 0.4,
			CostPriority:         0.9,
		},
	})
	if err != nil {
		return "", fmt.Errorf("elicitation failed: %w", err)
	}

	// Extract response
	if textContent, ok := result.Content.(*sdkmcp.TextContent); ok {
		response := strings.TrimSpace(textContent.Text)
		if response == "CANCELLED" || strings.ToLower(response) == "cancel" {
			return "", nil
		}
		return response, nil
	}

	return "", fmt.Errorf("unexpected response format")
}

// elicitCICDPlatform uses MCP elicitation to request the CI/CD platform from the user.
func (s *Server) elicitCICDPlatform(ctx context.Context) (string, error) {
	session := s.getActiveSession()
	if session == nil {
		return "", fmt.Errorf("no active session for elicitation")
	}

	s.logger.Info("Eliciting CI/CD platform from user")

	// Create elicitation request
	result, err := session.CreateMessage(ctx, &sdkmcp.CreateMessageParams{
		MaxTokens: 50,
		Messages: []*sdkmcp.SamplingMessage{
			{
				Role: "user",
				Content: &sdkmcp.TextContent{
					Text: `Could not auto-detect CI/CD tool. Please select your CI/CD platform:

1. GitHub Actions (github-actions)
2. GitLab CI/CD (gitlab-ci)
3. Jenkins (jenkins)
4. CircleCI (circleci)
5. Azure Pipelines (azure-pipelines)
6. Bitbucket Pipelines (bitbucket-pipelines)

Please respond with the platform name or number.`,
				},
			},
		},
		SystemPrompt: "Extract the CI/CD platform from the user's response. Respond with ONLY one of: github-actions, gitlab-ci, jenkins, circleci, azure-pipelines, bitbucket-pipelines. If unclear, respond with: github-actions",
		ModelPreferences: &sdkmcp.ModelPreferences{
			SpeedPriority:        0.9,
			IntelligencePriority: 0.4,
			CostPriority:         0.9,
		},
	})
	if err != nil {
		return "", fmt.Errorf("elicitation failed: %w", err)
	}

	// Extract response
	if textContent, ok := result.Content.(*sdkmcp.TextContent); ok {
		response := strings.TrimSpace(strings.ToLower(textContent.Text))
		// Normalize platform name
		return normalizePlatformName(response), nil
	}

	return "", fmt.Errorf("unexpected response format")
}

// detectCICDPlatform tries to auto-detect the CI/CD platform by checking for config files.
func (s *Server) detectCICDPlatform(ctx context.Context) (string, error) {
	s.logger.Info("Auto-detecting CI/CD platform")

	// Check for CI/CD configuration files
	cicdFiles := s.scanCICDFiles()

	// Build detection prompt for sampling
	session := s.getActiveSession()
	if session == nil {
		// Fall back to file-based detection without LLM
		return s.detectFromFiles(cicdFiles), nil
	}

	// Use MCP Sampling to leverage LLM for intelligent detection
	prompt := s.buildCICDDetectionPrompt(cicdFiles)

	result, err := session.CreateMessage(ctx, &sdkmcp.CreateMessageParams{
		MaxTokens: 50,
		Messages: []*sdkmcp.SamplingMessage{
			{
				Role: "user",
				Content: &sdkmcp.TextContent{
					Text: prompt,
				},
			},
		},
		SystemPrompt: "You are a CI/CD detection assistant. Analyze project files and identify the CI/CD platform in use. Be concise and respond with only the platform identifier.",
		ModelPreferences: &sdkmcp.ModelPreferences{
			SpeedPriority:        0.9,
			IntelligencePriority: 0.4,
			CostPriority:         0.9,
		},
	})
	if err != nil {
		s.logger.Warn("LLM-based CI/CD detection failed, falling back to file detection", zap.Error(err))
		return s.detectFromFiles(cicdFiles), nil
	}

	// Extract response
	if textContent, ok := result.Content.(*sdkmcp.TextContent); ok {
		response := strings.TrimSpace(strings.ToLower(textContent.Text))
		return normalizePlatformName(response), nil
	}

	return s.detectFromFiles(cicdFiles), nil
}

// scanCICDFiles checks for the existence of CI/CD configuration files.
func (s *Server) scanCICDFiles() CICDFiles {
	files := CICDFiles{}

	// Check for GitHub Actions
	if _, err := os.Stat(".github/workflows"); err == nil {
		files.GitHubWorkflows = true
	}

	// Check for GitLab CI
	if _, err := os.Stat(".gitlab-ci.yml"); err == nil {
		files.GitLabCI = true
	}

	// Check for Jenkinsfile
	if _, err := os.Stat("Jenkinsfile"); err == nil {
		files.Jenkinsfile = true
	}

	// Check for CircleCI
	if _, err := os.Stat(".circleci/config.yml"); err == nil {
		files.CircleCI = true
	}

	// Check for Azure Pipelines
	if _, err := os.Stat("azure-pipelines.yml"); err == nil {
		files.AzurePipelines = true
	}

	// Check for Bitbucket Pipelines
	if _, err := os.Stat("bitbucket-pipelines.yml"); err == nil {
		files.BitbucketPipelines = true
	}

	return files
}

// buildCICDDetectionPrompt builds the prompt for LLM-based CI/CD detection.
func (s *Server) buildCICDDetectionPrompt(files CICDFiles) string {
	return fmt.Sprintf(`Analyze the following project structure and determine which CI/CD tool is being used:

Files found:
- .github/workflows/ (directory exists: %t)
- .gitlab-ci.yml (exists: %t)
- Jenkinsfile (exists: %t)
- .circleci/config.yml (exists: %t)
- azure-pipelines.yml (exists: %t)
- bitbucket-pipelines.yml (exists: %t)

Respond with ONLY one of: github-actions, gitlab-ci, jenkins, circleci, azure-pipelines, bitbucket-pipelines, unknown`,
		files.GitHubWorkflows,
		files.GitLabCI,
		files.Jenkinsfile,
		files.CircleCI,
		files.AzurePipelines,
		files.BitbucketPipelines,
	)
}

// detectFromFiles performs simple file-based CI/CD detection.
func (s *Server) detectFromFiles(files CICDFiles) string {
	if files.GitHubWorkflows {
		return CICDGitHubActions
	}
	if files.GitLabCI {
		return CICDGitLabCI
	}
	if files.Jenkinsfile {
		return CICDJenkins
	}
	if files.CircleCI {
		return CICDCircleCI
	}
	if files.AzurePipelines {
		return CICDAzurePipelines
	}
	if files.BitbucketPipelines {
		return CICDBitbucketPipelines
	}
	return "unknown"
}

// generatePipelineContent generates the pipeline content based on the configuration.
func (s *Server) generatePipelineContent(config PipelineConfig) (content string, filePath string) {
	details := getPlatformDetails(config.CICDTool)
	filePath = details.FilePath

	switch config.CICDTool {
	case CICDGitHubActions:
		content = generateGitHubActionsWorkflow(config)
	case CICDGitLabCI:
		content = generateGitLabCIPipeline(config)
	case CICDJenkins:
		content = generateJenkinsfile(config)
	case CICDCircleCI:
		content = generateCircleCIConfig(config)
	case CICDAzurePipelines:
		content = generateAzurePipeline(config)
	case CICDBitbucketPipelines:
		content = generateBitbucketPipeline(config)
	default:
		// Default to GitHub Actions
		content = generateGitHubActionsWorkflow(config)
		filePath = ".github/workflows/keploy-mock-test.yml"
	}

	return content, filePath
}

// writePipelineFile writes the pipeline content to the specified file.
func (s *Server) writePipelineFile(filePath, content string) error {
	// Create directory if needed
	dir := filepath.Dir(filePath)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create directory: %w", err)
		}
	}

	// Write the file
	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}

	return nil
}

// buildPipelineGenerationPrompt builds the prompt for LLM-based pipeline generation.
func buildPipelineGenerationPrompt(config PipelineConfig) string {
	details := getPlatformDetails(config.CICDTool)

	return fmt.Sprintf(
		"Generate a %s for Keploy mock testing with the following configuration:\n"+
			"- App command: %s\n"+
			"- Default branch: %s\n"+
			"- Mock path: %s\n"+
			"- Run on: %s %s\n\n"+
			"Use the write_file tool to create the %s file.",
		details.PipelineName,
		config.AppCommand,
		config.DefaultBranch,
		config.MockPath,
		details.TriggerText,
		config.DefaultBranch,
		details.FilePath,
	)
}

// getPlatformDetails returns the details for a CI/CD platform.
func getPlatformDetails(platform string) PlatformDetails {
	switch platform {
	case CICDGitHubActions:
		return PlatformDetails{
			FilePath:     ".github/workflows/keploy-mock-test.yml",
			PipelineName: "GitHub Actions workflow",
			TriggerText:  "PR and merge to",
		}
	case CICDGitLabCI:
		return PlatformDetails{
			FilePath:     ".gitlab-ci.yml",
			PipelineName: "GitLab CI/CD pipeline",
			TriggerText:  "MR and merge to",
		}
	case CICDJenkins:
		return PlatformDetails{
			FilePath:     "Jenkinsfile",
			PipelineName: "Jenkins pipeline",
			TriggerText:  "PR and merge to",
		}
	case CICDCircleCI:
		return PlatformDetails{
			FilePath:     ".circleci/config.yml",
			PipelineName: "CircleCI pipeline",
			TriggerText:  "PR and merge to",
		}
	case CICDAzurePipelines:
		return PlatformDetails{
			FilePath:     "azure-pipelines.yml",
			PipelineName: "Azure Pipelines",
			TriggerText:  "PR and merge to",
		}
	case CICDBitbucketPipelines:
		return PlatformDetails{
			FilePath:     "bitbucket-pipelines.yml",
			PipelineName: "Bitbucket Pipelines",
			TriggerText:  "PR and merge to",
		}
	default:
		return PlatformDetails{
			FilePath:     ".github/workflows/keploy-mock-test.yml",
			PipelineName: "GitHub Actions workflow",
			TriggerText:  "PR and merge to",
		}
	}
}

// getPlatformDisplayName returns the display name for a CI/CD platform.
func getPlatformDisplayName(platform string) string {
	switch platform {
	case CICDGitHubActions:
		return "GitHub Actions"
	case CICDGitLabCI:
		return "GitLab CI/CD"
	case CICDJenkins:
		return "Jenkins"
	case CICDCircleCI:
		return "CircleCI"
	case CICDAzurePipelines:
		return "Azure Pipelines"
	case CICDBitbucketPipelines:
		return "Bitbucket Pipelines"
	default:
		return "GitHub Actions"
	}
}

// isValidCICDPlatform checks if the platform is valid.
func isValidCICDPlatform(platform string) bool {
	switch platform {
	case CICDGitHubActions, CICDGitLabCI, CICDJenkins, CICDCircleCI, CICDAzurePipelines, CICDBitbucketPipelines:
		return true
	default:
		return false
	}
}

// normalizePlatformName normalizes the platform name to the standard format.
func normalizePlatformName(input string) string {
	input = strings.TrimSpace(strings.ToLower(input))

	// Handle various formats
	switch {
	case strings.Contains(input, "github") || input == "1":
		return CICDGitHubActions
	case strings.Contains(input, "gitlab") || input == "2":
		return CICDGitLabCI
	case strings.Contains(input, "jenkins") || input == "3":
		return CICDJenkins
	case strings.Contains(input, "circle") || input == "4":
		return CICDCircleCI
	case strings.Contains(input, "azure") || input == "5":
		return CICDAzurePipelines
	case strings.Contains(input, "bitbucket") || input == "6":
		return CICDBitbucketPipelines
	default:
		return input
	}
}

// samplingRequestForPipelineGeneration creates a sampling request for pipeline generation.
// This is used when we want the LLM to generate the pipeline with tool use.
func (s *Server) samplingRequestForPipelineGeneration(ctx context.Context, config PipelineConfig) (string, error) {
	session := s.getActiveSession()
	if session == nil {
		return "", fmt.Errorf("no active session for sampling")
	}

	prompt := buildPipelineGenerationPrompt(config)

	// Define tools for the sampling request
	tools := []json.RawMessage{
		json.RawMessage(`{
			"name": "write_file",
			"description": "Write content to a file in the project",
			"inputSchema": {
				"type": "object",
				"properties": {
					"path": {
						"type": "string",
						"description": "File path relative to project root"
					},
					"content": {
						"type": "string",
						"description": "File content to write"
					}
				},
				"required": ["path", "content"]
			}
		}`),
		json.RawMessage(`{
			"name": "validate_yaml",
			"description": "Validate YAML syntax",
			"inputSchema": {
				"type": "object",
				"properties": {
					"content": {
						"type": "string",
						"description": "YAML content to validate"
					}
				},
				"required": ["content"]
			}
		}`),
	}

	// Log the tools for debugging (sampling with tools not directly supported by current SDK)
	s.logger.Debug("Pipeline generation sampling request prepared",
		zap.String("prompt", prompt),
		zap.Int("toolCount", len(tools)),
	)

	// For now, we use our template-based generation instead of LLM sampling with tools
	// This can be enhanced when the MCP SDK supports sampling with tools
	return prompt, nil
}
