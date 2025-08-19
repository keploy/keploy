package python

import (
	"fmt"
	"os"
	"strings"

	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func createPyCoverageConfig(logger *zap.Logger) {
	// Check if .coveragerc already exists
	existingConfig, err := readExistingCoverageConfig()
	if err != nil && !os.IsNotExist(err) {
		utils.LogError(logger, err, "failed to read existing .coveragerc file")
	}

	// Merge existing config with Keploy's required settings
	mergedConfig := mergeCoverageConfig(existingConfig, logger)

	// Write the merged configuration
	file, err := os.Create(".coveragerc")
	if err != nil {
		utils.LogError(logger, err, "failed to create .coveragerc file")
		return
	}
	defer func() {
		if err := file.Close(); err != nil {
			utils.LogError(logger, err, "failed to close coveragerc file", zap.String("file", file.Name()))
		}
	}()

	_, err = file.WriteString(mergedConfig)
	if err != nil {
		utils.LogError(logger, err, "failed to write to .coveragerc file")
		return
	}

	logger.Debug("Configuration written to .coveragerc with preserved user settings")
}

// readExistingCoverageConfig reads and returns the content of existing .coveragerc file
func readExistingCoverageConfig() (string, error) {
	content, err := os.ReadFile(".coveragerc")
	if err != nil {
		return "", err
	}
	return string(content), nil
}

// mergeCoverageConfig merges existing configuration with Keploy's required settings
func mergeCoverageConfig(existingConfig string, logger *zap.Logger) string {
	// In the below config, in the concurrency section, we are setting the concurrency to multiprocessing and thread.
	// Where multiprocessing is for collecting coverage for processes spawned by the Python application,
	// and thread is for collecting coverage for the main thread.
	keploySettings := map[string]string{
		"sigterm":     "true",
		"concurrency": "multiprocessing, thread",
		"parallel":    "true",
	}

	// If no existing config, create with Keploy defaults
	if existingConfig == "" {
		logger.Info("No existing .coveragerc found, creating with Keploy defaults")
		return createDefaultKeployConfig(keploySettings)
	}

	logger.Info("Existing .coveragerc found, merging with Keploy settings")

	// Parse existing config and merge with Keploy settings
	return parseAndMergeConfig(existingConfig, keploySettings, logger)
}

// createDefaultKeployConfig creates a basic configuration with only Keploy requirements
func createDefaultKeployConfig(keploySettings map[string]string) string {
	config := "[run]\n"
	config += "omit =\n    /usr/*\n"
	for key, value := range keploySettings {
		config += fmt.Sprintf("%s = %s\n", key, value)
	}
	return config
}

// parseAndMergeConfig parses the existing config and merges it with Keploy settings
func parseAndMergeConfig(existingConfig string, keploySettings map[string]string, logger *zap.Logger) string {
	lines := strings.Split(existingConfig, "\n")
	var result []string
	var currentSection string
	runSectionFound := false
	runSectionProcessed := false

	for i, line := range lines {
		trimmedLine := strings.TrimSpace(line)

		// Detect section headers
		if strings.HasPrefix(trimmedLine, "[") && strings.HasSuffix(trimmedLine, "]") {
			currentSection = trimmedLine
			if currentSection == "[run]" {
				runSectionFound = true
			} else if runSectionFound && !runSectionProcessed {
				// We're leaving the [run] section, add any missing Keploy settings
				result = append(result, addMissingKeploySettings(keploySettings)...)
				runSectionProcessed = true
			}
			result = append(result, line)
			continue
		}

		// Process settings within [run] section
		if currentSection == "[run]" && trimmedLine != "" && !strings.HasPrefix(trimmedLine, "#") {
			settingName := extractSettingName(trimmedLine)

			// Check if this is a Keploy required setting
			if keployValue, isKeploySetting := keploySettings[settingName]; isKeploySetting {
				// Special handling for omit - merge the values
				if settingName == "omit" {
					omitLines, skipCount := handleOmitMerging(lines, i, logger)
					result = append(result, omitLines...)
					// Skip ahead past the omit block
					i += skipCount
					continue
				} else {
					// Override with Keploy value
					result = append(result, fmt.Sprintf("%s = %s", settingName, keployValue))
					logger.Debug("Overriding setting with Keploy requirement", zap.String("setting", settingName), zap.String("value", keployValue))
					delete(keploySettings, settingName) // Mark as processed
					continue
				}
			}
		}

		// Add the original line if not processed above
		result = append(result, line)
	}

	// If [run] section was found but not all Keploy settings were processed
	if runSectionFound && !runSectionProcessed {
		result = append(result, addMissingKeploySettings(keploySettings)...)
	}

	// If no [run] section was found, add it with Keploy settings
	if !runSectionFound {
		result = append([]string{"[run]"}, append(addMissingKeploySettings(keploySettings), result...)...)
	}

	return strings.Join(result, "\n")
}

// extractSettingName extracts the setting name from a configuration line
func extractSettingName(line string) string {
	parts := strings.SplitN(line, "=", 2)
	if len(parts) > 0 {
		return strings.TrimSpace(parts[0])
	}
	return ""
}

// handleOmitMerging merges existing omit patterns with Keploy's requirements
func handleOmitMerging(lines []string, startIndex int, logger *zap.Logger) ([]string, int) {
	var result []string
	result = append(result, "omit =")

	// Add Keploy's required omit pattern first
	result = append(result, "    /usr/*")

	skipCount := 0
	// Add existing omit patterns (skip the "omit =" line and process continuation lines)
	for i := startIndex + 1; i < len(lines); i++ {
		line := lines[i]
		trimmedLine := strings.TrimSpace(line)

		// Stop if we hit a new setting or section
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") && trimmedLine != "" {
			break
		}

		skipCount++
		// Add non-empty continuation lines
		if trimmedLine != "" {
			result = append(result, line)
		}
	}

	logger.Debug("Merged omit patterns with existing configuration")
	return result, skipCount
}

// addMissingKeploySettings adds any Keploy settings that weren't found in the existing config
func addMissingKeploySettings(keploySettings map[string]string) []string {
	var result []string
	for key, value := range keploySettings {
		result = append(result, fmt.Sprintf("%s = %s", key, value))
	}
	return result
}
