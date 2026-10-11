package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	toolsSvc "go.keploy.io/server/v3/pkg/service/tools"
	"go.uber.org/zap"
)

// templatizeTools is a tools service whose Templatize returns err.
type templatizeTools struct {
	toolsSvc.Service
	err error
}

func (f templatizeTools) Templatize(context.Context) error { return f.err }

type templatizeFactory struct{ svc toolsSvc.Service }

func (f templatizeFactory) GetService(context.Context, string) (interface{}, error) {
	return f.svc, nil
}

// keploy templatize FAILS WHEN IT STOPS. The error Templatize returns (a set
// it could not read or write; it logged why and named the sets it did not
// reach) is the command's, so the process exits non-zero. Before, the command
// logged it and returned nil: a script or CI step read a templatize that
// stopped at its first set as a success.
func TestTemplatize_TheCommandFailsWhenTemplatizeStops(t *testing.T) {
	stop := errors.New("keploy templatize stopped at test set test-set-0")
	for _, c := range []struct {
		name string
		err  error
	}{{"stopped", stop}, {"finished", nil}} {
		t.Run(c.name, func(t *testing.T) {
			cmd := Templatize(context.Background(), zap.NewNop(), config.New(), templatizeFactory{templatizeTools{err: c.err}}, noFlags{})
			cmd.SetArgs(nil)
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SilenceErrors, cmd.SilenceUsage = true, true
			got := cmd.Execute()
			if !errors.Is(got, c.err) || (c.err == nil) != (got == nil) {
				t.Fatalf("the command returned %v, want %v", got, c.err)
			}
		})
	}
}

// keploy templatize, FAILING, PRINTS NO USAGE. Its error is a failure of the
// run, not of the command line, so the real command tree (Root, as the
// binary builds it) prints neither cobra's Usage nor its Flags block after
// it: the log already said what stopped templatize and what to do.
func TestTemplatize_AFailureThroughTheCommandTreePrintsNoUsage(t *testing.T) {
	stop := errors.New("keploy templatize stopped at test set test-set-0")
	root := Root(context.Background(), zap.NewNop(), templatizeFactory{templatizeTools{err: stop}}, noFlags{})
	if root == nil {
		t.Fatal("the command tree could not be built")
	}
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"templatize"})
	if err := root.Execute(); !errors.Is(err, stop) {
		t.Fatalf("keploy templatize returned %v, want %v", err, stop)
	}
	if strings.Contains(out.String(), "Usage:") || strings.Contains(out.String(), "Flags:") {
		t.Fatalf("a failing keploy templatize printed its usage:\n%s", out.String())
	}
}
