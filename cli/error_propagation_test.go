package cli

import (
	"context"
	"errors"
	"testing"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

type mockFailingServiceFactory struct {
	err error
}

func (m *mockFailingServiceFactory) GetService(_ context.Context, _ string) (interface{}, error) {
	if m.err != nil {
		return nil, m.err
	}
	// Return a dummy object that does not satisfy any service interface
	return "invalid-service-object", nil
}

type mockDummyCmdConfigurator struct{}

func (m *mockDummyCmdConfigurator) AddFlags(_ *cobra.Command) error {
	return nil
}

func (m *mockDummyCmdConfigurator) ValidateFlags(_ context.Context, _ *cobra.Command) error {
	return nil
}

func (m *mockDummyCmdConfigurator) Validate(_ context.Context, _ *cobra.Command) error {
	return nil
}

func TestCommandErrorPropagationOnServiceFailure(t *testing.T) {
	logger := zap.NewNop()
	cfg := config.New()
	ctx := context.Background()

	commands := []struct {
		name        string
		constructor func(ctx context.Context, logger *zap.Logger, cfg *config.Config, sf ServiceFactory, cc CmdConfigurator) *cobra.Command
	}{
		{"sanitize", Sanitize},
		{"normalize", Normalize},
		{"templatize", Templatize},
		{"record", Record},
		{"test", Test},
		{"update", Update},
		{"diff", Diff},
		{"report", Report},
	}

	for _, tc := range commands {
		t.Run(tc.name+"_get_service_error", func(t *testing.T) {
			utils.ErrCode = 0
			sf := &mockFailingServiceFactory{err: errors.New("failed to construct service")}
			cc := &mockDummyCmdConfigurator{}
			cmd := tc.constructor(ctx, logger, cfg, sf, cc)
			cmd.SilenceErrors = true
			cmd.SilenceUsage = true

			err := cmd.Execute()
			if err == nil && utils.ErrCode == 0 {
				t.Errorf("expected command %s to return an error or set non-zero exit code when GetService fails", tc.name)
			}
		})

		t.Run(tc.name+"_invalid_service_interface", func(t *testing.T) {
			utils.ErrCode = 0
			sf := &mockFailingServiceFactory{err: nil}
			cc := &mockDummyCmdConfigurator{}
			cmd := tc.constructor(ctx, logger, cfg, sf, cc)
			cmd.SilenceErrors = true
			cmd.SilenceUsage = true

			err := cmd.Execute()
			if err == nil && utils.ErrCode == 0 {
				t.Errorf("expected command %s to return an error or set non-zero exit code when service interface assertion fails", tc.name)
			}
		})
	}
}
