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

// ProjectInfo contains detected project information for dependency setup.
type ProjectInfo struct {
	Language        string   // go, node, python, java, etc.
	Framework       string   // gin, express, django, spring, etc.
	PackageManager  string   // go mod, npm, yarn, pip, maven, gradle, etc.
	RuntimeVersion  string   // e.g., "1.21", "18", "3.11"
	SetupSteps      []string // Additional setup commands
	DependencyFiles []string // Files found: go.mod, package.json, etc.
}

// handlePipeline handles the pipeline action for CI/CD generation.
// Workflow:
// Phase 0: Input Validation & Initialization
// Phase 1: Application Command Acquisition (via input or elicitation)
// Phase 2: CI/CD Platform Detection (file scan + sampling/elicitation)
// Phase 3: Project Analysis & Language Detection (sampling-based)
// Phase 4: Pipeline Content Generation (templates with setup steps)
// Phase 5: File System Operations (with overwrite protection)
// Phase 6: Response Construction
func (s *Server) handlePipeline(ctx context.Context, in PipelineInput) (*PipelineOutput, error) {
	s.logger.Info("Pipeline action started",
		zap.String("appCommand", in.AppCommand),
		zap.String("defaultBranch", in.DefaultBranch),
		zap.String("mockPath", in.MockPath),
		zap.String("cicdTool", in.CICDTool),
	)

	// Phase 0: Build pipeline configuration with defaults
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
	// Note: MockPath is NOT defaulted here - we need to detect/elicit the specific mock set

	// Phase 1: Application Command Acquisition
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

	// Phase 1.5: Mock Path Detection & Validation
	// The mockPath should point to a specific mock set directory (e.g., ./keploy/mock-set-0)
	// not the root keploy folder. We need to detect existing mock sets or elicit from user.
	if config.MockPath == "" {
		mockPath, err := s.detectOrElicitMockPath(ctx)
		if err != nil {
			s.logger.Warn("Failed to detect/elicit mock path", zap.Error(err))
			return &PipelineOutput{
				Success: false,
				Message: "Error: 'mockPath' is required for pipeline generation. Please provide the path to your mock set directory (e.g., './keploy/mock-set-0'). You can find this from your most recent 'keploy mock record' session.",
			}, nil
		}
		if mockPath == "" {
			return &PipelineOutput{
				Success: false,
				Message: "Pipeline creation cancelled: No mock path provided. Please run 'keploy mock record' first to create mock sets, then specify the mock set path.",
			}, nil
		}
		config.MockPath = mockPath
	} else {
		// Validate the provided mock path
		if !s.isValidMockPath(config.MockPath) {
			s.logger.Warn("Provided mock path may not be a specific mock set",
				zap.String("mockPath", config.MockPath))
			// Check if it's the root keploy folder and suggest specific mock sets
			if config.MockPath == "./keploy" || config.MockPath == "keploy" || config.MockPath == "./keploy/" {
				mockSetNames, err := s.scanMockSets("./keploy")
				if err == nil && len(mockSetNames) > 0 {
					s.logger.Info("Found mock sets in keploy folder, will use specific path")
					// Use the most recent mock set (first in the sorted list)
					config.MockPath = filepath.Join("./keploy", mockSetNames[0])
					s.logger.Info("Auto-selected most recent mock set", zap.String("mockPath", config.MockPath))
				}
			}
		}
	}

	// Phase 2: CI/CD Platform Detection
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

	// Phase 3: Project Analysis & Language Detection
	projectInfo := s.analyzeProject(ctx, config.AppCommand)
	s.logger.Info("Project analysis complete",
		zap.String("language", projectInfo.Language),
		zap.String("framework", projectInfo.Framework),
		zap.String("packageManager", projectInfo.PackageManager),
		zap.String("runtimeVersion", projectInfo.RuntimeVersion),
	)

	// Phase 4: Generate pipeline content with project-specific setup
	pipelineContent, filePath := s.generatePipelineContentWithSetup(config, projectInfo)

	// Phase 5: File System Operations with overwrite protection
	fileExists := s.checkFileExists(filePath)
	if fileExists {
		s.logger.Warn("Pipeline file already exists", zap.String("filePath", filePath))
		// For now, we overwrite but log a warning. Future: add user confirmation via elicitation
	}

	err := s.writePipelineFile(filePath, pipelineContent)
	if err != nil {
		s.logger.Error("Failed to write pipeline file", zap.Error(err), zap.String("filePath", filePath))

		// Build detected project info for error response too
		var detectedProject *DetectedProjectInfo
		if projectInfo.Language != "" {
			detectedProject = &DetectedProjectInfo{
				Language:       projectInfo.Language,
				Framework:      projectInfo.Framework,
				PackageManager: projectInfo.PackageManager,
				RuntimeVersion: projectInfo.RuntimeVersion,
				SetupSteps:     projectInfo.SetupSteps,
			}
		}

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
			DetectedProject: detectedProject,
		}, nil
	}

	s.logger.Info("Pipeline file created successfully",
		zap.String("filePath", filePath),
		zap.String("cicdTool", config.CICDTool),
	)

	// Phase 6: Response Construction
	platformName := getPlatformDisplayName(config.CICDTool)
	overrwriteNote := ""
	if fileExists {
		overrwriteNote = " (existing file was overwritten)"
	}

	message := fmt.Sprintf("Successfully created %s pipeline at '%s'%s. The pipeline will run Keploy mock tests on PRs and merges to '%s'.",
		platformName, filePath, overrwriteNote, config.DefaultBranch)

	if projectInfo.Language != "" {
		message += fmt.Sprintf(" Detected %s project with %s setup included.", projectInfo.Language, projectInfo.PackageManager)
	}

	// Build detected project info for response
	var detectedProject *DetectedProjectInfo
	if projectInfo.Language != "" {
		detectedProject = &DetectedProjectInfo{
			Language:       projectInfo.Language,
			Framework:      projectInfo.Framework,
			PackageManager: projectInfo.PackageManager,
			RuntimeVersion: projectInfo.RuntimeVersion,
			SetupSteps:     projectInfo.SetupSteps,
		}
	}

	return &PipelineOutput{
		Success:  true,
		CICDTool: config.CICDTool,
		FilePath: filePath,
		Content:  pipelineContent,
		Message:  message,
		Configuration: &PipelineConfiguration{
			AppCommand:    config.AppCommand,
			DefaultBranch: config.DefaultBranch,
			MockPath:      config.MockPath,
			CICDTool:      config.CICDTool,
		},
		DetectedProject: detectedProject,
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

// checkFileExists checks if a file exists at the given path.
func (s *Server) checkFileExists(filePath string) bool {
	_, err := os.Stat(filePath)
	return err == nil
}

// detectOrElicitMockPath detects existing mock sets or elicits the path from the user.
func (s *Server) detectOrElicitMockPath(ctx context.Context) (string, error) {
	s.logger.Info("Detecting mock path")

	// First, scan for existing mock sets in common locations using existing scanMockSets from tools.go
	mockSetNames, err := s.scanMockSets("./keploy")
	if err != nil {
		s.logger.Debug("Failed to scan mock sets", zap.Error(err))
	}

	// Convert mock set names to full paths
	var mockSets []string
	for _, name := range mockSetNames {
		mockSets = append(mockSets, filepath.Join("./keploy", name))
	}

	if len(mockSets) > 0 {
		s.logger.Info("Found mock sets", zap.Strings("mockSets", mockSets))

		// If only one mock set, use it automatically
		if len(mockSets) == 1 {
			s.logger.Info("Auto-selecting single mock set", zap.String("mockPath", mockSets[0]))
			return mockSets[0], nil
		}

		// Multiple mock sets found - try to elicit user choice
		return s.elicitMockPathFromOptions(ctx, mockSets)
	}

	// No mock sets found - elicit from user
	s.logger.Info("No mock sets found, eliciting from user")
	return s.elicitMockPath(ctx)
}

// isValidMockPath checks if the provided mock path is a specific mock set (not the root folder).
func (s *Server) isValidMockPath(mockPath string) bool {
	// Check if it's not just the root keploy folder
	normalized := strings.TrimSuffix(strings.TrimPrefix(mockPath, "./"), "/")
	if normalized == "keploy" || normalized == "" {
		return false
	}

	// Check if the path exists and contains mock files
	if _, err := os.Stat(mockPath); os.IsNotExist(err) {
		return false
	}

	return s.containsMockFiles(mockPath)
}

// containsMockFiles checks if a directory contains Keploy mock files.
func (s *Server) containsMockFiles(dir string) bool {
	// Check for mocks.yaml or mocks.yml
	if _, err := os.Stat(filepath.Join(dir, "mocks.yaml")); err == nil {
		return true
	}
	if _, err := os.Stat(filepath.Join(dir, "mocks.yml")); err == nil {
		return true
	}
	return false
}

// elicitMockPath elicits the mock path from the user when no mock sets are found.
func (s *Server) elicitMockPath(ctx context.Context) (string, error) {
	session := s.getActiveSession()
	if session == nil {
		return "", fmt.Errorf("no active session for elicitation")
	}

	s.logger.Info("Eliciting mock path from user")

	result, err := session.CreateMessage(ctx, &sdkmcp.CreateMessageParams{
		MaxTokens: 200,
		Messages: []*sdkmcp.SamplingMessage{
			{
				Role: "user",
				Content: &sdkmcp.TextContent{
					Text: `No mock sets were found in the ./keploy directory.

To generate a CI/CD pipeline for Keploy mock testing, I need the path to your mock set directory.

Mock sets are created when you run 'keploy mock record'. They are typically located at:
- ./keploy/mock-set-0
- ./keploy/mocks-<name>
- ./keploy/<custom-name>

Please provide the path to your mock set directory, or say "cancel" to abort.

Example: ./keploy/mock-set-0`,
				},
			},
		},
		SystemPrompt: "Extract the mock path from the user's response. If the user provides a path, respond with just the path (starting with ./ if relative). If the user wants to cancel, respond with 'CANCELLED'. If the response is unclear, ask for clarification.",
		ModelPreferences: &sdkmcp.ModelPreferences{
			SpeedPriority:        0.8,
			IntelligencePriority: 0.5,
			CostPriority:         0.9,
		},
	})
	if err != nil {
		return "", fmt.Errorf("elicitation failed: %w", err)
	}

	if textContent, ok := result.Content.(*sdkmcp.TextContent); ok {
		response := strings.TrimSpace(textContent.Text)
		if response == "CANCELLED" || strings.ToLower(response) == "cancel" {
			return "", nil
		}
		// Normalize the path
		if !strings.HasPrefix(response, "./") && !strings.HasPrefix(response, "/") {
			response = "./" + response
		}
		return response, nil
	}

	return "", fmt.Errorf("unexpected response format")
}

// elicitMockPathFromOptions presents the user with found mock sets to choose from.
func (s *Server) elicitMockPathFromOptions(ctx context.Context, mockSets []string) (string, error) {
	session := s.getActiveSession()
	if session == nil {
		// No session - use the most recent mock set
		return mockSets[0], nil
	}

	s.logger.Info("Eliciting mock path selection from user", zap.Int("options", len(mockSets)))

	// Build the options list
	var options strings.Builder
	options.WriteString("Multiple mock sets were found. Please select which one to use in your CI/CD pipeline:\n\n")
	for i, mockSet := range mockSets {
		options.WriteString(fmt.Sprintf("%d. %s\n", i+1, mockSet))
	}
	options.WriteString("\nPlease respond with the number or the full path. The first option is the most recent.")

	result, err := session.CreateMessage(ctx, &sdkmcp.CreateMessageParams{
		MaxTokens: 100,
		Messages: []*sdkmcp.SamplingMessage{
			{
				Role: "user",
				Content: &sdkmcp.TextContent{
					Text: options.String(),
				},
			},
		},
		SystemPrompt: fmt.Sprintf("The user is selecting a mock set. Available options are numbered 1-%d. If the user provides a number, respond with the corresponding path. If they provide a path directly, use that. If unclear, respond with the first option (most recent). Respond with ONLY the path.", len(mockSets)),
		ModelPreferences: &sdkmcp.ModelPreferences{
			SpeedPriority:        0.9,
			IntelligencePriority: 0.4,
			CostPriority:         0.9,
		},
	})
	if err != nil {
		s.logger.Warn("Failed to elicit mock set selection, using most recent", zap.Error(err))
		return mockSets[0], nil
	}

	if textContent, ok := result.Content.(*sdkmcp.TextContent); ok {
		response := strings.TrimSpace(textContent.Text)

		// Check if response is a number
		if num, err := fmt.Sscanf(response, "%d", new(int)); err == nil && num == 1 {
			var idx int
			fmt.Sscanf(response, "%d", &idx)
			if idx >= 1 && idx <= len(mockSets) {
				return mockSets[idx-1], nil
			}
		}

		// Check if response matches any mock set
		for _, mockSet := range mockSets {
			if strings.Contains(mockSet, response) || response == mockSet {
				return mockSet, nil
			}
		}

		// If it looks like a path, use it
		if strings.Contains(response, "/") || strings.Contains(response, "keploy") {
			if !strings.HasPrefix(response, "./") && !strings.HasPrefix(response, "/") {
				response = "./" + response
			}
			return response, nil
		}
	}

	// Default to most recent
	return mockSets[0], nil
}

// analyzeProject detects the programming language and framework from the project files and app command.
func (s *Server) analyzeProject(ctx context.Context, appCommand string) ProjectInfo {
	info := ProjectInfo{}

	// Scan for project files
	dependencyFiles := s.scanDependencyFiles()
	info.DependencyFiles = dependencyFiles

	// Detect language and package manager based on files found
	info = s.detectLanguageFromFiles(info, dependencyFiles)

	// If no files found, try to detect from app command
	if info.Language == "" {
		info = s.detectLanguageFromCommand(info, appCommand)
	}

	// Try to enhance detection via MCP sampling if session is available
	session := s.getActiveSession()
	if session != nil && info.Language != "" {
		enhanced := s.enhanceProjectInfoViaSampling(ctx, info, appCommand)
		if enhanced.RuntimeVersion != "" {
			info.RuntimeVersion = enhanced.RuntimeVersion
		}
		if enhanced.Framework != "" {
			info.Framework = enhanced.Framework
		}
	}

	// Set default runtime versions if not detected
	info = s.setDefaultRuntimeVersion(info)

	// Generate setup steps based on detected language
	info.SetupSteps = s.generateSetupSteps(info)

	return info
}

// scanDependencyFiles scans for common dependency/project files.
func (s *Server) scanDependencyFiles() []string {
	var files []string

	dependencyFilePatterns := []string{
		"go.mod",
		"go.sum",
		"package.json",
		"package-lock.json",
		"yarn.lock",
		"pnpm-lock.yaml",
		"requirements.txt",
		"Pipfile",
		"pyproject.toml",
		"setup.py",
		"pom.xml",
		"build.gradle",
		"build.gradle.kts",
		"Gemfile",
		"Cargo.toml",
		"composer.json",
	}

	for _, pattern := range dependencyFilePatterns {
		if _, err := os.Stat(pattern); err == nil {
			files = append(files, pattern)
		}
	}

	return files
}

// detectLanguageFromFiles detects language and package manager from dependency files.
func (s *Server) detectLanguageFromFiles(info ProjectInfo, files []string) ProjectInfo {
	for _, file := range files {
		switch file {
		case "go.mod", "go.sum":
			info.Language = "go"
			info.PackageManager = "go mod"
			return info
		case "package.json":
			info.Language = "node"
			// Check for yarn or pnpm
			for _, f := range files {
				if f == "yarn.lock" {
					info.PackageManager = "yarn"
					return info
				}
				if f == "pnpm-lock.yaml" {
					info.PackageManager = "pnpm"
					return info
				}
			}
			info.PackageManager = "npm"
			return info
		case "requirements.txt", "Pipfile", "pyproject.toml", "setup.py":
			info.Language = "python"
			if file == "Pipfile" {
				info.PackageManager = "pipenv"
			} else if file == "pyproject.toml" {
				info.PackageManager = "poetry"
			} else {
				info.PackageManager = "pip"
			}
			return info
		case "pom.xml":
			info.Language = "java"
			info.PackageManager = "maven"
			return info
		case "build.gradle", "build.gradle.kts":
			info.Language = "java"
			info.PackageManager = "gradle"
			return info
		case "Gemfile":
			info.Language = "ruby"
			info.PackageManager = "bundler"
			return info
		case "Cargo.toml":
			info.Language = "rust"
			info.PackageManager = "cargo"
			return info
		case "composer.json":
			info.Language = "php"
			info.PackageManager = "composer"
			return info
		}
	}
	return info
}

// detectLanguageFromCommand detects language from the application command.
func (s *Server) detectLanguageFromCommand(info ProjectInfo, appCommand string) ProjectInfo {
	cmd := strings.ToLower(appCommand)

	switch {
	case strings.HasPrefix(cmd, "go ") || strings.Contains(cmd, "go run") || strings.Contains(cmd, "go build"):
		info.Language = "go"
		info.PackageManager = "go mod"
	case strings.HasPrefix(cmd, "node ") || strings.HasPrefix(cmd, "npm ") || strings.HasPrefix(cmd, "yarn ") || strings.HasPrefix(cmd, "pnpm "):
		info.Language = "node"
		if strings.HasPrefix(cmd, "yarn ") {
			info.PackageManager = "yarn"
		} else if strings.HasPrefix(cmd, "pnpm ") {
			info.PackageManager = "pnpm"
		} else {
			info.PackageManager = "npm"
		}
	case strings.HasPrefix(cmd, "python ") || strings.HasPrefix(cmd, "python3 ") || strings.HasPrefix(cmd, "pip "):
		info.Language = "python"
		info.PackageManager = "pip"
	case strings.HasPrefix(cmd, "java ") || strings.Contains(cmd, "mvn ") || strings.Contains(cmd, "gradle "):
		info.Language = "java"
		if strings.Contains(cmd, "gradle") {
			info.PackageManager = "gradle"
		} else {
			info.PackageManager = "maven"
		}
	case strings.HasPrefix(cmd, "ruby ") || strings.HasPrefix(cmd, "rails ") || strings.HasPrefix(cmd, "bundle "):
		info.Language = "ruby"
		info.PackageManager = "bundler"
	case strings.HasPrefix(cmd, "cargo ") || strings.Contains(cmd, "cargo run"):
		info.Language = "rust"
		info.PackageManager = "cargo"
	case strings.HasPrefix(cmd, "php ") || strings.Contains(cmd, "artisan"):
		info.Language = "php"
		info.PackageManager = "composer"
	}

	return info
}

// enhanceProjectInfoViaSampling uses MCP sampling to get more details about the project.
func (s *Server) enhanceProjectInfoViaSampling(ctx context.Context, info ProjectInfo, appCommand string) ProjectInfo {
	session := s.getActiveSession()
	if session == nil {
		return info
	}

	prompt := fmt.Sprintf(`Analyze this project configuration and provide the recommended runtime version:

Language: %s
Package Manager: %s
App Command: %s
Dependency Files Found: %v

Respond with ONLY a JSON object in this format (no markdown, no explanation):
{"runtimeVersion": "1.21", "framework": "gin"}

Use appropriate version for the language:
- Go: "1.21" or "1.22"
- Node: "18" or "20"
- Python: "3.11" or "3.12"
- Java: "17" or "21"

For framework, detect from the command or common patterns (gin, echo, express, fastapi, django, spring, etc.)
If unsure, use empty string for framework.`,
		info.Language,
		info.PackageManager,
		appCommand,
		info.DependencyFiles,
	)

	result, err := session.CreateMessage(ctx, &sdkmcp.CreateMessageParams{
		MaxTokens: 100,
		Messages: []*sdkmcp.SamplingMessage{
			{
				Role: "user",
				Content: &sdkmcp.TextContent{
					Text: prompt,
				},
			},
		},
		SystemPrompt: "You are a project analysis assistant. Respond with ONLY valid JSON, no markdown code blocks, no explanation.",
		ModelPreferences: &sdkmcp.ModelPreferences{
			SpeedPriority:        0.9,
			IntelligencePriority: 0.5,
			CostPriority:         0.9,
		},
	})
	if err != nil {
		s.logger.Debug("Failed to enhance project info via sampling", zap.Error(err))
		return info
	}

	if textContent, ok := result.Content.(*sdkmcp.TextContent); ok {
		response := strings.TrimSpace(textContent.Text)
		// Try to parse JSON response
		var enhanced struct {
			RuntimeVersion string `json:"runtimeVersion"`
			Framework      string `json:"framework"`
		}
		if err := json.Unmarshal([]byte(response), &enhanced); err == nil {
			if enhanced.RuntimeVersion != "" {
				info.RuntimeVersion = enhanced.RuntimeVersion
			}
			if enhanced.Framework != "" {
				info.Framework = enhanced.Framework
			}
		}
	}

	return info
}

// setDefaultRuntimeVersion sets default runtime versions if not already set.
func (s *Server) setDefaultRuntimeVersion(info ProjectInfo) ProjectInfo {
	if info.RuntimeVersion != "" {
		return info
	}

	switch info.Language {
	case "go":
		info.RuntimeVersion = "1.21"
	case "node":
		info.RuntimeVersion = "20"
	case "python":
		info.RuntimeVersion = "3.11"
	case "java":
		info.RuntimeVersion = "17"
	case "ruby":
		info.RuntimeVersion = "3.2"
	case "rust":
		info.RuntimeVersion = "stable"
	case "php":
		info.RuntimeVersion = "8.2"
	}

	return info
}

// generateSetupSteps generates the setup commands for the detected language/framework.
func (s *Server) generateSetupSteps(info ProjectInfo) []string {
	var steps []string

	switch info.Language {
	case "go":
		steps = append(steps, "go mod download")
	case "node":
		switch info.PackageManager {
		case "yarn":
			steps = append(steps, "yarn install --frozen-lockfile")
		case "pnpm":
			steps = append(steps, "pnpm install --frozen-lockfile")
		default:
			steps = append(steps, "npm ci")
		}
	case "python":
		switch info.PackageManager {
		case "poetry":
			steps = append(steps, "pip install poetry", "poetry install")
		case "pipenv":
			steps = append(steps, "pip install pipenv", "pipenv install")
		default:
			steps = append(steps, "pip install -r requirements.txt")
		}
	case "java":
		switch info.PackageManager {
		case "gradle":
			steps = append(steps, "./gradlew build -x test")
		default:
			steps = append(steps, "mvn install -DskipTests")
		}
	case "ruby":
		steps = append(steps, "bundle install")
	case "rust":
		steps = append(steps, "cargo build")
	case "php":
		steps = append(steps, "composer install --no-dev")
	}

	return steps
}

// generatePipelineContentWithSetup generates the pipeline content with project-specific setup steps.
func (s *Server) generatePipelineContentWithSetup(config PipelineConfig, projectInfo ProjectInfo) (content string, filePath string) {
	details := getPlatformDetails(config.CICDTool)
	filePath = details.FilePath

	switch config.CICDTool {
	case CICDGitHubActions:
		content = generateGitHubActionsWorkflowWithSetup(config, projectInfo)
	case CICDGitLabCI:
		content = generateGitLabCIPipelineWithSetup(config, projectInfo)
	case CICDJenkins:
		content = generateJenkinsfileWithSetup(config, projectInfo)
	case CICDCircleCI:
		content = generateCircleCIConfigWithSetup(config, projectInfo)
	case CICDAzurePipelines:
		content = generateAzurePipelineWithSetup(config, projectInfo)
	case CICDBitbucketPipelines:
		content = generateBitbucketPipelineWithSetup(config, projectInfo)
	default:
		// Default to GitHub Actions
		content = generateGitHubActionsWorkflowWithSetup(config, projectInfo)
		filePath = ".github/workflows/keploy-mock-test.yml"
	}

	return content, filePath
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
