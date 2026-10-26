package http

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

// mainSchemaMatch and mainMatchURLPath are SchemaMatch and MatchURLPath as
// main has them (f6e2d456d, comment lines left out): every check in its old
// order and a url.Parse of every mock's URL on every call. The differential
// rows below hold the new SchemaMatch to their answers.
func (h *HTTP) mainSchemaMatch(ctx context.Context, input *req, unfilteredMocks []*models.Mock, headerNoise map[string][]string, urlNoise []string, autoDynamic bool) ([]*models.Mock, error) {
	var schemaMatched []*models.Mock

	reqQuery := input.url.Query()

	proven := provenswitch.On()
	var live destination
	if proven {
		live = liveDestination(ctx, input)
	}

	for _, mock := range unfilteredMocks {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		inputCTValues := input.header.Values("Content-Type")
		mockCT := mock.Spec.HTTPReq.Header["Content-Type"]
		if len(inputCTValues) > 0 && mockCT != "" {
			mockMediaTypes := parseMediaTypes(mockCT)
			inputMediaTypes := parseMediaTypes(strings.Join(inputCTValues, ","))
			if len(mockMediaTypes) == 0 || len(inputMediaTypes) == 0 || !mediaTypesOverlap(mockMediaTypes, inputMediaTypes) {
				h.Logger.Debug("The content type of mock and request aren't the same",
					zap.String("mock name", mock.Name),
					zap.Strings("input media types", inputMediaTypes),
					zap.Strings("mock media types", mockMediaTypes))
				continue
			}
		}
		if !h.MatchBodyType(mock.Spec.HTTPReq.Body, input.body) {
			h.Logger.Debug("The body of mock and request aren't of same type", zap.String("mock name", mock.Name))
			continue
		}

		if !h.mainMatchURLPath(mock.Spec.HTTPReq.URL, input.url.Path, urlNoise, autoDynamic) {
			h.Logger.Debug("The url path of mock and request aren't the same", zap.String("mock name", mock.Name), zap.Any("input url", input.url.Path), zap.Any("mock url", mock.Spec.HTTPReq.URL))
			continue
		}

		if mock.Spec.HTTPReq.Method != models.Method(input.method) {
			h.Logger.Debug("The method of mock and request aren't the same", zap.String("mock name", mock.Name))
			continue
		}

		if proven && !sameDestination(live, recordedDestination(mock)) {
			h.Logger.Debug("the mock was recorded for another destination", zap.String("mock name", mock.Name))
			continue
		}

		if !h.HeadersContainKeys(mock.Spec.HTTPReq.Header, input.header, headerNoise) {
			h.Logger.Debug("headers missing required keys for mock name",
				zap.String("mock name", mock.Name),
				zap.Any("expected header keys", mock.Spec.HTTPReq.Header),
				zap.Any("input header", input.header))
			continue
		}

		if !h.QueryParamsMatch(mock.Spec.HTTPReq.URLParams, reqQuery, urlNoise, autoDynamic) {
			h.Logger.Debug("The query params of mock and request aren't the same", zap.String("mock name", mock.Name))
			continue
		}

		schemaMatched = append(schemaMatched, mock)
	}

	return schemaMatched, nil
}

func (h *HTTP) mainMatchURLPath(mockURL, reqPath string, urlNoise []string, autoDynamic bool) bool {
	parsedURL, err := url.Parse(mockURL)
	if err != nil {
		return false
	}
	h.Logger.Debug("parsed URL", zap.Any("parsed URL", parsedURL.Path), zap.Any("req path", reqPath))
	mockPath := parsedURL.Path
	if mockPath == reqPath {
		return true
	}
	if len(urlNoise) > 0 {
		noiseRes := compileURLNoise(h.Logger, urlNoise)
		if maskURLNoise(mockPath, noiseRes) == maskURLNoise(reqPath, noiseRes) {
			return true
		}
	}
	if autoDynamic && pathMatchesModuloDynamicSegments(mockPath, reqPath) {
		return true
	}
	return false
}

