package mysql

import (
	"errors"
	"fmt"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/recorder"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// A connection whose packet framing is lost is logged once, at WARN and
// rate-limited, by the recorder where it happens; that includes a row or a
// definition that does not decode. recordV2 must not log it again at ERROR:
// one unlimited line per connection, under a message that does not say what
// happened. Any other error is still logged.
func TestRecordV2ErrorsAreLoggedOnce(t *testing.T) {
	for _, c := range []struct {
		name   string
		err    error
		errors int
	}{
		{"framing lost", fmt.Errorf("%w: the COM_QUERY response's first packet does not decode", recorder.ErrFramingLost), 0},
		{"a row that does not decode", fmt.Errorf("%w: row 3 of the COM_QUERY response does not decode: unexpected EOF", recorder.ErrFramingLost), 0},
		{"another error", errors.New("V2: failed to handle initial mysql handshake: unsupported authentication plugin: sha256_password"), 1},
		{"no error", nil, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.DebugLevel)
			logRecordV2Error(zap.New(core), c.err)
			if got := logs.FilterLevelExact(zapcore.ErrorLevel).Len(); got != c.errors {
				t.Fatalf("%d ERROR lines, want %d", got, c.errors)
			}
		})
	}
}
