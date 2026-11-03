package recorder

import (
	"context"
	"fmt"
	"runtime/debug"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	"go.uber.org/zap"
)

// recordV2NoPanic runs RecordV2 for a row and returns a panic in it as an
// error. Rows run RecordV2 in a goroutine of their own, and a panic there
// would end the test binary without naming the row. With t, the panic also
// fails the row, so a row that does not check RecordV2's result fails too; a
// helper that must not touch t passes nil and returns the error to its row.
func recordV2NoPanic(t testing.TB, ctx context.Context, logger *zap.Logger, sess *supervisor.Session) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("RecordV2 panicked: %v\n%s", r, debug.Stack())
			if t != nil {
				t.Errorf("%v", err)
			}
		}
	}()
	return RecordV2(ctx, logger, sess)
}