// schemaCall is one live request of a differential row.
type schemaCall struct {
	name  string
	input *req
}

// schemaLiveReq builds the req a decoded live request carries.
func schemaLiveReq(t testing.TB, method, rawURL string, header http.Header, body string) *req {
	t.Helper()
	u, err := url.ParseRequestURI(rawURL)
	if err != nil {
		t.Fatalf("live url %q: %v", rawURL, err)
	}
	if header == nil {
		header = http.Header{}
	}
	return &req{method: method, url: u, header: header, body: []byte(body)}
}

var schemaMethods = []string{"GET", "GET", "GET", "POST", "PUT", "DELETE"}

// schemaPathOf is the path of the i-th generated mock: mostly /item/<id>
// (the latency harness's calls), with a share of escaped, uuid, nested and
// trailing-slash paths, so the parsed form differs from the raw URL text.
func schemaPathOf(r *rand.Rand, i int) string {
	switch r.Intn(10) {
	case 0:
		return "/a%20b/" + strconv.Itoa(i)
	case 1:
		return fmt.Sprintf("/users/%08x-%04x-4%03x-8%03x-%012x", r.Uint32(), r.Intn(1<<16), r.Intn(1<<12), r.Intn(1<<12), r.Int63n(1<<48))
	case 2:
		return "/v1/orders/" + strconv.Itoa(i) + "/lines/"
	case 3:
		return "/item/" + strconv.Itoa(i%50) // shared by several mocks
	default:
		return "/item/" + strconv.Itoa(i)
	}
}

// schemaPool generates n HTTP mocks with the fields SchemaMatch reads drawn
// at random: method, URL (some that do not parse), query, content type, body,
// header keys and the recorded host.
func schemaPool(seed int64, n int) []*models.Mock {
	r := rand.New(rand.NewSource(seed))
	out := make([]*models.Mock, 0, n)
	for i := 0; i < n; i++ {
		host := "dep:8080"
		if r.Intn(8) == 0 {
			host = "other:9090"
		}
		p := schemaPathOf(r, i)
		params := map[string]string{}
		q := url.Values{}
		if r.Intn(3) > 0 {
			size := []string{"small", "large"}[r.Intn(2)]
			params["size"], q["size"] = size, []string{size}
		}
		if r.Intn(6) == 0 {
			v := strconv.Itoa(r.Intn(1000))
			params["page"], q["page"] = v, []string{v}
		}
		rawURL := "http://" + host + p
		if len(q) > 0 {
			rawURL += "?" + q.Encode()
		}
		switch r.Intn(40) {
		case 0:
			rawURL = "http://[::1" + p // does not parse
		case 1:
			rawURL = p // recorded without scheme and host
		case 2:
			rawURL += "#frag"
		}
		hdr := map[string]string{"Host": host, "Accept": "*/*"}
		body := ""
		method := schemaMethods[r.Intn(len(schemaMethods))]
		if method == "POST" || method == "PUT" {
			switch r.Intn(3) {
			case 0:
				hdr["Content-Type"], body = "application/json", `{"id":`+strconv.Itoa(i)+`}`
			case 1:
				hdr["Content-Type"], body = "text/plain; charset=utf-8", "item "+strconv.Itoa(i)
			}
		}
		if r.Intn(10) == 0 {
			hdr["X-Trace"] = "t"
		}
		out = append(out, &models.Mock{
			Kind: models.HTTP,
			Name: "mock-" + strconv.Itoa(i),
			Spec: models.MockSpec{HTTPReq: &models.HTTPReq{
				Method: models.Method(method), URL: rawURL, URLParams: params, Header: hdr, Body: body,
			}},
		})
	}
	return out
}

