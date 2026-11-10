package http

import (
	"context"
	"testing"
)

// The hook is called only when it is set, the run was not stopped, and
// something of the application runs on; it is handed that running.
func TestBeforeStopIsCalledOnlyForAnAppThatRunsOn(t *testing.T) {
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name    string
		ctx     context.Context
		running bool
		want    bool
	}{
		{"runs on", context.Background(), true, true},
		{"nothing runs on", context.Background(), false, false},
		{"the run was stopped", stopped, true, false},
	} {
		called := false
		running := func() bool { return tc.running }
		beforeStop(tc.ctx, func(_ context.Context, r func() bool) {
			called = true
			if !r() {
				t.Errorf("%s: the hook was handed a running that says nothing runs", tc.name)
			}
		}, running)
		if called != tc.want {
			t.Errorf("%s: hook called %v, want %v", tc.name, called, tc.want)
		}
	}
	// No hook: nothing to call, and nothing is asked.
	beforeStop(context.Background(), nil, func() bool {
		t.Error("running was asked with no hook to call")
		return true
	})
}
