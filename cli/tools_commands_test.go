package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	contractSvc "go.keploy.io/server/v3/pkg/service/contract"
	diffSvc "go.keploy.io/server/v3/pkg/service/diff"
	reportSvc "go.keploy.io/server/v3/pkg/service/report"
	toolsSvc "go.keploy.io/server/v3/pkg/service/tools"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

type stubServiceFactory struct {
	svc interface{}
	err error
}

func (s *stubServiceFactory) GetService(_ context.Context, _ string) (interface{}, error) {
	return s.svc, s.err
}

type failingToolsService struct {
	toolsSvc.Service
	err error
}

func (f failingToolsService) Export(context.Context) error                 { return f.err }
func (f failingToolsService) Import(context.Context, string, string) error { return f.err }
func (f failingToolsService) Normalize(context.Context) error              { return f.err }
func (f failingToolsService) Sanitize(context.Context) error               { return f.err }
func (f failingToolsService) Templatize(context.Context) error             { return f.err }
func (f failingToolsService) Update(context.Context) error                 { return f.err }

type failingDiffService struct {
	diffSvc.Service
	err error
}

func (f failingDiffService) Compare(context.Context, string, string, []string) error { return f.err }

type failingContractService struct {
	contractSvc.Service
	err error
}

func (f failingContractService) Generate(context.Context, bool) error { return f.err }
func (f failingContractService) GenerateFromTests(context.Context) error {
	return f.err
}
func (f failingContractService) Download(context.Context, bool) error { return f.err }
func (f failingContractService) Validate(context.Context) error       { return f.err }

type failingReportService struct {
	reportSvc.Service
	err error
}

func (f failingReportService) GenerateReport(context.Context) error { return f.err }

type diffFlagsConfigurator struct {
	noFlags
}

func (diffFlagsConfigurator) AddFlags(cmd *cobra.Command) error {
	if cmd.Name() == "diff" {
		cmd.Flags().String("run1", "", "")
		cmd.Flags().String("run2", "", "")
		cmd.Flags().StringSlice("test-sets", nil, "")
	}
	return nil
}

func TestUtilityCommandsPropagateErrorsWithoutPrintingUsage(t *testing.T) {
	getSvcErr := errors.New("service initialization failed")
	runErr := errors.New("command execution failed")

	cases := []struct {
		name string
		args []string
		svc  interface{}
	}{
		{name: "sanitize", args: []string{"sanitize"}, svc: failingToolsService{err: runErr}},
		{name: "normalize", args: []string{"normalize"}, svc: failingToolsService{err: runErr}},
		{name: "templatize", args: []string{"templatize"}, svc: failingToolsService{err: runErr}},
		{name: "export postman", args: []string{"export", "postman"}, svc: failingToolsService{err: runErr}},
		{name: "import postman", args: []string{"import", "postman"}, svc: failingToolsService{err: runErr}},
		{name: "update", args: []string{"update"}, svc: failingToolsService{err: runErr}},
		{name: "diff", args: []string{"diff", "run1", "run2"}, svc: failingDiffService{err: runErr}},
		{name: "contract generate", args: []string{"contract", "generate"}, svc: failingContractService{err: runErr}},
		{name: "contract generate --infer", args: []string{"contract", "generate", "--infer"}, svc: failingContractService{err: runErr}},
		{name: "contract download", args: []string{"contract", "download"}, svc: failingContractService{err: runErr}},
		{name: "contract test", args: []string{"contract", "test"}, svc: failingContractService{err: runErr}},
		{name: "report", args: []string{"report"}, svc: failingReportService{err: runErr}},
	}

	for _, tc := range cases {
		t.Run(tc.name+"/GetService error", func(t *testing.T) {
			root := Root(context.Background(), zap.NewNop(), &stubServiceFactory{err: getSvcErr}, diffFlagsConfigurator{})
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetArgs(tc.args)
			err := root.Execute()
			if !errors.Is(err, getSvcErr) {
				t.Fatalf("%v returned %v, want %v", tc.args, err, getSvcErr)
			}
			if strings.Contains(out.String(), "Usage:") || strings.Contains(out.String(), "Flags:") {
				t.Fatalf("%v printed usage on runtime error:\n%s", tc.args, out.String())
			}
		})

		t.Run(tc.name+"/wrong service interface", func(t *testing.T) {
			root := Root(context.Background(), zap.NewNop(), &stubServiceFactory{svc: struct{}{}}, diffFlagsConfigurator{})
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetArgs(tc.args)
			err := root.Execute()
			if err == nil || !strings.Contains(err.Error(), "doesn't satisfy") {
				t.Fatalf("%v returned %v, want interface mismatch error", tc.args, err)
			}
			if strings.Contains(out.String(), "Usage:") || strings.Contains(out.String(), "Flags:") {
				t.Fatalf("%v printed usage on interface error:\n%s", tc.args, out.String())
			}
		})

		t.Run(tc.name+"/service execution error", func(t *testing.T) {
			root := Root(context.Background(), zap.NewNop(), &stubServiceFactory{svc: tc.svc}, diffFlagsConfigurator{})
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetArgs(tc.args)
			err := root.Execute()
			if !errors.Is(err, runErr) {
				t.Fatalf("%v returned %v, want %v", tc.args, err, runErr)
			}
			if strings.Contains(out.String(), "Usage:") || strings.Contains(out.String(), "Flags:") {
				t.Fatalf("%v printed usage on execution error:\n%s", tc.args, out.String())
			}
		})
	}
}

func TestDiffMissingRunIDsReturnsUsageError(t *testing.T) {
	root := Root(context.Background(), zap.NewNop(), &stubServiceFactory{}, diffFlagsConfigurator{})
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"diff"})
	err := root.Execute()
	if err == nil || !utils.IsUsageError(err) {
		t.Fatalf("diff with missing args returned %v, want usage error", err)
	}
}
