package http

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// matchWithURLNoise runs the matcher with keploy.yml's url noise and no
// other.
func matchWithURLNoise(t *testing.T, db *mockMemDb, r *req, urlNoise []string) (bool, *models.Mock, *matchDiag) {
	t.Helper()
	ok, stub, diag, err := newHTTP().match(context.Background(), r, db, nil, nil, urlNoise, true, false, false, true, true)
	if err != nil {
		t.Fatal(err)
	}
	return ok, stub, diag
}

// a recording of another method is not this call.
func TestStrictAnotherMethodMisses(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	rec := onHost("get-order", "/orders/1", "store")
	db := &mockMemDb{mocks: []*models.Mock{rec}, updateUnFilteredReturn: true}
	live := getTo("/orders/1", "store")
	live.method = "DELETE"
	if ok, stub, _ := matchWithURLNoise(t, db, live, nil); ok {
		t.Fatalf("a DELETE was answered with the GET recording %s", stub.Name)
	}
}

// a recorded query parameter the call does not send.
func TestStrictAQueryParameterMissingFromTheCallMisses(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	rec := onHost("list", "/orders", "store")
	rec.Spec.HTTPReq.URL = "/orders?status=open&owner=me"
	db := &mockMemDb{mocks: []*models.Mock{rec}, updateUnFilteredReturn: true}
	live := getTo("/orders", "store")
	live.url = &url.URL{Path: "/orders", RawQuery: "status=open"}
	if ok, _, _ := matchWithURLNoise(t, db, live, nil); ok {
		t.Fatal("a call without the recorded owner= parameter was answered")
	}
}

// Differs names a field once, even when several array
// elements differ there.
func TestStrictDiffersNamesAFieldOnce(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	rec := onHost("batch", "/batch", "store")
	rec.Spec.HTTPReq.Method = "POST"
	rec.Spec.HTTPReq.Header["Content-Type"] = "application/json"
	rec.Spec.HTTPReq.Body = `{"items":[{"id":1},{"id":2}]}`
	db := &mockMemDb{mocks: []*models.Mock{rec}, updateUnFilteredReturn: true}
	live := getTo("/batch", "store")
	live.method = "POST"
	live.header.Set("Content-Type", "application/json")
	live.body = []byte(`{"items":[{"id":3},{"id":4}]}`)
	ok, _, diag := matchWithURLNoise(t, db, live, nil)
	if ok {
		t.Fatal("served")
	}
	if got := differsOf(diag.differs); strings.Join(got, ",") != "body.items[].id" {
		t.Fatalf("differs %v, want [body.items[].id]", got)
	}
}

// The turns survive a re-staging that renumbers the pool (TestModeInfo.ID is
// a mock's index in a staging).
func TestStrictTheTurnsSurviveARestaging(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	base := time.Unix(1_700_000_000, 0)
	cm := func(name string, ts time.Time, id int) *models.Mock {
		m := onHost(name, "/counter", "store")
		m.TestModeInfo.Consume = models.ConsumeCursorSaturate
		m.TestModeInfo.Lifetime = models.LifetimeSession
		m.Spec.ReqTimestampMock = ts
		m.TestModeInfo.ID = id
		return m
	}
	x1, x2 := cm("x1", base, 0), cm("x2", base.Add(time.Second), 1)
	db := &mockMemDb{mocks: []*models.Mock{x1, x2}, updateUnFilteredReturn: true}
	ok, stub, _ := matchWithURLNoise(t, db, getTo("/counter", "store"), nil)
	if !ok || stub.Name != "x1" {
		t.Fatalf("first call: %v %v", ok, stub)
	}
	// The next test's staging: another mock ahead of them renumbers both.
	other := onHost("other", "/health", "store")
	other.TestModeInfo.ID = 0
	x1.TestModeInfo.ID, x2.TestModeInfo.ID = 1, 2
	db.mocks = []*models.Mock{other, x1, x2}
	ok, stub, _ = matchWithURLNoise(t, db, getTo("/counter", "store"), nil)
	if !ok || stub.Name != "x2" {
		t.Fatalf("second call after a re-staging served %v, want x2: the sequence restarted", stub.Name)
	}
}

// repeated values keep their count.
func TestStrictRepeatedValuesKeepTheirCount(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	rec := onHost("q", "/items", "store")
	rec.Spec.HTTPReq.URL = "/items?id=1&id=1&id=2"
	db := &mockMemDb{mocks: []*models.Mock{rec}, updateUnFilteredReturn: true}
	live := getTo("/items", "store")
	live.url = &url.URL{Path: "/items", RawQuery: "id=1&id=2&id=2"}
	if ok, _, _ := matchWithURLNoise(t, db, live, nil); ok {
		t.Fatal("id=1,1,2 answered id=1,2,2")
	}
	live.url = &url.URL{Path: "/items", RawQuery: "id=1&id=2"}
	if ok, _, _ := matchWithURLNoise(t, db, live, nil); ok {
		t.Fatal("id=1,1,2 answered id=1,2")
	}
}

