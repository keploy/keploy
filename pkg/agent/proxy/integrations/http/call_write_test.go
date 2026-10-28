package http

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// An HTTP recording's call writes by its method: POST, PUT, PATCH and DELETE
// write; GET, HEAD, OPTIONS and TRACE read; any other method, a method in
// another case, and a recording with no request are calls keploy cannot
// tell. The parser registers it for its kind (models.CallWriteOf).
func TestAnHTTPCallWritesByItsMethod(t *testing.T) {
	for method, want := range map[string]string{
		"POST": "HTTP POST", "PUT": "HTTP PUT", "PATCH": "HTTP PATCH", "DELETE": "HTTP DELETE",
		"GET": "", "HEAD": "", "OPTIONS": "", "TRACE": "", "CONNECT": "", "PROPFIND": "", "post": "", "": "",
	} {
		m := httpMock("mock-1", method, "/keys/stamp")
		got, ok := callWrite(m)
		if got != want || ok != (want != "") {
			t.Errorf("%q: callWrite = %q, %v; want %q", method, got, ok, want)
		}
		if reg, _ := models.CallWriteOf(m); reg != want {
			t.Errorf("%q: the registered call write is %q; want %q", method, reg, want)
		}
	}
	if got, ok := callWrite(&models.Mock{Kind: models.HTTP}); ok || got != "" {
		t.Errorf("a recording with no request writes %q", got)
	}
	if _, ok := callWrite(nil); ok {
		t.Error("a nil recording writes")
	}
}