// schemaCalls are calls against pool: a mock's own request (and the same with
// another id, method, query, content type or host) and calls no mock made.
func schemaCalls(t testing.TB, seed int64, pool []*models.Mock, n int) []schemaCall {
	r := rand.New(rand.NewSource(seed))
	var out []schemaCall
	for len(out) < n {
		m := pool[r.Intn(len(pool))]
		u, err := url.Parse(m.Spec.HTTPReq.URL)
		if err != nil || u.Path == "" {
			continue
		}
		path, query := u.EscapedPath(), u.RawQuery
		method := string(m.Spec.HTTPReq.Method)
		host := m.Spec.HTTPReq.Header["Host"]
		hdr := http.Header{"Host": {host}, "Accept": {"*/*"}}
		if ct := m.Spec.HTTPReq.Header["Content-Type"]; ct != "" {
			hdr.Set("Content-Type", ct)
		}
		if _, ok := m.Spec.HTTPReq.Header["X-Trace"]; ok {
			hdr.Set("X-Trace", "t")
		}
		body := m.Spec.HTTPReq.Body
		how := "own"
		switch r.Intn(9) {
		case 1:
			how, path = "other id", strings.TrimSuffix(path, "/")+"9"
		case 2:
			how, method = "other method", schemaMethods[r.Intn(len(schemaMethods))]
		case 3:
			how, query = "other query", "size=medium"
		case 4:
			how = "other content type"
			hdr.Set("Content-Type", "application/xml")
		case 5:
			how, hdr["Host"] = "other host", []string{"other:9090"}
		case 6:
			how, path = "uuid id", fmt.Sprintf("/users/%08x-%04x-4%03x-8%03x-%012x", r.Uint32(), r.Intn(1<<16), r.Intn(1<<12), r.Intn(1<<12), r.Int63n(1<<48))
		case 7:
			how, path = "no mock", "/nowhere/"+strconv.Itoa(r.Intn(100))
		}
		raw := path
		if query != "" {
			raw += "?" + query
		}
		out = append(out, schemaCall{name: how + " " + method + " " + raw, input: schemaLiveReq(t, method, raw, hdr, body)})
	}
	return out
}

// sameSchemaMatch fails t unless the new SchemaMatch and main's pick the same
// mocks, in the same order, for each call, under each of: no url noise and
// url noise, auto-dynamic matching off and on, the proven switch off and on.
func sameSchemaMatch(t *testing.T, label string, pool []*models.Mock, calls []schemaCall) (matched int) {
	t.Helper()
	h := newHTTP()
	ctx := context.Background()
	headerNoise := map[string][]string{"x-trace": {}}
	for _, proven := range []bool{false, true} {
		if proven {
			t.Setenv(provenswitch.Env, "1")
		} else {
			t.Setenv(provenswitch.Env, "0")
		}
		for _, urlNoise := range [][]string{nil, {"/item/[0-9]+", "/a%20b"}} {
			for _, auto := range []bool{false, true} {
				for _, c := range calls {
					want, werr := h.mainSchemaMatch(ctx, c.input, pool, headerNoise, urlNoise, auto)
					got, gerr := h.SchemaMatch(ctx, c.input, pool, headerNoise, urlNoise, auto)
					if (werr == nil) != (gerr == nil) {
						t.Fatalf("%s, %s (proven %v, url noise %v, auto %v): error %v, main %v", label, c.name, proven, urlNoise, auto, gerr, werr)
					}
					if len(got) != len(want) {
						t.Fatalf("%s, %s (proven %v, url noise %v, auto %v): %d mocks, main %d: %v vs %v", label, c.name, proven, urlNoise, auto, len(got), len(want), mockNamesOf(got), mockNamesOf(want))
					}
					for i := range want {
						if got[i] != want[i] {
							t.Fatalf("%s, %s (proven %v, url noise %v, auto %v): mock %d is %s, main %s", label, c.name, proven, urlNoise, auto, i, got[i].Name, want[i].Name)
						}
					}
					if len(want) > 0 {
						matched++
					}
				}
			}
		}
	}
	return matched
}

