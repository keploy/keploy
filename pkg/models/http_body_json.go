package models

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"
)

// JSON for an HTTP body that is not UTF-8.
//
// A JSON string holds text: encoding/json writes each byte of a Go string that
// is not UTF-8 as U+FFFD. A body keploy records as raw bytes (protobuf over
// HTTP, an image, a file upload, a compressed body it did not decode) crossed
// every JSON hop changed: the agent's stream of test cases to the recorder,
// the upload to Keploy Cloud and the reads from it. A replay then sent, or
// compared against, other bytes. The YAML a recording keeps never changed
// them: yaml.v3 writes such a string as !!binary.
//
// A reader that can take such a body says so with BodyEncodingHeader, and only
// to it is the body written as an object that names its encoding:
//
//	"body": {"encoding": "base64", "data": "iVBORw0KGgo..."}
//
// HTTPReq and HTTPResp read either form back into the bytes, and write JSON as
// encoding/json always has. A writer encodes TestCaseJSON's answer for a test
// case, or turns a request or a response into HTTPReqBodyObject or
// HTTPRespBodyObject, only for a reader that asked. A body that is UTF-8 is
// written as the string it always was in either form.
//
// So a reader that does not ask, every reader built before this among them,
// gets the document it always got. The object, not a string beside an encoding
// field, keeps a reader that cannot decode it from using the body should it
// get one anyway: one that met base64 in a string would send or compare the
// base64 itself, and one that met an empty string an empty body. Decoding an
// object into a string field fails, so such a reader refuses the document, and
// its error names the body.

// BodyEncodingBase64 is the one encoding a JSON body object names.
const BodyEncodingBase64 = "base64"

// BodyEncodingHeader is the header with which a reader of test cases as JSON
// says it reads a body object: "Keploy-Body-Encoding: base64". Sent on a
// request, it asks for body objects in that request's answer.
const BodyEncodingHeader = "Keploy-Body-Encoding"

// ReadsBodyObjects reports whether h, the headers a reader sent, say that it
// reads a body object: BodyEncodingHeader names base64, alone or in a
// comma-separated list, in any case.
func ReadsBodyObjects(h http.Header) bool {
	for _, v := range h.Values(BodyEncodingHeader) {
		for _, enc := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(enc), BodyEncodingBase64) {
				return true
			}
		}
	}
	return false
}

// JSONBody is the JSON form of a body that is not UTF-8.
type JSONBody struct {
	Encoding string `json:"encoding"`
	Data     string `json:"data"`
}

// The types HTTPReq and HTTPResp are encoded as, with every field and tag
// theirs, and no methods, so encoding one does not call back into a
// MarshalJSON or an UnmarshalJSON.
type (
	httpReqFields  HTTPReq
	httpRespFields HTTPResp
)

// The types HTTPReq and HTTPResp are decoded through: their fields, with the
// body left for readJSONBody. They are named because a type error in a field
// names the struct it met it in, and an anonymous one has no name to give.
type (
	httpReq struct {
		*httpReqFields
		Body json.RawMessage `json:"body"`
	}
	httpResp struct {
		*httpRespFields
		Body json.RawMessage `json:"body"`
	}
)

// UnmarshalJSON reads h, its body in either form.
func (h *HTTPReq) UnmarshalJSON(data []byte) error {
	w := httpReq{httpReqFields: (*httpReqFields)(h)}
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	return readJSONBody(w.Body, &h.Body, "HTTPReq")
}

// UnmarshalJSON reads h, its body in either form.
func (h *HTTPResp) UnmarshalJSON(data []byte) error {
	w := httpResp{httpRespFields: (*httpRespFields)(h)}
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	return readJSONBody(w.Body, &h.Body, "HTTPResp")
}

// HTTPReqBodyObject is an HTTPReq as written to a reader that sent
// BodyEncodingHeader. Its JSON is the HTTPReq's, but for a body that is not
// UTF-8, which it writes as a JSONBody.
type HTTPReqBodyObject HTTPReq

// MarshalJSON writes h as encoding/json writes an HTTPReq, but for a body that
// is not UTF-8, which it writes as a JSONBody.
func (h HTTPReqBodyObject) MarshalJSON() ([]byte, error) {
	if utf8.ValidString(h.Body) {
		return marshalJSONAsIs(httpReqFields(h))
	}
	return marshalJSONAsIs(struct {
		httpReqFields
		Body JSONBody `json:"body"`
	}{httpReqFields(h), jsonBodyOf(h.Body)})
}

