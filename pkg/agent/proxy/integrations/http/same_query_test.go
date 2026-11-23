package http

import (
	"net/url"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// TestSameRecordedQuery holds that SameRecordedQuery reads two queries as
// the strict comparison does: the same parameters with the same values, a
// repeated one's in any order, keploy's own parameters skipped, a SigV4
// presigned URL's signing parameters by their grammar, and a learned
// parameter's one value free.
func TestSameRecordedQuery(t *testing.T) {
	rec := func(u string, params map[string]string) *models.Mock {
		return &models.Mock{Kind: models.HTTP, Spec: models.MockSpec{HTTPReq: &models.HTTPReq{Method: "GET", URL: u, URLParams: params}}}
	}
	q := func(raw string) url.Values {
		v, err := url.ParseQuery(raw)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	presigned := func(date, sig string) string {
		return "X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=AKIDEXAMPLE%2F20261103%2Fus-east-1%2Fs3%2Faws4_request&X-Amz-Date=" + date +
			"&X-Amz-Expires=900&X-Amz-SignedHeaders=host&X-Amz-Signature=" + sig + "&x-id=GetObject"
	}
	const sigA, sigB = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	otherScope := "X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=AKIDEXAMPLE%2F20261103%2Feu-west-1%2Fs3%2Faws4_request&X-Amz-Date=20261103T101500Z" +
		"&X-Amz-Expires=900&X-Amz-SignedHeaders=host&X-Amz-Signature=" + sigB + "&x-id=GetObject"

	rows := []struct {
		name    string
		rec     *models.Mock
		learned map[string][]string
		query   url.Values
		want    bool
	}{
		{"the same", rec("http://pay.test/v1/charges?a=1&b=2", nil), nil, q("a=1&b=2"), true},
		{"another order", rec("http://pay.test/v1/charges?a=1&b=2", nil), nil, q("b=2&a=1"), true},
		{"a repeated parameter's values in another order", rec("http://pay.test/v1/charges?a=1&a=2", nil), nil, q("a=2&a=1"), true},
		{"another value", rec("http://pay.test/v1/charges?a=1", nil), nil, q("a=2"), false},
		{"a parameter more", rec("http://pay.test/v1/charges?a=1", nil), nil, q("a=1&x=1"), false},
		{"a parameter less", rec("http://pay.test/v1/charges?a=1&x=1", nil), nil, q("a=1"), false},
		{"keploy's own parameter", rec("http://pay.test/v1/charges?a=1&keployRun=1", nil), nil, q("a=1&keployRun=2"), true},
		{"a learned value differs", rec("http://pay.test/v1/charges?a=1&ts=1", nil), map[string][]string{"url.query.ts": {}}, q("a=1&ts=2"), true},
		{"a learned parameter with two values", rec("http://pay.test/v1/charges?ts=1", nil), map[string][]string{"url.query.ts": {}}, q("ts=2&ts=3"), false},
		{"another value next to a learned one", rec("http://pay.test/v1/charges?a=1&ts=1", nil), map[string][]string{"url.query.ts": {}}, q("a=2&ts=2"), false},
		{"read from URLParams", rec("http://pay.test/v1/charges", map[string]string{"a": "1, 2"}), nil, q("a=2&a=1"), true},
		{"a presigned URL signed again", rec("http://s3.test/bucket/key?"+presigned("20261103T101500Z", sigA), nil), nil, q(presigned("20261103T102000Z", sigB)), true},
		{"a presigned URL of another scope", rec("http://s3.test/bucket/key?"+presigned("20261103T101500Z", sigA), nil), nil, q(otherScope), false},
		{"a lone signature is a value", rec("http://s3.test/bucket/key?X-Amz-Signature="+sigA, nil), nil, q("X-Amz-Signature=" + sigB), false},
		{"no mock", nil, nil, q("a=1"), false},
		{"no request", &models.Mock{Kind: models.HTTP}, nil, q("a=1"), false},
	}
	for _, r := range rows {
		if got := SameRecordedQuery(r.rec, r.learned, r.query); got != r.want {
			t.Errorf("%s: SameRecordedQuery = %v, want %v", r.name, got, r.want)
		}
	}
}
