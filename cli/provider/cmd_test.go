package provider

import (
    "testing"
    "github.com/spf13/pflag"
    "context"
    "os"
    "github.com/spf13/cobra"
    "go.keploy.io/server/v2/config"
    "go.uber.org/zap"
)


// Test generated using Keploy
func TestAliasNormalizeFunc(t *testing.T) {
    testCases := []struct {
        input    string
        expected string
    }{
        {"testCommand", "test-command"},
        {"configPath", "config-path"},
        {"unknownFlag", "unknownFlag"},
    }

    for _, tc := range testCases {
        normalized := aliasNormalizeFunc(nil, tc.input)
        if normalized != pflag.NormalizedName(tc.expected) {
            t.Errorf("Expected normalized name '%s' for input '%s', got '%s'",
                tc.expected, tc.input, normalized)
        }
    }
}

// Test generated using Keploy
func TestCmdConfigurator_ValidateFlags_GenCommand_NoAPIKey(t *testing.T) {
    logger := zap.NewNop()
    cfg := config.New()
    configurator := NewCmdConfigurator(logger, cfg)

    cmd := &cobra.Command{
        Use: "gen",
    }

    err := configurator.AddFlags(cmd)
    if err != nil {
        t.Fatalf("AddFlags returned error: %v", err)
    }
    cmd.Flags().Set("test-command", "go test ./...")

    // Ensure API_KEY is not set
    os.Unsetenv("API_KEY")

    // Call ValidateFlags
    err = configurator.ValidateFlags(context.Background(), cmd)
    if err == nil {
        t.Fatal("Expected error when API_KEY is not set, but got nil")
    }

    expectedErrMsg := "API_KEY is not set"
    if err.Error() != expectedErrMsg {
        t.Errorf("Expected error message '%s', got '%s'", expectedErrMsg, err.Error())
    }
}