// TestSchemaMatchPicksWhatMainPicks is the differential row: on generated
// pools of 100, 1k and 10k mocks, SchemaMatch (method and path first, each
// recorded path parsed once) picks the mocks main's picks, in its order.
func TestSchemaMatchPicksWhatMainPicks(t *testing.T) {
	for _, n := range []int{100, 1000, 10000} {
		calls := map[int]int{100: 200, 1000: 48, 10000: 6}[n]
		pool := schemaPool(int64(n), n)
		cs := schemaCalls(t, int64(n)+1, pool, calls)
		matched := sameSchemaMatch(t, fmt.Sprintf("pool %d", n), pool, cs)
		if matched == 0 {
			t.Fatalf("pool %d: no call matched a mock, the row compares nothing", n)
		}
		t.Logf("pool %d: %d calls x 8 settings equal, %d with mocks picked", n, len(cs), matched)
	}
}

// TestSchemaMatchPicksWhatMainPicksOnRecordedSets is the same comparison over
// recorded sets, when KEPLOY_HTTP_RECORDINGS names a directory of them (the
// 100/1k/10k sets of the replay latency harness, on the test boxes): every
// recorded call is matched against its set's HTTP mocks.
func TestSchemaMatchPicksWhatMainPicksOnRecordedSets(t *testing.T) {
	dir := os.Getenv("KEPLOY_HTTP_RECORDINGS")
	if dir == "" {
		t.Skip("KEPLOY_HTTP_RECORDINGS names no recorded sets")
	}
	sets := recordedHTTPSets(t, dir)
	if len(sets) == 0 {
		t.Fatalf("no HTTP mocks under %s", dir)
	}
	for path, pool := range sets {
		var calls []schemaCall
		for _, m := range pool {
			u, err := url.Parse(m.Spec.HTTPReq.URL)
			if err != nil {
				continue
			}
			hdr := http.Header{}
			for k, v := range m.Spec.HTTPReq.Header {
				hdr.Set(k, v)
			}
			calls = append(calls, schemaCall{name: m.Name, input: schemaLiveReq(t, string(m.Spec.HTTPReq.Method), u.RequestURI(), hdr, m.Spec.HTTPReq.Body)})
		}
		matched := sameSchemaMatch(t, path, pool, calls)
		t.Logf("%s: %d mocks, %d calls x 8 settings equal, %d with mocks picked", path, len(pool), len(calls), matched)
	}
}

