package http

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

// gqlBody is a GraphQL request body of the operation op.
func gqlBody(op string) string {
	return `{"operationName":"` + op + `","variables":{"cid":"c1"},"query":"query ` + op + `($cid: String!) { x(cid: $cid) { id } }"}`
}

// gqlMock is a recording of the GraphQL operation op posted to
// localhost:8083/query.
func gqlMock(name, op string) *models.Mock {
	return &models.Mock{Name: name, Kind: models.HTTP, Spec: models.MockSpec{HTTPReq: &models.HTTPReq{
		Method: "POST", URL: "http://localhost:8083/query",
		Header: map[string]string{"Content-Type": "application/json"},
		Body:   gqlBody(op),
	}}}
}

// gqlReq is a live GraphQL request of the operation op.
func gqlReq(op string) (*http.Request, []byte) {
	body := gqlBody(op)
	return &http.Request{Method: "POST", URL: &url.URL{Path: "/query"}, Host: "localhost:8083",
		Header: http.Header{"Content-Type": {"application/json"}}}, []byte(body)
}

// readOperations registers, for the test, a reader that names a JSON body's
// operationName as a query, as enterprise's GraphQL reader names one. It
// returns the count of the requests it has read.
func readOperations(t *testing.T) *atomic.Int64 {
	t.Helper()
	reads := &atomic.Int64{}
	unregister := models.RegisterHTTPOperation(func(r *models.HTTPReq) (models.HTTPOperation, bool) {
		reads.Add(1)
		var b struct {
			OperationName string `json:"operationName"`
		}
		if json.Unmarshal([]byte(r.Body), &b) != nil || b.OperationName == "" {
			return models.HTTPOperation{}, false
		}
		return models.HTTPOperation{Label: "query " + b.OperationName}, true
	})
	t.Cleanup(unregister)
	return reads
}

// A recording's operation is read once, however many misses compare
// themselves with it, and its entry goes when the recording is collected.
func TestARecordingsOperationIsReadOnce(t *testing.T) {
	reads := readOperations(t)
	table := &operationTable{}
	if got := table.of(nil); got != "" || reads.Load() != 0 {
		t.Fatalf("no request: %q after %d reads", got, reads.Load())
	}
	func() {
		m := gqlMock("labels", "GetAppLabels")
		for range 3 {
			if got := table.of(m.Spec.HTTPReq); got != "query GetAppLabels" {
				t.Fatalf("operation %q", got)
			}
		}
		plain := &models.HTTPReq{Method: "POST", URL: "http://localhost:8083/rest", Body: `{"a":1}`}
		for range 2 {
			if got := table.of(plain); got != "" {
				t.Fatalf("a request of no operation: %q", got)
			}
		}
		if reads.Load() != 2 {
			t.Fatalf("two requests asked for five times were read %d times; want 2", reads.Load())
		}
	}()
	// Cleanups run on their own goroutine after a collection; wait for it.
	for i := 0; entriesOfOperations(table) != 0; i++ {
		if i == 10_000 {
			t.Fatalf("after %d collections the table still holds %d entries", i, entriesOfOperations(table))
		}
		runtime.GC()
		time.Sleep(time.Millisecond)
	}
}

// entriesOfOperations is the count of t's entries.
func entriesOfOperations(t *operationTable) int {
	n := 0
	t.entries.Range(func(any, any) bool { n++; return true })
	return n
}

// A miss of one operation is diffed against a recording of that operation,
// not against the first recording posted to the same URL. The report names
// the operation in its own field; its summary is the call's alone.
func TestAMissIsDiffedAgainstItsOwnOperation(t *testing.T) {
	readOperations(t)
	sub, labels, sub2 := gqlMock("sub", "GetSubscription"), gqlMock("labels", "GetAppLabels"), gqlMock("sub-2", "GetSubscription")
	short := gqlMock("b", "B")
	plain := &models.Mock{Name: "plain", Kind: models.HTTP, Spec: models.MockSpec{HTTPReq: &models.HTTPReq{
		Method: "POST", URL: "http://localhost:8083/query", Body: `{"a":1}`}}}
	request, body := gqlReq("GetAppLabels")
	liveOp := liveOperation(request, body)
	if liveOp != "query GetAppLabels" {
		t.Fatalf("live operation %q", liveOp)
	}
	for _, tc := range []struct {
		name            string
		liveOp          string
		survivors, pool []*models.Mock
		want            string
	}{
		{"survivors", liveOp, []*models.Mock{sub, labels}, nil, "labels"},
		{"the pool", liveOp, nil, []*models.Mock{sub, labels}, "labels"},
		{"no survivor of the operation: the first", liveOp, []*models.Mock{sub, sub2}, nil, "sub"},
		{"a call of no operation: the first survivor", "", []*models.Mock{sub, plain}, nil, "sub"},
		{"the pool, where a shorter label is nearer the bare call", liveOp, nil, []*models.Mock{short, labels}, "labels"},
		{"a survivor without a request is passed over", liveOp, []*models.Mock{nil, {Name: "bare", Kind: models.HTTP}, sub}, nil, "sub"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := pickClosestCandidate(request, tc.liveOp, tc.survivors, tc.pool)
			if got == nil || got.Name != tc.want {
				t.Fatalf("closest %v; want %s", got, tc.want)
			}
		})
	}
	for _, tc := range []struct {
		name string
		diag *matchDiag
	}{
		{"a schema survivor", &matchDiag{phase: models.MatchPhaseSchema, candidates: 2, schemaMatched: []*models.Mock{sub, labels}}},
		{"no survivor", &matchDiag{phase: models.MatchPhaseExhausted, candidates: 2}},
	} {
		t.Run("the report, "+tc.name, func(t *testing.T) {
			h := newHTTP()
			pool := []*models.Mock{sub, labels}
			r := h.buildHTTPMismatchReport(request, body, &mockMemDb{mocks: pool}, pool, nil, nil, tc.diag)
			if r.ActualSummary != "POST /query" || r.Operation != "query GetAppLabels" || r.ClosestMock != "labels" {
				t.Fatalf("summary %q operation %q closest %q", r.ActualSummary, r.Operation, r.ClosestMock)
			}
		})
	}
}

