package proxy

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// The lines that tell a request was not recorded (tellClientLeft at Info,
// warnNotRecorded at Warn) are logged on every run: they name the request by
// its method, host, path shape and query keys (utils.GetReqMeta), never by its
// URL, whose path segments and query values are the app's content (an id, a
// token, a search).
func TestTheNotRecordedLinesNameTheRequestByItsShape(t *testing.T) {
	const segment, value = "PLANTEDsegment0123", "PLANTEDvalue0456"
	req := httptest.NewRequest(http.MethodGet, "http://app.local/users/"+segment+"/orders?token="+value+"&page=2", nil)
	for name, tell := range map[string]func(*zap.Logger){
		"its client left": func(l *zap.Logger) { tellClientLeft(l, req) },
		"the app answered early": func(l *zap.Logger) {
			warnNotRecorded(l, req, &http.Response{StatusCode: http.StatusRequestEntityTooLarge}, errors.New("answered early"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			core, logs := observer.New(zapcore.InfoLevel)
			tell(zap.New(core))
			if logs.Len() != 1 {
				t.Fatalf("logged %d lines, want 1: %+v", logs.Len(), logs.All())
			}
			e := logs.All()[0]
			line := fmt.Sprint(e.Message, e.ContextMap())
			for _, planted := range []string{segment, value} {
				if strings.Contains(line, planted) {
					t.Errorf("logged %q, which holds %q of the request's URL", line, planted)
				}
			}
			meta, _ := e.ContextMap()["metadata"].(map[string]string)
			want := map[string]string{"method": http.MethodGet, "host": "app.local", "path": "/{}/{}/{}", "query keys": "page,token"}
			if !maps.Equal(meta, want) {
				t.Errorf("metadata = %v, want %v", meta, want)
			}
		})
	}
}
