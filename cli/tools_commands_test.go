package cli

import (
	"context"
	"errors"
	"testing"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	"go.uber.org/zap"
)

type mockFaultyServiceFactory struct {
	err error
}

func (m *mockFaultyServiceFactory) GetService(ctx context.Context, name string) (interface{}, error) {
	return nil, m.err
}

type dummyConfigurator struct{}

func (d *dummyConfigurator) Validate(ctx context.Context, cmd *cobra.Command) error {
	return nil
}

func (d *dummyConfigurator) ValidateFlags(ctx context.Context, cmd *cobra.Command) error {
	return nil
}

func (d *dummyConfigurator) AddFlags(cmd *cobra.Command) error {
	cmd.Flags().StringSlice("test-sets", nil, "")
	return nil
}

func TestToolsCommandsReturnErrorOnFailure(t *testing.T) {
	ctx := context.Background()
	logger := zap.NewNop()
	conf := &config.Config{}
	expectedErr := errors.New("service initialization failed")
	factory := &mockFaultyServiceFactory{err: expectedErr}
	configurator := &dummyConfigurator{}

	t.Run("sanitize command error propagation", func(t *testing.T) {
		cmd := Sanitize(ctx, logger, conf, factory, configurator)
		err := cmd.RunE(cmd, []string{})
		if !errors.Is(err, expectedErr) {
			t.Fatalf("expected %q from sanitize command RunE, got %v", expectedErr, err)
		}
	})

	t.Run("normalize command error propagation", func(t *testing.T) {
		cmd := Normalize(ctx, logger, conf, factory, configurator)
		err := cmd.RunE(cmd, []string{})
		if !errors.Is(err, expectedErr) {
			t.Fatalf("expected %q from normalize command RunE, got %v", expectedErr, err)
		}
	})

	t.Run("templatize command error propagation", func(t *testing.T) {
		cmd := Templatize(ctx, logger, conf, factory, configurator)
		err := cmd.RunE(cmd, []string{})
		if !errors.Is(err, expectedErr) {
			t.Fatalf("expected %q from templatize command RunE, got %v", expectedErr, err)
		}
	})

	t.Run("export postman command error propagation", func(t *testing.T) {
		cmd := Export(ctx, logger, conf, factory, configurator)
		var postmanSubCmd *cobra.Command
		for _, c := range cmd.Commands() {
			if c.Name() == "postman" {
				postmanSubCmd = c
				break
			}
		}
		if postmanSubCmd == nil {
			t.Fatal("postman subcommand not found on export")
		}
		err := postmanSubCmd.RunE(postmanSubCmd, []string{})
		if !errors.Is(err, expectedErr) {
			t.Fatalf("expected %q from export postman command RunE, got %v", expectedErr, err)
		}
	})

	t.Run("import postman command error propagation", func(t *testing.T) {
		cmd := Import(ctx, logger, conf, factory, configurator)
		var postmanSubCmd *cobra.Command
		for _, c := range cmd.Commands() {
			if c.Name() == "postman" {
				postmanSubCmd = c
				break
			}
		}
		if postmanSubCmd == nil {
			t.Fatal("postman subcommand not found on import")
		}
		err := postmanSubCmd.RunE(postmanSubCmd, []string{})
		if !errors.Is(err, expectedErr) {
			t.Fatalf("expected %q from import postman command RunE, got %v", expectedErr, err)
		}
	})

	t.Run("diff command error propagation", func(t *testing.T) {
		cmd := Diff(ctx, logger, conf, factory, configurator)
		err := cmd.RunE(cmd, []string{"run1", "run2"})
		if !errors.Is(err, expectedErr) {
			t.Fatalf("expected %q from diff command RunE, got %v", expectedErr, err)
		}
	})
}
