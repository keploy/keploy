package mcp

import (
	_ "embed"
	"fmt"
	"strings"
	"sync"

	toml "github.com/pelletier/go-toml/v2"
)

//go:embed prompts.toml
var promptTemplatesTOML string

type promptTemplates struct {
	TestIntegration  string `toml:"test_integration"`
	PipelineCreation string `toml:"pipeline_creation"`
}

var (
	templatesOnce sync.Once
	templatesData promptTemplates
	templatesErr  error
)

func loadPromptTemplates() (promptTemplates, error) {
	templatesOnce.Do(func() {
		templatesErr = toml.Unmarshal([]byte(promptTemplatesTOML), &templatesData)
	})
	return templatesData, templatesErr
}

func buildTestIntegrationPrompt(command, scopePath string) string {
	templates, err := loadPromptTemplates()
	if err != nil || strings.TrimSpace(templates.TestIntegration) == "" {
		return fmt.Sprintf("Failed to load test integration prompt template from TOML: %v", err)
	}

	return renderPromptTemplate(templates.TestIntegration, map[string]string{
		"command":    displayOrDefault(command, "not provided"),
		"scope_path": displayOrDefault(scopePath, "not provided"),
	})
}

func buildPipelineCreationPrompt(appCommand, mockPath string) string {
	templates, err := loadPromptTemplates()
	if err != nil || strings.TrimSpace(templates.PipelineCreation) == "" {
		return fmt.Sprintf("Failed to load pipeline creation prompt template from TOML: %v", err)
	}

	if strings.TrimSpace(mockPath) == "" {
		mockPath = "./keploy"
	}

	return renderPromptTemplate(templates.PipelineCreation, map[string]string{
		"app_command":    displayOrDefault(appCommand, "go test -v ./..."),
		"mock_path":      mockPath,
		"keploy_command": fmt.Sprintf("keploy mock test -c \"%s\" -p \"%s\"", safePipelineCommand(appCommand), mockPath),
	})
}

func safePipelineCommand(appCommand string) string {
	cmd := strings.TrimSpace(appCommand)
	if cmd == "" {
		return "go test -v ./..."
	}
	return strings.ReplaceAll(cmd, "\"", "\\\"")
}

func displayOrDefault(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return strings.TrimSpace(s)
}

func renderPromptTemplate(tpl string, values map[string]string) string {
	replacements := make([]string, 0, len(values)*2)
	for key, value := range values {
		replacements = append(replacements, "{{"+key+"}}", value)
	}
	return strings.NewReplacer(replacements...).Replace(tpl)
}