// recordedHTTPSets reads the HTTP mocks of each mocks.yaml under dir.
func recordedHTTPSets(t *testing.T, dir string) map[string][]*models.Mock {
	t.Helper()
	out := map[string][]*models.Mock{}
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, "mocks.yaml") {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		dec := yamlLib.NewDecoder(f)
		var docs []*yaml.NetworkTrafficDoc
		for {
			var d yaml.NetworkTrafficDoc
			if err := dec.Decode(&d); err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				return fmt.Errorf("%s: %w", path, err)
			}
			docs = append(docs, &d)
		}
		ms, err := mockdb.DecodeMocks(docs, zap.NewNop())
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if hs := FilterHTTPMocks(ms); len(hs) > 0 {
			out[path] = hs
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// resetRecordedPaths empties the parsed-path memo and zeroes its counter.
func resetRecordedPaths() {
	recordedPaths.m.Clear()
	recordedPaths.n.Store(0)
	recordedPathParses.Store(0)
}

// TestSchemaMatchParsesEachRecordedPathOnce is the count row for the memo:
// a run of calls over one pool parses each distinct recorded URL once, in
// both passes, not once per mock per call.
func TestSchemaMatchParsesEachRecordedPathOnce(t *testing.T) {
	t.Setenv(provenswitch.Env, "0")
	pool := schemaPool(7, 1000)
	distinct := map[string]bool{}
	for _, m := range pool {
		distinct[m.Spec.HTTPReq.URL] = true
	}
	calls := schemaCalls(t, 8, pool, 50)
	resetRecordedPaths()
	t.Cleanup(resetRecordedPaths)
	h := newHTTP()
	for _, auto := range []bool{false, true} {
		for _, c := range calls {
			if _, err := h.SchemaMatch(context.Background(), c.input, pool, nil, nil, auto); err != nil {
				t.Fatal(err)
			}
		}
	}
	if got := recordedPathParses.Load(); got != int64(len(distinct)) {
		t.Fatalf("%d calls over %d mocks (%d distinct URLs) parsed %d recorded URLs; want each once", 2*len(calls), len(pool), len(distinct), got)
	}
}

// TestRecordedPathsStayBounded: the memo empties itself at its cap and still
// answers what url.Parse answers.
func TestRecordedPathsStayBounded(t *testing.T) {
	resetRecordedPaths()
	t.Cleanup(resetRecordedPaths)
	for i := 0; i < recordedPathsCap+10; i++ {
		u := "http://dep/item/" + strconv.Itoa(i)
		p, ok := recordedURLPath(u)
		if !ok || p != "/item/"+strconv.Itoa(i) {
			t.Fatalf("%s: path %q ok %v", u, p, ok)
		}
	}
	if n := recordedPaths.n.Load(); n > recordedPathsCap {
		t.Fatalf("%d entries kept, cap %d", n, recordedPathsCap)
	}
	if _, ok := recordedURLPath("http://[::1/x"); ok {
		t.Fatal("a URL url.Parse refuses has a path")
	}
}

// TestSchemaMatchAllocationsDoNotGrowWithThePool is the cost row (fails on
// main, which parsed every mock's URL, and read every mock's content type and
// body, on every call): a call over 2,000 mocks of which one has its method
// and path allocates what a call over 20 does, for GET calls and for POST
// calls with a JSON body.
func TestSchemaMatchAllocationsDoNotGrowWithThePool(t *testing.T) {
	if raceOn {
		t.Skip("allocation counts vary under -race")
	}
	t.Setenv(provenswitch.Env, "0")
	h := newHTTP()
	for _, method := range []string{"GET", "POST"} {
		mk := func(n int) []*models.Mock {
			out := make([]*models.Mock, n)
			for i := range out {
				r := &models.HTTPReq{
					Method: models.Method(method), URL: "http://dep:8080/item/" + strconv.Itoa(i) + "?size=small",
					URLParams: map[string]string{"size": "small"}, Header: map[string]string{"Host": "dep:8080"},
				}
				if method == "POST" {
					r.Header["Content-Type"], r.Body = "application/json; charset=utf-8", `{"id":`+strconv.Itoa(i)+`,"pad":"`+strings.Repeat("p", 64)+`"}`
				}
				out[i] = &models.Mock{Kind: models.HTTP, Name: "m" + strconv.Itoa(i), Spec: models.MockSpec{HTTPReq: r}}
			}
			return out
		}
		hdr := http.Header{"Host": {"dep:8080"}}
		body := ""
		if method == "POST" {
			hdr.Set("Content-Type", "application/json")
			body = `{"id":7}`
		}
		in := schemaLiveReq(t, method, "/item/7?size=small", hdr, body)
		allocs := func(pool []*models.Mock) float64 {
			return testing.AllocsPerRun(20, func() {
				got, err := h.SchemaMatch(context.Background(), in, pool, nil, nil, false)
				if err != nil || len(got) != 1 {
					t.Fatalf("%s: matched %d, err %v", method, len(got), err)
				}
			})
		}
		small, large := allocs(mk(20)), allocs(mk(2000))
		if large > small+2 {
			t.Fatalf("%s: a call allocates %.0f times over 2,000 mocks and %.0f over 20: the cost grows with the pool", method, large, small)
		}
		t.Logf("%s: %.0f allocations per call over 20 mocks, %.0f over 2,000", method, small, large)
	}
}
