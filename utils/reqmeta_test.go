package utils

import (
	"bufio"
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// GetReqMeta names a request's method, host, path shape, path tag (of its
// method and escaped path, models.PathTag) and query keys:
// never a path segment, a query value (one that holds "=" too: a key ends at its part's first "="),
// the userinfo of an absolute URL, a header or the body, and never a query
// part with no "=" (it may be a bare token). A bare token that holds "="
// (padded base64, ?dG9rZW4=) is named as the key before it: a stated limit.
func TestGetReqMetaNamesTheQueryKeysOnly(t *testing.T) {
	cases := []struct {
		name, req string
		want      map[string]string
	}{
		{"query values, a repeated key, an escaped key, a value with base64 padding, a bare part and an empty key",
			"POST /a/s3gm-v4lue/b?token=t0k-v4lue&page=2&token=t1-v4lue&b%61d=x-v4lue&sig=c2ln-v4lue==&b4re-t0ken&=n0key-v4lue HTTP/1.1\r\nHost: api.example\r\n" +
				"Authorization: Bearer au7h-v4lue\r\nContent-Length: 10\r\n\r\nb0dy-v4lue",
			map[string]string{"method": "POST", "host": "{}.{} " + models.HostTag(context.Background(), "api.example"), "path": "/{}/{}/{}", "path_tag": models.PathTag(context.Background(), "POST", "/a/s3gm-v4lue/b"), "query keys": "bad,page,sig,token"}},
		{"an absolute URL with userinfo",
			"GET http://us3r:pa55-v4lue@api.example/p?k=v-v4lue HTTP/1.1\r\nHost: api.example\r\n\r\n",
			map[string]string{"method": "GET", "host": "{}.{} " + models.HostTag(context.Background(), "api.example"), "path": "/{}", "path_tag": models.PathTag(context.Background(), "GET", "/p"), "query keys": "k"}},
		{"no query", "GET /p HTTP/1.1\r\nHost: api.example\r\n\r\n",
			map[string]string{"method": "GET", "host": "{}.{} " + models.HostTag(context.Background(), "api.example"), "path": "/{}", "path_tag": models.PathTag(context.Background(), "GET", "/p")}},
		{"an escaped slash in a segment, an empty segment and a trailing slash",
			"GET /a%2Fs3gm-v4lue//b/ HTTP/1.1\r\nHost: api.example\r\n\r\n",
			map[string]string{"method": "GET", "host": "{}.{} " + models.HostTag(context.Background(), "api.example"), "path": "/{}//{}/", "path_tag": models.PathTag(context.Background(), "GET", "/a%2Fs3gm-v4lue//b/")}},
		{"a Host header that names a tenant, with a port: the name by its labels' shape and tag", "GET /p HTTP/1.1\r\nHost: t0ken-tenant.example:8443\r\n\r\n",
			map[string]string{"method": "GET", "host": "{}.{}:8443 " + models.HostTag(context.Background(), "t0ken-tenant.example"), "path": "/{}", "path_tag": models.PathTag(context.Background(), "GET", "/p")}},
		{"a Host header that is an address: whole", "GET /p HTTP/1.1\r\nHost: 10.0.0.7:8080\r\n\r\n",
			map[string]string{"method": "GET", "host": "10.0.0.7:8080", "path": "/{}", "path_tag": models.PathTag(context.Background(), "GET", "/p")}},
		{"the root", "GET / HTTP/1.1\r\nHost: api.example\r\n\r\n",
			map[string]string{"method": "GET", "host": "{}.{} " + models.HostTag(context.Background(), "api.example"), "path": "/", "path_tag": models.PathTag(context.Background(), "GET", "/")}},
	}
	for _, c := range cases {
		r, err := http.ReadRequest(bufio.NewReader(strings.NewReader(c.req)))
		if err != nil {
			t.Fatalf("%s: ReadRequest: %v", c.name, err)
		}
		got := GetReqMeta(context.Background(), r)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: GetReqMeta = %v, want %v", c.name, got, c.want)
		}
		for k, v := range got {
			if strings.Contains(v, "v4lue") || strings.Contains(v, "t0ken") {
				t.Errorf("%s: GetReqMeta[%q] = %q carries a value of the request", c.name, k, v)
			}
		}
	}
	if got := GetReqMeta(context.Background(), nil); len(got) != 0 {
		t.Errorf("GetReqMeta(context.Background(), nil) = %v, want empty", got)
	}
}
