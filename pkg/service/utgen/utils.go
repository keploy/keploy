package utgen

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.keploy.io/server/v2/pkg/models"

	"go.keploy.io/server/v2/pkg/service/utgen/settings"
	"gopkg.in/yaml.v2"
)

// LoadYAML loads and parses YAML data from a given response text.
func LoadYAML(responseText string, keysFixYAML []string) map[string]interface{} {
	responseText = strings.TrimSpace(strings.TrimPrefix(responseText, "```yaml"))
	responseText = strings.TrimSuffix(responseText, "`")
	var data map[string]interface{}
	err := yaml.Unmarshal([]byte(responseText), &data)
	if err != nil {
		log.Printf("Failed to parse AI prediction: %v. Attempting to fix YAML formatting.", err)
		data = TryFixYAML(responseText, keysFixYAML)
		if data == nil {
			log.Printf("Failed to parse AI prediction after fixing YAML formatting.")
		}
	}
	return data
}

// TryFixYAML attempts to fix YAML formatting issues in the given response text.
func TryFixYAML(responseText string, keysFixYAML []string) map[string]interface{} {
	responseTextLines := strings.Split(responseText, "\n")

	// First fallback - try to convert 'relevant line: ...' to 'relevant line: |-\n        ...'
	responseTextLinesCopy := make([]string, len(responseTextLines))
	copy(responseTextLinesCopy, responseTextLines)
	for i := range responseTextLinesCopy {
		for _, key := range keysFixYAML {
			if strings.Contains(responseTextLinesCopy[i], key) && !strings.Contains(responseTextLinesCopy[i], "|-") {
				responseTextLinesCopy[i] = strings.Replace(responseTextLinesCopy[i], key, fmt.Sprintf("%s |-\\n        ", key), -1)
			}
		}
	}
	var data map[string]interface{}
	err := yaml.Unmarshal([]byte(strings.Join(responseTextLinesCopy, "\n")), &data)
	if err == nil {
		log.Printf("Successfully parsed AI prediction after adding |-\n")
		return data
	}

	// Second fallback - try to extract only range from first ```yaml to ```
	snippetPattern := regexp.MustCompile("```(yaml)?[\\s\\S]*?```")
	snippet := snippetPattern.FindString(strings.Join(responseTextLinesCopy, "\n"))
	if snippet != "" {
		snippetText := strings.TrimPrefix(snippet, "```yaml")
		snippetText = strings.TrimSuffix(snippetText, "`")
		err = yaml.Unmarshal([]byte(snippetText), &data)
		if err == nil {
			log.Printf("Successfully parsed AI prediction after extracting yaml snippet")
			return data
		}
	}

	// third fallback - try to remove leading and trailing curly brackets
	responseTextCopy := strings.TrimSpace(responseText)
	responseTextCopy = strings.TrimPrefix(responseTextCopy, "{")
	responseTextCopy = strings.TrimSuffix(responseTextCopy, "}")
	responseTextCopy = strings.TrimSuffix(responseTextCopy, ":")
	err = yaml.Unmarshal([]byte(responseTextCopy), &data)
	if err == nil {
		log.Printf("Successfully parsed AI prediction after removing curly brackets")
		return data
	}

	// Fourth fallback - try to remove last lines
	for i := 1; i < len(responseTextLines); i++ {
		responseTextLinesTmp := strings.Join(responseTextLines[:len(responseTextLines)-i], "\n")
		err = yaml.Unmarshal([]byte(responseTextLinesTmp), &data)
		if err == nil && containsLanguageKey(data) {
			log.Printf("Successfully parsed AI prediction after removing %d lines", i)
			return data
		}
	}

	// Fifth fallback - brute force: detect 'language:' key and use it as a starting point.
	// Look for last '\n\n' after last 'test_code:' and extract the yaml between them
	indexStart := strings.Index(responseText, "\nlanguage:")
	if indexStart == -1 {
		indexStart = strings.Index(responseText, "language:") // if response starts with 'language:'
	}
	indexLastCode := strings.LastIndex(responseText, "test_code:")
	indexEnd := strings.Index(responseText[indexLastCode:], "\n\n")
	if indexEnd == -1 {
		indexEnd = len(responseText) // response ends with valid yaml
	}
	responseTextCopy = strings.TrimSpace(responseText[indexStart:indexEnd])
	err = yaml.Unmarshal([]byte(responseTextCopy), &data)
	if err == nil {
		log.Printf("Successfully parsed AI prediction when using the language: key as a starting point")
		return data
	}

	log.Printf("Failed to fix and parse YAML.")
	return nil
}

func containsLanguageKey(data map[string]interface{}) bool {
	_, exists := data["language"]
	return exists
}

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