// readEveryForm registers, for the test, a reader that names the operation
// in each form a GraphQL request takes over HTTP, as enterprise's reader
// reads them: a GET's URL query, an application/graphql document, the
// "operations" part of a multipart upload. Each form is read from the URL
// with its query, the headers or the body, so a request built without one
// of them is not read.
func readEveryForm(t *testing.T) {
	t.Helper()
	name := func(r *models.HTTPReq) string {
		ct := r.Header["Content-Type"]
		switch {
		case r.Method == "GET":
			u, err := url.Parse(r.URL)
			if err != nil {
				return ""
			}
			return u.Query().Get("operationName")
		case ct == "application/graphql":
			if f := strings.Fields(r.Body); len(f) > 1 {
				return f[1]
			}
		case strings.HasPrefix(ct, "multipart/form-data"):
			_, params, err := mime.ParseMediaType(ct)
			if err != nil {
				return ""
			}
			part, err := multipart.NewReader(strings.NewReader(r.Body), params["boundary"]).NextPart()
			if err != nil || part.FormName() != "operations" {
				return ""
			}
			var b struct {
				OperationName string `json:"operationName"`
			}
			if json.NewDecoder(part).Decode(&b) != nil {
				return ""
			}
			return b.OperationName
		}
		return ""
	}
	t.Cleanup(models.RegisterHTTPOperation(func(r *models.HTTPReq) (models.HTTPOperation, bool) {
		if n := name(r); n != "" {
			return models.HTTPOperation{Label: "operation " + n}, true
		}
		return models.HTTPOperation{}, false
	}))
}

// A live call is read from what its recording holds: its URL with its
// query, its headers and its body, as the recorder takes them from the same
// request. So a GET whose URL query holds the request, an application/graphql
// document and a multipart upload are each named alike live and recorded.
func TestALiveCallIsReadAsItsRecording(t *testing.T) {
	readEveryForm(t)
	post := func(ct, body string) string {
		return fmt.Sprintf("POST /query HTTP/1.1\r\nHost: localhost:8083\r\nContent-Type: %s\r\nContent-Length: %d\r\n\r\n%s", ct, len(body), body)
	}
	upload := "--B\r\nContent-Disposition: form-data; name=\"operations\"\r\n\r\n" +
		`{"operationName":"Upload","query":"mutation Upload($f: Upload!) { upload(f: $f) }","variables":{"f":null}}` +
		"\r\n--B\r\nContent-Disposition: form-data; name=\"0\"; filename=\"a.txt\"\r\n\r\nfile bytes\r\n--B--\r\n"
	for _, tc := range []struct{ name, wire, want string }{
		{"a GET whose query holds the request", "GET /query?operationName=GetAppLabels&query=query+GetAppLabels+%7B+a+%7D HTTP/1.1\r\nHost: localhost:8083\r\n\r\n", "operation GetAppLabels"},
		{"an application/graphql document", post("application/graphql", "query GetSubscription { s }"), "operation GetSubscription"},
		{"a multipart upload", post("multipart/form-data; boundary=B", upload), "operation Upload"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request, err := http.ReadRequest(bufio.NewReader(strings.NewReader(tc.wire)))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Host", request.Host) // as the decoder and the recorder do
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Fatal(err)
			}
			// The request as the recorder keeps it (http.go).
			recorded := &models.HTTPReq{
				Method: models.Method(request.Method), ProtoMajor: request.ProtoMajor, ProtoMinor: request.ProtoMinor,
				URL: request.URL.String(), Header: pkg.ToYamlHTTPHeader(request.Header), Body: string(body),
				URLParams: pkg.URLParams(request),
			}
			if got := models.HTTPOperationLabel(recorded); got != tc.want {
				t.Fatalf("recorded: %q; want %q", got, tc.want)
			}
			if got := liveOperation(request, body); got != tc.want {
				t.Fatalf("live: %q; want %q", got, tc.want)
			}
		})
	}
}

// A live call's operation: none for a request with no URL, or none at all.
func TestLiveOperationOfNoRequest(t *testing.T) {
	readOperations(t)
	if got := liveOperation(nil, nil); got != "" {
		t.Fatalf("nil request: %q", got)
	}
	if got := liveOperation(&http.Request{Method: "POST"}, []byte(gqlBody("A"))); got != "" {
		t.Fatalf("no URL: %q", got)
	}
}

// A call answered by pairing names its operation in its own field.
func TestAPairingNamesItsOperation(t *testing.T) {
	readOperations(t)
	var got []*models.MockMismatchReport
	ctx := models.WithMockMismatchReporter(context.Background(), func(err error) {
		var withReport interface {
			MismatchReport() *models.MockMismatchReport
		}
		if errors.As(err, &withReport) {
			got = append(got, withReport.MismatchReport())
		}
	})
	in := &req{method: "POST", url: &url.URL{Path: "/query"}, header: http.Header{"Host": {"localhost:8083"}, "Content-Type": {"application/json"}}, body: []byte(gqlBody("GetAppLabels"))}
	reportPairing(ctx, in, gqlMock("labels", "GetAppLabels"), integrations.AnsweredCall{}, false, integrations.CallRound{})
	if len(got) != 1 || got[0].ActualSummary != "POST /query" || got[0].Operation != "query GetAppLabels" {
		t.Fatalf("reports %+v", got)
	}
}
