package models

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// Bytes no UTF-8 decoder accepts: a PNG's signature and a protobuf varint.
const (
	pngBody   = "\x89PNG\r\n\x1a\n\x00\xff\xfe\x01upload"
	protoBody = "\x08\x96\x01\x12\x04\xc3\x28\xff\xfeproto"
)

func binaryHTTPCase() TestCase {
	ts := time.Date(2026, 11, 6, 10, 0, 0, 123456789, time.UTC)
	return TestCase{
		Version: V1Beta1, Kind: HTTP, Name: "test-1",
		HTTPReq: HTTPReq{Method: "POST", ProtoMajor: 1, ProtoMinor: 1, URL: "http://app/upload?a=<b>&c",
			Header: map[string]string{"Content-Type": "application/octet-stream"}, Body: pngBody, Timestamp: ts},
		HTTPResp: HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "application/x-protobuf"},
			Body: protoBody, StreamBody: []HTTPStreamChunk{{Data: []HTTPStreamDataField{{Key: "raw", Value: "x"}}}}, Timestamp: ts},
	}
}

// encodeAgentStream is how the agent hands a test case to a recorder that
// asks for body objects, and how the recorder reads it
// (pkg/agent/routes/record.go, pkg/platform/http).
func encodeAgentStream(t *testing.T, tc TestCase) TestCase {
	t.Helper()
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(TestCaseBodyObjects{TestCase: &tc}); err != nil {
		t.Fatalf("encode: %v", err)
	}
	var out TestCase
	if err := json.NewDecoder(&buf).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

// textJSON is s as encoding/json writes and reads a string: each byte that is
// not UTF-8 becomes U+FFFD.
func textJSON(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var out string
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// oldTestCase is a test case as a reader built before body objects decodes it:
// its bodies are strings.
type oldTestCase struct {
	Name     string         `json:"name"`
	HTTPReq  httpReqFields  `json:"http_req"`
	HTTPResp httpRespFields `json:"http_resp"`
}

func TestHTTPBodyNotUTF8CrossesJSONUnchanged(t *testing.T) {
	if utf8.ValidString(pngBody) || utf8.ValidString(protoBody) {
		t.Fatal("fixtures must not be UTF-8")
	}
	got := encodeAgentStream(t, binaryHTTPCase())
	if got.HTTPReq.Body != pngBody {
		t.Errorf("request body = %q, want %q", got.HTTPReq.Body, pngBody)
	}
	if got.HTTPResp.Body != protoBody {
		t.Errorf("response body = %q, want %q", got.HTTPResp.Body, protoBody)
	}
	want := binaryHTTPCase()
	if got.HTTPReq.URL != want.HTTPReq.URL || got.HTTPReq.Method != want.HTTPReq.Method || got.HTTPReq.Header["Content-Type"] != "application/octet-stream" ||
		!got.HTTPReq.Timestamp.Equal(want.HTTPReq.Timestamp) || got.HTTPResp.StatusCode != 200 || got.HTTPResp.Header["Content-Type"] != "application/x-protobuf" {
		t.Errorf("other fields changed: req %+v resp %+v", got.HTTPReq, got.HTTPResp)
	}
	if got.HTTPResp.StreamBody != nil {
		t.Errorf("StreamBody crossed JSON: %+v", got.HTTPResp.StreamBody)
	}

	// A mock's request and response are pointers to the same types.
	b, err := json.Marshal(struct {
		Req HTTPReqBodyObject  `json:"req"`
		Res HTTPRespBodyObject `json:"res"`
	}{HTTPReqBodyObject(want.HTTPReq), HTTPRespBodyObject(want.HTTPResp)})
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Req *HTTPReq  `json:"req"`
		Res *HTTPResp `json:"res"`
	}
	if err := json.Unmarshal(b, &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Req.Body != pngBody || spec.Res.Body != protoBody {
		t.Errorf("through pointers: req %q resp %q", spec.Req.Body, spec.Res.Body)
	}
}

func TestHTTPBodyNotUTF8NamesItsEncoding(t *testing.T) {
	tc := binaryHTTPCase()
	whole, err := json.Marshal(TestCaseBodyObjects{TestCase: &tc})
	if err != nil {
		t.Fatal(err)
	}
	var parts map[string]json.RawMessage
	if err := json.Unmarshal(whole, &parts); err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]interface{}{"req": HTTPReqBodyObject(tc.HTTPReq), "resp": HTTPRespBodyObject(tc.HTTPResp),
		"test case req": parts["http_req"], "test case resp": parts["http_resp"]} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]json.RawMessage
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatal(err)
		}
		if _, ok := doc["stream_body"]; ok {
			t.Errorf("%s: StreamBody written: %s", name, b)
		}
		var jb JSONBody
		if err := json.Unmarshal(doc["body"], &jb); err != nil {
			t.Fatalf("%s: body is not a body object: %s (%v)", name, doc["body"], err)
		}
		want := pngBody
		if strings.HasSuffix(name, "resp") {
			want = protoBody
		}
		if jb.Encoding != BodyEncodingBase64 || jb.Data != base64.StdEncoding.EncodeToString([]byte(want)) {
			t.Errorf("%s: body = %+v", name, jb)
		}
	}
}

