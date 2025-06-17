package utgen

import (
	"bufio"
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.keploy.io/server/v2/pkg/models"
	settings "go.keploy.io/server/v2/pkg/service/utgen/assets"
	"go.uber.org/zap"

	"gopkg.in/yaml.v2"
)

func GetCodeLanguage(sourceFilePath string) string {
	// Retrieve the mapping of languages to their file extensions from settings
	// Create a map to hold the language extensions
	languageExtensionMapOrg := make(map[string][]string)

	setting := settings.GetSettings()

	// Unmarshal the language_extension_map_org section into the map
	if err := setting.UnmarshalKey("language_extension_map_org", &languageExtensionMapOrg); err != nil {
		log.Fatalf("Error unmarshaling language extension map, %s", err)
	}

	// Initialize a dictionary to map file extensions to their corresponding languages
	extensionToLanguage := make(map[string]string)

	// Populate the extensionToLanguage dictionary
	for language, extensions := range languageExtensionMapOrg {
		for _, ext := range extensions {
			extensionToLanguage[ext] = language
		}
	}

	// Extract the file extension from the source file path
	parts := strings.Split(sourceFilePath, ".")
	extensionS := "." + parts[len(parts)-1]
	// Initialize the default language name as 'unknown'
	languageName := "unknown"

	// Check if the extracted file extension is in the dictionary
	if val, ok := extensionToLanguage[extensionS]; ok {
		languageName = val
	}

	// Return the language name in lowercase
	return strings.ToLower(languageName)
}

func unmarshalYamlTestDetails(yamlStr string) (*models.UTDetails, error) {
	yamlStr = strings.TrimSpace(yamlStr)
	yamlStr = strings.TrimPrefix(yamlStr, "```yaml")
	yamlStr = strings.TrimSuffix(yamlStr, "```")
	var data *models.UTDetails
	err := yaml.Unmarshal([]byte(yamlStr), &data)
	if err != nil {
		return nil, fmt.Errorf("error unmarshaling yaml: %s", err)
	}
	return data, nil
}

func unmarshalYamlTestHeaders(yamlStr string) (*models.UTIndentationInfo, error) {
	yamlStr = strings.TrimSpace(yamlStr)
	yamlStr = strings.TrimPrefix(yamlStr, "```yaml")
	yamlStr = strings.TrimSuffix(yamlStr, "```")

	var data *models.UTIndentationInfo
	err := yaml.Unmarshal([]byte(yamlStr), &data)
	if err != nil {
		return nil, fmt.Errorf("error unmarshaling yaml: %s", err)
	}
	return data, nil
}

func unmarshalYamlTestLine(yamlStr string) (*models.UTInsertionInfo, error) {
	yamlStr = strings.TrimSpace(yamlStr)
	yamlStr = strings.TrimPrefix(yamlStr, "```yaml")
	yamlStr = strings.TrimSuffix(yamlStr, "```")
	var data *models.UTInsertionInfo
	err := yaml.Unmarshal([]byte(yamlStr), &data)
	if err != nil {
		return nil, fmt.Errorf("error unmarshaling yaml: %s", err)
	}
	return data, nil
}

func convertToInt(value interface{}) (int, error) {
	switch v := value.(type) {
	case int:
		return v, nil
	case float64:
		return int(v), nil
	case string:
		return strconv.Atoi(v)
	default:
		return 0, fmt.Errorf("unsupported type for conversion to int: %T", value)
	}
}

func extractErrorMessage(outputMessage, failMessage, language string) string {
	const maxLines = 15
	var pattern string
	message := failMessage
	switch language {
	case "python":
		pattern = `^=+ ERRORS =+$`
		message = outputMessage
	case "java":
		pattern = `^\[ERROR\].*`
		message = outputMessage
	case "go":
		pattern = `(?i)(^FAIL|panic:|undefined:)`
	case "javascript":
		pattern = `(?i)● .*`
	}
	re := regexp.MustCompile(pattern)
	scanner := bufio.NewScanner(strings.NewReader(message))
	var result []string
	matching := false

	for scanner.Scan() {
		line := scanner.Text()
		if re.MatchString(line) {
			matching = true
		}
		if matching {
			result = append(result, line)
			if len(result) >= maxLines {
				break
			}
		}
	}
	return strings.Join(result, "\n")
}

func getFilename(filePath string) string {
	return filepath.Base(filePath)
}

func formatDuration(duration time.Duration) string {
	if duration >= time.Minute {
		minutes := int(duration.Minutes())
		seconds := duration.Seconds() - float64(minutes*60)
		return fmt.Sprintf("%dm%.2fs", minutes, seconds)
	}
	return fmt.Sprintf("%.2fs", duration.Seconds())
}

func extractString(output []byte) []string {
	lines := strings.Split(string(output), "\n")
	var dependencies []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			dependencies = append(dependencies, trimmed)
		}
	}
	return dependencies
}

func isStringInarray(array []string, text string) bool {
	for _, elem := range array {
		if strings.EqualFold(elem, text) {
			return true
		}
	}
	return false
}

func mapKeysToSortedSlice(itemsMap map[string]bool) []string {
	items := []string{}
	for item := range itemsMap {
		items = append(items, item)
	}
	sort.Strings(items)
	return items
}

