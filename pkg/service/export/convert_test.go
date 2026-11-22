package export

import (
	"reflect"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	postmanimport "go.keploy.io/server/v3/pkg/service/import"
	"go.uber.org/zap"
)

// jsonOptions is the raw-JSON body-language hint that is set when a request
// body is JSON.
func jsonOptions() map[string]interface{} {
	return map[string]interface{}{
		"raw": map[string]interface{}{
			"headerFamily": "json",
			"language":     "json",
		},
	}
}

// convertSchema builds an HTTPSchema for a request with the given form data and
// a response with the given content type.
func convertSchema(reqForm []models.FormData, reqCT, respCT string) *models.HTTPSchema {
	header := map[string]string{}
	if reqCT != "" {
		header["Content-Type"] = reqCT
	}
	respHeader := map[string]string{}
	if respCT != "" {
		respHeader["Content-Type"] = respCT
	}
	return &models.HTTPSchema{
		Request: models.HTTPReq{
			Method: "POST",
			URL:    "http://example.com/upload",
			Header: header,
			Form:   reqForm,
			Body:   `{}`,
		},
		Response: models.HTTPResp{
			StatusCode: 200,
			Header:     respHeader,
			Body:       `{"ok":true}`,
		},
	}
}

func requestBodyOf(t *testing.T, s *models.HTTPSchema) postmanimport.PostmanRequestBody {
	t.Helper()
	c := ConvertKeployHTTPToPostmanCollection(zap.NewNop(), s)
	if c == nil {
		t.Fatal("collection is nil")
	}
	req, ok := c["request"].(postmanimport.PostmanRequest)
	if !ok {
		t.Fatalf("request has type %T, want PostmanRequest", c["request"])
	}
	return req.Body
}

// TestConvertRequestBodyOptionsFollowTheRequestContentType locks the body-language
// option to the REQUEST's content type. It used to be overwritten from the
// RESPONSE's content type: the second `strings.Contains(Response.Header...)`
// block set request.Body.Options, so a multipart (form-data) request paired
// with a JSON response was emitted with mode:"formdata" AND options.raw.language
// "json" — two contradictory claims about the same body — and a request whose
// own content type was not JSON inherited the response's language hint.
func TestConvertRequestBodyOptionsFollowTheRequestContentType(t *testing.T) {
	form := []models.FormData{{Key: "file", FileNames: []string{"a.txt"}, Paths: []string{"/tmp/a.txt"}}}

	tests := []struct {
		name     string
		form     []models.FormData
		reqCT    string
		respCT   string
		wantMode string
		wantOpts map[string]interface{}
	}{
		{
			name: "form-data request, JSON response: options stay clear",
			form: form, reqCT: "multipart/form-data", respCT: "application/json",
			wantMode: "formdata", wantOpts: nil,
		},
		{
			name: "JSON request, JSON response: options are JSON",
			form: nil, reqCT: "application/json", respCT: "application/json",
			wantMode: "raw", wantOpts: jsonOptions(),
		},
		{
			name: "form-data request, non-JSON response: options stay clear",
			form: form, reqCT: "multipart/form-data", respCT: "text/plain",
			wantMode: "formdata", wantOpts: nil,
		},
		{
			name: "JSON request, non-JSON response: options are still JSON",
			form: nil, reqCT: "application/json", respCT: "text/plain",
			wantMode: "raw", wantOpts: jsonOptions(),
		},
		{
			name: "plain-text raw request, JSON response: options stay clear",
			form: nil, reqCT: "text/plain", respCT: "application/json",
			wantMode: "raw", wantOpts: nil,
		},
		{
			name: "raw request without Content-Type, JSON response: options stay clear",
			form: nil, reqCT: "", respCT: "application/json",
			wantMode: "raw", wantOpts: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := requestBodyOf(t, convertSchema(tc.form, tc.reqCT, tc.respCT))
			if body.Mode != tc.wantMode {
				t.Errorf("body mode = %q, want %q", body.Mode, tc.wantMode)
			}
			if !reflect.DeepEqual(body.Options, tc.wantOpts) {
				t.Errorf("request body options = %#v; want %#v", body.Options, tc.wantOpts)
			}
		})
	}
}