// A reader built before HTTPReq and HTTPResp read a body object decodes into
// the plain structs. It never asks for one, but should it get one it must
// refuse rather than take another body.
func TestHTTPBodyNotUTF8OldReaderRefuses(t *testing.T) {
	tc := binaryHTTPCase()
	b, err := json.Marshal(HTTPReqBodyObject(tc.HTTPReq))
	if err != nil {
		t.Fatal(err)
	}
	var oldReq httpReqFields
	err = json.Unmarshal(b, &oldReq)
	var typeErr *json.UnmarshalTypeError
	if !errors.As(err, &typeErr) || typeErr.Field != "body" {
		t.Fatalf("old request reader: err = %v, want a type error on body", err)
	}
	b, err = json.Marshal(HTTPRespBodyObject(tc.HTTPResp))
	if err != nil {
		t.Fatal(err)
	}
	var oldResp httpRespFields
	if err := json.Unmarshal(b, &oldResp); !errors.As(err, &typeErr) || typeErr.Field != "body" {
		t.Fatalf("old response reader: err = %v, want a type error on body", err)
	}
}

// A body that is UTF-8 is written byte for byte as encoding/json writes it in
// the plain structs, HTML escaped or not as the caller's encoder says, whether
// the reader asked for body objects or not.
func TestHTTPBodyUTF8JSONUnchanged(t *testing.T) {
	bodies := []string{"", `{"a":"<b>&c"}`, "plain \u2028 text \ufffd é", "line\nbreak\t"}
	for _, body := range bodies {
		tc := binaryHTTPCase()
		tc.HTTPReq.Body, tc.HTTPResp.Body = body, body
		tc.HTTPResp.BodySkipped, tc.HTTPResp.BodySize = true, 7
		tc.HTTPReq.BodyRef = BodyRef{Path: "assets/x/body.txt", Size: 3}
		for _, escape := range []bool{true, false} {
			for name, vs := range map[string][]interface{}{
				"req":  {httpReqFields(tc.HTTPReq), tc.HTTPReq, HTTPReqBodyObject(tc.HTTPReq)},
				"resp": {httpRespFields(tc.HTTPResp), tc.HTTPResp, HTTPRespBodyObject(tc.HTTPResp)},
			} {
				want := encodeWith(t, vs[0], escape)
				for _, v := range vs[1:] {
					if got := encodeWith(t, v, escape); !bytes.Equal(got, want) {
						t.Errorf("%s %T body %q escapeHTML=%v:\n got %s\nwant %s", name, v, body, escape, got, want)
					}
				}
			}
			// The test case as a whole: the same fields and values, in
			// another order (encoding/json writes the shadowing fields last).
			if got, want := decodeAny(t, encodeWith(t, TestCaseBodyObjects{TestCase: &tc}, escape)), decodeAny(t, encodeWith(t, &tc, escape)); !reflect.DeepEqual(got, want) {
				t.Errorf("test case body %q escapeHTML=%v:\n got %v\nwant %v", body, escape, got, want)
			}
		}
		got := encodeAgentStream(t, tc)
		if got.HTTPReq.Body != body || got.HTTPResp.Body != body || got.HTTPReq.BodyRef != tc.HTTPReq.BodyRef ||
			!got.HTTPResp.BodySkipped || got.HTTPResp.BodySize != 7 {
			t.Errorf("body %q read back as req %+v resp %+v", body, got.HTTPReq, got.HTTPResp)
		}
	}
}

