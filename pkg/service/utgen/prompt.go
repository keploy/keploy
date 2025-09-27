package utgen

import (
	"bytes"
	"fmt"
	"html"
	"html/template"
	"os"
	"strings"

	settings "go.keploy.io/server/v2/pkg/service/utgen/assets"
	"go.uber.org/zap"
)

const MAX_TESTS_PER_RUN = 6

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

type Source struct {
	Name         string
	Code         string
	CodeNumbered string
}

type Test struct {
	Name         string
	Code         string
	CodeNumbered string
}

type PromptBuilder struct {
	Src                    *Source
	Test                   *Test
	CovReportContent       string
	IncludedFiles          string
	AdditionalInstructions string
	Language               string
	Logger                 *zap.Logger
	AdditionalPrompt       string
	InstalledPackages      []string
	FunctionUnderTest      string
	ImportDetails          string
	ModuleName             string
}

func NewPromptBuilder(srcPath, testPath, covReportContent, includedFiles, additionalInstructions, language, additionalPrompt, functionUnderTest string, logger *zap.Logger) (*PromptBuilder, error) {
	var err error
	src := &Source{
		Name: srcPath,
	}
	test := &Test{
		Name: testPath,
	}
	promptBuilder := &PromptBuilder{
		Src:               src,
		Test:              test,
		Language:          language,
		CovReportContent:  covReportContent,
		Logger:            logger,
		AdditionalPrompt:  additionalPrompt,
		FunctionUnderTest: functionUnderTest,
	}
	promptBuilder.Src.Code, err = readFile(srcPath)
	if err != nil {
		return nil, err
	}
	promptBuilder.Test.Code, err = readFile(testPath)
	if err != nil {
		return nil, err
	}
	promptBuilder.IncludedFiles, err = formatSection(includedFiles, ADDITIONAL_INCLUDES_TEXT)
	if err != nil {
		return nil, err
	}
	promptBuilder.AdditionalInstructions, err = formatSection(additionalInstructions, ADDITIONAL_INSTRUCTIONS_TEXT)
	if err != nil {
		return nil, err
	}
	return promptBuilder, nil
}

func readFile(filePath string) (string, error) {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return "", fmt.Errorf("error reading %s: %v", filePath, err)
	}
	return string(content), nil
}

func formatSection(content, templateText string) (string, error) {
	if content == "" {
		return "", nil
	}
	tmpl, err := template.New("section").Parse(templateText)
	if err != nil {
		return "", fmt.Errorf("error parsing section template: %v", err)
	}
	var buffer bytes.Buffer
	err = tmpl.Execute(&buffer, map[string]string{
		"IncludedFiles":          content,
		"AdditionalInstructions": content,
		"FailedTestRuns":         content,
	})
	if err != nil {
		return "", fmt.Errorf("error executing section template: %v", err)
	}
	return buffer.String(), nil
}

func (pb *PromptBuilder) BuildPrompt(file, failedTestRuns string) (*Prompt, error) {
	pb.Src.CodeNumbered = numberLines(pb.Src.Code)
	pb.Test.CodeNumbered = numberLines(pb.Test.Code)
	variables := map[string]interface{}{
		"source_file_name":             pb.Src.Name,
		"test_file_name":               pb.Test.Name,
		"source_file_numbered":         pb.Src.CodeNumbered,
		"test_file_numbered":           pb.Test.CodeNumbered,
		"source_file":                  pb.Src.Code,
		"test_file":                    pb.Test.Code,
		"code_coverage_report":         pb.CovReportContent,
		"additional_includes_section":  pb.IncludedFiles,
		"failed_tests_section":         failedTestRuns,
		"additional_instructions_text": pb.AdditionalInstructions,
		"language":                     pb.Language,
		"max_tests":                    MAX_TESTS_PER_RUN,
		"additional_command":           pb.AdditionalPrompt,
		"function_under_test":          pb.FunctionUnderTest,
		"installed_packages":           formatInstalledPackages(pb.InstalledPackages),
		"import_details":               pb.ImportDetails,
		"module_name":                  pb.ModuleName,
	}

	settings := settings.GetSettings()

	prompt := &Prompt{}

	systemPrompt, err := renderTemplate(settings.GetString(file+".system"), variables)
	if err != nil {
		prompt.System = ""
		prompt.User = ""
		return prompt, fmt.Errorf("error rendering system prompt: %v", err)
	}
	prompt.System = systemPrompt

	userPrompt, err := renderTemplate(settings.GetString(file+".user"), variables)
	if err != nil {
		prompt.System = ""
		prompt.User = ""
		return prompt, fmt.Errorf("error rendering user prompt: %v", err)
	}
	userPrompt = html.UnescapeString(userPrompt)
	prompt.User = userPrompt
	return prompt, nil
}

func formatInstalledPackages(packages []string) string {
	var sb strings.Builder
	for _, pkg := range packages {
		sb.WriteString(fmt.Sprintf("- %s\n", pkg))
	}
	return sb.String()
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
