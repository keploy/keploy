package learn

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unsafe"

	"go.keploy.io/server/v3/pkg/models"
)

// payloadSpecOf is an HTTP spec carrying the fuzzer's strings: req and res
// as the request's and answer's bodies, and key and val in the places a
// spec holds other strings (a header, a URL parameter, a form field, the
// URL, the metadata). shape's bits leave the request out (1), the answer
// out (2), and the HTTP sides out for a generic payload carrying the
// strings instead (4).
func payloadSpecOf(req, res, key, val string, shape uint8) models.MockSpec {
	spec := models.MockSpec{Metadata: map[string]string{key: val, "type": "config"}}
	if shape&1 == 0 {
		spec.HTTPReq = &models.HTTPReq{Method: "POST", URL: "http://api.local/v1/" + val, URLParams: map[string]string{key: val},
			Header: map[string]string{"Content-Type": "application/json", key: val}, Body: req,
			Form: []models.FormData{{Key: key, Values: []string{val, req}}}}
	}
	if shape&2 == 0 {
		spec.HTTPResp = &models.HTTPResp{StatusCode: 200, Header: map[string]string{key: val}, Body: res, StatusMessage: val}
	}
	if shape&4 != 0 {
		spec.HTTPReq, spec.HTTPResp = nil, nil
		spec.GenericRequests = []models.Payload{{Origin: models.FromClient, Message: []models.OutputBinary{{Type: key, Data: req}}}}
		spec.GenericResponses = []models.Payload{{Origin: models.FromServer, Message: []models.OutputBinary{{Type: val, Data: res}}}}
	}
	return spec
}

// sameAsTheRoundTrip checks payloadValue of spec against the round trip it
// stands for (JSON encode and DecodeJSON of the whole spec): the same ok,
// the same value, and the same bytes when each is encoded again.
func sameAsTheRoundTrip(t *testing.T, spec models.MockSpec) {
	t.Helper()
	got, gotOK := payloadValue(spec)
	want, wantOK := roundTrip(spec)
	if gotOK != wantOK {
		t.Fatalf("payloadValue read ok=%v; the round trip ok=%v", gotOK, wantOK)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("payloadValue read %#v; the round trip reads %#v", got, want)
	}
	gb, gerr := json.Marshal(got)
	wb, werr := json.Marshal(want)
	if gerr != nil || werr != nil || !bytes.Equal(gb, wb) {
		t.Fatalf("payloadValue's value encodes as %q (%v); the round trip's as %q (%v)", gb, gerr, wb, werr)
	}
}

// payloadValueCorpus is the fuzzer's seeds: bodies with what JSON escapes
// (quotes, backslashes, controls, HTML's <>&, U+2028 and U+2029), bodies
// that are not valid UTF-8 (a stray byte, a cut rune, a surrogate's bytes),
// JSON documents as bodies, empty and huge bodies, and keys with the same.
func payloadValueCorpus() []struct {
	req, res, key, val string
	shape              uint8
} {
	huge := strings.Repeat(`{"blob":"w7x1 \"q\" <a&b> é `+" \t\r\n"+`"}`, 1<<15)
	return []struct {
		req, res, key, val string
		shape              uint8
	}{
		{`{"user":"u-7"}`, `{"token":"tok-0c41b2"}`, "X-Id", "a1", 0},
		{"", "", "", "", 0},
		{`say "hi" \ back`, "line\nfeed\r\ttab\x00\x01\x1f", "k\"ey", "v\\al", 0},
		{"<script>&amp;</script>", "  ", "<k>", "&v", 0},
		{"bad \xff byte", "cut rune \xe2\x82", "k\xfe", "v\xc0\x80", 0},
		{"\xed\xa0\x80 surrogate", `{"ok":1}`, "é", "日本", 0},
		{`{"nested":{"a":[1,"two",{"b":"é\n"}]}}`, `[]`, "Content-Type", "text/plain", 0},
		{"only the request", "", "k", "v", 2},
		{"", "only the answer", "k", "v", 1},
		{"neither", "side", "k", "v", 3},
		{"generic \xff", "generic ok", "t", "u", 4},
		{huge, huge + "\xff", "k", "v", 0},
		{huge, huge, "k", "v", 0},
	}
}

