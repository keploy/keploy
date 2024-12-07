package provider

import (
    "context"
    "testing"
    "go.uber.org/zap"
    "go.keploy.io/server/v2/config"
)


// Test generated using Keploy
func TestStartInDocker_InDockerTrue(t *testing.T) {
    ctx := context.Background()
    logger := zap.NewNop()
    conf := &config.Config{InDocker: true}
    
    err := StartInDocker(ctx, logger, conf)
    if err != nil {
        t.Errorf("Expected nil error, got %v", err)
    }
}

// Test generated using Keploy
func TestGenerateDockerEnvs_FormatsEnvVariables(t *testing.T) {
    config := DockerConfigStruct{
        Envs: map[string]string{
            "KEY1": "value1",
            "KEY2": "value2",
        },
    }
    expected := "-e KEY1='value1' -e KEY2='value2'"
    result := GenerateDockerEnvs(config)
    if result != expected {
        t.Errorf("Expected %v, got %v", expected, result)
    }
}


// Test generated using Keploy
// Test that GenerateDockerEnvs returns an empty string when Envs is empty.
func TestGenerateDockerEnvs_NoEnvs(t *testing.T) {
    config := DockerConfigStruct{
        Envs: map[string]string{},
    }
    expected := ""
    result := GenerateDockerEnvs(config)
    if result != expected {
        t.Errorf("Expected %v, got %v", expected, result)
    }
}

