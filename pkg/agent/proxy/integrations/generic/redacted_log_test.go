package generic

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// A generic miss's report carries none of the live request's bytes: only
// how many buffers it read and the first one's length, the closest
// recording's name and its similarity. Its Debug line prints only what the
// report prints, so a live buffer at a place the recording redacted (a token
// the obfuscator took out) is never shown, at any log level. It printed the
// first 64 live bytes ("preview"): a live token the recording had redacted
// showed in full. A buffer no recording redacted is not shown either, since
// the report shows it nowhere.
func TestAGenericMissLogsNoLiveByteTheReportDoesNotShow(t *testing.T) {
	liveToken := "ghp_LIVEsecretLIVEsecretLIVEsecretLIVE01"
	for _, row := range []struct {
		name  string
		mocks []*models.Mock
		live  []byte
		want  string // the live bytes no line may show, nor six of them
	}{
		{"a live token at a buffer the per-test recording redacted",
			[]*models.Mock{tokenMock("mock-token", true, "OK-USER-42", 0, "")}, []byte(liveToken), liveToken},
		{"a live token at a buffer the session recording redacted",
			[]*models.Mock{tokenMock("mock-token", false, "OK-USER-42", 0, "")}, []byte(liveToken), liveToken},
		{"a token prefixed by a command, the recording redacting the buffer",
			[]*models.Mock{tokenMock("mock-token", true, "OK-USER-42", 0, "")}, []byte("AUTH " + liveToken), liveToken},
		{"a buffer no recording redacted",
			[]*models.Mock{priceMock("mock-price", []byte("PRICE item=book;"), "9", true)}, []byte("PRICE item=pens;SHOWN-NOWHERE"), "SHOWN-NOWHERE"},
	} {
		t.Run(row.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.DebugLevel)
			on := true
			var consumed []string
			db := switchedStore{servedStore{mocks: row.mocks, consumed: &consumed}, &on}
			app, proxySide := net.Pipe()
			t.Cleanup(func() { _ = app.Close(); _ = proxySide.Close() })
			err := decodeGeneric(context.Background(), zap.New(core), row.live, proxySide, nil, db, models.OutgoingOptions{})
			var mm interface {
				MismatchReport() *models.MockMismatchReport
			}
			if !errors.As(err, &mm) || mm.MismatchReport() == nil {
				t.Fatalf("the live request did not miss with a report: %v", err)
			}
			if len(consumed) != 0 {
				t.Fatalf("a recording was consumed: %v", consumed)
			}
			report := fmt.Sprintf("%+v", *mm.MismatchReport())
			// Any six bytes running in the secret, so that a part of it
			// shows too: a line cut to its first bytes still shows them.
			shows := func(s []byte) []byte {
				for i := 0; i+6 <= len(row.want); i++ {
					if w := []byte(row.want[i : i+6]); bytes.Contains(s, w) {
						return w
					}
				}
				return nil
			}
			if w := shows([]byte(report)); w != nil {
				t.Fatalf("the report carries %q of the live bytes %q: %s", w, row.want, report)
			}
			misses := 0
			for _, e := range logs.All() {
				if e.Message == "no matching generic mock found for outgoing call" {
					misses++
				}
				if w := shows([]byte(e.Message)); w != nil {
					t.Errorf("the %s line %q carries %q of the live bytes", e.Level, e.Message, w)
				}
				enc := zapcore.NewMapObjectEncoder()
				for _, f := range e.Context {
					f.AddTo(enc)
				}
				for k, v := range enc.Fields {
					s, ok := v.([]byte)
					if !ok {
						s = []byte(fmt.Sprint(v))
					}
					if w := shows(s); w != nil {
						t.Errorf("the %s line %q carries %q of the live bytes %q in its field %s: %q", e.Level, e.Message, w, row.want, k, s)
					}
				}
			}
			if misses != 1 {
				t.Fatalf("the miss was logged %d times at Debug, want once: the row reads no log line", misses)
			}
		})
	}
}
