package http

import (
	"bufio"
	"bytes"
	"net/http"
	"reflect"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
	yamlLib "gopkg.in/yaml.v3"
)

// A mock answers with the response header lines the dependency sent, not one
// comma-joined line per name. Two Set-Cookie lines joined into one is a single
// cookie to every client (the Expires date carries a comma of its own, so the
// join is not even splittable), and the app under test loses the second.
// Recorded by both record paths, written as mocks.yaml writes it, read back and
// served.
func TestAMockServesRepeatedResponseHeaderLinesAsRecorded(t *testing.T) {
	req := []byte("GET /login HTTP/1.1\r\nHost: upstream\r\n\r\n")
	resp := []byte("HTTP/1.1 200 OK\r\n" +
		"Set-Cookie: a=1; Path=/\r\n" +
		"Content-Type: text/plain\r\n" +
		"Set-Cookie: b=2; Expires=Wed, 21 Oct 2015 07:28:00 GMT\r\n" +
		"Content-Length: 2\r\n\r\nok")
	want := []string{"a=1; Path=/", "b=2; Expires=Wed, 21 Oct 2015 07:28:00 GMT"}

	h := &HTTP{Logger: zaptest.NewLogger(t)}
	v2, err := h.buildHTTPMock(&FinalHTTP{Req: req, Resp: resp, ReqTimestampMock: time.Now(), ResTimestampMock: time.Now()},
		80, "test-conn", models.OutgoingOptions{})
	if err != nil || v2 == nil {
		t.Fatalf("buildHTTPMock = (%v, %v)", v2, err)
	}
	legacy := runLegacyParseFinalHTTP(t, h, req, resp, 80, "test-conn")

	for path, recorded := range map[string]*models.Mock{"V2 record": v2, "legacy record": legacy} {
		doc, err := mockdb.EncodeMock(recorded, zap.NewNop())
		if err != nil {
			t.Fatalf("%s: EncodeMock: %v", path, err)
		}
		onDisk, err := yamlLib.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		var readBack yaml.NetworkTrafficDoc
		if err := yamlLib.Unmarshal(onDisk, &readBack); err != nil {
			t.Fatal(err)
		}
		mocks, err := mockdb.DecodeMocks([]*yaml.NetworkTrafficDoc{&readBack}, zap.NewNop())
		if err != nil || len(mocks) != 1 {
			t.Fatalf("%s: DecodeMocks = (%d mocks, %v)", path, len(mocks), err)
		}

		served, err := h.buildMockResponseBytes(mocks[0])
		if err != nil {
			t.Fatalf("%s: buildMockResponseBytes: %v", path, err)
		}
		got, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(served)), nil)
		if err != nil {
			t.Fatalf("%s: the served response does not parse: %v\n%s", path, err, served)
		}
		if !reflect.DeepEqual(got.Header.Values("Set-Cookie"), want) {
			t.Errorf("%s: served Set-Cookie %q, recorded %q\n%s", path, got.Header.Values("Set-Cookie"), want, onDisk)
		}
		if ct := got.Header.Values("Content-Type"); !reflect.DeepEqual(ct, []string{"text/plain"}) {
			t.Errorf("%s: served Content-Type %q, recorded [text/plain]", path, ct)
		}
	}
}
