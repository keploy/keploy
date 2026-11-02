package models

import (
	"context"
	"testing"
)

// A context with no listener, nil among them, carries none: a runner asked
// of one tells nothing, and does not fail on it.
func TestAppExitOfAContextWithNoListenerIsNil(t *testing.T) {
	for name, ctx := range map[string]context.Context{"nil": nil, "with none": context.Background()} {
		if told := AppExitOf(ctx); told != nil {
			t.Errorf("%s: AppExitOf gave a listener; want none", name)
		}
	}
	var got AppError
	ctx := WithAppExit(context.Background(), func(e AppError) { got = e })
	AppExitOf(ctx)(AppError{AppErrorType: ErrUnExpected, ExitCode: 3})
	if got.ExitCode != 3 {
		t.Fatalf("the listener ctx carries was told %+v; want the exit, code 3", got)
	}
}
