package learn

import (
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// The learn step reads a mock's payload without the fields that say when it
// was recorded (payloadKeys, payloadSpec). An HTTP answer's marks say when
// each part of it reached the app, as resTimestampMock does: none of their
// keys, offsets or times is a word of the payload.
func TestARawLeavesOutAnAnswersMarks(t *testing.T) {
	at := time.Date(2026, 11, 7, 10, 0, 41, 123456789, time.UTC)
	m := &models.Mock{Version: "api.keploy.io/v1beta1", Name: "m", Kind: models.HTTP, Spec: models.MockSpec{
		Metadata: map[string]string{"type": models.HTTPClient},
		HTTPReq:  &models.HTTPReq{Method: "GET", URL: "http://deps.example/x"},
		HTTPResp: &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Length": "4"}, Body: "done",
			Marks: &models.HTTPAnswerMarks{Head: at, Writes: []models.HTTPAnswerWrite{{Offset: 4, TS: at}}}},
		ReqTimestampMock: at.Add(-time.Millisecond),
		ResTimestampMock: at,
	}}
	r := rawOfNow(m, false)
	if !r.rRead {
		t.Fatal("precondition: the payload was read")
	}
	if m.Spec.HTTPResp.Marks == nil {
		t.Fatal("reading the payload took the mock's own marks off it")
	}
	for _, s := range []string{"2026-11-07T10:00:41", "123456789", "marks", "offset"} {
		if strings.Contains(r.rText, s) {
			t.Errorf("the raw payload text holds %q of the answer's marks: %q", s, r.rText)
		}
	}
}

// A mock with no HTTP answer is read as before: nothing of its spec is
// taken off for marks it cannot have.
func TestARawOfAMockWithNoHTTPAnswer(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("reading a mock with no HTTP answer panicked: %v", r)
		}
	}()
	m := &models.Mock{Version: "api.keploy.io/v1beta1", Name: "m", Kind: models.GENERIC, Spec: models.MockSpec{
		GenericResponses: []models.Payload{{Origin: models.FromServer, Message: []models.OutputBinary{{Type: "utf-8", Data: "sentinel-answer"}}}},
	}}
	if r := rawOfNow(m, false); !r.rRead || !strings.Contains(r.rText, "sentinel-answer") {
		t.Fatalf("read %v, text %q; want the answer read", r.rRead, r.rText)
	}
}