func RunCommand(command string, cwd string, logger *zap.Logger) (stdout string, stderr string, exitCode int, commandStartTime int64, err error) {
	// Get the current time before running the test command, in milliseconds
	commandStartTime = time.Now().UnixNano() / int64(time.Millisecond)

	var cmd *exec.Cmd

	if runtime.GOOS == "windows" {
		cmdArgs := strings.Fields(command)
		cmd = exec.Command(cmdArgs[0], cmdArgs[1:]...)
	} else {
		// Create the command with the specified working directory
		cmd = exec.Command("sh", "-c", command)
		if cwd != "" {
			cmd.Dir = cwd
		}
	}

	// Capture the stdout and stderr
	var outBuf, errBuf bytes.Buffer

	// Set the output of the command
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	if logger.Level() == zap.DebugLevel {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	}

	// Run the command
	err = cmd.Run()

	// Get the exit code
	exitCode = cmd.ProcessState.ExitCode()

	// Return the captured output and other information
	stdout = outBuf.String()
	stderr = errBuf.String()
	return stdout, stderr, exitCode, commandStartTime, err
}

func getTestFilePath(sourceFilePath, testDirectory string) (string, error) {

	language := GetCodeLanguage(sourceFilePath)

	// Extract the base name and extension of the source file
	baseName := filepath.Base(sourceFilePath)
	extension := filepath.Ext(sourceFilePath)

	// Remove the extension from the base name
	baseNameWithoutExt := strings.TrimSuffix(baseName, extension)

	var testFileBaseNames []string

	switch language {
	case "go":
		testFileBaseNames = []string{baseNameWithoutExt + "_test" + extension}
	case "javascript":
		testFileBaseNames = []string{baseNameWithoutExt + ".test" + extension}
	case "python":
		testFileBaseNames = []string{baseNameWithoutExt + "_test" + extension, "test_" + baseName}
	default:
		return "", fmt.Errorf("unsupported language: %s", language)
	}

	// Find existing test file
	existingTestFilePath, err := findTestFile(testDirectory, testFileBaseNames)
	if err != nil {
		return "", err
	}
	// If a test file was found
	if existingTestFilePath != "" {
		return existingTestFilePath, nil
	}

	// If no existing test file is found
	chosenTestFileName := testFileBaseNames[0]
	srcFileDir := filepath.Dir(sourceFilePath)

	if testDirectory == "" || testDirectory == "." {
		return filepath.Join(srcFileDir, chosenTestFileName), nil
	}
	return filepath.Join(testDirectory, srcFileDir, chosenTestFileName), nil
}

func findTestFile(testDirectory string, testFileBaseNames []string) (string, error) {
	var bestMatch string

	err := filepath.Walk(testDirectory, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			for _, testFileBaseName := range testFileBaseNames {
				if strings.HasSuffix(path, testFileBaseName) {
					if bestMatch == "" || len(path) < len(bestMatch) {
						bestMatch = path
					}
				}
			}
		}
		return nil
	})

	if err != nil {
		return "", err
	}

	return bestMatch, nil
}

func createTestFile(testFilePath string, sourceFilePath string) (bool, error) {
	// Ensure the directory exists
	err := os.MkdirAll(filepath.Dir(testFilePath), os.ModePerm)
	if err != nil {
		return false, err
	}

	// Check if the test file exists
	if _, err := os.Stat(testFilePath); os.IsNotExist(err) {
		// Create the test file if it does not exist
		file, err := os.OpenFile(testFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return false, err
		}
		defer func() {
			if err := file.Close(); err != nil {
				return
			}
		}()

		language := GetCodeLanguage(sourceFilePath)
		var initialContent string

		switch language {
		case "go":
			pkgName := "main"
			srcBytes, readErr := os.ReadFile(sourceFilePath)
			if readErr == nil {
				lines := strings.Split(string(srcBytes), "\n")
				for _, line := range lines {
					trimmedLine := strings.TrimSpace(line)
					if strings.HasPrefix(trimmedLine, "package ") {
						pkgName = strings.TrimSpace(strings.TrimPrefix(trimmedLine, "package "))
						break
					}
				}
			}
			initialContent = fmt.Sprintf("package %s\n", pkgName)
		case "python":
			initialContent = fmt.Sprintf("# Test file for %s\nimport unittest\n\nclass TestMyModule(unittest.TestCase):\n    pass\n\nif __name__ == '__main__':\n    unittest.main()\n", filepath.Base(sourceFilePath))
		case "javascript":
			initialContent = fmt.Sprintf("// Test file for %s\n\ndescribe('%s tests', () => {\n  it('should pass', () => {\n    expect(true).toBe(true);\n  });\n});\n", filepath.Base(sourceFilePath), filepath.Base(sourceFilePath))
		default:
			initialContent = fmt.Sprintf("// Unit test for %s\n", filepath.Base(sourceFilePath))
		}

		if _, err = file.WriteString(initialContent); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

func ExtractYAML(s string) string {

	re := regexp.MustCompile("(?s)```yaml\\n(.*?)\\n```")
	matches := re.FindStringSubmatch(s)
	if len(matches) > 1 {
		result := strings.TrimSpace(matches[1])
		return result
	}

	re = regexp.MustCompile("(?s)```\\n(.*?)\\n```")
	matches = re.FindStringSubmatch(s)
	if len(matches) > 1 {
		result := strings.TrimSpace(matches[1])
		return result
	}

	lines := strings.Split(s, "\n")
	var yamlLines []string
	inYAML := false

	for _, line := range lines {
		if strings.HasPrefix(line, "language:") || strings.HasPrefix(line, "refactored_source_code:") {
			inYAML = true
		}
		if inYAML {
			yamlLines = append(yamlLines, line)
		}
	}

	if len(yamlLines) > 0 {
		result := strings.Join(yamlLines, "\n")
		return result
	}

	result := strings.TrimSpace(s)
	return result
}
