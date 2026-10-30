package utils

import (
	"bufio"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// GetReqMeta names a request's method, host, path shape and query keys'
// shapes: never a query value (one that holds "=" too: a key ends at its
// part's first "="), a path segment that may hold a value (an ID, an e-mail
// address), the userinfo of an absolute URL, a header or the body, and never
// a query part with no "=" (it may be a bare token). A bare token that holds
// "=" (padded base64, ?dG9rZW4=) is named as the key before it, by that
// key's shape: {} unless it reads as a name. It logged the path whole.
func TestGetReqMetaNamesTheQueryKeysOnly(t *testing.T) {
	cases := []struct {
		name, req string
		want      map[string]string
	}{
		{"query values, a repeated key, an escaped key, a value with base64 padding, a bare part and an empty key",
			"POST /a/b?token=t0k-v4lue&page=2&token=t1-v4lue&b%61d=x-v4lue&sig=c2ln-v4lue==&b4re-t0ken&=n0key-v4lue HTTP/1.1\r\nHost: api.example\r\n" +
				"Authorization: Bearer au7h-v4lue\r\nContent-Length: 10\r\n\r\nb0dy-v4lue",
			map[string]string{"method": "POST", "host": "api.example", "path": "/a/b", "query keys": "bad,page,sig,token"}},
		{"an absolute URL with userinfo",
			"GET http://us3r:pa55-v4lue@api.example/p?k=v-v4lue HTTP/1.1\r\nHost: api.example\r\n\r\n",
			map[string]string{"method": "GET", "host": "api.example", "path": "/p", "query keys": "k"}},
		{"no query", "GET /p HTTP/1.1\r\nHost: api.example\r\n\r\n",
			map[string]string{"method": "GET", "host": "api.example", "path": "/p"}},
		{"a path of values and a bare token with base64 padding",
			"GET /users/4207v4lue/orders/ada.v4lue@example.com?dG9rZW4t0ken=&page=2 HTTP/1.1\r\nHost: api.example\r\n\r\n",
			map[string]string{"method": "GET", "host": "api.example", "path": "/users/{}/orders/{}", "query keys": "page,{}"}},
	}
	for _, c := range cases {
		r, err := http.ReadRequest(bufio.NewReader(strings.NewReader(c.req)))
		if err != nil {
			t.Fatalf("%s: ReadRequest: %v", c.name, err)
		}
		got := GetReqMeta(r)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: GetReqMeta = %v, want %v", c.name, got, c.want)
		}
		for k, v := range got {
			if strings.Contains(v, "v4lue") || strings.Contains(v, "t0ken") {
				t.Errorf("%s: GetReqMeta[%q] = %q carries a value of the request", c.name, k, v)
			}
		}
	}
	if got := GetReqMeta(nil); len(got) != 0 {
		t.Errorf("GetReqMeta(nil) = %v, want empty", got)
	}
}

// URLMeta names a URL as GetReqMeta names a request's: its host, its path's
// shape and its query keys' shapes, never a query value, the userinfo or the
// fragment; a URL that does not parse is said to be one, and none of it is
// named (url.Parse's error quotes it). keploy test logged a test's URL whole.
func TestURLMetaNamesTheHostThePathShapeAndTheQueryKeysOnly(t *testing.T) {
	for _, c := range []struct {
		name, url string
		want      map[string]string
	}{
		{"userinfo, a value segment, query values and a fragment",
			"http://us3r:pa55-v4lue@api.example:8080/pay/ada.lovelace@example.com?token=t0k-v4lue&page=2#fr4g-v4lue",
			map[string]string{"host": "api.example:8080", "path": "/pay/{}", "query keys": "page,token"}},
		{"no query", "https://api.example/p", map[string]string{"host": "api.example", "path": "/p"}},
		{"unparseable", "http://api.example/%zz?token=t0k-v4lue", map[string]string{"url": "unparseable"}},
	} {
		if got := URLMeta(c.url); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: URLMeta(%q) = %v, want %v", c.name, c.url, got, c.want)
		}
	}
}
