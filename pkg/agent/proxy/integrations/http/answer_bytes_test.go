package http

import (
	"bytes"
	"compress/gzip"
	"crypto/md5"
	"encoding/base64"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/bodydigest"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// mainBuildMockResponseBytes is buildMockResponseBytes as origin/main
// (f6e2d456d) has it, copied verbatim but for its receiver: the rows compare
// the answers served with it.
func mainBuildMockResponseBytes(h *HTTP, stub *models.Mock) ([]byte, error) {
	name := ""
	if stub != nil {
		name = stub.Name
	}
	stub, err := withResponse(stub)
	if err != nil {
		return nil, err
	}
	if stub == nil || stub.Spec.HTTPResp == nil {
		return nil, fmt.Errorf("http: mock %q has no response to serialize", name)
	}
	protoMajor, protoMinor := 1, 1
	method := ""
	if stub.Spec.HTTPReq != nil {
		protoMajor, protoMinor = stub.Spec.HTTPReq.ProtoMajor, stub.Spec.HTTPReq.ProtoMinor
		method = string(stub.Spec.HTTPReq.Method)
	}
	statusLine := fmt.Sprintf("HTTP/%d.%d %d %s\r\n", protoMajor, protoMinor,
		stub.Spec.HTTPResp.StatusCode, http.StatusText(stub.Spec.HTTPResp.StatusCode))
	body := stub.Spec.HTTPResp.Body
	header := pkg.ToWireHTTPHeader(stub.Spec.HTTPResp.Header, stub.Spec.HTTPResp.HeaderLineLengths)
	var respBody string
	if encoding, ok := header["Content-Encoding"]; ok && len(encoding) > 0 {
		compressed, err := pkg.Compress(h.Logger, encoding[0], []byte(body))
		if err != nil {
			return nil, err
		}
		respBody = string(compressed)
	} else {
		respBody = body
	}
	// The digests the recorded body matched as its server sent it, over the
	// bytes sent now (models.MetadataBodyDigestsKey): obfuscation, encryption
	// and a variable rewrite a body, and keploy's compressor is not the
	// server's. Those it did not match are sent as recorded.
	bodydigest.Apply(stub.Spec.Metadata[models.MetadataBodyDigestsKey], header, body, respBody)
	noBody := responseHasNoBody(method, stub.Spec.HTTPResp.StatusCode)
	var headers string
	for key, values := range header {
		if key == "Content-Length" && !noBody {
			values = []string{strconv.Itoa(len(respBody))}
		}
		for _, value := range values {
			headers += fmt.Sprintf("%s: %s\r\n", key, value)
		}
	}
	return []byte(statusLine + headers + "\r\n" + respBody), nil
}

// answerShape splits an answer into its status line, its header lines sorted
// (a Go map gives them in a different order on each build) and its body.
func answerShape(t *testing.T, wire []byte) (string, []string, []byte) {
	t.Helper()
	head, body, ok := bytes.Cut(wire, []byte("\r\n\r\n"))
	if !ok {
		t.Fatalf("answer has no end of header: %q", wire)
	}
	lines := strings.Split(string(head), "\r\n")
	status, fields := lines[0], append([]string(nil), lines[1:]...)
	if len(fields) == 1 && fields[0] == "" {
		fields = nil
	}
	sort.Strings(fields)
	return status, fields, body
}

// sameAnswer fails unless got is the answer want is: the same status line,
// the same header lines and the same body bytes.
func sameAnswer(t *testing.T, what string, want, got []byte) {
	t.Helper()
	ws, wf, wb := answerShape(t, want)
	gs, gf, gb := answerShape(t, got)
	if ws != gs || strings.Join(wf, "\n") != strings.Join(gf, "\n") || !bytes.Equal(wb, gb) {
		t.Fatalf("%s: answer differs from main's\nmain:   %q\nbranch: %q", what, want, got)
	}
}

func gzipAt(t *testing.T, s string, level int) []byte {
	t.Helper()
	var b bytes.Buffer
	w, err := gzip.NewWriterLevel(&b, level)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// answerPool is n recorded HTTP mocks of every shape the serializer reads:
// request protocol and method (a HEAD's answer has no body), status codes with
// and without a body or a status text, no to a dozen header lines, a stale or
// missing Content-Length, gzip, brotli and an encoding keploy does not
// compress, a value recorded from several lines, and digest headers over the
// body and over a body compressed otherwise than keploy compresses it.
func answerPool(t *testing.T, n int) []*models.Mock {
	t.Helper()
	r := rand.New(rand.NewSource(int64(n)))
	methods := []models.Method{"GET", "POST", "HEAD", "PUT", "DELETE"}
	statuses := []int{200, 201, 204, 206, 301, 304, 404, 418, 500, 299, 103}
	encodings := []string{"", "", "gzip", "br", "deflate", "identity"}
	protos := [][2]int{{1, 1}, {1, 1}, {1, 0}, {2, 0}}
	pool := make([]*models.Mock, 0, n)
	for i := range n {
		body := ""
		switch r.Intn(4) {
		case 1:
			body = fmt.Sprintf(`{"id":%d,"name":"item-%d"}`, i, i)
		case 2:
			body = strings.Repeat(fmt.Sprintf("row %d;", i), 1+r.Intn(400))
		case 3:
			body = strings.Repeat("x", r.Intn(70_000))
		}
		header := map[string]string{"Content-Type": "application/json"}
		for h := range r.Intn(12) {
			header["X-H"+strconv.Itoa(h)] = strings.Repeat("v", r.Intn(40))
		}
		switch r.Intn(3) {
		case 0:
			header["Content-Length"] = strconv.Itoa(len(body))
		case 1:
			header["Content-Length"] = "999"
		}
		var lines models.HeaderLineLengths
		if r.Intn(4) == 0 {
			header["Set-Cookie"] = "a=1,b=22,c=333"
			lines = models.HeaderLineLengths{"Set-Cookie": {3, 4, 5}}
		}
		wire := []byte(body)
		if enc := encodings[r.Intn(len(encodings))]; enc != "" {
			header["Content-Encoding"] = enc
			if enc == "gzip" {
				wire = gzipAt(t, body, gzip.BestSpeed)
			}
		}
		if r.Intn(3) == 0 {
			sum := md5.Sum([]byte(body))
			if r.Intn(2) == 0 {
				sum = md5.Sum(wire)
			}
			header["Content-MD5"] = base64.StdEncoding.EncodeToString(sum[:])
		}
		meta := map[string]string{"type": "mocks"}
		if note := bodydigest.Note(header, []byte(body), wire); note != "" {
			meta[models.MetadataBodyDigestsKey] = note
		}
		proto := protos[r.Intn(len(protos))]
		m := &models.Mock{
			Kind: models.HTTP,
			Name: fmt.Sprintf("mock-%d", i),
			Spec: models.MockSpec{
				Metadata: meta,
				HTTPReq:  &models.HTTPReq{Method: methods[r.Intn(len(methods))], ProtoMajor: proto[0], ProtoMinor: proto[1], URL: fmt.Sprintf("http://dep/items/%d", i)},
				HTTPResp: &models.HTTPResp{StatusCode: statuses[r.Intn(len(statuses))], Header: header, Body: body, HeaderLineLengths: lines},
			},
		}
		m.MarkPooled()
		pool = append(pool, m)
	}
	return pool
}

// TestAnswersAreMainsAnswers serves every mock of pools of 100, 1k and 10k
// three times (built, built and kept, written from the kept bytes) with the
// proven switch off and on, and compares each answer with main's.
func TestAnswersAreMainsAnswers(t *testing.T) {
	for _, proven := range []bool{false, true} {
		for _, n := range []int{100, 1_000, 10_000} {
			t.Run(fmt.Sprintf("proven=%v/n=%d", proven, n), func(t *testing.T) {
				t.Setenv(provenswitch.Env, map[bool]string{false: "0", true: "1"}[proven])
				h := newHTTP()
				pool := answerPool(t, n)
				for _, m := range pool {
					want, err := mainBuildMockResponseBytes(h, m)
					if err != nil {
						t.Fatalf("%s: main: %v", m.Name, err)
					}
					var kept []byte
					for serve := range 3 {
						got, err := h.buildMockResponseBytes(m)
						if err != nil {
							t.Fatalf("%s serve %d: %v", m.Name, serve, err)
						}
						sameAnswer(t, fmt.Sprintf("%s serve %d", m.Name, serve), want, got)
						if serve == 2 && len(got) <= answerKeepLimit && !bytes.Equal(got, kept) {
							t.Fatalf("%s: third serve is not the bytes kept at the second", m.Name)
						}
						kept = got
					}
					// A copy of the pooled mock (not pooled) is built.
					got, err := h.buildMockResponseBytes(m.ShallowCopy())
					if err != nil {
						t.Fatal(err)
					}
					sameAnswer(t, m.Name+" copy", want, got)
				}
			})
		}
	}
}

// TestAnswersAreMainsOnRecordedSets compares every answer of the HTTP mocks
// recorded under KEPLOY_HTTP_RECORDINGS (the box harness's sets) with main's.
func TestAnswersAreMainsOnRecordedSets(t *testing.T) {
	dir := os.Getenv("KEPLOY_HTTP_RECORDINGS")
	if dir == "" {
		t.Skip("KEPLOY_HTTP_RECORDINGS is not set")
	}
	h := newHTTP()
	sets := recordedHTTPSets(t, dir)
	if len(sets) == 0 {
		t.Fatalf("no HTTP mocks under %s", dir)
	}
	for path, pool := range sets {
		for _, m := range pool {
			want, err := mainBuildMockResponseBytes(h, m)
			if err != nil {
				t.Fatalf("%s %s: main: %v", path, m.Name, err)
			}
			for serve := range 3 {
				got, err := h.buildMockResponseBytes(m)
				if err != nil {
					t.Fatalf("%s %s serve %d: %v", path, m.Name, serve, err)
				}
				sameAnswer(t, fmt.Sprintf("%s %s serve %d", path, m.Name, serve), want, got)
			}
		}
		t.Logf("%s: %d answers x 3 serves equal to main's", path, len(pool))
	}
}

// TestAnswersServedRewrittenAreBuiltEachServe: an answer read back per serve
// (kept apart from its mock, and the copy it is read back into), one the
// variable pass or the correlation pass rewrites, and one served from a mock
// that reads its response otherwise (a HEAD's) are built on every serve, and
// keep main's bytes.
func TestAnswersServedRewrittenAreBuiltEachServe(t *testing.T) {
	h := newHTTP()
	resp := func() *models.HTTPResp {
		return &models.HTTPResp{StatusCode: 200, Body: `{"id":"r-1"}`, Header: map[string]string{"Content-Length": "12", "Content-Type": "application/json"}}
	}
	spilled := &models.Mock{Kind: models.HTTP, Name: "spilled", Spec: models.MockSpec{HTTPReq: &models.HTTPReq{Method: "GET", ProtoMajor: 1, ProtoMinor: 1}}}
	spilled.SetResponseHydrator(func() (*models.HTTPResp, []models.MongoResponse, error) { return resp(), nil, nil })
	hydrated, err := withResponse(spilled)
	if err != nil {
		t.Fatal(err)
	}
	hydrated.Name = "hydrated"
	withVars := &models.Mock{Kind: models.HTTP, Name: "vars", Spec: models.MockSpec{HTTPReq: &models.HTTPReq{Method: "GET", ProtoMajor: 1, ProtoMinor: 1}, HTTPResp: resp(),
		Variables: models.MockVariables{"id": {Recorded: "r-1"}}}}
	correlated := &models.Mock{Kind: models.HTTP, Name: "corr", Spec: models.MockSpec{HTTPReq: &models.HTTPReq{Method: "GET", ProtoMajor: 1, ProtoMinor: 1}, HTTPResp: resp(),
		Correlations: []models.FieldCorrelation{{RequestPath: "body.id", RecordedValue: "r-1"}}}}
	for _, m := range []*models.Mock{spilled, hydrated, withVars, correlated} {
		m.MarkPooled()
		if m == hydrated {
			// withResponse's copy, which no pool holds.
			m = m.ShallowCopy()
		}
		if keepsAnswer(m) {
			t.Fatalf("%s: keepsAnswer = true", m.Name)
		}
		before := keptAnswers.builds.Load()
		for range 3 {
			want, err := mainBuildMockResponseBytes(h, m)
			if err != nil {
				t.Fatal(err)
			}
			got, err := h.buildMockResponseBytes(m)
			if err != nil {
				t.Fatal(err)
			}
			sameAnswer(t, m.Name, want, got)
		}
		if n := keptAnswers.builds.Load() - before; n != 0 {
			t.Fatalf("%s: %d builds went through the kept answers", m.Name, n)
		}
	}
	// One response, served from a GET and from a HEAD: a HEAD's answer keeps
	// its recorded Content-Length, a GET's has it recomputed.
	shared := resp()
	shared.Header["Content-Length"] = "999"
	get := &models.Mock{Kind: models.HTTP, Name: "get", Spec: models.MockSpec{HTTPReq: &models.HTTPReq{Method: "GET", ProtoMajor: 1, ProtoMinor: 1}, HTTPResp: shared}}
	head := &models.Mock{Kind: models.HTTP, Name: "head", Spec: models.MockSpec{HTTPReq: &models.HTTPReq{Method: "HEAD", ProtoMajor: 1, ProtoMinor: 1}, HTTPResp: shared}}
	get.MarkPooled()
	head.MarkPooled()
	for range 4 {
		for _, m := range []*models.Mock{get, head} {
			want, err := mainBuildMockResponseBytes(h, m)
			if err != nil {
				t.Fatal(err)
			}
			got, err := h.buildMockResponseBytes(m)
			if err != nil {
				t.Fatal(err)
			}
			sameAnswer(t, m.Name, want, got)
		}
	}
}

// reusedAnswer is a recorded answer with a dozen header lines.
func reusedAnswer(body string) *models.Mock {
	header := map[string]string{"Content-Type": "application/json", "Content-Length": strconv.Itoa(len(body))}
	for i := range 10 {
		header["X-Header-"+strconv.Itoa(i)] = "value-" + strconv.Itoa(i)
	}
	m := &models.Mock{Kind: models.HTTP, Name: "reused", Spec: models.MockSpec{
		HTTPReq:  &models.HTTPReq{Method: "GET", ProtoMajor: 1, ProtoMinor: 1},
		HTTPResp: &models.HTTPResp{StatusCode: 200, Header: header, Body: body},
	}}
	m.MarkPooled()
	return m
}

// TestAnAnswerServedAgainIsNotBuiltAgain: an answer served 1,000 times is
// built twice (served once, then served again and kept) and written from the
// kept bytes after, with no allocation. On main every serve built it: 68
// allocations a serve for this answer.
func TestAnAnswerServedAgainIsNotBuiltAgain(t *testing.T) {
	h := newHTTP()
	m := reusedAnswer(`{"ok":true}`)
	before := keptAnswers.builds.Load()
	for range 1_000 {
		if _, err := h.buildMockResponseBytes(m); err != nil {
			t.Fatal(err)
		}
	}
	if n := keptAnswers.builds.Load() - before; n != 2 {
		t.Fatalf("1,000 serves built the answer %d times, want 2", n)
	}
	allocs := testing.AllocsPerRun(1_000, func() {
		if _, err := h.buildMockResponseBytes(m); err != nil {
			t.Fatal(err)
		}
	})
	if allocs != 0 {
		t.Fatalf("a serve of a kept answer allocates %v times, want 0", allocs)
	}
}

// TestAFirstServeAllocatesTheBufferAndAnEntry: an answer a test consumes once
// is built into one buffer of its size. Beyond the header map the wire header
// reads into (pkg.ToWireHTTPHeader), a serve allocates that buffer and the
// kept answers' entry: 25 allocations for this answer of 12 header lines, 12
// bytes or 64 KB. On main a serve allocated 68 times: a fmt.Sprintf and a
// longer string for each header line.
func TestAFirstServeAllocatesTheBufferAndAnEntry(t *testing.T) {
	h := newHTTP()
	const runs = 200
	for _, size := range []int{12, 64 << 10} {
		body := strings.Repeat("b", size)
		mocks := make([]*models.Mock, runs+1)
		for i := range mocks {
			mocks[i] = reusedAnswer(body)
		}
		wireHeader := testing.AllocsPerRun(runs, func() {
			pkg.ToWireHTTPHeader(mocks[0].Spec.HTTPResp.Header, nil)
		})
		next := 0
		allocs := testing.AllocsPerRun(runs, func() {
			if _, err := h.buildMockResponseBytes(mocks[next]); err != nil {
				t.Fatal(err)
			}
			next++
		})
		// The buffer (1), and the kept answers' entry (7: its weak handle,
		// the entry, the map's nodes and the cleanup with its call).
		if limit := wireHeader + 8; allocs > limit {
			t.Fatalf("%d B answer: a first serve allocates %v times, want at most %v (%v for the wire header)", size, allocs, limit, wireHeader)
		}
	}
}

// TestKeptAnswersGoWithTheirRecording: the kept bytes of an answer go when its
// recording is collected, and a table that is full keeps no more.
func TestKeptAnswersGoWithTheirRecording(t *testing.T) {
	h := newHTTP()
	table := newAnswerTable(1 << 20)
	func() {
		m := reusedAnswer(strings.Repeat("k", 1_000))
		for range 3 {
			if _, err := table.serve(m, h.encodeAnswer); err != nil {
				t.Fatal(err)
			}
		}
	}()
	if table.held.Load() == 0 {
		t.Fatal("an answer served three times is not kept")
	}
	// Cleanups run on their own goroutine after a collection; wait for it.
	for i := 0; table.held.Load() != 0 || entriesOf(table) != 0; i++ {
		if i == 10_000 {
			t.Fatalf("after %d collections the table still holds %d bytes in %d entries", i, table.held.Load(), entriesOf(table))
		}
		runtime.GC()
		time.Sleep(time.Millisecond)
	}

	small := newAnswerTable(100)
	m := reusedAnswer(strings.Repeat("s", 1_000))
	for range 5 {
		if _, err := small.serve(m, h.encodeAnswer); err != nil {
			t.Fatal(err)
		}
	}
	if small.held.Load() != 0 || small.builds.Load() != 5 {
		t.Fatalf("a full table holds %d bytes after %d builds, want 0 bytes after 5", small.held.Load(), small.builds.Load())
	}
	runtime.KeepAlive(m)
}

func entriesOf(t *answerTable) int {
	n := 0
	t.entries.Range(func(any, any) bool { n++; return true })
	return n
}
