package reportdb

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sync"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A report is never seen part-way written: while InsertReport writes a new
// report, a reader polling for it finds no report, or the whole one. Before,
// the writer created the report file empty and then renamed the whole one
// over it, and a reader that came between read an empty file: "failed to
// decode the report file. error: EOF", with a zero report beside the error.
func TestAReportBeingWrittenIsNeverReadEmpty(t *testing.T) {
	dir := t.TempDir()
	rdb := New(zap.NewNop(), dir)
	for i := 0; i < 200; i++ {
		readWhileWritten(t, rdb, dir, fmt.Sprintf("run-%d", i))
	}
}

// readWhileWritten polls a new report while InsertReport writes it, until it
// reads the whole one, and waits for the writer however the reads end.
func readWhileWritten(t *testing.T, rdb *TestReport, dir, run string) {
	t.Helper()
	ctx := context.Background()
	want := &models.TestReport{Status: string(models.TestSetStatusPassed), Success: 3, Total: 3}
	var wg sync.WaitGroup
	wg.Add(1)
	done := make(chan struct{})
	go func() {
		defer wg.Done()
		defer close(done)
		if err := rdb.InsertReport(ctx, run, "set", want); err != nil {
			t.Errorf("InsertReport: %v", err)
		}
	}()
	defer wg.Wait()
	for read := false; !read; {
		select {
		case <-done:
			read = true
		default:
		}
		got, err := New(zap.NewNop(), dir).GetReport(ctx, run, "set")
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if read {
				t.Fatalf("%s: no report once InsertReport returned", run)
			}
		case err != nil:
			t.Fatalf("%s: a report being written was read part-way: %v", run, err)
		case got.Success != 3 || got.Status != want.Status:
			t.Fatalf("%s: read %+v; want the whole report", run, got)
		default:
			read = true
		}
	}
}
