package utgen

import (
	"bytes"
	"fmt"
	"html/template"
	"io/ioutil"
	"log"
	"path/filepath"
	"strings"

	"go.keploy.io/server/v2/pkg/service/utgen/settings"
)

const MAX_TESTS_PER_RUN = 4

// Constants for additional includes, instructions, and failed tests
const ADDITIONAL_INCLUDES_TEXT = `
## Additional Includes
The following is a set of included files used as context for the source code above. This is usually included libraries needed as context to write better tests:
======
{{.IncludedFiles}}
======
`

const ADDITIONAL_INSTRUCTIONS_TEXT = `
## Additional Instructions
======
{{.AdditionalInstructions}}
======
`

const FAILED_TESTS_TEXT = `
## Previous Iterations Failed Tests
Below is a list of failed tests that you generated in previous iterations. Do not generate the same tests again, and take the failed tests into account when generating new tests.
======
{{.FailedTestRuns}}
======
`

type PromptBuilder struct {
	SourceFileName         string
	TestFileName           string
	SourceFile             string
	TestFile               string
	CodeCoverageReport     string
	IncludedFiles          string
	AdditionalInstructions string
	FailedTestRuns         string
	Language               string
	SourceFileNumbered     string
	TestFileNumbered       string
}

func NewPromptBuilder(
	sourceFilePath, testFilePath, codeCoverageReport, includedFiles, additionalInstructions, failedTestRuns, language string,
) *PromptBuilder {
	return &PromptBuilder{
		SourceFileName:         filepath.Base(sourceFilePath),
		TestFileName:           filepath.Base(testFilePath),
		SourceFile:             readFile(sourceFilePath),
		TestFile:               readFile(testFilePath),
		CodeCoverageReport:     codeCoverageReport,
		IncludedFiles:          formatSection(includedFiles, ADDITIONAL_INCLUDES_TEXT),
		AdditionalInstructions: formatSection(additionalInstructions, ADDITIONAL_INSTRUCTIONS_TEXT),
		FailedTestRuns:         formatSection(failedTestRuns, FAILED_TESTS_TEXT),
		Language:               language,
	}
}

func readFile(filePath string) string {
	content, err := ioutil.ReadFile(filePath)
	if err != nil {
		return fmt.Sprintf("Error reading %s: %v", filePath, err)
	}
	return string(content)
}

func formatSection(content, templateText string) string {
	if content == "" {
		return ""
	}
	tmpl, err := template.New("section").Parse(templateText)
	if err != nil {
		log.Printf("Error parsing section template: %v", err)
		return ""
	}
	var buffer bytes.Buffer
	err = tmpl.Execute(&buffer, map[string]string{
		"IncludedFiles":          content,
		"AdditionalInstructions": content,
		"FailedTestRuns":         content,
	})
	if err != nil {
		log.Printf("Error executing section template: %v", err)
		return ""
	}
	return buffer.String()
}

func (pb *PromptBuilder) BuildPrompt() *Prompt {
	pb.SourceFileNumbered = numberLines(pb.SourceFile)
	pb.TestFileNumbered = numberLines(pb.TestFile)

	variables := map[string]interface{}{
		"source_file_name":             pb.SourceFileName,
		"test_file_name":               pb.TestFileName,
		"source_file_numbered":         pb.SourceFileNumbered,
		"test_file_numbered":           pb.TestFileNumbered,
		"source_file":                  pb.SourceFile,
		"test_file":                    pb.TestFile,
		"code_coverage_report":         pb.CodeCoverageReport,
		"additional_includes_section":  pb.IncludedFiles,
		"failed_tests_section":         pb.FailedTestRuns,
		"additional_instructions_text": pb.AdditionalInstructions,
		"language":                     pb.Language,
		"max_tests":                    MAX_TESTS_PER_RUN,
	}

	settings := settings.GetSettings()
	prompt := &Prompt{}

	systemPrompt, err := renderTemplate(settings.GetString("test_generation_prompt.system"), variables)
	if err != nil {
		log.Printf("Error rendering system prompt: %v", err)
		prompt.System = ""
		prompt.User = ""
		return prompt
	}

	userPrompt, err := renderTemplate(settings.GetString("test_generation_prompt.user"), variables)
	if err != nil {
		log.Printf("Error rendering user prompt: %v", err)
		prompt.System = ""
		prompt.User = ""
		return prompt
	}

	prompt.System = systemPrompt
	prompt.User = userPrompt
	return prompt
}

func (pb *PromptBuilder) BuildPromptCustom(file string) *Prompt {
	pb.SourceFileNumbered = numberLines(pb.SourceFile)
	pb.TestFileNumbered = numberLines(pb.TestFile)

	variables := map[string]interface{}{
		"source_file_name":             pb.SourceFileName,
		"test_file_name":               pb.TestFileName,
		"source_file_numbered":         pb.SourceFileNumbered,
		"test_file_numbered":           pb.TestFileNumbered,
		"source_file":                  pb.SourceFile,
		"test_file":                    pb.TestFile,
		"code_coverage_report":         pb.CodeCoverageReport,
		"additional_includes_section":  pb.IncludedFiles,
		"failed_tests_section":         pb.FailedTestRuns,
		"additional_instructions_text": pb.AdditionalInstructions,
		"language":                     pb.Language,
		"max_tests":                    MAX_TESTS_PER_RUN,
	}

	settings := settings.GetSettings()

	prompt := &Prompt{}

	systemPrompt, err := renderTemplate(settings.GetString(file+".system"), variables)
	if err != nil {
		log.Printf("Error rendering system prompt: %v", err)
		prompt.System = ""
		prompt.User = ""
		return prompt
	}

	userPrompt, err := renderTemplate(settings.GetString(file+".user"), variables)
	if err != nil {
		log.Printf("Error rendering user prompt: %v", err)
		prompt.System = ""
		prompt.User = ""
		return prompt
	}
	prompt.System = systemPrompt
	prompt.User = userPrompt
	return prompt
}

func renderTemplate(templateText string, variables map[string]interface{}) (string, error) {
	funcMap := template.FuncMap{
		"trim": strings.TrimSpace,
	}
	tmpl, err := template.New("prompt").Funcs(funcMap).Parse(templateText)
	if err != nil {
		return "", err
	}
	var buffer bytes.Buffer
	err = tmpl.Execute(&buffer, variables)
	if err != nil {
		return "", err
	}
	return buffer.String(), nil
}

func numberLines(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = fmt.Sprintf("%d %s", i+1, line)
	}
	return strings.Join(lines, "\n")
}
