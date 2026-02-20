package mcp

import (
	"embed"
	"fmt"
	"strings"
)

//go:embed prompts/*.toml
var promptTemplatesFS embed.FS

func loadPromptTemplate(path string) (string, error) {
	data, err := promptTemplatesFS.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func buildTestCommandPrompt(testCommand string) string {
	template, err := loadPromptTemplate("prompts/app_test_command.toml")
	if err != nil || strings.TrimSpace(template) == "" {
		return fmt.Sprintf("Failed to load test command prompt template: %v", err)
	}

	return renderPromptTemplate(template, map[string]string{
		"test_command": strings.TrimSpace(testCommand),
	})
}

func buildDependencyStartPrompt(appCommand, scopePath string) string {
	template, err := loadPromptTemplate("prompts/dependency_start.toml")
	if err != nil || strings.TrimSpace(template) == "" {
		return fmt.Sprintf("Failed to load dependency start prompt template: %v", err)
	}

	if strings.TrimSpace(scopePath) == "" {
		scopePath = "."
	}

	return renderPromptTemplate(template, map[string]string{
		"app_command": strings.TrimSpace(appCommand),
		"scope_path":  strings.TrimSpace(scopePath),
	})
}

func buildTestIntegrationPrompt(testCommand, scopePath string) string {
	template, err := loadPromptTemplate("prompts/test_integration.toml")
	if err != nil || strings.TrimSpace(template) == "" {
		return fmt.Sprintf("Failed to load test integration prompt template: %v", err)
	}

	return renderPromptTemplate(template, map[string]string{
		"test_command": strings.TrimSpace(testCommand),
		"scope_path":   strings.TrimSpace(scopePath),
	})
}

func buildPipelineCreationPrompt(testCommand string) string {
	template, err := loadPromptTemplate("prompts/pipeline_creation.toml")
	if err != nil || strings.TrimSpace(template) == "" {
		return fmt.Sprintf("Failed to load pipeline creation prompt template: %v", err)
	}

	trimmedCommand := strings.TrimSpace(testCommand)

	return renderPromptTemplate(template, map[string]string{
		"test_command": trimmedCommand,
	})
}

func renderPromptTemplate(tpl string, values map[string]string) string {
	replacements := make([]string, 0, len(values)*2)
	for key, value := range values {
		replacements = append(replacements, "{{"+key+"}}", value)
	}
	return strings.NewReplacer(replacements...).Replace(tpl)
}