func TestPayloadValueIsTheRoundTripsValue(t *testing.T) {
	for _, c := range payloadValueCorpus() {
		sameAsTheRoundTrip(t, payloadSpecOf(c.req, c.res, c.key, c.val, c.shape))
	}
}

func FuzzPayloadValueIsTheRoundTripsValue(f *testing.F) {
	for _, c := range payloadValueCorpus() {
		f.Add(c.req, c.res, c.key, c.val, c.shape)
	}
	f.Fuzz(func(t *testing.T, req, res, key, val string, shape uint8) {
		sameAsTheRoundTrip(t, payloadSpecOf(req, res, key, val, shape))
	})
}

// A body that is valid UTF-8 is put back as it is, never encoded and
// decoded: the value's body is the spec's own string, its bytes not
// copied. One that is not goes through the round trip (a decoded copy,
// each invalid byte read as U+FFFD).
func TestPayloadValuePutsBackAValidBodyWithoutTheRoundTrip(t *testing.T) {
	big := strings.Repeat("w3x1 é \"q\"\n", 1<<16)
	spec := payloadSpecOf(big, big+"\xff", "k", "v", 0)
	v, ok := payloadValue(spec)
	if !ok {
		t.Fatal("payloadValue read nothing of a spec the round trip reads")
	}
	top, _ := v.(map[string]interface{})
	req, _ := top["Req"].(map[string]interface{})
	res, _ := top["Res"].(map[string]interface{})
	reqBody, _ := req["body"].(string)
	resBody, _ := res["body"].(string)
	if reqBody != big || unsafe.StringData(reqBody) != unsafe.StringData(spec.HTTPReq.Body) {
		t.Errorf("the request's valid body was not put back as the spec's own string (%d bytes read, the same bytes: %v)",
			len(reqBody), unsafe.StringData(reqBody) == unsafe.StringData(spec.HTTPReq.Body))
	}
	if want := big + "�"; resBody != want || unsafe.StringData(resBody) == unsafe.StringData(spec.HTTPResp.Body) {
		t.Errorf("the answer's body with an invalid byte was not read as the round trip reads it (%d bytes, want %d)", len(resBody), len(want))
	}
	if spec.HTTPReq.Body != big || spec.HTTPResp.Body != big+"\xff" {
		t.Error("payloadValue changed the spec it read")
	}
}

// putBack writes a kept body only where the round trip left its emptied
// body, the empty string at "body" under the side's key; anywhere else it
// reports false, and payloadValue reads the whole spec by the round trip.
func TestPutBackRefusesAPlaceThatIsNotTheEmptiedBody(t *testing.T) {
	kept := []keptBody{{key: "Res", body: "kept"}}
	for _, c := range []struct {
		what string
		v    interface{}
		want bool
	}{
		{"the emptied body", map[string]interface{}{"Res": map[string]interface{}{"body": ""}}, true},
		{"no value", nil, false},
		{"no side", map[string]interface{}{"Req": map[string]interface{}{"body": ""}}, false},
		{"a side with no body", map[string]interface{}{"Res": map[string]interface{}{}}, false},
		{"a body that is no string", map[string]interface{}{"Res": map[string]interface{}{"body": json.Number("1")}}, false},
		{"a body that is not empty", map[string]interface{}{"Res": map[string]interface{}{"body": "x"}}, false},
	} {
		if got := putBack(c.v, kept); got != c.want {
			t.Errorf("%s: putBack reported %v; want %v", c.what, got, c.want)
		}
		if c.want {
			if b := c.v.(map[string]interface{})["Res"].(map[string]interface{})["body"]; b != "kept" {
				t.Errorf("%s: the body put back is %v; want the kept body", c.what, b)
			}
		}
	}
}
