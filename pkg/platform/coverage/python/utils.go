package python

import (
	"fmt"
	"os"
	"strings"

	"go.keploy.io/server/v3/utils"
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
		"data_file":   ".coverage.keploy",
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
	// Add all Keploy settings since there are no existing settings to process
	emptyProcessedSettings := make(map[string]bool)
	settingLines := addMissingKeploySettings(keploySettings, emptyProcessedSettings)
	for _, line := range settingLines {
		config += line + "\n"
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
	processedSettings := make(map[string]bool) // Track which Keploy settings have been processed

	for _, line := range lines {
		trimmedLine := strings.TrimSpace(line)

		// Detect section headers
		if strings.HasPrefix(trimmedLine, "[") && strings.HasSuffix(trimmedLine, "]") {
			currentSection = trimmedLine
			if currentSection == "[run]" {
				runSectionFound = true
			} else if runSectionFound && !runSectionProcessed {
				// We're leaving the [run] section, add any missing Keploy settings
				result = append(result, addMissingKeploySettings(keploySettings, processedSettings)...)
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
				if settingName != "omit" {
					// Override with Keploy value
					result = append(result, fmt.Sprintf("%s = %s", settingName, keployValue))
					logger.Debug("Overriding setting with Keploy requirement", zap.String("setting", settingName), zap.String("value", keployValue))
					processedSettings[settingName] = true // Mark as processed
					continue
				}
			}
		}

		// Add the original line if not processed above
		result = append(result, line)
	}

	// If [run] section was found but not all Keploy settings were processed
	if runSectionFound && !runSectionProcessed {
		result = append(result, addMissingKeploySettings(keploySettings, processedSettings)...)
	}

	// If no [run] section was found, add it with Keploy settings
	if !runSectionFound {
		result = append([]string{"[run]"}, append(addMissingKeploySettings(keploySettings, processedSettings), result...)...)
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

// addMissingKeploySettings adds any Keploy settings that weren't found in the existing config
func addMissingKeploySettings(keploySettings map[string]string, processedSettings map[string]bool) []string {
	var result []string
	for key, value := range keploySettings {
		if !processedSettings[key] {
			result = append(result, fmt.Sprintf("%s = %s", key, value))
		}
	}
	return result
}
