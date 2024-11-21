package config

import (
    "testing"
    "sigs.k8s.io/kustomize/kyaml/yaml"
)


// Test generated using Keploy
func TestSetDefaultConfig_UpdatesDefaultConfig(t *testing.T) {
    newConfig := `
    path: "/new/path"
    appId: 1
    `
    SetDefaultConfig(newConfig)
    result := GetDefaultConfig()
    if result != newConfig {
        t.Errorf("Expected %v, got %v", newConfig, result)
    }
}

// Test generated using Keploy
func TestMergeStrings_ValidYAML_MergesSuccessfully(t *testing.T) {
    src := `
    path: "/src/path"
    appId: 1
    `
    dest := `
    appName: "TestApp"
    `
    result, err := mergeStrings(src, dest, false, yaml.MergeOptions{})
    if err != nil {
        t.Errorf("Expected no error, got %v", err)
    }
    if result == "" {
        t.Errorf("Expected non-empty result, got empty string")
    }
    // Additional checks can be added here to verify specific fields in the merged result
}


// Test generated using Keploy
func TestMergeStrings_InvalidSrcYAML_ReturnsError(t *testing.T) {
    src := `
    invalid_yaml: [unclosed_list
    `
    dest := `
    appName: "TestApp"
    `
    result, err := mergeStrings(src, dest, false, yaml.MergeOptions{})
    if err == nil {
        t.Errorf("Expected error due to invalid src YAML, got none")
    }
    if result != "" {
        t.Errorf("Expected empty result due to error, got %v", result)
    }
}


// Test generated using Keploy
func TestMergeStrings_InvalidDestYAML_ReturnsError(t *testing.T) {
    src := `
    appId: 1
    `
    dest := `
    invalid_yaml: {unclosed_map
    `
    result, err := mergeStrings(src, dest, false, yaml.MergeOptions{})
    if err == nil {
        t.Errorf("Expected error due to invalid dest YAML, got none")
    }
    if result != "" {
        t.Errorf("Expected empty result due to error, got %v", result)
    }
}

