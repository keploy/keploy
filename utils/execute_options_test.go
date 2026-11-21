package utils

import (
	"bytes"
	"io"
	"testing"
)

// WithOutputAlso has the child's output written to its writer as well as to
// the tee an earlier option set, and alone, to its writer only.
func TestWithOutputAlsoWritesBesideTheTeeBeforeIt(t *testing.T) {
	var tee, also bytes.Buffer
	o := execOptionsOf([]ExecOption{WithOutputTee(func() io.Writer { return &tee }), WithOutputAlso(&also)})
	if o.outputTee == nil {
		t.Fatal("no tee")
	}
	_, _ = o.outputTee().Write([]byte("refused\n"))
	if tee.String() != "refused\n" || also.String() != "refused\n" {
		t.Fatalf("the tee got %q and the writer beside it %q; want both the output", tee.String(), also.String())
	}

	also.Reset()
	o = execOptionsOf([]ExecOption{WithOutputAlso(&also)})
	if o.outputTee == nil {
		t.Fatal("no tee")
	}
	_, _ = o.outputTee().Write([]byte("refused\n"))
	if also.String() != "refused\n" {
		t.Fatalf("the writer alone got %q; want the output", also.String())
	}
}