// a multipart field renamed, its value the same.
func TestStrictAMultipartFieldRenamedMisses(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	body := func(name string) string {
		return "--B\r\nContent-Disposition: form-data; name=\"" + name + "\"\r\n\r\nv\r\n--B--\r\n"
	}
	rec := onHost("up", "/upload", "store")
	rec.Spec.HTTPReq.Method = "POST"
	rec.Spec.HTTPReq.Header["Content-Type"] = "multipart/form-data; boundary=B"
	rec.Spec.HTTPReq.Body = body("role")
	db := &mockMemDb{mocks: []*models.Mock{rec}, updateUnFilteredReturn: true}
	live := getTo("/upload", "store")
	live.method = "POST"
	live.header.Set("Content-Type", "multipart/form-data; boundary=B")
	live.body = []byte(body("admin"))
	if ok, _, _ := matchWithURLNoise(t, db, live, nil); ok {
		t.Fatal("a renamed multipart field was answered")
	}
}

// keploy.yml's url noise widens a recording with
// variables outside their places.
func TestVariables_URLNoiseWidensTheComparison(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	read := at(varMock("read", "GET", "http://store/orders/ord-rec?ts=111", "", 200, `{}`,
		models.MockVariables{"orderId": useV("ord-rec", []string{"url.path[1]"})}), 1)
	create := at(varMock("create", "POST", "http://store/orders", `{"id":"ord-rec"}`, 201, `{}`,
		models.MockVariables{"orderId": bindV("ord-rec", "body.id")}), 0)
	db := &varDb{perTest: []*models.Mock{create, read}}
	call := func(r *req) bool {
		ok, _, _, err := h.match(context.Background(), r, db, nil, nil, []string{`ts=\d+`, `^\d{3}$`}, true, false, false, true, true)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !call(liveReq("POST", "/orders", `{"id":"ord-live"}`)) {
		t.Fatal("create missed")
	}
	r := liveReq("GET", "/orders/ord-live?ts=222", "")
	r.header = http.Header{"Content-Type": []string{"application/json"}, "Host": []string{"store"}}
	if !call(r) {
		t.Fatal("url noise did not excuse ts in the variable pass")
	}
}

// A path variable the client percent-encodes otherwise than Go's PathEscape
// (an email's @ as %40, a literal comma, lowercase hex) is put back as it was
// recorded and the read-back is answered: the path is compared decoded.
func TestVariables_APathVariableTheClientEncodedIsAnswered(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, tc := range []struct{ name, recSeg, liveSeg, recVal string }{
		{"email %40", "a%40rec.com", "b%40live.com", "a@rec.com"},
		{"comma literal", "1,2", "3,4", "1,2"},
		{"lowercase hex", "caf%c3%a9", "th%c3%a9", "café"},
		{"control: plain", "o-rec", "o-live", "o-rec"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHTTP()
			create := at(varMock("mock-1", "POST", "http://store/users", `{"email":"`+tc.recVal+`"}`, 201, `{}`,
				models.MockVariables{"email": bindV(tc.recVal, "body.email")}), 1)
			read := at(varMock("mock-2", "GET", "http://store/users/"+tc.recSeg, "", 200, `{"ok":true}`,
				models.MockVariables{"email": useV(tc.recVal, []string{"url.path[1]"})}), 2)
			db := &varDb{perTest: []*models.Mock{create, read}}
			liveVal, _ := url.PathUnescape(tc.liveSeg)
			ok, _, diag := matchVar(t, h, db, liveReq("POST", "/users", `{"email":"`+liveVal+`"}`))
			if !ok {
				t.Fatalf("create missed: %+v", diag)
			}
			ok, stub, diag := matchVar(t, h, db, liveReq("GET", "/users/"+tc.liveSeg, ""))
			if !ok {
				t.Fatalf("read-back missed (false red): diag phase=%v differs=%v variable=%+v", diag.phase, diag.differs, diag.variable)
			}
			if stub.Name != "mock-2" {
				t.Fatalf("read-back served %s, want mock-2", stub.Name)
			}
		})
	}
}

// The variable pass compares the destination too: a recording with
// variables made for one host answers no call to another.
func TestVariables_ARecordingWithVariablesIsItsHostsOnly(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	onStore := at(varMock("on-store", "POST", "http://store/orders", `{"id":"ord-rec"}`, 201, `{"id":"ord-rec"}`,
		models.MockVariables{"orderId": bindV("ord-rec", "body.id", "body.id")}), 1)
	if ok, stub, _ := matchVar(t, h, &varDb{perTest: []*models.Mock{onStore}}, liveReq("POST", "http://other/orders", `{"id":"ord-live"}`)); ok {
		t.Fatalf("a call to host other was answered with %s, recorded for host store", stub.Name)
	}
	if ok, _, diag := matchVar(t, h, &varDb{perTest: []*models.Mock{onStore}}, liveReq("POST", "http://store/orders", `{"id":"ord-live"}`)); !ok {
		t.Fatalf("a call to host store was not answered: %+v", diag)
	}
}
