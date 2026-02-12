package config

import (
	"fmt"
	"os"
	"path/filepath"

	yaml3 "gopkg.in/yaml.v3"
)

// UpdateTestDelay reads the existing keploy.yml config file at the given configPath,
// updates the test.delay field with the measured startup delay, and writes it back.
// This preserves all other user-configured values in the file.
func UpdateTestDelay(configPath string, delay uint64) error {
	configFilePath := filepath.Join(configPath, "keploy.yml")

	// Read existing config file
	data, err := os.ReadFile(configFilePath)
	if err != nil {
		return fmt.Errorf("failed to read config file %s: %w", configFilePath, err)
	}

	// Unmarshal into a generic map to preserve all existing fields
	var configMap map[string]interface{}
	if err := yaml3.Unmarshal(data, &configMap); err != nil {
		return fmt.Errorf("failed to unmarshal config file: %w", err)
	}

	// Get or create the test section
	testSection, ok := configMap["test"]
	if !ok {
		testSection = make(map[string]interface{})
		configMap["test"] = testSection
	}

	testMap, ok := testSection.(map[string]interface{})
	if !ok {
		return fmt.Errorf("unexpected type for 'test' section in config")
	}

	// Update the delay value
	testMap["delay"] = delay

	// Write back
	updatedData, err := yaml3.Marshal(configMap)
	if err != nil {
		return fmt.Errorf("failed to marshal updated config: %w", err)
	}

	if err := os.WriteFile(configFilePath, updatedData, 0644); err != nil {
		return fmt.Errorf("failed to write updated config file: %w", err)
	}

	return nil
}