func decodeAny(t *testing.T, b []byte) interface{} {
	t.Helper()
	var v interface{}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("%s: %v", b, err)
	}
	return v
}

func encodeWith(t *testing.T, v interface{}, escapeHTML bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(escapeHTML)
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A writer the reader did not ask writes a body that is not UTF-8 as it always
// did, so a reader built before body objects reads the test case, its bodies
// as today's text.
func TestHTTPBodyNotUTF8WrittenAsBeforeUnlessAsked(t *testing.T) {
	tc := binaryHTTPCase()
	for name, pair := range map[string][2]interface{}{
		"req":  {tc.HTTPReq, httpReqFields(tc.HTTPReq)},
		"resp": {tc.HTTPResp, httpRespFields(tc.HTTPResp)},
	} {
		for _, escape := range []bool{true, false} {
			if got, want := encodeWith(t, pair[0], escape), encodeWith(t, pair[1], escape); !bytes.Equal(got, want) {
				t.Errorf("%s escapeHTML=%v:\n got %s\nwant %s", name, escape, got, want)
			}
		}
	}
	b, err := json.Marshal(&tc)
	if err != nil {
		t.Fatal(err)
	}
	var old oldTestCase
	if err := json.Unmarshal(b, &old); err != nil {
		t.Fatalf("a reader built before body objects: %v", err)
	}
	if old.HTTPReq.Body != textJSON(t, pngBody) || old.HTTPResp.Body != textJSON(t, protoBody) {
		t.Errorf("old reader: req body %q resp body %q, want today's text", old.HTTPReq.Body, old.HTTPResp.Body)
	}
}

// One test case with binary bodies among many with text: the document a
// writer that was not asked writes is read by an old reader and a new one
// alike, and the one written for a reader that asked gives it every body's
// bytes.
func TestHTTPBodyMixedDocumentReadByBoth(t *testing.T) {
	var tcs []TestCase
	for i := 0; i < 5; i++ {
		tc := binaryHTTPCase()
		tc.Name = fmt.Sprintf("text-%d", i)
		tc.HTTPReq.Body, tc.HTTPResp.Body = fmt.Sprintf(`{"n":%d,"s":"<é>"}`, i), "ok \u2028"
		tcs = append(tcs, tc)
	}
	tcs = append(tcs[:2], append([]TestCase{binaryHTTPCase()}, tcs[2:]...)...)
	asked := make([]TestCaseBodyObjects, len(tcs))
	for i := range tcs {
		asked[i] = TestCaseBodyObjects{TestCase: &tcs[i]}
	}
	plain, err := json.Marshal(tcs)
	if err != nil {
		t.Fatal(err)
	}
	objects, err := json.Marshal(asked)
	if err != nil {
		t.Fatal(err)
	}
	var old []oldTestCase
	if err := json.Unmarshal(plain, &old); err != nil {
		t.Fatalf("old reader: %v", err)
	}
	var fromPlain, fromObjects []TestCase
	if err := json.Unmarshal(plain, &fromPlain); err != nil {
		t.Fatalf("new reader, not asked: %v", err)
	}
	if err := json.Unmarshal(objects, &fromObjects); err != nil {
		t.Fatalf("new reader, asked: %v", err)
	}
	if len(old) != len(tcs) || len(fromPlain) != len(tcs) || len(fromObjects) != len(tcs) {
		t.Fatalf("read %d, %d and %d test cases, want %d", len(old), len(fromPlain), len(fromObjects), len(tcs))
	}
	for i, tc := range tcs {
		req, resp := tc.HTTPReq.Body, tc.HTTPResp.Body
		if old[i].Name != tc.Name || old[i].HTTPReq.Body != textJSON(t, req) || old[i].HTTPResp.Body != textJSON(t, resp) {
			t.Errorf("old reader, %s: %q %q", tc.Name, old[i].HTTPReq.Body, old[i].HTTPResp.Body)
		}
		if fromPlain[i].HTTPReq.Body != textJSON(t, req) || fromPlain[i].HTTPResp.Body != textJSON(t, resp) {
			t.Errorf("new reader, not asked, %s: %q %q", tc.Name, fromPlain[i].HTTPReq.Body, fromPlain[i].HTTPResp.Body)
		}
		if fromObjects[i].Name != tc.Name || fromObjects[i].HTTPReq.Body != req || fromObjects[i].HTTPResp.Body != resp {
			t.Errorf("new reader, asked, %s: %q %q, want %q %q", tc.Name, fromObjects[i].HTTPReq.Body, fromObjects[i].HTTPResp.Body, req, resp)
		}
	}
	if fromObjects[2].HTTPReq.Body != pngBody {
		t.Fatalf("the binary test case is not the third: %+v", fromObjects[2].HTTPReq)
	}
}

// MarshalJSON called by itself returns one JSON value, as json.Marshal of
// the plain struct does: no newline after it, the bytes of the encoder's
// output.
func TestHTTPBodyMarshalJSONIsOneValue(t *testing.T) {
	for _, body := range []string{"text <b>", pngBody} {
		tc := binaryHTTPCase()
		tc.HTTPReq.Body, tc.HTTPResp.Body = body, body
		req, err := HTTPReqBodyObject(tc.HTTPReq).MarshalJSON()
		if err != nil {
			t.Fatal(err)
		}
		resp, err := HTTPRespBodyObject(tc.HTTPResp).MarshalJSON()
		if err != nil {
			t.Fatal(err)
		}
		whole, err := TestCaseBodyObjects{TestCase: &tc}.MarshalJSON()
		if err != nil {
			t.Fatal(err)
		}
		for name, got := range map[string][]byte{"req": req, "resp": resp, "test case": whole} {
			if !json.Valid(got) || got[len(got)-1] != '}' {
				t.Errorf("%s body %q: MarshalJSON = %q, want one JSON object and nothing after it", name, body, got)
			}
		}
		if body != pngBody {
			if want := bytes.TrimSuffix(encodeWith(t, httpReqFields(tc.HTTPReq), false), []byte("\n")); !bytes.Equal(req, want) {
				t.Errorf("req MarshalJSON = %s, want %s", req, want)
			}
			if want := bytes.TrimSuffix(encodeWith(t, httpRespFields(tc.HTTPResp), false), []byte("\n")); !bytes.Equal(resp, want) {
				t.Errorf("resp MarshalJSON = %s, want %s", resp, want)
			}
		}
	}
	if got, err := (TestCaseBodyObjects{}).MarshalJSON(); err != nil || string(got) != "null" {
		t.Errorf("nil test case: %q, %v; want null", got, err)
	}
	if got, err := json.Marshal([]TestCaseBodyObjects{{}}); err != nil || string(got) != "[null]" {
		t.Errorf("nil test case in a list: %q, %v; want [null]", got, err)
	}
}

func TestReadsBodyObjects(t *testing.T) {
	for _, c := range []struct {
		values []string
		want   bool
	}{
		{nil, false},
		{[]string{""}, false},
		{[]string{"base64"}, true},
		{[]string{" Base64 "}, true},
		{[]string{"BASE64"}, true},
		{[]string{"gzip, base64"}, true},
		{[]string{"gzip", "base64"}, true},
		{[]string{"base64x"}, false},
		{[]string{"hex", "gzip,  deflate"}, false},
	} {
		h := http.Header{}
		for _, v := range c.values {
			h.Add(BodyEncodingHeader, v)
		}
		h.Set("Other", BodyEncodingBase64)
		if got := ReadsBodyObjects(h); got != c.want {
			t.Errorf("ReadsBodyObjects(%q) = %v, want %v", c.values, got, c.want)
		}
	}
}

func TestHTTPBodyJSONRead(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString([]byte(pngBody))
	for _, c := range []struct {
		name, doc, want, errHas string
	}{
		{name: "string", doc: `{"method":"GET","body":"hi"}`, want: "hi"},
		{name: "string that is valid base64 stays as it stands", doc: `{"method":"GET","body":"aGVsbG8="}`, want: "aGVsbG8="},
		{name: "object", doc: `{"method":"GET","body":{"encoding":"base64","data":"` + b64 + `"}}`, want: pngBody},
		{name: "object with space", doc: `{"method":"GET","body": {"data":"` + b64 + `", "encoding":"base64"} }`, want: pngBody},
		// Each still names its bytes, read as encoding/json reads any object.
		{name: "object with a field keploy does not know", doc: `{"method":"GET","body":{"encoding":"base64","data":"` + b64 + `","extra":1}}`, want: pngBody},
		{name: "object with keys in another case", doc: `{"method":"GET","body":{"ENCODING":"base64","DATA":"` + b64 + `"}}`, want: pngBody},
		{name: "absent keeps", doc: `{"method":"GET"}`, want: "kept"},
		{name: "null keeps", doc: `{"method":"GET","body":null}`, want: "kept"},
		{name: "other encoding", doc: `{"body":{"encoding":"hex","data":"00"}}`, errHas: `encoding "hex" is not one keploy reads`},
		{name: "no encoding", doc: `{"body":{"data":"` + b64 + `"}}`, errHas: `encoding "" is not one keploy reads`},
		{name: "encoding in another case", doc: `{"body":{"encoding":"BASE64","data":"` + b64 + `"}}`, errHas: `encoding "BASE64" is not one keploy reads`},
		{name: "no data", doc: `{"body":{"encoding":"base64"}}`, errHas: "HTTPReq body: a body object holds no data"},
		{name: "null data", doc: `{"body":{"encoding":"base64","data":null}}`, errHas: "HTTPReq body: a body object holds no data"},
		{name: "empty data", doc: `{"body":{"encoding":"base64","data":""}}`, errHas: "HTTPReq body: a body object holds no data"},
		{name: "data of one line end", doc: `{"body":{"encoding":"base64","data":"\n"}}`, errHas: "HTTPReq body: a body object holds no data"},
		{name: "data of line ends", doc: `{"body":{"encoding":"base64","data":"\r\n\r\n"}}`, errHas: "HTTPReq body: a body object holds no data"},
		{name: "bad base64", doc: `{"body":{"encoding":"base64","data":"@@@"}}`, errHas: "does not decode"},
		{name: "object of wrong shape", doc: `{"body":{"encoding":1}}`, errHas: "HTTPReq body: json: cannot unmarshal number"},
		{name: "other field of wrong type", doc: `{"proto_major":"one","body":"hi"}`, errHas: "proto_major"},
		{name: "number", doc: `{"body":12}`, errHas: "neither a string nor a body object"},
		{name: "array", doc: `{"body":["a"]}`, errHas: "neither a string nor a body object"},
	} {
		t.Run(c.name, func(t *testing.T) {
			req := HTTPReq{Body: "kept"}
			err := json.Unmarshal([]byte(c.doc), &req)
			if c.errHas != "" {
				if err == nil || !strings.Contains(err.Error(), c.errHas) {
					t.Fatalf("err = %v, want one with %q", err, c.errHas)
				}
				if req.Body != "kept" {
					t.Errorf("a refused body replaced the one there: %q", req.Body)
				}
				resp := HTTPResp{Body: "kept"}
				respHas := strings.Replace(c.errHas, "HTTPReq", "HTTPResp", 1)
				if err := json.Unmarshal([]byte(c.doc), &resp); err == nil || !strings.Contains(err.Error(), respHas) {
					t.Fatalf("response: err = %v, want one with %q", err, respHas)
				}
				if resp.Body != "kept" {
					t.Errorf("a refused response body replaced the one there: %q", resp.Body)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if req.Body != c.want {
				t.Errorf("body = %q, want %q", req.Body, c.want)
			}
			if strings.Contains(c.doc, `"method"`) && req.Method != "GET" {
				t.Errorf("method = %q", req.Method)
			}
			resp := HTTPResp{Body: "kept"}
			if err := json.Unmarshal([]byte(strings.Replace(c.doc, `"method":"GET"`, `"status_code":204`, 1)), &resp); err != nil {
				t.Fatal(err)
			}
			if resp.Body != c.want {
				t.Errorf("response body = %q, want %q", resp.Body, c.want)
			}
		})
	}
	var resp HTTPResp
	if err := json.Unmarshal([]byte(`{"body":{"encoding":"hex"}}`), &resp); err == nil || !strings.Contains(err.Error(), "HTTPResp body") {
		t.Errorf("response: err = %v", err)
	}
}

// The header is the contract between releases: a reader and a writer built with
// different spellings each fall back to strings, and the bytes are lost with no
// error anywhere. So its name and value are pinned here as literals, not
// through the constants every other row uses.
func TestBodyEncodingHeaderIsItsWireName(t *testing.T) {
	if BodyEncodingHeader != "Keploy-Body-Encoding" {
		t.Errorf("header = %q", BodyEncodingHeader)
	}
	if BodyEncodingBase64 != "base64" {
		t.Errorf("encoding = %q", BodyEncodingBase64)
	}
	h := http.Header{}
	h.Set("Keploy-Body-Encoding", "base64")
	if !ReadsBodyObjects(h) {
		t.Error("the wire header is not read")
	}
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(mustJSON(t, HTTPReqBodyObject{Body: "\xff"})), &obj); err != nil {
		t.Fatal(err)
	}
	if want := map[string]interface{}{"encoding": "base64", "data": "/w=="}; !reflect.DeepEqual(obj["body"], want) {
		t.Errorf("body = %v, want %v", obj["body"], want)
	}
}

func mustJSON(t *testing.T, v interface{}) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A body too short to look like a file is still bytes: nothing about being
// binary depends on its length.
func TestHTTPBodyNotUTF8ShortBody(t *testing.T) {
	for _, body := range []string{"\xff", "\x80a", "a\xc3", "\x00\xfe"} {
		tc := binaryHTTPCase()
		tc.HTTPReq.Body, tc.HTTPResp.Body = body, body
		var got TestCase
		if err := json.Unmarshal([]byte(mustJSON(t, TestCaseBodyObjects{TestCase: &tc})), &got); err != nil {
			t.Fatalf("%q: %v", body, err)
		}
		if got.HTTPReq.Body != body || got.HTTPResp.Body != body {
			t.Errorf("%q: request %q, response %q", body, got.HTTPReq.Body, got.HTTPResp.Body)
		}
	}
}

// A writer goes through the body objects only for a reader that asked and a
// test case that needs one. Any other test case is handed back as it is, so
// the plain encoder writes it in one pass.
func TestTestCaseJSONTakesThePlainEncoderUnlessABodyNeedsAnObject(t *testing.T) {
	for _, c := range []struct {
		name, req, resp string
		binary          bool
	}{
		{"both binary", pngBody, protoBody, true},
		{"binary request", pngBody, "text", true},
		{"binary response", "text", protoBody, true},
		{"both text", "text", "\u00e9 text", false},
		{"both empty", "", "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			tc := binaryHTTPCase()
			tc.HTTPReq.Body, tc.HTTPResp.Body = c.req, c.resp

			if v, ok := TestCaseJSON(&tc, false).(*TestCase); !ok || v != &tc {
				t.Errorf("a reader that did not ask got %T", TestCaseJSON(&tc, false))
			}

			v := TestCaseJSON(&tc, true)
			if c.binary {
				if view, ok := v.(TestCaseBodyObjects); !ok || view.TestCase != &tc {
					t.Fatalf("a reader that asked got %T, want its body objects", v)
				}
			} else if p, ok := v.(*TestCase); !ok || p != &tc {
				t.Fatalf("a text test case took the second pass: %T", v)
			}
			var got TestCase
			if err := json.Unmarshal([]byte(mustJSON(t, v)), &got); err != nil {
				t.Fatal(err)
			}
			if got.HTTPReq.Body != c.req || got.HTTPResp.Body != c.resp {
				t.Errorf("read back %q, %q", got.HTTPReq.Body, got.HTTPResp.Body)
			}
		})
	}
	for _, asked := range []bool{true, false} {
		if got := mustJSON(t, TestCaseJSON(nil, asked)); got != "null" {
			t.Errorf("nil test case, asked %v: %s", asked, got)
		}
	}
}

// A type error in a field other than the body says which field, in which
// struct, as it did before these types read their own JSON. It still fails the
// read.
func TestHTTPBodyReadKeepsATypeErrorsFieldPath(t *testing.T) {
	var typeErr *json.UnmarshalTypeError
	err := json.Unmarshal([]byte(`{"proto_major":"one"}`), &HTTPReq{})
	if !errors.As(err, &typeErr) || typeErr.Struct == "" || !strings.HasSuffix(typeErr.Field, "proto_major") {
		t.Errorf("request: %#v", err)
	}
	err = json.Unmarshal([]byte(`{"status_code":"ok"}`), &HTTPResp{})
	if !errors.As(err, &typeErr) || typeErr.Struct == "" || !strings.HasSuffix(typeErr.Field, "status_code") {
		t.Errorf("response: %#v", err)
	}
	err = json.Unmarshal([]byte(`{"http_req":{"proto_major":"one"}}`), &TestCase{})
	if err == nil || !strings.Contains(err.Error(), "httpReq.proto_major") {
		t.Errorf("inside a test case: %v", err)
	}
}
