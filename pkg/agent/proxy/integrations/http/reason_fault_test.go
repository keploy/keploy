package http

import (
	"net/http"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

func TestCustomReasonKeepsOnlyANonStandardPhrase(t *testing.T) {
	for _, tc := range []struct {
		status string
		code   int
		want   string
	}{
		{"402 Card Declined", 402, "Card Declined"},
		{"402 Payment Required", 402, ""},
		{"299 Custom", 299, "Custom"},
		{"200", 200, ""},
		{"", 0, ""},
	} {
		if got := customReason(&http.Response{Status: tc.status, StatusCode: tc.code}); got != tc.want {
			t.Errorf("customReason(%q) = %q, want %q", tc.status, got, tc.want)
		}
	}
}

func TestReasonPhraseServesTheRecordedOne(t *testing.T) {
	for _, tc := range []struct {
		msg  string
		code int
		want string
	}{
		{"Card Declined", 402, "Card Declined"},
		{"402 Card Declined", 402, "Card Declined"},
		{"", 402, "Payment Required"},
		{"402 ", 402, "Payment Required"},
		{"Bad\r\nX-Injected: 1", 402, "Payment Required"},
		{"Bad\nline", 200, "OK"},
	} {
		if got := reasonPhrase(&models.HTTPResp{StatusCode: tc.code, StatusMessage: tc.msg}); got != tc.want {
			t.Errorf("reasonPhrase(%d, %q) = %q, want %q", tc.code, tc.msg, got, tc.want)
		}
	}
}

func TestCloseDelimited(t *testing.T) {
	for _, tc := range []struct {
		resp   http.Response
		method string
		want   bool
	}{
		{http.Response{ContentLength: -1, StatusCode: 200}, "GET", true},
		{http.Response{ContentLength: 5, StatusCode: 200}, "GET", false},
		{http.Response{ContentLength: 0, StatusCode: 200}, "GET", false},
		{http.Response{ContentLength: -1, TransferEncoding: []string{"chunked"}, StatusCode: 200}, "GET", false},
		{http.Response{ContentLength: -1, StatusCode: 204}, "GET", false},
		{http.Response{ContentLength: -1, StatusCode: 200}, "HEAD", false},
	} {
		if got := closeDelimited(&tc.resp, tc.method); got != tc.want {
			t.Errorf("closeDelimited(%+v, %s) = %v, want %v", tc.resp, tc.method, got, tc.want)
		}
	}
}
