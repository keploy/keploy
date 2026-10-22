package http

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"reflect"
	"strconv"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.keploy.io/server/v3/pkg/provenswitch"
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

// A response with no body by rule (to a HEAD, a 204, a 304) keeps the
// Content-Length it was sent with, recorded by both record paths, written as
// mocks.yaml writes it, read back and served. A HEAD's is the size of the
// object: recorded and served as 0, an S3 client's Stat took every object for
// empty and its read returned EOF without sending the GET. A response with a
// body still gets the length of the body served.
func TestANoBodyResponseKeepsItsContentLength(t *testing.T) {
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte("hello, object"))
	_ = zw.Close()

	for _, tc := range []struct {
		name, method, resp string
		wantCL             []string // nil: the body's own length
	}{
		{"HEAD", "HEAD", "HTTP/1.1 200 OK\r\nContent-Type: application/octet-stream\r\nContent-Length: 37267\r\n\r\n", []string{"37267"}},
		{"204 without a Content-Length", "DELETE", "HTTP/1.1 204 No Content\r\n\r\n", []string{}},
		{"304", "GET", "HTTP/1.1 304 Not Modified\r\nEtag: \"v1\"\r\nContent-Length: 37267\r\n\r\n", []string{"37267"}},
		{"GET with a body", "GET", "HTTP/1.1 200 OK\r\nContent-Encoding: gzip\r\nContent-Length: " + strconv.Itoa(gz.Len()) + "\r\n\r\n" + gz.String(), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := []byte(tc.method + " /bucket/mocks.bin HTTP/1.1\r\nHost: s3\r\n\r\n")
			h := &HTTP{Logger: zaptest.NewLogger(t)}
			v2, err := h.buildHTTPMock(&FinalHTTP{Req: req, Resp: []byte(tc.resp), ReqTimestampMock: time.Now(), ResTimestampMock: time.Now()},
				80, "test-conn", models.OutgoingOptions{})
			if err != nil || v2 == nil {
				t.Fatalf("buildHTTPMock = (%v, %v)", v2, err)
			}
			legacy := runLegacyParseFinalHTTP(t, h, req, []byte(tc.resp), 80, "test-conn")

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
				head, body, ok := bytes.Cut(served, []byte("\r\n\r\n"))
				if !ok {
					t.Fatalf("%s: served no header section:\n%s", path, served)
				}
				got, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(append(head, "\r\n\r\n"...))), &http.Request{Method: tc.method})
				if err != nil {
					t.Fatalf("%s: the served response does not parse: %v\n%s", path, err, served)
				}
				want := tc.wantCL
				if want == nil {
					want = []string{strconv.Itoa(len(body))}
					if dec, err := pkg.Decompress(zap.NewNop(), "gzip", body, pkg.MaxDecompressedSize); err != nil || string(dec) != "hello, object" {
						t.Errorf("%s: served body decodes to (%q, %v), want %q", path, dec, err, "hello, object")
					}
				} else if len(body) != 0 {
					t.Errorf("%s: served a %d-byte body with a response that has none", path, len(body))
				}
				if cl := got.Header.Values("Content-Length"); !reflect.DeepEqual(cl, want) && !(len(cl) == 0 && len(want) == 0) {
					t.Errorf("%s: served Content-Length %q, want %q\n%s", path, cl, want, onDisk)
				}
			}
		})
	}
}

// Under the proven switch keploy.yml's url noise comes in one order on every
// call (NewRequestNoise). Masking applies the patterns in turn, so where two
// overlap their order decides what is compared: in a map's order, new per
// call, one call fit its recording and the same call next did not. And the
// lookup of a recording made elsewhere keeps the staged recordings' digests
// under the patterns in their order (requestDigestOf): each order a call came
// with digested every staged recording again.
func TestRequestNoiseKeepsOneURLNoiseOrderUnderTheSwitch(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	// a+ first masks /v1/ab and /v1/aab alike (each to the placeholder and
	// a b); ab first masks /v1/ab whole and /v1/aab but for its first a.
	opts := models.OutgoingOptions{Learn: true, NoiseConfig: map[string]map[string][]string{
		"url": {`a+`: {}, `ab`: {}, `/v9/c[0-9]+`: {}, `/v9/d[0-9]+`: {}, `/v9/e[0-9]+`: {}},
	}}
	const host = "api.test"
	rec := onHost("rec", "/v1/ab", host)
	db := stagedDb{mockMemDb: &mockMemDb{}, staged: []*models.Mock{rec}}
	ctx := context.Background()
	var order []string
	digestOf, fits := map[string]bool{}, map[bool]int{}
	for i := 0; i < 200; i++ {
		rn := NewRequestNoise(h.Logger, opts)
		if i == 0 {
			order = rn.URL
		} else if !reflect.DeepEqual(rn.URL, order) {
			t.Fatalf("call %d read the url noise as %q, call 0 as %q", i, rn.URL, order)
		}
		digestOf[requestDigestOf(h.newLearnNoise(opts, rn).strict)] = true
		named, _, _ := elsewhereRecording(ctx, getTo("/v1/aab", host), db, h.newStrictNoise(rn.Header, rn.Body, rn.URL))
		fits[named == rec]++
	}
	if len(digestOf) != 1 {
		t.Fatalf("one keploy.yml named %d digest tables over 200 calls", len(digestOf))
	}
	if len(fits) != 1 {
		t.Fatalf("one call fit its recording on %d calls of 200 and not on %d", fits[true], fits[false])
	}
}