// HTTPRespBodyObject is an HTTPResp as written to a reader that sent
// BodyEncodingHeader. Its JSON is the HTTPResp's, but for a body that is not
// UTF-8, which it writes as a JSONBody. StreamBody stays out of JSON, as its
// tag says.
type HTTPRespBodyObject HTTPResp

// MarshalJSON writes h as encoding/json writes an HTTPResp, but for a body
// that is not UTF-8, which it writes as a JSONBody.
func (h HTTPRespBodyObject) MarshalJSON() ([]byte, error) {
	if utf8.ValidString(h.Body) {
		return marshalJSONAsIs(httpRespFields(h))
	}
	return marshalJSONAsIs(struct {
		httpRespFields
		Body JSONBody `json:"body"`
	}{httpRespFields(h), jsonBodyOf(h.Body)})
}

// TestCaseBodyObjects is a test case as written to a reader that sent
// BodyEncodingHeader. Its JSON is the TestCase's, with every field, but for an
// HTTP request or response body that is not UTF-8, which it writes as a
// JSONBody; the mocks a test case holds are written as before. A nil TestCase
// is written as null.
type TestCaseBodyObjects struct {
	TestCase *TestCase
}

// MarshalJSON writes v's test case, its HTTP bodies that are not UTF-8 as
// JSONBody objects.
func (v TestCaseBodyObjects) MarshalJSON() ([]byte, error) {
	tc := v.TestCase
	if tc == nil {
		return []byte("null"), nil
	}
	return marshalJSONAsIs(struct {
		*TestCase
		HTTPReq  HTTPReqBodyObject  `json:"http_req"`
		HTTPResp HTTPRespBodyObject `json:"http_resp"`
	}{tc, HTTPReqBodyObject(tc.HTTPReq), HTTPRespBodyObject(tc.HTTPResp)})
}

// TestCaseJSON is what a writer encodes for tc: its TestCaseBodyObjects when
// the reader sent BodyEncodingHeader (bodyObjects) and a body of tc is not
// UTF-8, and tc itself otherwise. The JSON of a test case whose bodies are both
// UTF-8 is the same either way, and tc itself is written in one pass, where a
// MarshalJSON's output is held and copied again: writing a text test case for a
// reader that asked costs what it always did.
func TestCaseJSON(tc *TestCase, bodyObjects bool) interface{} {
	if bodyObjects && tc != nil && (!utf8.ValidString(tc.HTTPReq.Body) || !utf8.ValidString(tc.HTTPResp.Body)) {
		return TestCaseBodyObjects{TestCase: tc}
	}
	return tc
}

// jsonBodyOf is the JSON form of a body that is not UTF-8.
func jsonBodyOf(body string) JSONBody {
	return JSONBody{Encoding: BodyEncodingBase64, Data: base64.StdEncoding.EncodeToString([]byte(body))}
}

// readJSONBody sets *body from its JSON form raw, the value's bytes as
// encoding/json hands them to a json.RawMessage, with no space around them: a
// string as it stands, an object to the bytes it encodes. No body, or null,
// leaves *body as it is, as decoding into a string does. An object that names
// no encoding keploy reads, holds no data, or data that is not base64, is an
// error: the body is not guessed at. A writer writes an object only for a body
// that has bytes, so one with no data (absent, null or empty) is not a body.
func readJSONBody(raw json.RawMessage, body *string, of string) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	switch raw[0] {
	case '"':
		return json.Unmarshal(raw, body)
	case '{':
		var jb JSONBody
		if err := json.Unmarshal(raw, &jb); err != nil {
			return fmt.Errorf("models: %s body: %w", of, err)
		}
		if jb.Encoding != BodyEncodingBase64 {
			return fmt.Errorf("models: %s body: encoding %q is not one keploy reads (only %q)", of, jb.Encoding, BodyEncodingBase64)
		}
		if jb.Data == "" {
			return fmt.Errorf("models: %s body: a body object holds no data", of)
		}
		b, err := base64.StdEncoding.DecodeString(jb.Data)
		if err != nil {
			return fmt.Errorf("models: %s body: its %s data does not decode: %w", of, BodyEncodingBase64, err)
		}
		*body = string(b)
		return nil
	}
	return fmt.Errorf("models: %s body: %.16s is neither a string nor a body object", of, raw)
}

// marshalJSONAsIs encodes v without escaping HTML. The encoder that called a
// MarshalJSON escapes its output if it is set to (json.Encoder.SetEscapeHTML),
// so a field is written exactly as encoding/json writes it in the plain
// struct, whatever the caller's setting.
func marshalJSONAsIs(v interface{}) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
