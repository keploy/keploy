package cli

import (
	"context"
	"errors"
	"testing"

	"github.com/spf13/cobra"
	mockSvc "go.keploy.io/server/v3/pkg/service/mock"
	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/log"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

type failingMock struct{ err error }

func (f failingMock) Record(context.Context) error { return f.err }
func (f failingMock) Replay(context.Context) error { return f.err }

type mockFactory struct{ svc mockSvc.Service }

func (f mockFactory) GetService(context.Context, string) (interface{}, error) { return f.svc, nil }

func TestAMockFailureIsShownUnlessAnEndBlockPrintsIt(t *testing.T) {
	boom := errors.New("keploy did not complete the run: boom")
	utils.SetCancel(func() {})
	for _, tc := range []struct {
		name  string
		err   error
		shown bool
	}{
		{name: "oss store", err: boom, shown: true},
		{name: "a store that prints the end", err: mockSvc.Ended{Err: boom}, shown: false},
	} {
		for _, build := range []func(context.Context, *zap.Logger, ServiceFactory, CmdConfigurator) *cobra.Command{MockRecord, MockReplay} {
			core, logs := observer.New(zap.ErrorLevel)
			cmd := build(context.Background(), zap.New(core), mockFactory{failingMock{tc.err}}, noFlags{})
			cmd.SetArgs(nil)
			cmd.SilenceErrors = true
			if err := cmd.Execute(); !errors.Is(err, boom) {
				t.Fatalf("%s: Execute = %v", tc.name, err)
			}
			if logs.Len() != 1 {
				t.Fatalf("%s: %d error lines", tc.name, logs.Len())
			}
			hidden := false
			for _, f := range logs.All()[0].Context {
				hidden = hidden || f.Key == log.Hide.Key
			}
			if hidden == tc.shown {
				t.Errorf("%s %s: shown on a quiet console = %v, want %v", tc.name, cmd.Name(), !hidden, tc.shown)
			}
		}
	}
}
