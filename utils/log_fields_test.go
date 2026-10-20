package utils

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"testing"

	"go.keploy.io/server/v3/utils/logcontent/logtest"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// A map's keys are logged sorted, whatever its value type.
func TestSortedKeys_LogsTheKeysSorted(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	zap.New(core).Debug("headers",
		zap.Array("names", SortedKeys(http.Header{"X-B": {"2"}, "Authorization": {"x"}, "X-A": {"1"}})),
		zap.Array("params", SortedKeys(map[string]string{"q": "1", "page": "2"})))
	got := logs.All()[0].ContextMap()
	if want := []interface{}{"Authorization", "X-A", "X-B"}; !reflect.DeepEqual(got["names"], want) {
		t.Fatalf("names = %v, want %v", got["names"], want)
	}
	if want := []interface{}{"page", "q"}; !reflect.DeepEqual(got["params"], want) {
		t.Fatalf("params = %v, want %v", got["params"], want)
	}
}

// A line whose level is off costs nothing for the keys: they are read when
// the line is encoded.
func TestSortedKeys_CostsADisabledLineNothing(t *testing.T) {
	logger := zap.New(zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(io.Discard), zapcore.InfoLevel))
	h := http.Header{"Authorization": {"x"}, "Content-Type": {"y"}, "X-A": {"1"}, "X-B": {"2"}}
	ours := testing.AllocsPerRun(1000, func() { logger.Debug("headers", zap.Array("names", SortedKeys(h))) })
	viaAny := testing.AllocsPerRun(1000, func() { logger.Debug("headers", zap.Any("names", h)) })
	if ours > viaAny {
		t.Fatalf("a disabled line with SortedKeys allocates %v times, with zap.Any of the map %v", ours, viaAny)
	}
}

// LogContentError logs what LogError does, with the error as logcontent.Error
// logs it, and like LogError nothing for a cancellation.
func TestLogContentError_LogsTheErrorAsLogcontentAllows(t *testing.T) {
	const canary = "kp-canary-utils-3f9a"
	parseErr := fmt.Errorf("parse request: %w", errors.New("malformed MIME header line: "+canary))
	logtest.Each(t, func(t *testing.T, on bool) {
		logger, logs := logtest.Observe()
		LogContentError(logger, parseErr, "failed to parse the request", zap.String("conn", "1"))
		LogContentError(logger, fmt.Errorf("read: %w", context.Canceled), "canceled")
		logtest.Check(t, logs, canary, on, "failed to parse the request")
		if logs.FilterMessage("canceled").Len() != 0 {
			t.Fatal("a cancellation was logged")
		}
		if got := logs.All()[0].ContextMap()["conn"]; got != "1" {
			t.Fatalf("the line's own fields were dropped: conn = %v", got)
		}
	})
}