func unmarshalYamlTestDetails(yamlStr string) *models.UnitTestsDetails {
	yamlStr = strings.TrimSpace(yamlStr)
	yamlStr = strings.TrimPrefix(yamlStr, "```yaml")
	yamlStr = strings.TrimSuffix(yamlStr, "```")
	var data *models.UnitTestsDetails
	err := yaml.Unmarshal([]byte(yamlStr), &data)
	if err != nil {
		fmt.Println(err)
		return nil
	}
	return data
}

func unmarshalYamlTestHeaders(yamlStr string) *models.UnitTestsIndentation {
	yamlStr = strings.TrimSpace(yamlStr)
	yamlStr = strings.TrimPrefix(yamlStr, "```yaml")
	yamlStr = strings.TrimSuffix(yamlStr, "```")

	var data *models.UnitTestsIndentation
	err := yaml.Unmarshal([]byte(yamlStr), &data)
	if err != nil {
		fmt.Println(err)
		return nil
	}
	return data
}

func unmarshalYamlTestLine(yamlStr string) *models.UnitTestInsertionDetails {
	yamlStr = strings.TrimSpace(yamlStr)
	yamlStr = strings.TrimPrefix(yamlStr, "```yaml")
	yamlStr = strings.TrimSuffix(yamlStr, "```")
	var data *models.UnitTestInsertionDetails
	err := yaml.Unmarshal([]byte(yamlStr), &data)
	if err != nil {
		fmt.Println(err)
		return nil
	}
	return data
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

func extractErrorMessage(failMessage string) string {
	const MAX_LINES = 15
	pattern := `={3,} FAILURES ={3,}(.*?)(={3,}|$)`
	re := regexp.MustCompile(pattern)
	match := re.FindStringSubmatch(failMessage)
	if len(match) > 1 {
		errStr := strings.TrimSpace(match[1])
		errStrLines := strings.Split(errStr, "\n")
		if len(errStrLines) > MAX_LINES {
			errStr = "...\n" + strings.Join(errStrLines[len(errStrLines)-MAX_LINES:], "\n")
		}
		return errStr
	}
	return ""
}

func getFilename(filePath string) string {
	return filepath.Base(filePath)
}

func RunCommand(command string, cwd string) (stdout string, stderr string, exitCode int, commandStartTime int64, err error) {
	// Get the current time before running the test command, in milliseconds
	commandStartTime = time.Now().UnixNano() / int64(time.Millisecond)

	// Create the command with the specified working directory
	cmd := exec.Command("sh", "-c", command)
	if cwd != "" {
		cmd.Dir = cwd
	}

	// Capture the stdout and stderr
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	// Run the command
	err = cmd.Run()

	// Get the exit code
	exitCode = cmd.ProcessState.ExitCode()

	// Return the captured output and other information
	stdout = outBuf.String()
	stderr = errBuf.String()
	return stdout, stderr, exitCode, commandStartTime, err
}

func getTestFilePath(sourceFilePath, testDirectory, language string) (string, error) {

	var testFileIdentifier string

	switch language {

	case "go":
		testFileIdentifier = "_test"
	case "javascript":
		testFileIdentifier = ".test"
	default:
		return "", fmt.Errorf("unsupported language: %s", language)
	}
	// Extract the base name and extension of the source file
	baseName := filepath.Base(sourceFilePath)
	extension := filepath.Ext(sourceFilePath)

	// Remove the extension from the base name
	baseNameWithoutExt := strings.TrimSuffix(baseName, extension)

	// Find the most specific existing test file
	testFilePath, err := findTestFile(testDirectory, baseNameWithoutExt, extension)
	if err != nil {
		return "", err
	}

	// If a test file was found, return it
	if testFilePath != "" {
		return testFilePath, nil
	}

	// Construct the relative path for the new test file
	relativeDir := strings.TrimPrefix(filepath.Dir(sourceFilePath), "src")
	testFilePath = filepath.Join(testDirectory, relativeDir, baseNameWithoutExt+testFileIdentifier+extension)

	return testFilePath, nil
}

func findTestFile(testDirectory, baseNameWithoutExt, extension string) (string, error) {
	var bestMatch string

	err := filepath.Walk(testDirectory, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			if strings.HasSuffix(path, baseNameWithoutExt+".test"+extension) {
				if bestMatch == "" || len(path) < len(bestMatch) {
					bestMatch = path
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
				// Handle the error returned by Close()
				// You can log the error or take appropriate action
			}
		}()

		// Write initial content to the test file
		_, err = file.WriteString(fmt.Sprintf("// Unit test for %s\n", filepath.Base(sourceFilePath)))
		if err != nil {
			return false, err
		}

		return true, nil
	}

	return false, nil
}
