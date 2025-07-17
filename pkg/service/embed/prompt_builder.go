package embed

import (
	"bytes"
	"fmt"
	"html"
	"strings"
	"text/template"

	"go.keploy.io/server/v2/pkg/service/embed/assets"
	"go.uber.org/zap"
)

type Prompt struct {
	System string
	User   string
}

type PromptBuilder struct {
	Question    string
	CodeContext string
	Logger      *zap.Logger
}

func NewPromptBuilder(question, codeContext string, logger *zap.Logger) *PromptBuilder {
	return &PromptBuilder{
		Question:    question,
		CodeContext: codeContext,
		Logger:      logger,
	}
}

func (pb *PromptBuilder) BuildPrompt(file string) (*Prompt, error) {
	pb.Logger.Debug("Building prompt for conversation", zap.String("file", file))

	variables := map[string]interface{}{
		"Question":    pb.Question,
		"CodeContext": pb.CodeContext,
	}

	settings := assets.GetSettings()
	prompt := &Prompt{}

	val := settings.Get(file + ".system")
	systemPromptTemplate, _ := val.(string)
	if systemPromptTemplate == "" {
		pb.Logger.Error("System prompt template not found", zap.String("templateKey", file+".system"))
		return nil, fmt.Errorf("system prompt template not found for: %s.system", file)
	}

	systemPrompt, err := renderTemplate(systemPromptTemplate, variables)
	if err != nil {
		pb.Logger.Error("Error rendering system prompt", zap.Error(err), zap.String("templateKey", file+".system"))
		return nil, fmt.Errorf("error rendering system prompt: %v", err)
	}
	prompt.System = systemPrompt

	val = settings.Get(file + ".user")
	userPromptTemplate, _ := val.(string)
	if userPromptTemplate == "" {
		pb.Logger.Error("User prompt template not found", zap.String("templateKey", file+".user"))
		return nil, fmt.Errorf("user prompt template not found for: %s.user", file)
	}

	userPrompt, err := renderTemplate(userPromptTemplate, variables)
	if err != nil {
		pb.Logger.Error("Error rendering user prompt", zap.Error(err), zap.String("templateKey", file+".user"))
		return nil, fmt.Errorf("error rendering user prompt: %v", err)
	}
	prompt.User = html.UnescapeString(userPrompt)

	return prompt, nil
}

func renderTemplate(templateText string, variables map[string]interface{}) (string, error) {
	if templateText == "" {
		return "", fmt.Errorf("template text is empty")
	}

	funcMap := template.FuncMap{
		"trim": strings.TrimSpace,
	}
	tmpl, err := template.New("prompt").Funcs(funcMap).Parse(templateText)
	if err != nil {
		return "", fmt.Errorf("error parsing template: %w", err)
	}
	var buffer bytes.Buffer
	err = tmpl.Execute(&buffer, variables)
	if err != nil {
		return "", fmt.Errorf("error executing template: %w", err)
	}
	return buffer.String(), nil
}
