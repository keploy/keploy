package utgen

import (
	"fmt"
	"log"
	"regexp"
	"strings"

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
