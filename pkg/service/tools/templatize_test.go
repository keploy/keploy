package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	httpMatcher "go.keploy.io/server/v3/pkg/matcher/http"
	"go.keploy.io/server/v3/pkg/models"
	setfile "go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/platform/yaml/testdb"
	"go.keploy.io/server/v3/pkg/templates"
	"go.keploy.io/server/v3/utils/logcontent"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// stagedWrites are the files the in-memory stores below were asked for and
// have not put in place, by path: what ReplaceFiles puts in place together.
var stagedWrites sync.Map

// placementTestDB keeps what templatize wrote for each test case. files are
// the set's test case files as testdb stores them (caseFiles), which
// templatize reads to tell a plain recording from a set that holds
// placeholders; a test of a plain recording may leave them out.
type placementTestDB struct {
	written map[string]models.TestCase
	files   []setfile.File
}

func (f *placementTestDB) GetAllTestSetIDs(context.Context) ([]string, error) { return nil, nil }
func (f *placementTestDB) GetTestCases(context.Context, string) ([]*models.TestCase, error) {
	return nil, nil
}
func (f *placementTestDB) UpdateTestCase(_ context.Context, tc *models.TestCase, _ string, _ bool) error {
	f.written[tc.Name] = *tc
	return nil
}
func (f *placementTestDB) DeleteTests(context.Context, string, []string) error { return nil }
func (f *placementTestDB) DeleteTestSet(context.Context, string) error         { return nil }
func (f *placementTestDB) TestCaseFiles(context.Context, string) ([]setfile.File, error) {
	return f.files, nil
}
func (f *placementTestDB) TestCaseFile(_ context.Context, tc *models.TestCase, _ string) (setfile.File, error) {
	cp := *tc
	path := fmt.Sprintf("%p/tests/%s.yaml", f, tc.Name)
	stagedWrites.Store(path, func() { f.written[cp.Name] = cp })
	return setfile.File{Path: path}, nil
}

// ReplaceFiles puts every staged file in place: a test case into written,
// config.yaml into its store.
func (f *placementTestDB) ReplaceFiles(_ context.Context, files []setfile.File) error {
	for _, file := range files {
		if put, ok := stagedWrites.LoadAndDelete(file.Path); ok {
			put.(func())()
		}
	}
	return nil
}

// caseFiles are cases as testdb stores them, one YAML file each.
func caseFiles(t *testing.T, cases []*models.TestCase) []setfile.File {
	t.Helper()
	db := testdb.New(zap.NewNop(), "keploy")
	var out []setfile.File
	for _, tc := range cases {
		// Encoding a test case adds the noise it finds to its map: a copy's.
		cp := *tc
		cp.Noise = map[string][]string{}
		for k, v := range tc.Noise {
			cp.Noise[k] = v
		}
		f, err := db.TestCaseFile(context.Background(), &cp, "test-set-0")
		require.NoError(t, err)
		out = append(out, f)
	}
	return out
}

// placementConfStore is config.yaml: what it holds, and whether templatize
// wrote it.
type placementConfStore struct {
	stored  models.TestSet
	written *models.TestSet
}

func (f *placementConfStore) Read(context.Context, string) (*models.TestSet, error) {
	cp := f.stored
	cp.Template = copyTemplate(f.stored.Template)
	return &cp, nil
}
func (f *placementConfStore) ReadForUpdate(ctx context.Context, id string) (*models.TestSet, error) {
	return f.Read(ctx, id)
}
func (f *placementConfStore) Write(_ context.Context, _ string, ts *models.TestSet) error {
	cp := *ts
	cp.Template = copyTemplate(ts.Template)
	f.written = &cp
	return nil
}
func (f *placementConfStore) ReadSecret(context.Context, string) (map[string]interface{}, error) {
	return nil, nil
}
func (f *placementConfStore) ConfigFile(_ context.Context, _ string, ts *models.TestSet) (setfile.File, error) {
	cp := *ts
	cp.Template = copyTemplate(ts.Template)
	path := fmt.Sprintf("%p/config.yaml", f)
	stagedWrites.Store(path, func() { f.written = &cp })
	return setfile.File{Path: path}, nil
}

func copyTemplate(in map[string]interface{}) map[string]interface{} {
	if in == nil {
		return nil
	}
	out := make(map[string]interface{}, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// runTemplatize runs keploy templatize over fresh copies of the cases, with
// config.yaml holding template, and returns what it wrote.
func runTemplatize(t *testing.T, cases []*models.TestCase, template map[string]interface{}, cfg *config.Config) (map[string]models.TestCase, *models.TestSet) {
	t.Helper()
	db := &placementTestDB{written: map[string]models.TestCase{}, files: caseFiles(t, cases)}
	conf := &placementConfStore{stored: models.TestSet{Template: copyTemplate(template)}}
	if cfg == nil {
		cfg = &config.Config{}
	}
	tools := &Tools{logger: zap.NewNop(), testDB: db, testSetConf: conf, config: cfg}
	require.NoError(t, tools.ProcessTestCasesV2(context.Background(), cases, "test-set-0"))
	return db.written, conf.written
}

// writtenFiles is the part of a templatize run a reader sees in the files, in a
// fixed order.
func writtenFiles(written map[string]models.TestCase, conf *models.TestSet) string {
	names := make([]string, 0, len(written))
	for n := range written {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		tc := written[n]
		reqH, _ := json.Marshal(tc.HTTPReq.Header)
		respH, _ := json.Marshal(tc.HTTPResp.Header)
		req := strings.TrimRight(fmt.Sprintf("%s %s %s %s", tc.HTTPReq.Method, tc.HTTPReq.URL, reqH, tc.HTTPReq.Body), " ")
		fmt.Fprintf(&b, "%s\n  req:  %s\n  resp: %s %s\n", n, req, respH, tc.HTTPResp.Body)
	}
	var tmpl map[string]interface{}
	if conf != nil {
		tmpl = conf.Template
	}
	tj, _ := json.Marshal(tmpl)
	fmt.Fprintf(&b, "config.yaml template: %s\n", tj)
	return b.String()
}

func tcase(name, method, url string, header map[string]string, body string, status int, respHeader map[string]string, respBody string) *models.TestCase {
	if header == nil {
		header = map[string]string{}
	}
	if respHeader == nil {
		respHeader = map[string]string{}
	}
	return &models.TestCase{
		Name: name, Kind: models.HTTP,
		HTTPReq:  models.HTTPReq{Method: models.Method(method), URL: url, Header: header, Body: body},
		HTTPResp: models.HTTPResp{StatusCode: status, Header: respHeader, Body: respBody},
	}
}

// createAndRead is a user create, an order create that names the user, and
// a read of the order by both ids: two values that flow, one of them held by
// two fields of one response.
func createAndRead() []*models.TestCase {
	jsonType := map[string]string{"Content-Type": "application/json"}
	return []*models.TestCase{
		tcase("test-1", "POST", "http://localhost:8080/users", jsonType, `{"name":"alice"}`,
			201, jsonType, `{"id":"usr_01J9REC","stored_id":"usr_01J9REC","name":"alice"}`),
		tcase("test-2", "POST", "http://localhost:8080/orders", jsonType, `{"user":"usr_01J9REC","qty":2}`,
			201, jsonType, `{"id":"ord_77","user":"usr_01J9REC","qty":2}`),
		tcase("test-3", "GET", "http://localhost:8080/users/usr_01J9REC/orders/ord_77?view=full", nil, "",
			200, jsonType, `{"id":"ord_77","user":"usr_01J9REC","qty":2}`),
	}
}

// Where templatize puts each placeholder: a capture where the value first
// appears, in a response; a use at every later request that sends it, and at
// a later response only where that test case's own request sent the key (a
// read-back answers what it was asked for). Nothing before the producer, and
// nothing where a response merely holds an equal value: stored_id beside the
// producer stays written out.
func TestProcessTestCasesV2_CapturesAtTheProducerAndUsesWhereTheKeyIsSent(t *testing.T) {
	written, conf := runTemplatize(t, createAndRead(), nil, nil)
	assert.Equal(t, `test-1
  req:  POST http://localhost:8080/users {"Content-Type":"application/json"} {"name":"alice"}
  resp: {"Content-Type":"application/json"} {"id":"{{capture string .id}}","stored_id":"usr_01J9REC","name":"alice"}
test-2
  req:  POST http://localhost:8080/orders {"Content-Type":"application/json"} {"user":"{{string .id}}","qty":2}
  resp: {"Content-Type":"application/json"} {"id":"{{capture string .id1}}","user":"{{string .id}}","qty":2}
test-3
  req:  GET http://localhost:8080/users/{{string .id}}/orders/{{string .id1}}?view=full {}
  resp: {"Content-Type":"application/json"} {"id":"{{string .id1}}","user":"{{string .id}}","qty":2}
config.yaml template: {"id":"usr_01J9REC","id1":"ord_77"}
`, writtenFiles(written, conf))
}

// SAME INPUT, SAME FILES (the bug: which of {"id":V,"stored_id":V} produced V,
// and so the key's name, followed Go map order). Places are read in a fixed
// order, so 200 runs write one result.
func TestProcessTestCasesV2_SameRecordingWritesTheSameFiles(t *testing.T) {
	const runs = 200
	outputs := map[string]int{}
	for i := 0; i < runs; i++ {
		written, conf := runTemplatize(t, createAndRead(), nil, nil)
		outputs[writtenFiles(written, conf)]++
	}
	if !assert.Len(t, outputs, 1, "%d templatize runs over one recording wrote %d different results", runs, len(outputs)) {
		for out, n := range outputs {
			t.Logf("written by %d of %d runs:\n%s", n, runs, out)
		}
	}
}

// A test case with nothing to template is not rewritten, so it stays byte
// for byte as recorded.
func TestProcessTestCasesV2_LeavesACaseWithNothingToTemplateUnwritten(t *testing.T) {
	cases := append(createAndRead(), tcase("test-4", "GET", "http://localhost:8080/health", nil, "", 200, nil, `{"ok":true}`))
	written, _ := runTemplatize(t, cases, nil, nil)
	assert.Contains(t, written, "test-3")
	assert.NotContains(t, written, "test-4")
}

// Places are structural only: a whole JSON value, a URL path segment or query
// value, a cookie, a Bearer token, a whole request header, a form value, and
// a response's Set-Cookie cookies and Location. A value inside free text is
// not touched. A JSON number goes only where a JSON number goes: never into a
// path, a query, a header or a form, as main's templatize never wrote it.
func TestProcessTestCasesV2_PlacesOnlyAtStructuralPositions(t *testing.T) {
	cases := []*models.TestCase{
		tcase("test-1", "POST", "http://h/login", map[string]string{"Accept": "application/json"}, `{"user":"alice"}`,
			200, map[string]string{"Set-Cookie": "sid=S0rec00001; Path=/; HttpOnly", "Content-Type": "application/json"},
			`{"token":"T0rec00001","order_id":7654321,"msg":"welcome T0rec00001"}`),
		tcase("test-2", "GET", "http://h/orders/7654321?token=T0rec00001",
			map[string]string{"Authorization": "Bearer T0rec00001", "Cookie": "theme=dark; sid=S0rec00001", "Content-Type": "application/json", "X-Note": "for T0rec00001"},
			"", 200, map[string]string{"Content-Type": "application/json"}, `{"order_id":7654321,"note":"T0rec00001 ok"}`),
		tcase("test-3", "POST", "http://h/pay", map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, "order=7654321&note=T0rec00001",
			200, nil, ""),
		tcase("test-4", "POST", "http://h/refund", map[string]string{"Content-Type": "application/json"}, `{"order_id":7654321}`,
			200, nil, `{"order_id":7654321,"refunded":true}`),
	}
	written, conf := runTemplatize(t, cases, nil, nil)
	t1, t2, t3, t4 := written["test-1"], written["test-2"], written["test-3"], written["test-4"]

	assert.Equal(t, `{"token":"{{capture string .token}}","order_id":"{{capture int .orderId}}","msg":"welcome T0rec00001"}`, t1.HTTPResp.Body)
	assert.Equal(t, "sid={{capture string .sid}}; Path=/; HttpOnly", t1.HTTPResp.Header["Set-Cookie"])
	assert.Equal(t, "application/json", t1.HTTPResp.Header["Content-Type"])

	assert.Equal(t, "http://h/orders/7654321?token={{string .token}}", t2.HTTPReq.URL, "a JSON number never flows into a path")
	assert.Equal(t, "Bearer {{string .token}}", t2.HTTPReq.Header["Authorization"])
	assert.Equal(t, "theme=dark; sid={{string .sid}}", t2.HTTPReq.Header["Cookie"])
	assert.Equal(t, "application/json", t2.HTTPReq.Header["Content-Type"], "a constant a response header holds is no template")
	assert.Equal(t, "for T0rec00001", t2.HTTPReq.Header["X-Note"], "a value inside free text is left as written")
	assert.Equal(t, `{"order_id":7654321,"note":"T0rec00001 ok"}`, t2.HTTPResp.Body, "test-2's request did not send orderId, so its answer's equal number stays written out")

	assert.Equal(t, "order=7654321&note={{string .token}}", t3.HTTPReq.Body, "a JSON number never flows into a form")
	assert.Equal(t, `{"order_id":"{{int .orderId}}"}`, t4.HTTPReq.Body, "a JSON number flows into a JSON number")
	assert.Equal(t, `{"order_id":"{{int .orderId}}","refunded":true}`, t4.HTTPResp.Body, "test-4 sent orderId, so its answer is asserted to hold it")
	assert.Equal(t, map[string]interface{}{"token": "T0rec00001", "orderId": "7654321", "sid": "S0rec00001"}, conf.Template)
}

// RULE 7 for response headers: only Set-Cookie and Location produce. A
// header whose value first appears in a response (X-Trace) and that a later
// request repeats is not a producer, so it is never made an unasserted
// capture.
func TestProcessTestCasesV2_AResponseHeaderIsNotAProducer(t *testing.T) {
	cases := []*models.TestCase{
		tcase("test-1", "POST", "http://h/login", nil, `{}`, 200, map[string]string{"X-Trace": "trc_0123456789", "Content-Type": "application/json"}, `{}`),
		tcase("test-2", "GET", "http://h/me", map[string]string{"X-Trace": "trc_0123456789", "Content-Type": "application/json"}, "", 200, nil, `{}`),
	}
	written, conf := runTemplatize(t, cases, nil, nil)
	assert.Empty(t, written, "%s", writtenFiles(written, conf))
}

// A header the client computes (Content-Length, Host, Accept-Encoding...) is
// never a template, and a JSON number reaches no URL: a page count of 17 that
// equals a later Content-Length, or a total of 2 that equals a later path
// segment, is a coincidence of value.
func TestProcessTestCasesV2_ComputedHeadersAndNumbersInURLsAreNotTemplated(t *testing.T) {
	cases := []*models.TestCase{
		tcase("test-1", "GET", "http://h/stats", nil, "", 200, nil, `{"page":1,"total":2,"pageSize":17,"enc":"gzip","host":"api.local:8080"}`),
		tcase("test-2", "POST", "http://h/items", map[string]string{"Content-Type": "application/json", "Content-Length": "17", "Accept-Encoding": "gzip", "Host": "api.local:8080"}, `{"name":"bananas"}`, 201, nil, `{"ok":true}`),
		tcase("test-3", "GET", "http://h/items/2?page=1", nil, "", 200, nil, `{"ok":true}`),
	}
	written, conf := runTemplatize(t, cases, nil, nil)
	assert.Empty(t, written, "%s", writtenFiles(written, conf))
}

// A use goes into a later response only where that test case's request sent
// the key: an answer that merely holds an equal value (qty 20 = page_size 20)
// stays written out and compared as recorded.
func TestProcessTestCasesV2_AResponseGetsAUseOnlyWhereItsRequestSentTheKey(t *testing.T) {
	jsonType := map[string]string{"Content-Type": "application/json"}
	cases := []*models.TestCase{
		tcase("test-1", "GET", "http://localhost:8080/config", nil, "", 200, jsonType, `{"currency":"USD","page_size":20,"region":"eu-west-1"}`),
		tcase("test-2", "GET", "http://localhost:8080/prices?currency=USD&limit=20", nil, "", 200, jsonType, `{"currency":"USD","count":20}`),
		tcase("test-3", "GET", "http://localhost:8080/stock?region=eu-west-1", nil, "", 200, jsonType, `{"region":"eu-west-1","qty":20,"currency":"USD"}`),
	}
	written, conf := runTemplatize(t, cases, nil, nil)
	assert.Equal(t, `test-1
  req:  GET http://localhost:8080/config {}
  resp: {"Content-Type":"application/json"} {"currency":"{{capture string .currency}}","page_size":20,"region":"{{capture string .region}}"}
test-2
  req:  GET http://localhost:8080/prices?currency={{string .currency}}&limit=20 {}
  resp: {"Content-Type":"application/json"} {"currency":"{{string .currency}}","count":20}
test-3
  req:  GET http://localhost:8080/stock?region={{string .region}} {}
  resp: {"Content-Type":"application/json"} {"region":"{{string .region}}","qty":20,"currency":"USD"}
config.yaml template: {"currency":"USD","region":"eu-west-1"}
`, writtenFiles(written, conf))
}

// SET-COOKIE PER COOKIE. keploy joins a login's Set-Cookie lines into one
// value. Each cookie a later request sends gets its own capture, written over
// its value only: its Expires date is no part of it.
func TestProcessTestCasesV2_EachCookieALaterRequestSendsIsCaptured(t *testing.T) {
	cases := []*models.TestCase{
		tcase("test-1", "POST", "http://localhost:8080/login", nil, `{"u":"a"}`, 200,
			map[string]string{"Set-Cookie": "csrftoken=C0rec1234567; Path=/; Expires=Thu, 23 Oct 2026 10:00:00 GMT,sessionid=S0rec1234567; Path=/; HttpOnly,theme=dark; Path=/"}, `{"ok":true}`),
		tcase("test-2", "POST", "http://localhost:8080/transfer", map[string]string{"Cookie": "csrftoken=C0rec1234567; sessionid=S0rec1234567", "X-CSRFToken": "C0rec1234567"}, `{"amt":1}`, 200, nil, `{"ok":true}`),
	}
	written, conf := runTemplatize(t, cases, nil, nil)
	assert.Equal(t, "csrftoken={{capture string .csrftoken}}; Path=/; Expires=Thu, 23 Oct 2026 10:00:00 GMT,sessionid={{capture string .sessionid}}; Path=/; HttpOnly,theme=dark; Path=/", written["test-1"].HTTPResp.Header["Set-Cookie"])
	assert.Equal(t, "csrftoken={{string .csrftoken}}; sessionid={{string .sessionid}}", written["test-2"].HTTPReq.Header["Cookie"])
	assert.Equal(t, "{{string .csrftoken}}", written["test-2"].HTTPReq.Header["X-CSRFToken"])
	assert.Equal(t, map[string]interface{}{"csrftoken": "C0rec1234567", "sessionid": "S0rec1234567"}, conf.Template)
}

// TWO RUNS WRITE THE SAME FILES, whatever the set's shape. Run on its own
// output, a set that now holds placeholders, templatize writes nothing and
// says so in one line. Among the shapes: two cookies, the second echoed by a
// later response and sent in a later path.
func TestProcessTestCasesV2_TemplatizeOnItsOwnOutputWritesNothing(t *testing.T) {
	jsonType := map[string]string{"Content-Type": "application/json"}
	shapes := map[string][]*models.TestCase{
		"create and read": createAndRead(),
		"two cookies and a body echo": {
			tcase("test-1", "POST", "http://h/login", nil, `{"u":"a"}`, 200,
				map[string]string{"Set-Cookie": "XSRF-TOKEN=xsrfREC0001; Path=/,app_session=sessREC0001; Path=/; Expires=Thu, 23 Oct 2026 10:00:00 GMT; HttpOnly"}, `{"ok":true}`),
			tcase("test-2", "GET", "http://h/me", map[string]string{"Cookie": "XSRF-TOKEN=xsrfREC0001"}, "", 200, nil, `{"session":"sessREC0001"}`),
			tcase("test-3", "GET", "http://h/sessions/sessREC0001", nil, "", 200, nil, `{"ok":true}`),
		},
		// The first run captures 7654321 as a string; a later answer holds
		// it as a JSON number, and a later request sends that number, which
		// no string flows into.
		"one value, two types": {
			tcase("test-1", "POST", "http://h/codes", jsonType, `{"n":1}`, 201, nil, `{"code":"7654321"}`),
			tcase("test-2", "GET", "http://h/items/7654321", nil, "", 200, nil, `{"ok":true}`),
			tcase("test-3", "GET", "http://h/stats", nil, "", 200, nil, `{"n":7654321}`),
			tcase("test-4", "POST", "http://h/check", jsonType, `{"n":7654321}`, 200, nil, `{"ok":true}`),
		},
		"numbers, forms and an echo": {
			tcase("test-1", "POST", "http://h/orders", jsonType, `{"item":"book"}`, 201, map[string]string{"Location": "/orders/ord_9"}, `{"id":"ord_9","no":7654321}`),
			tcase("test-2", "POST", "http://h/pay", jsonType, `{"no":7654321,"id":"ord_9"}`, 200, nil, `{"no":7654321,"id":"ord_9","paid":true}`),
			tcase("test-3", "POST", "http://h/note", map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, "id=ord_9&no=7654321", 200, nil, `{"no":7654321}`),
		},
	}
	for name, cases := range shapes {
		t.Run(name, func(t *testing.T) {
			first, conf := runTemplatize(t, cases, nil, nil)
			require.NotEmpty(t, first, "precondition: the first run templatizes")
			all := make([]*models.TestCase, 0, len(cases))
			for _, tc := range cases {
				if w, ok := first[tc.Name]; ok {
					tc = &w
				}
				all = append(all, tc)
			}
			written, wroteConf, logs := templatizeAll(t, all, conf.Template, nil)
			assert.Empty(t, written, "a second run rewrote test cases:\n%s", writtenFiles(written, wroteConf))
			assert.Nil(t, wroteConf, "a second run rewrote config.yaml")
			assertLeftAsItIs(t, logs, nil, nil)
		})
	}
}

// templatizeAll runs keploy templatize over cases with config.yaml holding
// template and keploy.yml's settings cfg (nil for none), and returns the test
// cases it wrote, the config.yaml it wrote (nil for none) and every line it
// logged.
func templatizeAll(t *testing.T, cases []*models.TestCase, template map[string]interface{}, cfg *config.Config) (map[string]models.TestCase, *models.TestSet, *observer.ObservedLogs) {
	t.Helper()
	if cfg == nil {
		cfg = &config.Config{}
	}
	core, logs := observer.New(zap.InfoLevel)
	db := &placementTestDB{written: map[string]models.TestCase{}, files: caseFiles(t, cases)}
	conf := &placementConfStore{stored: models.TestSet{Template: copyTemplate(template)}}
	tools := &Tools{logger: zap.New(core), testDB: db, testSetConf: conf, config: cfg}
	require.NoError(t, tools.ProcessTestCasesV2(context.Background(), cases, "test-set-0"))
	return db.written, conf.written, logs
}

// heldStep is the next step templatize gives for a set that holds
// placeholders already.
const heldStep = `if this keploy templatize wrote them, nothing to do. Otherwise (a set an older keploy templatized, or one a person edited), re-record the set and run keploy templatize on the new recording; or write back, in place of each placeholder, what the recording held there, and run keploy templatize again: config.yaml's template has each key's recorded value, a placeholder of type int or float that stands in quotes as a whole JSON value ("{{int .n}}", "{{capture float .n}}") goes back as the bare number, without the quotes, a value inside a JSON string goes back with JSON's escapes (a " as \", a \ as \\), and an expression goes back as the text it rendered. If the text is the app's own (a mail template, a chart the API serves), there is nothing to write back: templatize cannot tell it from a placeholder, and leaves this set as it is`

// heldAt is where a set's first placeholder is: its text as the line names
// it, and either the test case file it is in with the texts its line holds
// (line, each part between "…"), or (where) a place that is no file's line,
// a key of config.yaml's template.
type heldAt struct {
	text, file, line string
	where            string
}

var (
	heldLineRe  = regexp.MustCompile(`^test-set-0 holds template placeholders already \((.*) at (.*)\): keploy templatize writes only into a plain recording, so it changes nothing in this set$`)
	heldWhereRe = regexp.MustCompile(`^(\S+) line (\d+)$`)
)

// assertLeftAsItIs checks that templatize said the one line for a set that
// holds placeholders already, and nothing else: the set, its first
// placeholder and where it is (want; nil for any), and the next step. A line
// number is checked against files, the set's files: that line holds the text.
func assertLeftAsItIs(t *testing.T, logs *observer.ObservedLogs, files []setfile.File, want *heldAt) {
	t.Helper()
	all := logs.All()
	require.Len(t, all, 1, "one line, and nothing else: %v", all)
	assert.Equal(t, zap.WarnLevel, all[0].Level)
	assert.Equal(t, heldStep, all[0].ContextMap()["next_step"])
	m := heldLineRe.FindStringSubmatch(all[0].Message)
	require.NotNil(t, m, all[0].Message)
	if want == nil {
		return
	}
	// Template text outside the grammar is named by its shape (templates.Shape).
	assert.Equal(t, templates.Shape(want.text), m[1], "the placeholder the line names")
	if want.where != "" {
		assert.Equal(t, want.where, m[2])
		return
	}
	at := heldWhereRe.FindStringSubmatch(m[2])
	require.NotNil(t, at, "a file and a line: %q", m[2])
	assert.Equal(t, want.file, at[1], "the file the line names")
	n, err := strconv.Atoi(at[2])
	require.NoError(t, err)
	for _, f := range files {
		if !strings.HasSuffix(f.Path, "/"+at[1]) {
			continue
		}
		lines := strings.Split(string(f.Data), "\n")
		require.True(t, n >= 1 && n <= len(lines), "line %d of %d", n, len(lines))
		for _, part := range strings.Split(want.line, "…") {
			assert.Contains(t, lines[n-1], part, "line %d of %s", n, at[1])
		}
		return
	}
	t.Errorf("the set has no file %s", at[1])
}

// A SET THAT HOLDS PLACEHOLDERS ALREADY IS LEFT AS IT IS. templatize writes
// only into a plain recording. A set that holds any template text anywhere in
// a test case file or in config.yaml's template (a placeholder an older
// keploy templatize wrote, a person wrote, or templatize itself wrote on an
// earlier run; of a key config.yaml defines or not; text outside the grammar
// such as {{.a.b}}; any expression, a pipe included, whether an earlier keploy
// rendered it or not; at a place keploy test reads or at none) is left byte
// for byte as it is, a plain value that would otherwise be captured included,
// and one line names the set, its first placeholder, its file and line, and
// what to do. A capture written beside such a placeholder can turn an
// existing use into an echo of the same answer, so a constant regressed in
// both places would pass where main and the set as given fail it; the MV, Z,
// Q29 and M2 rows are such sets. Left as they are, they replay as before
// templatize ran.
func TestProcessTestCasesV2_ASetThatHoldsPlaceholdersIsLeftAsItIs(t *testing.T) {
	js := map[string]string{"Content-Type": "application/json"}
	grpcCase := &models.TestCase{Name: "test-1", Kind: models.GRPC_EXPORT,
		GrpcReq: models.GrpcReq{Headers: models.GrpcHeaders{OrdinaryHeaders: map[string]string{"x-user": "{{string .id}}"}}}}
	streamed := tcase("test-1", "GET", "http://h/events", nil, "", 200, map[string]string{"Content-Type": "text/event-stream"}, "")
	streamed.HTTPResp.StreamBody = []models.HTTPStreamChunk{{Data: []models.HTTPStreamDataField{{Key: "data", Value: `{"user":"{{string .id}}"}`}}}}
	pipeFrame := tcase("test-1", "GET", "http://h/events", nil, "", 200, map[string]string{"Content-Type": "text/event-stream"}, "")
	pipeFrame.HTTPResp.StreamBody = []models.HTTPStreamChunk{{Data: []models.HTTPStreamDataField{{Key: "data", Value: `{"p":"{{ .price | print }}"}`}}}}
	withParams := tcase("test-1", "GET", "http://h/search", nil, "", 200, js, `{"ok":true}`)
	withParams.HTTPReq.URLParams = map[string]string{"q": "{{string .q}}"}
	withForm := tcase("test-1", "POST", "http://h/token", nil, "", 200, js, `{"ok":true}`)
	withForm.HTTPReq.Form = []models.FormData{{Key: "code", Values: []string{"{{string .code}}"}}}
	asserted := tcase("test-1", "GET", "http://h/me", nil, "", 200, map[string]string{"X-User": "u_REC1"}, `{"ok":true}`)
	asserted.Assertions = map[models.AssertionType]interface{}{"header_equal": map[string]interface{}{"X-User": "{{string .id}}"}}
	for _, c := range []struct {
		name  string
		tmpl  map[string]interface{}
		cases []*models.TestCase
		at    heldAt
	}{
		{"main's output: the create's id a use, the read-back sends it", map[string]interface{}{"id": "u_REC1"}, []*models.TestCase{
			tcase("test-1", "POST", "http://h/users", js, `{}`, 201, js, `{"id":"{{string .id}}"}`),
			tcase("test-2", "GET", "http://h/users/{{string .id}}", nil, "", 200, js, `{"ok":true}`),
		}, heldAt{text: "{{string .id}}", file: "test-1.yaml", line: `body:…{"id":"{{string .id}}"}`}},
		{"MV1 main's output: a later answer holds the use, the 201's Location written out", map[string]interface{}{"latest": "ord_REC1"}, []*models.TestCase{
			tcase("test-1", "POST", "http://h/orders", js, `{}`, 201, map[string]string{"Location": "/orders/ord_REC1"}, ``),
			tcase("test-2", "GET", "http://h/orders?latest=1", nil, "", 200, js, `{"latest":"{{string .latest}}","n":1}`),
			tcase("test-3", "DELETE", "http://h/orders/{{string .latest}}", nil, "", 204, nil, ``),
		}, heldAt{text: "{{string .latest}}", file: "test-2.yaml", line: `{"latest":"{{string .latest}}","n":1}`}},
		{"MV3 main's output: GET /me holds the use, the 201's Location written out", map[string]interface{}{"id": "u_REC1"}, []*models.TestCase{
			tcase("test-1", "POST", "http://h/users", js, `{}`, 201, map[string]string{"Location": "/users/u_REC1"}, ``),
			tcase("test-2", "GET", "http://h/me", nil, "", 200, js, `{"id":"{{string .id}}"}`),
			tcase("test-3", "GET", "http://h/users/{{string .id}}", nil, "", 200, js, `{"ok":true}`),
		}, heldAt{text: "{{string .id}}", file: "test-2.yaml", line: `body:…{"id":"{{string .id}}"}`}},
		{"MV5 a person's: the session cookie written out, a later answer uses it", map[string]interface{}{"sid": "s_REC1"}, []*models.TestCase{
			tcase("test-1", "POST", "http://h/login", js, `{}`, 200, map[string]string{"Set-Cookie": "sid=s_REC1; Path=/"}, `{"ok":true}`),
			tcase("test-2", "GET", "http://h/whoami", nil, "", 200, js, `{"session":"{{string .sid}}"}`),
			tcase("test-3", "GET", "http://h/me", map[string]string{"Cookie": "sid={{string .sid}}"}, "", 200, js, `{"ok":true}`),
		}, heldAt{text: "{{string .sid}}", file: "test-2.yaml", line: `{"session":"{{string .sid}}"}`}},
		{"Z1 a person's: the create written out, a later answer uses it", map[string]interface{}{"id": "u_REC1abc"}, []*models.TestCase{
			tcase("test-1", "POST", "http://h/users", js, `{}`, 201, js, `{"id":"u_REC1abc"}`),
			tcase("test-2", "GET", "http://h/users/{{string .id}}", nil, "", 200, js, `{"ok":true}`),
			tcase("test-3", "GET", "http://h/stats", nil, "", 200, js, `{"last":"{{string .id}}"}`),
		}, heldAt{text: "{{string .id}}", file: "test-2.yaml", line: "url:…http://h/users/{{string .id}}"}},
		{"Q29 main's output: the id written out, its copy a use", map[string]interface{}{"id": "u_REC1abc"}, []*models.TestCase{
			tcase("test-1", "POST", "http://h/users", js, `{}`, 201, js, `{"id":"u_REC1abc","user":{"id":"{{string .id}}","name":"a"}}`),
			tcase("test-2", "GET", "http://h/users/{{string .id}}", nil, "", 200, js, `{"ok":true}`),
		}, heldAt{text: "{{string .id}}", file: "test-1.yaml", line: `"user":{"id":"{{string .id}}"`}},
		// M2 (round 11's false pass): the cookie's use stands where no cookie
		// is read (the value has no name=), and the body gives the id written
		// out. A capture written there made the header's use an echo of it.
		{"M2 a person's: a use that is a Set-Cookie value's first part, the body written out", map[string]interface{}{"sid": "s_REC1abc"}, []*models.TestCase{
			tcase("test-1", "POST", "http://h/login", js, `{}`, 200, map[string]string{"Set-Cookie": "{{string .sid}}; Path=/"}, `{"sid":"s_REC1abc"}`),
			tcase("test-2", "GET", "http://h/me", map[string]string{"X-Sid": "s_REC1abc"}, "", 200, js, `{"ok":true}`),
		}, heldAt{text: "{{string .sid}}", file: "test-1.yaml", line: "Set-Cookie:…{{string .sid}}; Path=/"}},
		{"M2b a whole Set-Cookie value that is one use", map[string]interface{}{"c": "sid=abc"}, []*models.TestCase{
			tcase("test-1", "POST", "http://h/login", js, `{}`, 200, map[string]string{"Set-Cookie": "{{string .c}}"}, `{"ok":true}`),
		}, heldAt{text: "{{string .c}}", file: "test-1.yaml", line: "Set-Cookie:…{{string .c}}"}},
		{"a Set-Cookie value that starts with ';'", map[string]interface{}{"sid": "s_REC1"}, []*models.TestCase{
			tcase("test-1", "POST", "http://h/login", js, `{}`, 200, map[string]string{"Set-Cookie": "; {{string .sid}}; Path=/"}, `{"ok":true}`),
		}, heldAt{text: "{{string .sid}}", file: "test-1.yaml", line: "Set-Cookie:…; {{string .sid}}; Path=/"}},
		{"in a cookie's attribute", map[string]interface{}{"p": "app"}, []*models.TestCase{
			tcase("test-1", "POST", "http://h/login", js, `{}`, 200, map[string]string{"Set-Cookie": "sid=abc; Path=/{{string .p}}"}, `{"ok":true}`),
		}, heldAt{text: "{{string .p}}", file: "test-1.yaml", line: "Set-Cookie:…sid=abc; Path=/{{string .p}}"}},
		{"a capture a person wrote", map[string]interface{}{"id": "u_REC1"}, []*models.TestCase{
			tcase("test-1", "POST", "http://h/users", js, `{}`, 201, js, `{"id":"{{capture string .id}}"}`),
			tcase("test-2", "GET", "http://h/users/u_REC1", nil, "", 200, js, `{"ok":true}`),
		}, heldAt{text: "{{capture string .id}}", file: "test-1.yaml", line: `{"id":"{{capture string .id}}"}`}},
		{"a key config.yaml does not define", nil, []*models.TestCase{
			tcase("test-1", "POST", "http://h/users", js, `{}`, 201, js, `{"id":"u_REC1"}`),
			tcase("test-2", "GET", "http://h/users/u_REC1", map[string]string{"X-Trace": "{{string .trace}}"}, "", 200, js, `{"ok":true}`),
		}, heldAt{text: "{{string .trace}}", file: "test-2.yaml", line: "X-Trace:…{{string .trace}}"}},
		{"text outside the grammar", nil, []*models.TestCase{
			tcase("test-1", "POST", "http://h/users", js, `{"owner":"{{.user.id}}"}`, 201, js, `{"id":"u_REC1"}`),
		}, heldAt{text: "{{.user.id}}", file: "test-1.yaml", line: `body:…{"owner":"{{.user.id}}"}`}},
		{"a capture of a key outside the grammar", nil, []*models.TestCase{
			tcase("test-1", "POST", "http://h/users", js, `{}`, 201, map[string]string{"X-Owner": "{{capture string .user.id}}"}, `{"id":"u_REC1"}`),
		}, heldAt{text: "{{capture string .user.id}}", file: "test-1.yaml", line: "X-Owner:…{{capture string .user.id}}"}},
		{"untyped text an app answers (a mail template)", nil, []*models.TestCase{
			tcase("test-1", "GET", "http://h/mail", nil, "", 200, js, `{"tpl":"Hello {{ .Name }}"}`),
		}, heldAt{text: "{{ .Name }}", file: "test-1.yaml", line: `{"tpl":"Hello {{ .Name }}"}`}},
		{"a bare dot (a Mustache section's)", nil, []*models.TestCase{
			tcase("test-1", "GET", "http://h/tpl", nil, "", 200, js, `{"tpl":"{{#items}}{{.}}{{/items}}"}`),
		}, heldAt{text: "{{.}}", file: "test-1.yaml", line: `{"tpl":"{{#items}}{{.}}{{/items}}"}`}},
		{"an expression an earlier keploy rendered, which keploy test reads as text", map[string]interface{}{"id": "u_REC1"}, []*models.TestCase{
			tcase("test-1", "GET", "http://h/users/{{ .id | print }}", nil, "", 200, js, `{"ok":true}`),
		}, heldAt{text: "{{ .id | print }}", file: "test-1.yaml", line: "url:…http://h/users/{{ .id | print }}"}},
		{"an expression that holds a double quote, which an earlier keploy read as text too", map[string]interface{}{"price": "9.5"}, []*models.TestCase{
			tcase("test-1", "POST", "http://h/quote", js, `{"p":"{{ float .price | printf \"%.2f\" }}"}`, 200, js, `{"ok":true}`),
		}, heldAt{text: `{{ float .price | printf \"%.2f\" }}`, file: "test-1.yaml", line: `{"p":"{{ float .price | printf \"%.2f\" }}"}`}},
		{"an expression only in a response body", map[string]interface{}{"price": "9.5"}, []*models.TestCase{
			tcase("test-1", "GET", "http://h/quote", nil, "", 200, js, `{"p":"{{ .price | print }}"}`),
		}, heldAt{text: "{{ .price | print }}", file: "test-1.yaml", line: `body:…{"p":"{{ .price | print }}"}`}},
		{"an expression only in a streamed frame", map[string]interface{}{"price": "9.5"}, []*models.TestCase{pipeFrame},
			heldAt{text: "{{ .price | print }}", file: "test-1.yaml", line: `{"p":"{{ .price | print }}"}`}},
		{"a capture with no type, which keploy test refuses", map[string]interface{}{"id": "u_REC1"}, []*models.TestCase{
			tcase("test-1", "POST", "http://h/users", js, `{}`, 201, js, `{"id":"{{capture .id}}"}`),
		}, heldAt{text: "{{capture .id}}", file: "test-1.yaml", line: `{"id":"{{capture .id}}"}`}},
		{"a capture of a secret, which keploy sanitize never writes and keploy test refuses", nil, []*models.TestCase{
			tcase("test-1", "POST", "http://h/login", js, `{}`, 200, js, `{"token":"{{capture string .secret.token}}"}`),
		}, heldAt{text: "{{capture string .secret.token}}", file: "test-1.yaml", line: `{"token":"{{capture string .secret.token}}"}`}},
		{"percent-encoded in a URL", map[string]interface{}{"id": "u_REC1"}, []*models.TestCase{
			tcase("test-1", "GET", "http://h/users/%7B%7Bstring%20.id%7D%7D", nil, "", 200, js, `{"ok":true}`),
		}, heldAt{text: "{{string .id}}", file: "test-1.yaml", line: "url:…http://h/users/%7B%7Bstring%20.id%7D%7D"}},
		{"in a header's name", map[string]interface{}{"h": "Key"}, []*models.TestCase{
			tcase("test-1", "GET", "http://h/x", map[string]string{"X-{{string .h}}": "1"}, "", 200, js, `{"ok":true}`),
		}, heldAt{text: "{{string .h}}", file: "test-1.yaml", line: `X-{{string .h}}`}},
		{"in a URL's params", map[string]interface{}{"q": "shoes"}, []*models.TestCase{withParams},
			heldAt{text: "{{string .q}}", file: "test-1.yaml", line: "q:…{{string .q}}"}},
		{"in a form value", map[string]interface{}{"code": "c_REC1"}, []*models.TestCase{withForm},
			heldAt{text: "{{string .code}}", file: "test-1.yaml", line: "{{string .code}}"}},
		{"in a Set-Cookie", map[string]interface{}{"sid": "s_REC1"}, []*models.TestCase{
			tcase("test-1", "POST", "http://h/login", js, `{}`, 200, map[string]string{"Set-Cookie": "sid={{capture string .sid}}; Path=/"}, `{"ok":true}`),
		}, heldAt{text: "{{capture string .sid}}", file: "test-1.yaml", line: "Set-Cookie:…sid={{capture string .sid}}; Path=/"}},
		{"in a streamed frame", map[string]interface{}{"id": "u_REC1"}, []*models.TestCase{streamed},
			heldAt{text: "{{string .id}}", file: "test-1.yaml", line: `{"user":"{{string .id}}"}`}},
		{"in a gRPC request header", map[string]interface{}{"id": "u_REC1"}, []*models.TestCase{grpcCase},
			heldAt{text: "{{string .id}}", file: "test-1.yaml", line: "x-user:…{{string .id}}"}},
		{"in an assertion's value, which keploy test refuses", map[string]interface{}{"id": "u_REC1"}, []*models.TestCase{asserted},
			heldAt{text: "{{string .id}}", file: "test-1.yaml", line: "X-User:…{{string .id}}"}},
		{"the leftmost of a text", map[string]interface{}{"id": "u_REC1"}, []*models.TestCase{
			tcase("test-1", "GET", "http://h/x", map[string]string{"X-Pair": "{{.a.b}}-{{string .id}}"}, "", 200, js, `{"ok":true}`),
		}, heldAt{text: "{{.a.b}}", file: "test-1.yaml", line: "X-Pair:…{{.a.b}}-{{string .id}}"}},
		{"after a secret, which is no placeholder of the set", map[string]interface{}{"id": "u_REC1"}, []*models.TestCase{
			tcase("test-1", "GET", "http://h/users?key={{string .secret.apiKey}}", map[string]string{"X-Id": "{{string .id}}"}, "", 200, js, `{"ok":true}`),
		}, heldAt{text: "{{string .id}}", file: "test-1.yaml", line: "X-Id:…{{string .id}}"}},
		{"only in config.yaml's template, a value", map[string]interface{}{"greeting": "Hi {{ .Name }}"}, nil,
			heldAt{text: "{{ .Name }}", where: "config.yaml template.greeting"}},
		{"only in config.yaml's template, a key", map[string]interface{}{"{{string .k}}": "v"}, nil,
			heldAt{text: "{{string .k}}", where: "config.yaml template.{{string .k}}"}},
		{"only in config.yaml's template, inside a list", map[string]interface{}{"ids": []interface{}{"a", "{{string .id}}"}}, nil,
			heldAt{text: "{{string .id}}", where: "config.yaml template.ids[1]"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			// A value templatize captures in a plain recording.
			cases := append(c.cases,
				tcase("test-8", "POST", "http://h/items", js, `{}`, 201, js, `{"item":"it_REC9"}`),
				tcase("test-9", "GET", "http://h/items/it_REC9", nil, "", 200, js, `{"ok":true}`))
			before := ""
			for _, tc := range cases {
				before += fmt.Sprintf("%+v\n", *tc)
			}
			written, conf, logs := templatizeAll(t, cases, c.tmpl, nil)
			assert.Empty(t, written, "%s", writtenFiles(written, conf))
			assert.Nil(t, conf, "config.yaml is not written")
			assertLeftAsItIs(t, logs, caseFiles(t, cases), &c.at)
			after := ""
			for _, tc := range cases {
				after += fmt.Sprintf("%+v\n", *tc)
			}
			assert.Equal(t, before, after, "the test cases in memory are as they were")
		})
	}
}

// THE RULE READS THE FILES AS THEY ARE, NOT AS THE STORE LOADS THEM. Two files
// the yaml store would not hand a test case from still count: template text
// in a second document of a file (the store reads its first), written with an
// escape so only the file as read shows it; and a !!binary value folded over
// two lines (the fold's space is no base64, so the store fails on it), whose
// bytes hold a capture. Each leaves the set as it is, with the one line.
func TestProcessTestCasesV2_TemplateTextTheStoreWouldNotLoadStillHoldsTheSet(t *testing.T) {
	binary := base64.StdEncoding.EncodeToString([]byte(`{"id":"{{capture string .id}}"}`))
	for _, c := range []struct {
		name   string
		change func(string) string
		at     heldAt
	}{
		{"in a second document, written with YAML's \\x escape", func(s string) string {
			return s + "---\nreviewer_note: \"\\x7B\\x7Bstring .id}}\"\n"
		}, heldAt{text: "{{string .id}}", file: "test-1.yaml", line: `reviewer_note: "\x7B\x7Bstring .id}}"`}},
		{"a !!binary value folded over two lines", func(s string) string {
			return s + "reviewer_note: !!binary " + binary[:20] + "\n    " + binary[20:] + "\n"
		}, heldAt{text: "{{capture string .id}}", file: "test-1.yaml", line: "reviewer_note: !!binary " + binary[:20]}},
	} {
		t.Run(c.name, func(t *testing.T) {
			cases := createAndRead()
			files := caseFiles(t, cases)
			files[0].Data = []byte(c.change(string(files[0].Data)))
			db := &placementTestDB{written: map[string]models.TestCase{}, files: files}
			conf := &placementConfStore{}
			core, logs := observer.New(zap.InfoLevel)
			tools := &Tools{logger: zap.New(core), testDB: db, testSetConf: conf, config: &config.Config{}}
			require.NoError(t, tools.ProcessTestCasesV2(context.Background(), cases, "test-set-0"))
			assert.Empty(t, db.written)
			assert.Nil(t, conf.written)
			assertLeftAsItIs(t, logs, files, &c.at)
		})
	}
}

// A FILE OF THE SET THAT DOES NOT READ STOPS THE RUN. A test case file that
// is no YAML (or no JSON) cannot be said to hold no template text: the run
// returns the error naming the file, writes nothing, and says it changed
// nothing, with what to do.
func TestProcessTestCasesV2_AFileThatDoesNotReadStopsTheRun(t *testing.T) {
	for _, c := range []struct {
		name, file, data string
	}{
		{"a YAML file", "test-2.yaml", "spec: [unclosed\n"},
		{"a JSON file", "test-2.json", `{"spec": @}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			cases := createAndRead()
			files := caseFiles(t, cases)
			files[1] = setfile.File{Path: "keploy/test-set-0/tests/" + c.file, Data: []byte(c.data)}
			db := &placementTestDB{written: map[string]models.TestCase{}, files: files}
			conf := &placementConfStore{}
			core, logs := observer.New(zap.InfoLevel)
			tools := &Tools{logger: zap.New(core), testDB: db, testSetConf: conf, config: &config.Config{}}
			err := tools.ProcessTestCasesV2(context.Background(), cases, "test-set-0")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "failed to read "+c.file+" of test-set-0: ")
			assert.Empty(t, db.written)
			assert.Nil(t, conf.written)
			assert.Empty(t, logs.FilterMessageSnippet("holds template placeholders already").All())
			said := logs.FilterMessageSnippet("failed to read the test set's files; keploy templatize changed nothing in this set").All()
			require.Len(t, said, 1, "%v", logs.All())
			assert.Equal(t, "test-set-0", said[0].ContextMap()["testSet"])
			assert.Equal(t, "do what the error says, then run keploy templatize again", said[0].ContextMap()["next_step"])
		})
	}
}

// A SET THAT HOLDS ONLY SECRETS IS A PLAIN RECORDING. {{string .secret.K}},
// which keploy sanitize writes over a value the recording held, is the
// literal it redacts, never a placeholder of the set: templatize writes into
// such a set as into any plain recording, and leaves every secret as it is.
func TestProcessTestCasesV2_ASetThatHoldsOnlySecretsIsTemplatized(t *testing.T) {
	js := map[string]string{"Content-Type": "application/json"}
	bearer := map[string]string{"Authorization": "Bearer {{string .secret.token}}", "Content-Type": "application/json"}
	cases := []*models.TestCase{
		// The login's answer is the first place of the token's secret: it is
		// still no value a capture takes.
		tcase("test-1", "POST", "http://h/login", js, `{"u":"a"}`, 200, js, `{"token":"{{string .secret.token}}"}`),
		tcase("test-2", "POST", "http://h/users?key={{string .secret.apiKey}}", bearer, `{"pw":"{{string .secret.pw}}"}`,
			201, js, `{"id":"u_REC1","echo":"{{string .secret.token}}"}`),
		tcase("test-3", "GET", "http://h/users/u_REC1?key={{string .secret.apiKey}}", bearer, "", 200, js, `{"id":"u_REC1"}`),
	}
	written, conf, logs := templatizeAll(t, cases, nil, nil)
	assert.NotContains(t, written, "test-1", "no capture over a secret")
	assert.Equal(t, `{"id":"{{capture string .id}}","echo":"{{string .secret.token}}"}`, written["test-2"].HTTPResp.Body)
	assert.Equal(t, "http://h/users?key={{string .secret.apiKey}}", written["test-2"].HTTPReq.URL)
	assert.Equal(t, `{"pw":"{{string .secret.pw}}"}`, written["test-2"].HTTPReq.Body)
	assert.Equal(t, "Bearer {{string .secret.token}}", written["test-2"].HTTPReq.Header["Authorization"])
	assert.Equal(t, "http://h/users/{{string .id}}?key={{string .secret.apiKey}}", written["test-3"].HTTPReq.URL)
	assert.Equal(t, "Bearer {{string .secret.token}}", written["test-3"].HTTPReq.Header["Authorization"])
	assert.Equal(t, `{"id":"{{string .id}}"}`, written["test-3"].HTTPResp.Body)
	require.NotNil(t, conf)
	assert.Equal(t, map[string]interface{}{"id": "u_REC1"}, conf.Template)
	assert.Empty(t, logs.FilterMessageSnippet("holds template placeholders already").All())

	// A URL segment that follows a secret's is named after its header: the
	// segment before it is a value (a placeholder), never a name.
	afterSecret := []*models.TestCase{
		tcase("test-1", "POST", "http://h/t/users", js, `{}`, 201, map[string]string{"Location": "/t/{{string .secret.tenant}}/u_REC1abc"}, ``),
		tcase("test-2", "GET", "http://h/users/u_REC1abc", nil, "", 200, js, `{"ok":true}`),
	}
	written, conf, _ = templatizeAll(t, afterSecret, nil, nil)
	assert.Equal(t, "/t/{{string .secret.tenant}}/{{capture string .location}}", written["test-1"].HTTPResp.Header["Location"])
	assert.Equal(t, "http://h/users/{{string .location}}", written["test-2"].HTTPReq.URL)
	require.NotNil(t, conf)
	assert.Equal(t, map[string]interface{}{"location": "u_REC1abc"}, conf.Template)
}

// TEXT NO KEPLOY RENDERS IS NO TEMPLATE TEXT. Braces with no '.' after them
// (a Handlebars {{name}}, LaTeX's {{\pi}}) were never a placeholder, for this
// keploy or an earlier one: a recording that holds such text is a plain
// recording, and templatize writes into it, leaving that text as it is.
func TestProcessTestCasesV2_BracesNoKeployRendersAreAPlainRecording(t *testing.T) {
	js := map[string]string{"Content-Type": "application/json"}
	cases := []*models.TestCase{
		tcase("test-1", "POST", "http://h/users", js, `{"greeting":"Hi {{name}}"}`, 201, js, `{"id":"u_REC1","tex":"{{\\pi}} r^2"}`),
		tcase("test-2", "GET", "http://h/users/u_REC1", nil, "", 200, js, `{"ok":true}`),
	}
	written, conf, logs := templatizeAll(t, cases, nil, nil)
	assert.Equal(t, `{"id":"{{capture string .id}}","tex":"{{\\pi}} r^2"}`, written["test-1"].HTTPResp.Body)
	assert.Equal(t, `{"greeting":"Hi {{name}}"}`, written["test-1"].HTTPReq.Body)
	assert.Equal(t, "http://h/users/{{string .id}}", written["test-2"].HTTPReq.URL)
	require.NotNil(t, conf)
	assert.Equal(t, map[string]interface{}{"id": "u_REC1"}, conf.Template)
	assert.Empty(t, logs.FilterMessageSnippet("holds template placeholders already").All())
}

// Each capture templatize writes is said once: that place's value is no
// longer compared, only checked.
func TestProcessTestCasesV2_SaysWhatEachCaptureNoLongerCompares(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	db := &placementTestDB{written: map[string]models.TestCase{}}
	tools := &Tools{logger: zap.New(core), testDB: db, testSetConf: &placementConfStore{}, config: &config.Config{}}
	require.NoError(t, tools.ProcessTestCasesV2(context.Background(), createAndRead(), "test-set-0"))
	var lines []string
	for _, e := range logs.All() {
		if strings.Contains(e.Message, "now captures") {
			lines = append(lines, e.Message)
		}
	}
	assert.Equal(t, []string{
		"test-set-0/test-1 body.id now captures id: its value is no longer compared, only that it is present, of type string, and not empty",
		"test-set-0/test-2 body.id now captures id1: its value is no longer compared, only that it is present, of type string, and not empty",
	}, lines)
}

// ASSERT_CHAINS_WITH compares keploy's chains with go-fuzzers' baseline,
// which writes a body path without "body." and an array index in brackets
// (rerecord/client extractNestedValues: items[0].id). Both sides are read
// with each index as a path step, so the two forms meet.
func TestAssertChains_ReadsTheFuzzerBaselineAsTheFuzzerWritesIt(t *testing.T) {
	tools := &Tools{logger: zap.NewNop()}
	got := tools.convertToCanonical([]*TemplateChain{{
		TemplateKey: "id", Value: "usr_1",
		Producer: &ValueLocation{TestCaseIndex: 0, Part: ResponseBody, Path: "body.data.items[0].id"},
		Consumers: []*ValueLocation{
			{TestCaseIndex: 1, Part: RequestBody, Path: "body.owner"},
			{TestCaseIndex: 2, Part: RequestBody, Path: "body.[1].ids[2]"},
			{TestCaseIndex: 3, Part: RequestHeader, Path: "Cookie.sid"},
		},
	}}, nil)
	require.Len(t, got, 1)
	assert.Equal(t, "data.items.0.id", got[0].Producer.Path)
	assert.Equal(t, []string{"owner", "1.ids.2", "Cookie.sid"}, []string{got[0].Consumers[0].Path, got[0].Consumers[1].Path, got[0].Consumers[2].Path})

	// The baseline as go-fuzzers writes it.
	baseline := map[string]interface{}{"chains": []interface{}{map[string]interface{}{
		"variable_name": "{{id}}", "value": "usr_1",
		"producer": map[string]interface{}{"request_id": "test-1", "part": "ResponseBody", "path": "data.items[0].id"},
		"consumers": []interface{}{
			map[string]interface{}{"request_id": "test-2", "part": "RequestBody", "path": "owner"},
			map[string]interface{}{"request_id": "test-3", "part": "RequestBody", "path": "[1].ids[2]"},
			map[string]interface{}{"request_id": "test-4", "part": "RequestHeader", "path": "Cookie.sid"},
		},
	}}}
	fuzzer, err := buildCanonicalChainsFromMap(baseline)
	require.NoError(t, err)
	require.Len(t, fuzzer, 1)
	assert.Equal(t, "data.items.0.id", fuzzer[0].Producer.Path)
	assert.Equal(t, "1.ids.2", fuzzer[0].Consumers[1].Path)
	passed, report := tools.compareChainSets(fuzzer, got)
	assert.True(t, passed, report)
}

// A CREATE'S OWN LOCATION is the resource it created (RFC 9110): the id the
// body produces is a use there, asserted equal to what was just captured,
// since the id is its Location's one candidate, where a capture itself could
// stand. Any other place of that response that repeats the value stays as
// recorded, its Content-Location included (a use there would accept whatever
// the body answers, where main compares it as written), and templatize says
// so, one line per place.
func TestProcessTestCasesV2_TheCreatesOwnLocationUsesTheCapturedID(t *testing.T) {
	// The lines name the recorded values only with logcontent on.
	t.Cleanup(logcontent.Set(true))
	jsonType := map[string]string{"Content-Type": "application/json"}
	cases := []*models.TestCase{
		tcase("test-1", "POST", "http://h/users", jsonType, `{"name":"alice"}`, 201,
			map[string]string{"Location": "/users/usr_01J9REC", "Content-Location": "/users/usr_01J9REC?v=1", "Set-Cookie": "last=usr_01J9REC; Path=/"},
			`{"id":"usr_01J9REC","stored_id":"usr_01J9REC"}`),
		tcase("test-2", "GET", "http://h/users/usr_01J9REC", nil, "", 200, nil, `{"id":"usr_01J9REC"}`),
	}
	core, logs := observer.New(zap.InfoLevel)
	db := &placementTestDB{written: map[string]models.TestCase{}}
	tools := &Tools{logger: zap.New(core), testDB: db, testSetConf: &placementConfStore{}, config: &config.Config{}}
	require.NoError(t, tools.ProcessTestCasesV2(context.Background(), cases, "test-set-0"))
	t1 := db.written["test-1"]
	assert.Equal(t, `{"id":"{{capture string .id}}","stored_id":"usr_01J9REC"}`, t1.HTTPResp.Body)
	assert.Equal(t, "/users/{{string .id}}", t1.HTTPResp.Header["Location"])
	assert.Equal(t, "/users/usr_01J9REC?v=1", t1.HTTPResp.Header["Content-Location"])
	assert.Equal(t, "last=usr_01J9REC; Path=/", t1.HTTPResp.Header["Set-Cookie"])
	var lines []string
	for _, e := range logs.All() {
		if strings.Contains(e.Message, " holds the value ") {
			lines = append(lines, e.Message)
		}
	}
	assert.Equal(t, []string{
		"test-set-0/test-1 body.stored_id holds the value id captures and stays as recorded; this test case fails there if the app issues a fresh value each run",
		"test-set-0/test-1 Content-Location holds the value id captures and stays as recorded; this test case fails there if the app issues a fresh value each run",
		"test-set-0/test-1 Set-Cookie.last holds the value id captures and stays as recorded; this test case fails there if the app issues a fresh value each run",
	}, lines)
}

// A CROWDED LOCATION BESIDE ITS RESPONSE'S OWN CAPTURE is named, with the
// use as its edit, and templatize writes nothing in it. The body captures the
// id; the constant collection name beside it in the Location, which the
// request did not send, is a second candidate, so one recording cannot tell
// which of the two the app issues: a use of the body's capture there would
// accept whatever the body answers, and a constant id regressed in both
// places would pass where main fails it. The header stays as recorded, and
// the line names both values with the use as the edit for an id issued anew.
// Applied, the edit passes Check, and the next run, over a set that now
// holds placeholders, writes nothing.
// A LINE OF TEMPLATIZE NAMES NO RECORDED VALUE without KEPLOY_LOG_CONTENT:
// a crowded header's line names its places, and its edit, and a place left
// written out, the value recorded there by its length alone, while the files
// it writes are what they are with the switch on. The values are the app's
// (an id, a token in a Location), and the line lands in keploy's log.
func TestProcessTestCasesV2_ALineNamesNoRecordedValueWithoutLogContent(t *testing.T) {
	t.Cleanup(logcontent.Set(false))
	crowded := []*models.TestCase{
		tcase("test-1", "POST", "http://h/signup", nil, `{}`, 201, map[string]string{"Location": "/users/usr_REC01"}, `{"id":"usr_REC01"}`),
		tcase("test-2", "GET", "http://h/users/usr_REC01", nil, "", 200, nil, `{"ok":true}`),
	}
	written, _, logs := templatizeConf(t, crowded, nil)
	assert.Equal(t, "http://h/users/{{string .id}}", written["test-2"].HTTPReq.URL, "the switch changes no file templatize writes")
	said := logs.FilterMessageSnippet("holds more than one value").All()
	require.Len(t, said, 1)
	assert.Contains(t, said[0].Message, "(Location.path.0, Location.path.1)")
	next := fmt.Sprint(said[0].ContextMap()["next_step"])
	assert.Contains(t, next, "write {{string .id}} in place of the recorded value (9 bytes)")
	for _, e := range logs.All() {
		assert.NotContains(t, e.Message, "usr_REC01")
		assert.NotContains(t, fmt.Sprint(e.ContextMap()), "usr_REC01")
	}

	repeated := []*models.TestCase{
		tcase("test-1", "POST", "http://h/orders", nil, `{}`, 303, map[string]string{"Location": "/orders/ord_REC1"}, `{"id":"ord_REC1"}`),
		tcase("test-2", "GET", "http://h/orders/ord_REC1", nil, "", 200, nil, `{"ok":true}`),
	}
	_, _, logs = templatizeConf(t, repeated, nil)
	repeat := logs.FilterMessageSnippet("test-set-0/test-1 Location holds the value id captures and stays as recorded").All()
	require.Len(t, repeat, 1)
	assert.Contains(t, fmt.Sprint(repeat[0].ContextMap()["next_step"]), "write {{string .id}} in place of the recorded value (8 bytes) there")
	for _, e := range logs.All() {
		assert.NotContains(t, e.Message, "ord_REC1")
		assert.NotContains(t, fmt.Sprint(e.ContextMap()), "ord_REC1")
	}
}

func TestProcessTestCasesV2_ACrowdedLocationBesideItsOwnCaptureIsNamedWithTheUse(t *testing.T) {
	// The lines name the recorded values only with logcontent on.
	t.Cleanup(logcontent.Set(true))
	cases := []*models.TestCase{
		tcase("test-1", "POST", "http://h/signup", nil, `{}`, 201, map[string]string{"Location": "/users/usr_REC01"}, `{"id":"usr_REC01"}`),
		tcase("test-2", "GET", "http://h/users/usr_REC01", nil, "", 200, nil, `{"ok":true}`),
	}
	written, template, logs := templatizeConf(t, cases, nil)
	assert.Equal(t, `{"id":"{{capture string .id}}"}`, written["test-1"].HTTPResp.Body)
	assert.Equal(t, "/users/usr_REC01", written["test-1"].HTTPResp.Header["Location"], "a crowded header holds no use of its response's capture")
	assert.Equal(t, "http://h/users/{{string .id}}", written["test-2"].HTTPReq.URL)
	crowded := logs.FilterMessageSnippet("holds more than one value").All()
	require.Len(t, crowded, 1)
	assert.Contains(t, crowded[0].Message, `(Location.path.0 "users", Location.path.1 "usr_REC01")`)
	assert.Contains(t, fmt.Sprint(crowded[0].ContextMap()["next_step"]), `if the value is one its response issues anew each run, write {{string .id}} in place of "usr_REC01": the header then asserts it names the value its response captures`)
	assert.Contains(t, fmt.Sprint(crowded[0].ContextMap()["next_step"]), "if it is the same every run, nothing to do")

	// The edit applied: Check accepts it, and templatize writes nothing more.
	t1, t2 := written["test-1"], written["test-2"]
	t1.HTTPResp.Header = map[string]string{"Location": "/users/{{string .id}}"}
	applied := []*models.TestCase{&t1, &t2}
	assert.Empty(t, templates.NewRun(nil, template, nil).Check(applied, templates.CheckOptions{}).Refused)
	again, _, logs := templatizeAll(t, applied, template, nil)
	assert.Empty(t, again)
	// The first in the file: a response's headers stand before its body.
	assertLeftAsItIs(t, logs, caseFiles(t, applied), &heldAt{text: "{{string .id}}", file: "test-1.yaml", line: "Location:…/users/{{string .id}}"})
}

// A CLEAR-THEN-SET LOGIN (Express res.clearCookie, then res.cookie): of two
// cookies of one name, Domain and Path the later replaces the earlier, so the
// capture goes on the cookie that stays, and an empty value ("" too) is
// never a value.
func TestProcessTestCasesV2_CapturesTheCookieThatStays(t *testing.T) {
	cases := []*models.TestCase{
		tcase("test-1", "POST", "http://h/login", nil, `{}`, 200,
			map[string]string{"Set-Cookie": `sid=S0rec00001; Path=/; Max-Age=0,sid=S1rec00001; Path=/,flag=""; Path=/`}, `{"ok":true}`),
		tcase("test-2", "GET", "http://h/me", map[string]string{"Cookie": "sid=S1rec00001"}, "", 200, nil, `{}`),
		tcase("test-3", "GET", "http://h/old", map[string]string{"Cookie": `sid=S0rec00001; flag=""`}, "", 200, nil, `{}`),
	}
	written, conf := runTemplatize(t, cases, nil, nil)
	assert.Equal(t, `sid=S0rec00001; Path=/; Max-Age=0,sid={{capture string .sid}}; Path=/,flag=""; Path=/`, written["test-1"].HTTPResp.Header["Set-Cookie"])
	assert.Equal(t, "sid={{string .sid}}", written["test-2"].HTTPReq.Header["Cookie"])
	assert.NotContains(t, written, "test-3")
	assert.Equal(t, map[string]interface{}{"sid": "S1rec00001"}, conf.Template)
}

// Headers the client sets itself (Content-Type, Accept, Accept-Language,
// User-Agent, Origin, Referer) never take an earlier answer's value, whatever
// a body field equals.
func TestProcessTestCasesV2_HeadersTheClientSetsAreNotTemplated(t *testing.T) {
	cases := []*models.TestCase{
		tcase("test-1", "GET", "http://h/prefs", nil, "", 200, nil,
			`{"mime":"application/vnd.api+json","lang":"fr-CA","agent":"okhttp/4.12.0","site":"https://app.example.com","page":"https://app.example.com/cart"}`),
		tcase("test-2", "GET", "http://h/items", map[string]string{
			"Content-Type": "application/vnd.api+json", "Accept": "application/vnd.api+json", "Accept-Language": "fr-CA",
			"User-Agent": "okhttp/4.12.0", "Origin": "https://app.example.com", "Referer": "https://app.example.com/cart",
		}, "", 200, nil, `{}`),
	}
	written, conf := runTemplatize(t, cases, nil, nil)
	assert.Empty(t, written, "%s", writtenFiles(written, conf))
}

// ONE KEY GRAMMAR. Whatever a field is called, the key templatize names after
// it is one the replay reads back, and so is sanitize's: on main both wrote
// keys of any script and text/template read them.
func TestTemplateKeys_TheReplayReadsBackEveryKeyAWriterNames(t *testing.T) {
	inputs := []string{"id", "número", "Contraseña", "订单号", "ключ-2", "2fa", "x٣y", "user id", "__", "", "  ", "a.b", "é́", "\xff\xfe", "İstanbul", "ǅemal", "١٢٣", "user-ID_v2", "日本語のキー", "😀key"}
	for i := 0; i < 2000; i++ {
		b := make([]rune, 1+i%12)
		for j := range b {
			b[j] = rune(0x20 + (i*7919+j*104729)%0x2FFF)
		}
		inputs = append(inputs, string(b))
	}
	for _, x := range inputs {
		key := normalizeBaseKey(x)
		for _, capture := range []bool{false, true} {
			text := templates.Format(capture, templates.TypeString, key)
			ps := templates.Find(text)
			if assert.Len(t, ps, 1, "templatize key %q (from %q)", key, x) {
				assert.Equal(t, key, ps[0].Key)
			}
		}
		secret := keyToSecretKey(x)
		for _, k := range []string{secret, testPrefixedBase("test-2", secret), uniqueKeyForValue(secret, "v", map[string]string{secret: "other"})} {
			ps := templates.Find(fmt.Sprintf("{{string .secret.%s }}", k))
			if assert.Len(t, ps, 1, "sanitize key %q (from %q)", k, x) {
				assert.Equal(t, "secret."+k, ps[0].Key)
			}
		}
	}
}

// A field named in another script is templatized, and the replay reads the
// placeholders templatize wrote.
func TestProcessTestCasesV2_AKeyInAnotherScriptIsReadBack(t *testing.T) {
	cases := []*models.TestCase{
		tcase("test-1", "POST", "http://h/orders", nil, `{}`, 201, nil, `{"número":"ORD_REC_0001"}`),
		tcase("test-2", "GET", "http://h/orders/ORD_REC_0001", nil, "", 200, nil, `{}`),
	}
	written, conf := runTemplatize(t, cases, nil, nil)
	assert.Equal(t, `{"número":"{{capture string .número}}"}`, written["test-1"].HTTPResp.Body)
	assert.Equal(t, "http://h/orders/{{string .número}}", written["test-2"].HTTPReq.URL)
	run := templates.NewRun(nil, conf.Template, nil)
	res := run.Expected(written["test-1"].HTTPResp, models.HTTPResp{Body: `{"número":"ORD_LIVE_0002"}`}, templates.ExpectOptions{})
	assert.Empty(t, res.Failures)
	assert.Equal(t, "http://h/orders/ORD_LIVE_0002", run.Request(written["test-2"].HTTPReq).URL)
}

// A value a request sends before any response holds it is the client's: it
// is no template, whatever later responses echo.
func TestProcessTestCasesV2_AValueARequestSendsFirstIsTheClients(t *testing.T) {
	cases := []*models.TestCase{
		tcase("test-1", "POST", "http://h/users", nil, `{"email":"a@x.io"}`, 201, nil, `{"email":"a@x.io"}`),
		tcase("test-2", "GET", "http://h/users?email=a@x.io", nil, "", 200, nil, `{"email":"a@x.io"}`),
	}
	written, conf := runTemplatize(t, cases, nil, nil)
	assert.Empty(t, written)
	assert.Nil(t, conf)
}

// What config.yaml already holds stays: a key whose value a new chain has
// keeps its name, and a key it does not is left alone.
func TestProcessTestCasesV2_KeepsExistingKeys(t *testing.T) {
	cases := []*models.TestCase{
		tcase("test-1", "POST", "http://localhost:8080/users", nil, `{}`, 201, nil, `{"id":"usr_1"}`),
		tcase("test-2", "GET", "http://localhost:8080/users/usr_1", nil, "", 200, nil, `{"id":"usr_1"}`),
	}
	written, conf := runTemplatize(t, cases, map[string]interface{}{"host": "localhost:8080", "userId": "usr_1"}, nil)
	assert.Equal(t, `{"id":"{{capture string .userId}}"}`, written["test-1"].HTTPResp.Body)
	assert.Equal(t, "http://localhost:8080/users/{{string .userId}}", written["test-2"].HTTPReq.URL)
	assert.Equal(t, map[string]interface{}{"host": "localhost:8080", "userId": "usr_1"}, conf.Template)
}

// With test.ignoreOrdering on, which array element produced a value cannot be
// decided, so a value produced inside an array is not templatized (keploy
// test would refuse the capture).
func TestProcessTestCasesV2_NoCaptureInAnArrayUnderIgnoreOrdering(t *testing.T) {
	cases := func() []*models.TestCase {
		return []*models.TestCase{
			tcase("test-1", "POST", "http://h/batch", nil, `{}`, 201, nil, `{"items":[{"id":"it_1"}]}`),
			tcase("test-2", "GET", "http://h/items/it_1", nil, "", 200, nil, `{"id":"it_1"}`),
		}
	}
	cfg := &config.Config{}
	cfg.Test.IgnoreOrdering = true
	written, _ := runTemplatize(t, cases(), nil, cfg)
	assert.Empty(t, written)

	written, _ = runTemplatize(t, cases(), nil, nil)
	assert.Equal(t, `{"items":[{"id":"{{capture string .id}}"}]}`, written["test-1"].HTTPResp.Body)
}

// RemoveDoubleQuotes ran over the template map before config.yaml was
// written, so a recorded W/"x1-abc" was stored as W/x1-abc, a value the
// recording never held and every later run then sent. config.yaml now stores
// the recorded value as recorded.
func TestProcessTestCasesV2_StoresTheRecordedValueAsRecorded(t *testing.T) {
	const etag = `W/"x1-abc"`
	cases := []*models.TestCase{
		tcase("test-1", "GET", "http://localhost:8080/doc", nil, "", 200, map[string]string{"Content-Type": "application/json"}, `{"etag":"W/\"x1-abc\""}`),
		tcase("test-2", "GET", "http://localhost:8080/doc", map[string]string{"If-None-Match": etag}, "", 304, nil, ""),
	}
	written, conf := runTemplatize(t, cases, nil, nil)
	require.NotNil(t, conf, "templatize wrote no config.yaml")
	require.Contains(t, written["test-2"].HTTPReq.Header["If-None-Match"], "{{", "precondition: the value was templatized:\n%s", writtenFiles(written, conf))
	assert.Equal(t, map[string]interface{}{"etag": etag}, conf.Template, "config.yaml stores a value the recording never held")
}

// ONLY A KEY OF THE GRAMMAR IS REUSED. config.yaml may hold a key no
// placeholder can name (user-id): templatize names the value's key afresh, so
// every placeholder it writes is one the replay reads back.
func TestProcessTestCasesV2_ReusesOnlyAConfigKeyOfTheGrammar(t *testing.T) {
	cases := []*models.TestCase{
		tcase("test-1", "POST", "http://h/users", nil, `{}`, 201, nil, `{"id":"usr_01J9REC"}`),
		tcase("test-2", "GET", "http://h/users/usr_01J9REC", nil, "", 200, nil, `{"ok":true}`),
	}
	written, conf := runTemplatize(t, cases, map[string]interface{}{"user-id": "usr_01J9REC"}, nil)
	assert.Equal(t, `{"id":"{{capture string .id}}"}`, written["test-1"].HTTPResp.Body)
	assert.Equal(t, "http://h/users/{{string .id}}", written["test-2"].HTTPReq.URL)
	require.NotNil(t, conf)
	assert.Equal(t, map[string]interface{}{"user-id": "usr_01J9REC", "id": "usr_01J9REC"}, conf.Template)
	for _, tc := range written {
		for _, text := range []string{tc.HTTPReq.URL, tc.HTTPResp.Body} {
			assert.Empty(t, templates.FindOutsideGrammar(text), "templatize wrote a key outside the grammar in %q", text)
		}
	}
}

// EVERY PLACE OF THE PRODUCER'S OWN RESPONSE that still holds the value it
// captures written out is said, a longer text that holds it included (a url,
// _links.self.href, X-Resource-Id); a number only where it stands whole.
func TestProcessTestCasesV2_SaysEveryPlaceThatStillHoldsTheCapturedValue(t *testing.T) {
	cases := []*models.TestCase{
		tcase("test-1", "POST", "http://h/users", nil, `{}`, 201,
			map[string]string{"X-Resource-Id": "usr_01J9REC", "Link": "</users/usr_01J9REC>; rel=self"},
			`{"id":"usr_01J9REC","url":"http://h/users/usr_01J9REC/","_links":{"self":{"href":"/users/usr_01J9REC"}},"order_no":5940419,"ref":"ORD-5940419","other":159404190}`),
		tcase("test-2", "GET", "http://h/users/usr_01J9REC", nil, `{"order_no":5940419}`, 200, nil, `{"ok":true}`),
	}
	core, logs := observer.New(zap.InfoLevel)
	db := &placementTestDB{written: map[string]models.TestCase{}}
	tools := &Tools{logger: zap.New(core), testDB: db, testSetConf: &placementConfStore{}, config: &config.Config{}}
	require.NoError(t, tools.ProcessTestCasesV2(context.Background(), cases, "test-set-0"))
	var places []string
	for _, e := range logs.All() {
		if strings.Contains(e.Message, " holds the value ") {
			places = append(places, strings.Fields(e.Message)[1]+" "+strings.Fields(e.Message)[5])
		}
	}
	assert.Equal(t, []string{
		"body.url id", "body._links.self.href id", "Link id", "X-Resource-Id id",
		"body.ref orderNo",
	}, places)
}

// A NESTED CREATE answers only a Location: /orgs/<org>/users/<new id>. The
// org is a use (the request sent it) and the new id a capture beside it, so
// the read-back is sent the id this run created.
func TestProcessTestCasesV2_ANestedCreateCapturesTheChildBesideTheParentsUse(t *testing.T) {
	cases := []*models.TestCase{
		tcase("test-1", "POST", "http://h/orgs", nil, `{}`, 201, nil, `{"id":"org_REC01"}`),
		tcase("test-2", "POST", "http://h/orgs/org_REC01/users", nil, `{}`, 201, map[string]string{"Location": "/orgs/org_REC01/users/usr_REC02"}, ""),
		tcase("test-3", "GET", "http://h/orgs/org_REC01/users/usr_REC02", nil, "", 200, nil, `{"ok":true}`),
	}
	written, conf := runTemplatize(t, cases, nil, nil)
	assert.Equal(t, "http://h/orgs/{{string .id}}/users", written["test-2"].HTTPReq.URL)
	assert.Equal(t, "/orgs/{{string .id}}/users/{{capture string .users}}", written["test-2"].HTTPResp.Header["Location"])
	assert.Equal(t, "http://h/orgs/{{string .id}}/users/{{string .users}}", written["test-3"].HTTPReq.URL)
	require.NotNil(t, conf)
	assert.Equal(t, map[string]interface{}{"id": "org_REC01", "users": "usr_REC02"}, conf.Template)
	run := templates.NewRun(nil, conf.Template, nil)
	var all []*models.TestCase
	for _, n := range []string{"test-1", "test-2", "test-3"} {
		tc := written[n]
		all = append(all, &tc)
	}
	assert.Empty(t, run.Check(all, templates.CheckOptions{}).Refused, "the replay runs what templatize wrote")

	again, _ := runTemplatize(t, all, conf.Template, nil)
	assert.Empty(t, again, "templatize over its own output writes nothing")
}

// templatizeConf is templatizeLogs with the config.yaml template the set
// holds after the run (template when templatize wrote none).
func templatizeConf(t *testing.T, cases []*models.TestCase, template map[string]interface{}) (map[string]models.TestCase, map[string]interface{}, *observer.ObservedLogs) {
	t.Helper()
	core, logs := observer.New(zap.InfoLevel)
	db := &placementTestDB{written: map[string]models.TestCase{}, files: caseFiles(t, cases)}
	conf := &placementConfStore{stored: models.TestSet{Template: copyTemplate(template)}}
	tools := &Tools{logger: zap.New(core), testDB: db, testSetConf: conf, config: &config.Config{}}
	require.NoError(t, tools.ProcessTestCasesV2(context.Background(), cases, "test-set-0"))
	out := copyTemplate(template)
	if conf.written != nil {
		out = conf.written.Template
	}
	return db.written, out, logs
}

// templatizeLogs runs keploy templatize over cases with config.yaml holding
// template, and returns what it wrote and every line it logged.
func templatizeLogs(t *testing.T, cases []*models.TestCase, template map[string]interface{}) (map[string]models.TestCase, *observer.ObservedLogs) {
	t.Helper()
	return templatizeLogsWith(t, cases, template, &config.Config{})
}

// templatizeLogsWith is templatizeLogs with keploy.yml's settings cfg.
func templatizeLogsWith(t *testing.T, cases []*models.TestCase, template map[string]interface{}, cfg *config.Config) (map[string]models.TestCase, *observer.ObservedLogs) {
	t.Helper()
	core, logs := observer.New(zap.InfoLevel)
	db := &placementTestDB{written: map[string]models.TestCase{}, files: caseFiles(t, cases)}
	conf := &placementConfStore{stored: models.TestSet{Template: copyTemplate(template)}}
	tools := &Tools{logger: zap.New(core), testDB: db, testSetConf: conf, config: cfg}
	require.NoError(t, tools.ProcessTestCasesV2(context.Background(), cases, "test-set-0"))
	return db.written, logs
}

// replaySet replays a set as keploy test does, one pass: the set's template
// at its recorded values, each test case's request rendered with the values
// so far, its expected response resolved against the answer given for it
// (captures first, then uses), and compared by the matcher. It returns the
// test cases that fail, and each request as sent.
func replaySet(t *testing.T, cases []*models.TestCase, tmpl map[string]interface{}, answers map[string]models.HTTPResp) (failed []string, sent map[string]string) {
	t.Helper()
	return replaySetWith(t, cases, tmpl, answers, false)
}

// replaySetWith is replaySet under keploy.yml's test.ignoreOrdering.
func replaySetWith(t *testing.T, cases []*models.TestCase, tmpl map[string]interface{}, answers map[string]models.HTTPResp, ignoreOrdering bool) (failed []string, sent map[string]string) {
	t.Helper()
	run := templates.NewRun(nil, tmpl, nil)
	sent = map[string]string{}
	for _, tc := range cases {
		answer, ok := answers[tc.Name]
		require.True(t, ok, "an answer for %s", tc.Name)
		sent[tc.Name] = run.Request(tc.HTTPReq).URL
		res := run.Expected(tc.HTTPResp, answer, templates.ExpectOptions{IgnoreOrdering: ignoreOrdering})
		exp := *tc
		exp.HTTPResp = res.Resp
		var opts []httpMatcher.MatchOption
		if res.BodyTemplated {
			opts = append(opts, httpMatcher.WithRecordedBody(res.RecordedBody))
		}
		got := answer
		got.Header = map[string]string{}
		for k, v := range answer.Header {
			got.Header[k] = v
		}
		pass, _ := httpMatcher.Match(&exp, &got, map[string]map[string][]string{}, ignoreOrdering, false, zap.NewNop(), false, opts...)
		if !pass || len(res.Failures) > 0 {
			failed = append(failed, tc.Name)
		}
	}
	return failed, sent
}

// A CROWDED LOCATION captures nothing. A 201's Location holds one capture,
// and when it holds more than one value its response gives first (a
// constant collection name and the created id alike, which one recording
// cannot tell apart), and a later request sends one of them, templatize
// captures none of them and names each, on one line per header: before, it
// captured the leftmost (users, api), left the id written out, and a
// regressed path passed (D1, /api/v1/…). A value no later request sends, or
// sends only as a JSON number or inside a longer string, still counts (G1b,
// G2, G3): it is the value the app issues there, and the constant beside it
// was captured and a create answering /accounts/<id> passed. The edit names
// a key for each value as templatize would: the one config.yaml gives it
// already, or a new one, never one named after an earlier test case's id.
func TestProcessTestCasesV2_ACrowdedLocationCapturesNone(t *testing.T) {
	// The lines name the recorded values only with logcontent on.
	t.Cleanup(logcontent.Set(true))
	jsonType := map[string]string{"Content-Type": "application/json"}
	for _, c := range []struct {
		name       string
		cases      []*models.TestCase
		template   map[string]interface{}
		candidates string
		edits      []string
		// regressed is a Location the create must fail on, as templatize
		// left it.
		regressed string
	}{
		{"D1 a signup answering /users/<id>", []*models.TestCase{
			tcase("test-1", "POST", "http://h/signup", nil, `{"u":"alice"}`, 201, map[string]string{"Location": "/users/usr_REC01"}, ""),
			tcase("test-2", "GET", "http://h/users/usr_REC01", nil, "", 200, nil, `{"ok":true}`),
		}, nil, `Location.path.0 "users", Location.path.1 "usr_REC01"`, []string{`{{capture string .location}} in place of "users" (and {{string .location}} where later requests send it)`, `{{capture string .users}} in place of "usr_REC01" (and {{string .users}} where`, "if its key is new (location, users), give it the value it replaced"}, "/accounts/usr_REC01"},
		{"G1b the id is never sent again, the collection is", []*models.TestCase{
			tcase("test-1", "POST", "http://h/signup", nil, `{}`, 201, map[string]string{"Location": "/users/usr_REC1"}, ""),
			tcase("test-2", "GET", "http://h/users?page=2", nil, "", 200, nil, `{"ok":true}`),
		}, nil, `Location.path.0 "users", Location.path.1 "usr_REC1"`, []string{`{{capture string .users}} in place of "usr_REC1"`}, "/accounts/usr_REC1"},
		{"G2 the id is sent again only as a JSON number", []*models.TestCase{
			tcase("test-1", "POST", "http://h/reports", nil, `{}`, 201, map[string]string{"Location": "/jobs/7654321"}, `{"id":7654321}`),
			tcase("test-2", "POST", "http://h/cancel", jsonType, `{"jobId":7654321}`, 200, nil, `{"ok":true}`),
			tcase("test-3", "GET", "http://h/jobs?state=queued", nil, "", 200, nil, `{"ok":true}`),
		}, nil, `Location.path.0 "jobs", Location.path.1 "7654321"`, []string{`write {{string .id}} in place of "7654321": the header then asserts it names the value its response captures`}, "/tasks/7654321"},
		{"G3 the id is sent again only inside a longer string", []*models.TestCase{
			tcase("test-1", "POST", "http://h/reports", nil, `{}`, 201, map[string]string{"Location": "/jobs/j_REC1"}, ""),
			tcase("test-2", "POST", "http://h/notify", jsonType, `{"url":"/jobs/j_REC1"}`, 200, nil, `{"ok":true}`),
			tcase("test-3", "GET", "http://h/jobs", nil, "", 200, nil, `{"ok":true}`),
		}, nil, `Location.path.0 "jobs", Location.path.1 "j_REC1"`, []string{`{{capture string .jobs}} in place of "j_REC1"`}, "/tasks/j_REC1"},
		{"G4 a nested create whose child id is never sent again", []*models.TestCase{
			tcase("test-1", "POST", "http://h/orgs", nil, `{}`, 201, nil, `{"id":"org_REC1"}`),
			tcase("test-2", "POST", "http://h/orgs/org_REC1/members", nil, `{}`, 201, map[string]string{"Location": "/orgs/org_REC1/users/usr_REC2"}, ""),
			tcase("test-3", "GET", "http://h/orgs/org_REC1/users", nil, "", 200, nil, `{"ok":true}`),
		}, nil, `Location.path.2 "users", Location.path.3 "usr_REC2"`, []string{`{{capture string .location}} in place of "users"`, `{{capture string .users}} in place of "usr_REC2"`}, "/orgs/org_REC1/groups/usr_REC2"},
		{"a versioned API path", []*models.TestCase{
			tcase("test-1", "POST", "http://h/signup", nil, `{}`, 201, map[string]string{"Location": "/api/v1/users/usr_REC01"}, ""),
			tcase("test-2", "GET", "http://h/api/v1/users/usr_REC01", nil, "", 200, nil, `{"ok":true}`),
		}, nil, `Location.path.0 "api", Location.path.1 "v1", Location.path.2 "users", Location.path.3 "usr_REC01"`, []string{`{{capture string .users}} in place of "usr_REC01"`}, "/api/v2/users/usr_REC01"},
		{"a network-path reference: its host is no segment", []*models.TestCase{
			tcase("test-1", "POST", "http://h/upload", nil, `{}`, 201, map[string]string{"Location": "//cdn.h/files/f_REC01"}, ""),
			tcase("test-2", "GET", "http://cdn.h/files/f_REC01", nil, "", 200, nil, `{"ok":true}`),
		}, nil, `Location.path.0 "files", Location.path.1 "f_REC01"`, []string{`{{capture string .files}} in place of "f_REC01"`}, "//cdn.h/blobs/f_REC01"},
		{"R7d an id after an id the header gives first keeps the segment's name (a limit)", []*models.TestCase{
			tcase("test-1", "POST", "http://h/signup", nil, `{}`, 201, map[string]string{"Location": "/accounts/acc_REC1/users/usr_REC2"}, ""),
			tcase("test-2", "GET", "http://h/accounts/acc_REC1/users/usr_REC2", nil, "", 200, nil, `{"ok":true}`),
		}, nil, `Location.path.0 "accounts", Location.path.1 "acc_REC1", Location.path.2 "users", Location.path.3 "usr_REC2"`, []string{`{{capture string .users}} in place of "usr_REC2"`}, "/accounts/acc_REC1/groups/usr_REC2"},
		{"a value config.yaml names already keeps its key", []*models.TestCase{
			tcase("test-1", "POST", "http://h/signup", nil, `{}`, 201, map[string]string{"Location": "/users/usr_REC01"}, ""),
			tcase("test-2", "GET", "http://h/users/usr_REC01", nil, "", 200, nil, `{"ok":true}`),
		}, map[string]interface{}{"user": "usr_REC01", "location": "x"}, `Location.path.0 "users", Location.path.1 "usr_REC01"`, []string{`{{capture string .location1}} in place of "users"`, `{{capture string .user}} in place of "usr_REC01"`, "if its key is new (location1), give it"}, "/accounts/usr_REC01"},
	} {
		t.Run(c.name, func(t *testing.T) {
			written, template, logs := templatizeConf(t, c.cases, c.template)
			for name, tc := range written {
				assert.NotContains(t, tc.HTTPResp.Header["Location"], "{{capture", "%s: a capture in a crowded Location", name)
			}
			var lines []string
			for _, e := range logs.FilterMessageSnippet("holds more than one value its response gives first").All() {
				lines = append(lines, e.Message)
				next := fmt.Sprint(e.ContextMap()["next_step"])
				assert.NotContains(t, next, ".KEY", "each candidate's edit names its own key")
				assert.NotContains(t, next, "orgRec1", "a key is never named after an earlier test case's id")
				if strings.Contains(next, "the header then asserts it names the value its response captures") {
					// A value its response captures elsewhere: a use of
					// that capture, and no capture beside it.
					assert.NotContains(t, next, "{{capture", "a capture beside a use of its response's own key is refused")
				} else {
					assert.Contains(t, next, "capture the one value the app issues anew each run")
				}
				for _, want := range c.edits {
					assert.Contains(t, next, want)
				}
			}
			// The create, as templatize left it, fails on a regressed
			// Location: the header is compared as recorded.
			var create models.HTTPResp
			var creator string
			for _, tc := range c.cases {
				if tc.HTTPResp.Header["Location"] != "" {
					creator, create = tc.Name, tc.HTTPResp
					if w, ok := written[tc.Name]; ok {
						create = w.HTTPResp
					}
				}
			}
			require.Len(t, lines, 1, "one line per crowded header")
			assert.Contains(t, lines[0], "test-set-0/"+creator+" response header Location holds")
			assert.Contains(t, lines[0], "("+c.candidates+"), and a later request sends one of them")

			run := templates.NewRun(nil, template, nil)
			answer := models.HTTPResp{StatusCode: 201, Header: map[string]string{"Location": c.regressed}, Body: `{"id":7654321}`}
			res := run.Expected(create, answer, templates.ExpectOptions{})
			assert.NotContains(t, res.Resp.Header["Location"], "{{", "every placeholder of the Location resolves with the set's template")
			assert.True(t, len(res.Failures) > 0 || res.Resp.Header["Location"] != c.regressed, "a regressed Location must fail the create: %+v", res)
		})
	}

	// A 201 Location whose values no later request sends is said nowhere.
	quiet := []*models.TestCase{
		tcase("test-1", "POST", "http://h/signup", nil, `{}`, 201, map[string]string{"Location": "/users/usr_REC01"}, ""),
		tcase("test-2", "GET", "http://h/health", nil, "", 200, nil, `{"ok":true}`),
	}
	_, logs := templatizeLogs(t, quiet, nil)
	assert.Empty(t, logs.FilterMessageSnippet("holds more than one value").All())
	assert.Len(t, logs.FilterMessageSnippet("nothing to templatize").All(), 1)

	// Nor one whose value a later request sends only as a JSON number: the
	// header's text could not hand it on (the body captures it as a number).
	asNumber := []*models.TestCase{
		tcase("test-1", "POST", "http://h/reports", nil, `{}`, 201, map[string]string{"Location": "/jobs/7654321"}, `{"id":7654321}`),
		tcase("test-2", "POST", "http://h/cancel", jsonType, `{"jobId":7654321}`, 200, nil, `{"ok":true}`),
	}
	written, logs := templatizeLogs(t, asNumber, nil)
	assert.Equal(t, `{"id":"{{capture int .id}}"}`, written["test-1"].HTTPResp.Body)
	assert.Equal(t, "/jobs/7654321", written["test-1"].HTTPResp.Header["Location"])
	assert.Empty(t, logs.FilterMessageSnippet("holds more than one value").All())

	// R7b: a signup whose body gives the id as a JSON number and whose
	// Location names it, beside "users", which a later list request sends.
	// The body captures the id, and the advice for the Location is the use
	// of that capture: never a capture of the id (a second capture of a key
	// in one response is refused), nor of "users" (beside a use of the
	// response's own key, refused too). The edit applied passes the set
	// check, a fresh id passes, and a Location naming another id fails.
	signup := []*models.TestCase{
		tcase("test-1", "POST", "http://h/signup", jsonType, `{}`, 201, map[string]string{"Location": "/users/7654321"}, `{"id":7654321}`),
		tcase("test-2", "GET", "http://h/users?page=2", nil, "", 200, jsonType, `{"n":1}`),
		tcase("test-3", "POST", "http://h/cancel", jsonType, `{"userId":7654321}`, 200, jsonType, `{"ok":true}`),
	}
	const useAdvice = `write {{string .id}} in place of "7654321": the header then asserts it names the value its response captures`
	adviceOf := func(logs *observer.ObservedLogs) string {
		t.Helper()
		lines := logs.FilterMessageSnippet("holds more than one value its response gives first").All()
		require.Len(t, lines, 1)
		return fmt.Sprint(lines[0].ContextMap()["next_step"])
	}
	first, template, logs := templatizeConf(t, signup, nil)
	assert.Equal(t, `{"id":"{{capture int .id}}"}`, first["test-1"].HTTPResp.Body)
	assert.Equal(t, "/users/7654321", first["test-1"].HTTPResp.Header["Location"])
	advice := adviceOf(logs)
	assert.Contains(t, advice, useAdvice)
	assert.NotContains(t, advice, "{{capture")
	var again []*models.TestCase
	for _, tc := range signup {
		c := *tc
		if w, ok := first[tc.Name]; ok {
			c = w
		}
		again = append(again, &c)
	}
	second, _, logs := templatizeAll(t, again, template, nil)
	assert.Empty(t, second, "a second run writes nothing")
	assertLeftAsItIs(t, logs, caseFiles(t, again), &heldAt{text: "{{capture int .id}}", file: "test-1.yaml", line: `body:…{"id":"{{capture int .id}}"}`})

	again[0].HTTPResp.Header["Location"] = "/users/{{string .id}}"
	run := templates.NewRun(nil, template, nil)
	assert.Empty(t, run.Check(again, templates.CheckOptions{}).Refused, "the advised edit passes the set check")
	fresh := run.Expected(again[0].HTTPResp, models.HTTPResp{StatusCode: 201, Header: map[string]string{"Location": "/users/8000001"}, Body: `{"id":8000001}`}, templates.ExpectOptions{})
	assert.Empty(t, fresh.Failures, "a fresh id passes")
	assert.Equal(t, "/users/8000001", fresh.Resp.Header["Location"])
	run.Reset()
	other := run.Expected(again[0].HTTPResp, models.HTTPResp{StatusCode: 201, Header: map[string]string{"Location": "/users/9999999"}, Body: `{"id":8000001}`}, templates.ExpectOptions{})
	assert.Equal(t, "/users/8000001", other.Resp.Header["Location"], "a Location naming another id is compared against the captured one, and fails")
}

// ONLY THE LOCATION OF A 201 CAPTURES (decision a). By RFC 9110 only a 201's
// Location names a resource the request created; a 3xx's is the target to
// redirect to, a Content-Location the representation's resource, and on a 202
// a Location has no defined meaning, and keploy test without templates
// compares each as written. A capture there would
// let a regression to another target pass (a login redirecting to /oops,
// Django's /accounts/profile/, an OAuth callback), so templatize captures
// nothing there and says so, once per header, with the edit for a value the
// app does issue anew each run. A value the body gives first is captured
// there, and the header keeps it written out, as main compares it.
func TestProcessTestCasesV2_OnlyTheLocationOfA201Captures(t *testing.T) {
	// The lines name the recorded values only with logcontent on.
	t.Cleanup(logcontent.Set(true))
	for _, c := range []struct {
		name, method, url string
		status            int
		header            map[string]string
		next              string
		said              string
		edits             []string
	}{
		{"T1 a login redirecting home", "POST", "http://h/login", 302, map[string]string{"Location": "/home"}, "http://h/home",
			`response header Location of a 302 response holds Location.path.0 "home", which a later request sends`, []string{`{{capture string .location}} in place of "home"`}},
		{"T2 Django's profile redirect", "POST", "http://h/accounts/login/", 302, map[string]string{"Location": "/accounts/profile/"}, "http://h/accounts/profile/",
			`Location.path.1 "profile"`, []string{`{{capture string .accounts}} in place of "profile"`}},
		{"an OAuth redirect to its callback", "GET", "http://h/authorize", 302, map[string]string{"Location": "http://h/callback?code=code_REC123&state=st_REC9"}, "http://h/callback?code=code_REC123&state=st_REC9",
			`Location.path.0 "callback", Location.query.code "code_REC123", Location.query.state "st_REC9"`, []string{`{{capture string .code}} in place of "code_REC123"`}},
		{"G1 a 202's job monitor", "POST", "http://h/reports", 202, map[string]string{"Location": "/jobs/j_REC1"}, "http://h/jobs",
			`of a 202 response holds Location.path.0 "jobs", which a later request sends`, []string{`{{capture string .location}} in place of "jobs"`}},
		{"a 200's Location", "POST", "http://h/reports", 200, map[string]string{"Location": "/jobs/j_REC1"}, "http://h/jobs",
			`of a 200 response holds Location.path.0 "jobs", which a later request sends`, []string{`{{capture string .location}} in place of "jobs"`}},
		{"a Content-Location of a 201", "POST", "http://h/signup", 201, map[string]string{"Content-Location": "/me"}, "http://h/me",
			`response header Content-Location of a 201 response holds Content-Location.path.0 "me"`, []string{`{{capture string .location}} in place of "me"`}},
		{"a Content-Location of a 302", "POST", "http://h/views", 302, map[string]string{"Content-Location": "/views/v_REC9"}, "http://h/views/v_REC9",
			`response header Content-Location of a 302 response holds Content-Location.path.1 "v_REC9"`, []string{`{{capture string .views}} in place of "v_REC9"`}},
	} {
		t.Run(c.name, func(t *testing.T) {
			cases := []*models.TestCase{
				tcase("test-1", c.method, c.url, nil, `{}`, c.status, c.header, ""),
				tcase("test-2", "GET", c.next, nil, "", 200, nil, `{"ok":true}`),
			}
			written, logs := templatizeLogs(t, cases, nil)
			assert.Empty(t, written, "nothing is captured, so nothing is written")
			said := logs.FilterMessageSnippet("templatize captures nothing there and it stays compared as recorded").All()
			require.Len(t, said, 1, "one line per header")
			assert.Contains(t, said[0].Message, c.said)
			next := fmt.Sprint(said[0].ContextMap()["next_step"])
			assert.Contains(t, next, "if the app issues one anew each run (a code, a job id), capture it, which then checks only that a value stands there")
			// Only a 3xx's Location is called a redirect's target: a 202's job
			// monitor, a 200's Location and a Content-Location, a 3xx's
			// included, are not.
			if c.status/100 == 3 && c.header["Location"] != "" {
				assert.Contains(t, next, "if the value is the same every run (a redirect's target), nothing to do")
			} else {
				assert.Contains(t, next, "if the value is the same every run, nothing to do")
				assert.NotContains(t, next, "redirect")
			}
			for _, want := range c.edits {
				assert.Contains(t, next, want)
			}
		})
	}

	// A 303 after a create whose body gives the id: the body captures it,
	// and the redirect's target stays as recorded, as main compares it (a
	// use there would accept whatever the body answers, R8s), said as a
	// place of the response that still holds the captured value.
	prg := []*models.TestCase{
		tcase("test-1", "POST", "http://h/orders", nil, `{}`, 303, map[string]string{"Location": "/orders/ord_REC1"}, `{"id":"ord_REC1"}`),
		tcase("test-2", "GET", "http://h/orders/ord_REC1", nil, "", 200, nil, `{"ok":true}`),
	}
	written, template, logs := templatizeConf(t, prg, nil)
	assert.Equal(t, `{"id":"{{capture string .id}}"}`, written["test-1"].HTTPResp.Body)
	assert.Equal(t, "/orders/ord_REC1", written["test-1"].HTTPResp.Header["Location"])
	assert.Equal(t, "http://h/orders/{{string .id}}", written["test-2"].HTTPReq.URL)
	repeat := logs.FilterMessageSnippet("test-set-0/test-1 Location holds the value id captures and stays as recorded").All()
	require.Len(t, repeat, 1)
	assert.Empty(t, logs.FilterMessageSnippet("stays compared as recorded").All(), "a value the body gives first is captured there")
	// Its line names the edit for an id the app issues anew (R7e, Q17), and a
	// second run, over a set that now holds placeholders, writes nothing.
	assert.Equal(t, `if the app issues the value anew each run, write {{string .id}} in place of "ord_REC1" there: that place then asserts it holds what this same answer gives at the capture of id, whatever that is (a constant's regression in both places included); if it is the same every run, nothing to do`,
		fmt.Sprint(repeat[0].ContextMap()["next_step"]))
	t1, t2 := written["test-1"], written["test-2"]
	again, _, logs := templatizeAll(t, []*models.TestCase{&t1, &t2}, template, nil)
	assert.Empty(t, again)
	assertLeftAsItIs(t, logs, caseFiles(t, []*models.TestCase{&t1, &t2}), &heldAt{text: "{{capture string .id}}", file: "test-1.yaml", line: `body:…{"id":"{{capture string .id}}"}`})
	// The edit applied passes keploy test's set check, passes a fresh id and
	// fails a redirect naming another order.
	t1.HTTPResp.Header = map[string]string{"Location": "/orders/{{string .id}}"}
	applied := []*models.TestCase{&t1, &t2}
	require.Empty(t, templates.NewRun(nil, template, nil).Check(applied, templates.CheckOptions{}).Refused)
	fine := models.HTTPResp{StatusCode: 200, Header: map[string]string{}, Body: `{"ok":true}`}
	failed, sent := replaySet(t, applied, template, map[string]models.HTTPResp{"test-1": {StatusCode: 303, Header: map[string]string{"Location": "/orders/ord_LIVE2"}, Body: `{"id":"ord_LIVE2"}`}, "test-2": fine})
	assert.Empty(t, failed)
	assert.Equal(t, "http://h/orders/ord_LIVE2", sent["test-2"])
	failed, _ = replaySet(t, applied, template, map[string]models.HTTPResp{"test-1": {StatusCode: 303, Header: map[string]string{"Location": "/orders/ord_OTHER"}, Body: `{"id":"ord_LIVE2"}`}, "test-2": fine})
	assert.Equal(t, []string{"test-1"}, failed)

	// Also where the body gives it first as a JSON number, which the header's
	// text cannot use: the header did not issue it, and is not said.
	prgNumber := []*models.TestCase{
		tcase("test-1", "POST", "http://h/orders", nil, `{}`, 303, map[string]string{"Location": "/orders/7654321"}, `{"id":7654321}`),
		tcase("test-2", "POST", "http://h/pay", map[string]string{"Content-Type": "application/json"}, `{"id":7654321}`, 200, nil, `{"ok":true}`),
		tcase("test-3", "GET", "http://h/orders/7654321", nil, "", 200, nil, `{"ok":true}`),
	}
	written, logs = templatizeLogs(t, prgNumber, nil)
	assert.Equal(t, `{"id":"{{capture int .id}}"}`, written["test-1"].HTTPResp.Body)
	assert.Equal(t, "/orders/7654321", written["test-1"].HTTPResp.Header["Location"])
	assert.Empty(t, logs.FilterMessageSnippet("stays compared as recorded").All(), "a value the body gives first is not the header's")
}

// A VALUE A URL-VALUED HEADER CANNOT HOLD AT ITS PART (an id that decodes to
// a '/' in a path segment) is not read there: the replay could not tell it
// from two segments, and would refuse the capture.
//
// Such a value still counts as a candidate of its header: a constant segment
// beside it is not taken for the value the app issues (the header is
// crowded, and named), and alone it is said and left as written.
func TestProcessTestCasesV2_ALocationValueWithADelimiterIsNotRead(t *testing.T) {
	// The lines name the recorded values only with logcontent on.
	t.Cleanup(logcontent.Set(true))
	for _, c := range []struct {
		name, loc, crowded string
	}{
		{"a path segment that decodes to a '/'", "/files/dir%2Freport", `Location.path.0 "files", Location.path.1 "dir/report"`},
		{"a query value that decodes to a '&' and a '='", "/cb?code=a%26b%3Dc", `Location.path.0 "cb", Location.query.code "a&b=c"`},
		{"alone in its header", "/dir%2Freport", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			// The create's own path names nothing the Location holds, so
			// every part of the Location is first a value the app answers.
			written, logs := templatizeLogs(t, []*models.TestCase{
				tcase("test-1", "POST", "http://h/upload", nil, `{}`, 201, map[string]string{"Location": c.loc}, ""),
				tcase("test-2", "GET", "http://h"+c.loc, nil, "", 200, nil, `{"ok":true}`),
			}, nil)
			assert.Empty(t, written, "nothing is captured: the header's other parts are not the issued value")
			crowded := logs.FilterMessageSnippet("holds more than one value its response gives first").All()
			if c.crowded == "" {
				assert.Empty(t, crowded)
				assert.Len(t, logs.FilterMessageSnippet("where it reads as two parts").All(), 1, "the value is said")
				assert.Empty(t, logs.FilterMessageSnippet("found no value").All(), "a value said is not no value")
				return
			}
			require.Len(t, crowded, 1)
			assert.Contains(t, crowded[0].Message, "("+c.crowded+")")
			// Its edit is never a capture there: the capture would be
			// refused.
			next := fmt.Sprint(crowded[0].ContextMap()["next_step"])
			assert.Contains(t, next, "holds a delimiter of its part and cannot be captured here; if a response also gives it whole (a JSON value), capture it there")
			assert.NotContains(t, next, "in place of \"dir/report\"")
			assert.NotContains(t, next, "in place of \"a&b=c\"")
		})
	}

	// A value a JSON body issues is captured there, and no use of it is
	// written where a Location holds it as two parts.
	written, _ := templatizeLogs(t, []*models.TestCase{
		tcase("test-1", "POST", "http://h/codes", nil, `{}`, 201, map[string]string{"Location": "/cb?code=a%26b"}, `{"code":"a&b"}`),
		tcase("test-2", "POST", "http://h/redeem", map[string]string{"Content-Type": "application/json"}, `{"code":"a&b"}`, 200, nil, `{"ok":true}`),
	}, nil)
	for name, tc := range written {
		assert.NotContains(t, tc.HTTPResp.Header["Location"], "{{", "%s: a placeholder where the Location reads it as two parts", name)
	}

	// Also where that part is its 201 Location's one candidate, where a use
	// of the response's own capture may otherwise stand: a use there could
	// not be told from two parts and would fail every run.
	written, _ = templatizeLogs(t, []*models.TestCase{
		tcase("test-1", "POST", "http://h/files", nil, `{}`, 201, map[string]string{"Location": "/files/a%2Fb"}, `{"path":"a/b"}`),
		tcase("test-2", "POST", "http://h/open", map[string]string{"Content-Type": "application/json"}, `{"path":"a/b"}`, 200, nil, `{"ok":true}`),
	}, nil)
	assert.Equal(t, `{"path":"{{capture string .path}}"}`, written["test-1"].HTTPResp.Body)
	assert.Equal(t, "/files/a%2Fb", written["test-1"].HTTPResp.Header["Location"], "no use where the Location reads the value as two parts")
}

// A REPEATED KEY is read at its last occurrence, as the replay and every
// JSON reader read it (r4-3 finding 5): a value only an earlier occurrence
// holds is no value of the response, and the capture goes where the
// response's value is.
func TestProcessTestCasesV2_ARepeatedKeyIsReadAtItsLastOccurrence(t *testing.T) {
	first := []*models.TestCase{
		tcase("test-1", "POST", "http://h/users", nil, `{}`, 201, nil, `{"id":"usr_FIRST01","id":"usr_LAST002"}`),
		tcase("test-2", "GET", "http://h/users/usr_FIRST01", nil, "", 200, nil, `{"ok":true}`),
	}
	written, _ := runTemplatize(t, first, nil, nil)
	assert.Empty(t, written, "usr_FIRST01 is no value the response gives")

	last := []*models.TestCase{
		tcase("test-1", "POST", "http://h/users", nil, `{}`, 201, nil, `{"id":"usr_FIRST01","id":"usr_LAST002"}`),
		tcase("test-2", "GET", "http://h/users/usr_LAST002", nil, "", 200, nil, `{"ok":true}`),
	}
	written, conf := runTemplatize(t, last, nil, nil)
	assert.Equal(t, `{"id":"usr_FIRST01","id":"{{capture string .id}}"}`, written["test-1"].HTTPResp.Body)
	require.NotNil(t, conf)
	assert.Equal(t, map[string]interface{}{"id": "usr_LAST002"}, conf.Template)
	run := templates.NewRun(nil, conf.Template, nil)
	var all []*models.TestCase
	for _, n := range []string{"test-1", "test-2"} {
		tc := written[n]
		all = append(all, &tc)
	}
	assert.Empty(t, run.Check(all, templates.CheckOptions{}).Refused, "the replay runs what templatize wrote")
}

// A LOCATION'S CANDIDATES ARE EVERY VALUE ITS RESPONSE ISSUES FIRST (T21,
// T21b, T21c). A part of a Location is a producer candidate when its value is
// first placed in the same test case's response, the body before it
// included, whether or not a later request sends it and whether or not the
// value is captured elsewhere. A created id the body holds as a JSON number,
// or inside an array under ignoreOrdering, is still the value the app issues
// there, so the constant collection name beside it is not taken for it: the
// header holds two candidates and captures none. Before, "jobs" was captured
// and a create answering /tasks/<id> passed, and the next request went to
// /tasks/<id>.
func TestProcessTestCasesV2_ALocationCountsEveryValueItsResponseIssues(t *testing.T) {
	// The lines name the recorded values only with logcontent on.
	t.Cleanup(logcontent.Set(true))
	for _, c := range []struct {
		name       string
		cfg        *config.Config
		cases      []*models.TestCase
		candidates string
		answer     string
	}{
		{"T21 the body issues the id as a JSON number", nil, []*models.TestCase{
			tcase("test-1", "POST", "http://h/reports", nil, `{}`, 201, map[string]string{"Location": "/jobs/7654321"}, `{"id":7654321}`),
			tcase("test-2", "GET", "http://h/jobs/7654321", nil, "", 200, nil, `{"state":"done"}`),
		}, `Location.path.0 "jobs", Location.path.1 "7654321"`, "/tasks/7654321"},
		{"T21b the body issues the id inside an array under ignoreOrdering", &config.Config{Test: config.Test{IgnoreOrdering: true}}, []*models.TestCase{
			tcase("test-1", "POST", "http://h/signup", nil, `{}`, 201, map[string]string{"Location": "/users/usr_REC1"}, `{"items":[{"id":"usr_REC1"}]}`),
			tcase("test-2", "GET", "http://h/users/usr_REC1", nil, "", 200, nil, `{"ok":true}`),
		}, `Location.path.0 "users", Location.path.1 "usr_REC1"`, "/accounts/usr_REC1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			core, logs := observer.New(zap.InfoLevel)
			db := &placementTestDB{written: map[string]models.TestCase{}}
			conf := &placementConfStore{}
			cfg := c.cfg
			if cfg == nil {
				cfg = &config.Config{}
			}
			tools := &Tools{logger: zap.New(core), testDB: db, testSetConf: conf, config: cfg}
			require.NoError(t, tools.ProcessTestCasesV2(context.Background(), c.cases, "test-set-0"))
			assert.Empty(t, db.written, "nothing is captured in the Location, and the id has no chain to write")
			assert.Nil(t, conf.written, "config.yaml is not written")
			crowded := logs.FilterMessageSnippet("holds more than one value its response gives first").All()
			require.Len(t, crowded, 1, "the header is said once")
			assert.Contains(t, crowded[0].Message, "("+c.candidates+")")

			// The create, as templatize left it, fails on a Location naming
			// another collection: the header is compared as recorded.
			run := templates.NewRun(nil, nil, nil)
			create := c.cases[0].HTTPResp
			if w, ok := db.written["test-1"]; ok {
				create = w.HTTPResp
			}
			answer := models.HTTPResp{StatusCode: 201, Header: map[string]string{"Location": c.answer}, Body: c.cases[0].HTTPResp.Body}
			res := run.Expected(create, answer, templates.ExpectOptions{IgnoreOrdering: cfg.Test.IgnoreOrdering})
			assert.True(t, len(res.Failures) > 0 || res.Resp.Header["Location"] != c.answer, "a Location naming another collection must fail the create: %+v", res)
		})
	}

	// A nested create keeps its one candidate: the parent id, first placed in
	// an earlier test case, is not one, nor is what the request sent.
	nested := []*models.TestCase{
		tcase("test-1", "POST", "http://h/orgs", nil, `{}`, 201, nil, `{"id":7001}`),
		tcase("test-2", "POST", "http://h/orgs/7001/users", nil, `{}`, 201, map[string]string{"Location": "/orgs/7001/users/usr_REC02"}, ""),
		tcase("test-3", "GET", "http://h/orgs/7001/users/usr_REC02", nil, "", 200, nil, `{"ok":true}`),
	}
	written, _ := runTemplatize(t, nested, nil, nil)
	assert.Equal(t, "/orgs/7001/users/{{capture string .users}}", written["test-2"].HTTPResp.Header["Location"])

	// A value the header holds twice is one candidate: captured at its first
	// part, the other left as recorded (and said).
	twice := []*models.TestCase{
		tcase("test-1", "POST", "http://h/signup", nil, `{}`, 201, map[string]string{"Location": "/usr_REC01?self=usr_REC01"}, ""),
		tcase("test-2", "GET", "http://h/usr_REC01", nil, "", 200, nil, `{"ok":true}`),
	}
	written, logs := templatizeLogs(t, twice, nil)
	assert.Equal(t, "/{{capture string .location}}?self=usr_REC01", written["test-1"].HTTPResp.Header["Location"])
	assert.Empty(t, logs.FilterMessageSnippet("holds more than one value").All())
}

// A NUMBER STANDS WHOLE: templatize says a place of the producer's own
// response holds a captured number only where no letter or digit, of any
// script, sits on either side of it. The 2 inside an ObjectId (662508d2a3)
// or an ETag (W/"1ca2-…") is no place of the quantity 2.
func TestOccurs_ANumberStandsWholeBetweenNonAlphanumerics(t *testing.T) {
	for _, c := range []struct {
		text string
		want bool
	}{
		{`"quantity":"2"`, true},
		{"a 2 b", true},
		{"1.2", true},
		{"2", true},
		{"662508d2a390", false},
		{`W/"1ca2-abc"`, false},
		{"x2", false},
		{"2x", false},
		{"12", false},
		{"23", false},
		{"é2", false},
		{"2٣", false},
	} {
		assert.Equal(t, c.want, occurs(c.text, "2", true), "%q", c.text)
	}
	assert.True(t, occurs("x2y", "2", false), "a string is found anywhere")

	cases := []*models.TestCase{
		tcase("test-1", "POST", "http://h/cart", nil, `{}`, 201, map[string]string{"Etag": `W/"1ca2-abc"`},
			`{"quantity":2,"_id":"662508d2a390f1","note":"take 2"}`),
		tcase("test-2", "POST", "http://h/orders", map[string]string{"Content-Type": "application/json"}, `{"quantity":2}`, 201, nil, `{"ok":true}`),
	}
	_, logs := templatizeLogs(t, cases, nil)
	var places []string
	for _, e := range logs.FilterMessageSnippet(" holds the value ").All() {
		places = append(places, strings.Fields(e.Message)[1])
	}
	assert.Equal(t, []string{"body.note"}, places, "only the number standing whole is said")
}

// A SET LEFT FOR THE USER TO EDIT is not called done (F11a, F11b): a crowded
// Location beside nothing else to templatize does not print "nothing to
// templatize", and beside a chain templatize writes it is still said. The
// crowded line says what is left.
func TestProcessTestCasesV2_ACrowdedSetIsNotCalledDone(t *testing.T) {
	crowded := []*models.TestCase{
		tcase("test-1", "POST", "http://h/signup", nil, `{}`, 201, map[string]string{"Location": "/users/usr_REC01"}, ""),
		tcase("test-2", "GET", "http://h/users/usr_REC01", nil, "", 200, nil, `{"ok":true}`),
	}
	_, logs := templatizeLogs(t, crowded, nil)
	assert.Len(t, logs.FilterMessageSnippet("holds more than one value").All(), 1)
	assert.Empty(t, logs.FilterMessageSnippet("nothing to templatize").All())

	beside := []*models.TestCase{
		tcase("test-1", "POST", "http://h/signup", nil, `{}`, 201, map[string]string{"Location": "/users/usr_REC01"}, `{"tok":"tok_REC1"}`),
		tcase("test-2", "GET", "http://h/users/usr_REC01", map[string]string{"X-Tok": "tok_REC1"}, "", 200, nil, `{"ok":true}`),
	}
	written, logs := templatizeLogs(t, beside, nil)
	assert.Equal(t, `{"tok":"{{capture string .tok}}"}`, written["test-1"].HTTPResp.Body)
	assert.Equal(t, "/users/usr_REC01", written["test-1"].HTTPResp.Header["Location"])
	assert.Len(t, logs.FilterMessageSnippet("holds more than one value").All(), 1)
}

// AN ELEMENT OF A TOP-LEVEL ARRAY is named body[0].id, not body.[0].id, and
// its chain's canonical path is 0.id, as the ASSERT_CHAINS_WITH comparison
// reads it.
func TestProcessTestCasesV2_ATopLevelArrayElementIsNamedWithoutADot(t *testing.T) {
	cases := []*models.TestCase{
		tcase("test-1", "GET", "http://h/users", nil, "", 200, nil, `[{"id":"usr_REC01"}]`),
		tcase("test-2", "GET", "http://h/users/usr_REC01", nil, "", 200, nil, `{"ok":true}`),
	}
	_, logs := templatizeLogs(t, cases, nil)
	var captured []string
	for _, e := range logs.FilterMessageSnippet(" now captures ").All() {
		captured = append(captured, strings.Fields(e.Message)[1])
	}
	assert.Equal(t, []string{"body[0].id"}, captured)
	assert.Equal(t, "0.id", canonicalPath("body[0].id"))
	assert.Equal(t, "items.0.id", canonicalPath("body.items[0].id"))
}

// A READ-BACK'S VALUE INSIDE AN ARRAY THAT keploy test COULD NOT TIE TO ITS
// ELEMENT is left as recorded. Under test.ignoreOrdering a use inside an
// array is satisfied by any element that holds the value, and with a noise
// entry over its place, --schema-match, or assertions that judge the test
// case without json_equal, nothing else ties it to its element, so keploy
// test refuses it when the set loads. templatize writes no use there: the
// value stays as recorded, as before templatize ran, and the line says why.
// Without these, with json_equal among the assertions, or with
// ignoreOrdering off, the use is written. The request's use is written in
// every case.
func TestProcessTestCasesV2_AnUntiedReadBackInAnArrayIsLeftAsRecorded(t *testing.T) {
	const inArray, outside = `{"items":[{"id":"usr_REC01","name":"alice"}]}`, `{"id":"usr_REC01","name":"alice"}`
	var assertions map[models.AssertionType]interface{}
	cases := func(tcNoise map[string][]string, body string) []*models.TestCase {
		read := tcase("test-2", "GET", "http://h/users?ids=usr_REC01", nil, "", 200, nil, body)
		read.Noise = tcNoise
		read.Assertions = assertions
		return []*models.TestCase{
			tcase("test-1", "POST", "http://h/users", nil, `{}`, 201, nil, `{"id":"usr_REC01"}`),
			read,
		}
	}
	idNoise := config.GlobalNoise{"body": {"id": {}}}
	ordering := config.Config{Test: config.Test{IgnoreOrdering: true}}
	for _, c := range []struct {
		name       string
		cfg        config.Config
		tcNoise    map[string][]string
		written    bool
		body       string
		assertions map[models.AssertionType]interface{}
	}{
		{"ignoreOrdering, noise over the field", config.Config{Test: config.Test{IgnoreOrdering: true, GlobalNoise: config.Globalnoise{Global: idNoise}}}, nil, false, "", nil},
		{"ignoreOrdering, the test set's noise over the field", config.Config{Test: config.Test{IgnoreOrdering: true, GlobalNoise: config.Globalnoise{Testsets: config.TestsetNoise{"test-set-0": idNoise}}}}, nil, false, "", nil},
		{"ignoreOrdering, the test case's noise over the array", config.Config{Test: config.Test{IgnoreOrdering: true}}, map[string][]string{"body.items": {}}, false, "", nil},
		{"ignoreOrdering, --schema-match", config.Config{Test: config.Test{IgnoreOrdering: true, SchemaMatch: true}}, nil, false, "", nil},
		{"ignoreOrdering, no noise", config.Config{Test: config.Test{IgnoreOrdering: true}}, nil, true, "", nil},
		{"ignoreOrdering off, noise over the field", config.Config{Test: config.Test{GlobalNoise: config.Globalnoise{Global: idNoise}}}, nil, true, "", nil},
		{"ignoreOrdering, noise over a field outside any array", config.Config{Test: config.Test{IgnoreOrdering: true, GlobalNoise: config.Globalnoise{Global: idNoise}}}, nil, true, outside, nil},
		{"ignoreOrdering, a status_code assertion", ordering, nil, false, "", map[models.AssertionType]interface{}{models.StatusCode: 200}},
		{"ignoreOrdering, a header_exists assertion", ordering, nil, false, "", map[models.AssertionType]interface{}{models.HeaderExists: []interface{}{"Content-Type"}}},
		{"ignoreOrdering, json_equal among the assertions", ordering, nil, true, "", map[models.AssertionType]interface{}{models.StatusCode: 200, models.JsonEqual: true}},
		{"ignoreOrdering, a noise assertion alone", ordering, nil, true, "", map[models.AssertionType]interface{}{models.NoiseAssertion: map[string]interface{}{}}},
		{"ignoreOrdering off, a status_code assertion", config.Config{}, nil, true, "", map[models.AssertionType]interface{}{models.StatusCode: 200}},
	} {
		t.Run(c.name, func(t *testing.T) {
			assertions = c.assertions
			cfg := c.cfg
			core, logs := observer.New(zap.InfoLevel)
			db := &placementTestDB{written: map[string]models.TestCase{}}
			conf := &placementConfStore{stored: models.TestSet{}}
			tools := &Tools{logger: zap.New(core), testDB: db, testSetConf: conf, config: &cfg}
			body := c.body
			if body == "" {
				body = inArray
			}
			require.NoError(t, tools.ProcessTestCasesV2(context.Background(), cases(c.tcNoise, body), "test-set-0"))
			assert.Equal(t, `{"id":"{{capture string .id}}"}`, db.written["test-1"].HTTPResp.Body)
			assert.Equal(t, "http://h/users?ids={{string .id}}", db.written["test-2"].HTTPReq.URL, "the request's use is written")
			said := logs.FilterMessageSnippet("holds a read-back's value inside an array, and it stays as recorded").All()
			if c.written {
				assert.Equal(t, strings.ReplaceAll(body, `"usr_REC01"`, `"{{string .id}}"`), db.written["test-2"].HTTPResp.Body)
				assert.Empty(t, said)
				return
			}
			assert.Equal(t, `{"items":[{"id":"usr_REC01","name":"alice"}]}`, db.written["test-2"].HTTPResp.Body, "no use keploy test would refuse")
			require.Len(t, said, 1)
			assert.True(t, strings.HasPrefix(said[0].Message, "test-set-0/test-2 body.items[0].id holds"), "the line names the test set, the test case and the place: %s", said[0].Message)
			// The one thing that unties it, and no other.
			causes := map[string]string{
				"noise":      "the noise entry body.",
				"schema":     "test.schemaMatch is on",
				"assertions": "the test case's assertions (",
			}
			want := "noise"
			switch {
			case c.cfg.Test.SchemaMatch:
				want = "schema"
			case c.assertions != nil:
				want = "assertions"
			}
			for name, cause := range causes {
				if name == want {
					assert.Contains(t, said[0].Message, cause)
				} else {
					assert.NotContains(t, said[0].Message, cause)
				}
			}
		})
	}
}

// THE PRODUCER IS THE FIRST PLACE OF ITS RESPONSE WHERE A CAPTURE MAY STAND
// (R7w, R7x, R7c, R7t). A part of a URL-valued header that may not capture
// (a 302's Location, any Content-Location) is passed over for a later place
// of the same response, and stays as recorded, as main compares it: a login
// that sets its session cookie and carries the id in its redirect captures
// the cookie, and the redirect is still asserted as written (said once, with
// the use as the edit for a session issued anew); a 201 with a
// Content-Location and a Location both naming the id captures in the
// Location, and the Content-Location stays as recorded. Before, the first
// place was taken whatever it was, so the header-name order decided:
// "Location" sorts before "Set-Cookie" and nothing was captured, while
// "location" sorts after it and the cookie was captured. Header names are
// read whatever their letter case.
func TestProcessTestCasesV2_TheProducerIsTheFirstPlaceThatMayCapture(t *testing.T) {
	// The lines name the recorded values only with logcontent on.
	t.Cleanup(logcontent.Set(true))
	form := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	login := func(location string) []*models.TestCase {
		return []*models.TestCase{
			tcase("test-1", "POST", "http://h/login", form, `u=a&p=b`, 302, map[string]string{location: "/home?sid=s_REC1", "Set-Cookie": "sid=s_REC1; Path=/"}, ``),
			tcase("test-2", "GET", "http://h/me", map[string]string{"Cookie": "sid=s_REC1"}, "", 200, nil, `{"ok":true}`),
		}
	}
	var outputs []string
	for _, name := range []string{"Location", "location", "LOCATION"} {
		written, logs := templatizeLogs(t, login(name), nil)
		assert.Equal(t, "sid={{capture string .sid}}; Path=/", written["test-1"].HTTPResp.Header["Set-Cookie"], name)
		assert.Equal(t, "/home?sid=s_REC1", written["test-1"].HTTPResp.Header[name], "%s: the redirect stays as recorded", name)
		assert.Equal(t, "sid={{string .sid}}", written["test-2"].HTTPReq.Header["Cookie"])
		said := logs.FilterMessageSnippet("stays compared as recorded").All()
		require.Len(t, said, 1, name)
		assert.Contains(t, said[0].Message, `test-set-0/test-1 response header `+name+` of a 302 response holds `+name+`.query.sid "s_REC1"`)
		assert.Equal(t, `if the value is one its response issues anew each run, write {{string .sid}} in place of "s_REC1": the header then asserts it names the value its response captures, and every other value of it stays as recorded (a header value that holds a use of a key its response captures holds no capture); if it is the same every run (a redirect's target), nothing to do`,
			fmt.Sprint(said[0].ContextMap()["next_step"]), name)
		w := written["test-1"]
		w.HTTPResp.Header = map[string]string{"Location": w.HTTPResp.Header[name], "Set-Cookie": w.HTTPResp.Header["Set-Cookie"]}
		written["test-1"] = w
		outputs = append(outputs, writtenFiles(written, nil))
	}
	assert.Equal(t, outputs[0], outputs[1], "the output does not depend on the header name's letter case")
	assert.Equal(t, outputs[0], outputs[2])

	// The redirect is compared as recorded: a fresh cookie beside the
	// recorded redirect passes, and a redirect naming another session fails,
	// as on main.
	written, template, _ := templatizeConf(t, login("Location"), nil)
	pass, _ := replayOnce(t, written["test-1"], template, models.HTTPResp{StatusCode: 302, Header: map[string]string{"Location": "/home?sid=s_REC1", "Set-Cookie": "sid=s_LIVE2; Path=/"}})
	assert.True(t, pass)
	pass, _ = replayOnce(t, written["test-1"], template, models.HTTPResp{StatusCode: 302, Header: map[string]string{"Location": "/home?sid=s_LIVE2", "Set-Cookie": "sid=s_LIVE2; Path=/"}})
	assert.False(t, pass, "the redirect is asserted as written")

	// A 201 with a Content-Location and a Location naming the id (R7c, T3),
	// with the id in its body or without (R7t): the Location captures, and
	// the Content-Location stays as recorded, said as a place that still
	// holds the captured value.
	for _, body := range []string{``, `{"n":"a"}`} {
		cases := []*models.TestCase{
			tcase("test-1", "POST", "http://h/users", nil, `{"n":"a"}`, 201, map[string]string{"Content-Location": "/users/u_REC1", "Location": "/users/u_REC1"}, body),
			tcase("test-2", "GET", "http://h/users/u_REC1", nil, "", 200, nil, `{"id":"u_REC1"}`),
		}
		written, logs := templatizeLogs(t, cases, nil)
		assert.Equal(t, "/users/{{capture string .users}}", written["test-1"].HTTPResp.Header["Location"])
		assert.Equal(t, "/users/u_REC1", written["test-1"].HTTPResp.Header["Content-Location"])
		assert.Equal(t, "http://h/users/{{string .users}}", written["test-2"].HTTPReq.URL)
		assert.Len(t, logs.FilterMessageSnippet("test-set-0/test-1 Content-Location holds the value users captures and stays as recorded").All(), 1)
		said := logs.FilterMessageSnippet("stays compared as recorded").All()
		require.Len(t, said, 1)
		assert.Contains(t, said[0].Message, `test-set-0/test-1 response header Content-Location of a 201 response holds Content-Location.path.1 "u_REC1"`)
		assert.Contains(t, fmt.Sprint(said[0].ContextMap()["next_step"]), `if the value is one its response issues anew each run, write {{string .users}} in place of "u_REC1"`)
	}

	// A 201 Location part that holds a delimiter of its part (a%2Fb) is
	// passed over for the cookie its response sets to the same value.
	files := []*models.TestCase{
		tcase("test-1", "POST", "http://h/files", nil, `{}`, 201, map[string]string{"Location": "/files/a%2Fb", "Set-Cookie": "f=a/b; Path=/"}, ``),
		tcase("test-2", "GET", "http://h/me", map[string]string{"Cookie": "f=a/b"}, "", 200, nil, `{"ok":true}`),
	}
	written, _ = templatizeLogs(t, files, nil)
	assert.Equal(t, "f={{capture string .f}}; Path=/", written["test-1"].HTTPResp.Header["Set-Cookie"])
	assert.Equal(t, "/files/a%2Fb", written["test-1"].HTTPResp.Header["Location"], "a part that reads as two is written out")

	// Under ignoreOrdering, a value an array gives first is captured where
	// its response gives it outside the array.
	cfg := &config.Config{Test: config.Test{IgnoreOrdering: true}}
	listed := []*models.TestCase{
		tcase("test-1", "POST", "http://h/users", nil, `{}`, 201, nil, `{"items":[{"id":"usr_REC1"}],"self":"usr_REC1"}`),
		tcase("test-2", "GET", "http://h/users/usr_REC1", nil, "", 200, nil, `{"ok":true}`),
	}
	written, _ = runTemplatize(t, listed, nil, cfg)
	assert.Equal(t, `{"items":[{"id":"usr_REC1"}],"self":"{{capture string .self}}"}`, written["test-1"].HTTPResp.Body)
	assert.Equal(t, "http://h/users/{{string .self}}", written["test-2"].HTTPReq.URL)

	// A 201 whose Location and Set-Cookie both give the session id: the
	// Location comes first by name, whatever its letter case.
	var created []string
	for _, name := range []string{"Location", "location"} {
		cases := []*models.TestCase{
			tcase("test-1", "POST", "http://h/sessions", nil, `{}`, 201, map[string]string{name: "/sessions/s_REC1", "Set-Cookie": "sid=s_REC1; Path=/"}, ``),
			tcase("test-2", "GET", "http://h/me", map[string]string{"Cookie": "sid=s_REC1"}, "", 200, nil, `{"ok":true}`),
		}
		written, _ := templatizeLogs(t, cases, nil)
		w := written["test-1"]
		assert.Equal(t, "/sessions/{{capture string .sessions}}", w.HTTPResp.Header[name])
		w.HTTPResp.Header = map[string]string{"Location": w.HTTPResp.Header[name], "Set-Cookie": w.HTTPResp.Header["Set-Cookie"]}
		written["test-1"] = w
		created = append(created, writtenFiles(written, nil))
	}
	assert.Equal(t, created[0], created[1], "the output does not depend on the header name's letter case")
}

// A TARGET BESIDE A CAPTURE OF ITS RESPONSE STAYS COMPARED AS WRITTEN (R8a,
// R8c, R8s, R8d). Only where a capture itself could stand, the one candidate
// of a 201's Location, does the producer's own response get a use of its
// capture. A redirect's target, a Content-Location and a crowded 201
// Location stay as recorded, as main compares them: a use there would accept
// whatever the response's capture takes, so a constant regressed in both
// places (a language redirect /en/home to /fr/home beside its lang cookie, a
// catalog section, a constant session id) passed with templates where the
// same set without them fails. Each set below, answered with such a
// regression, fails after templatize, as the set as recorded fails, and the
// header is written as recorded whatever its name's letter case.
func TestProcessTestCasesV2_ATargetBesideACaptureStaysComparedAsWritten(t *testing.T) {
	// The lines name the recorded values only with logcontent on.
	t.Cleanup(logcontent.Set(true))
	form := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	fine := models.HTTPResp{StatusCode: 200, Header: map[string]string{}, Body: `{"ok":true}`}
	for _, c := range []struct {
		name    string
		cases   []*models.TestCase
		header  string // the header that stays as recorded
		value   string
		capture string // what templatize captures instead, in test-1
		answer  models.HTTPResp
	}{
		{"R8a a language redirect beside its cookie", []*models.TestCase{
			tcase("test-1", "POST", "http://h/login", form, `u=a&p=b`, 302, map[string]string{"Location": "/en/home", "Set-Cookie": "lang=en; Path=/"}, ``),
			tcase("test-2", "GET", "http://h/en/home", map[string]string{"Cookie": "lang=en"}, "", 200, nil, `{"ok":true}`),
		}, "Location", "/en/home", "lang={{capture string .lang}}; Path=/",
			models.HTTPResp{StatusCode: 302, Header: map[string]string{"Location": "/fr/home", "Set-Cookie": "lang=fr; Path=/"}}},
		{"R8a2 the same with lower-case header names", []*models.TestCase{
			tcase("test-1", "POST", "http://h/login", form, `u=a&p=b`, 302, map[string]string{"location": "/en/home", "set-cookie": "lang=en; Path=/"}, ``),
			tcase("test-2", "GET", "http://h/en/home", map[string]string{"Cookie": "lang=en"}, "", 200, nil, `{"ok":true}`),
		}, "location", "/en/home", "lang={{capture string .lang}}; Path=/",
			models.HTTPResp{StatusCode: 302, Header: map[string]string{"location": "/fr/home", "set-cookie": "lang=fr; Path=/"}}},
		{"R8c a 200's Content-Location beside its cookie", []*models.TestCase{
			tcase("test-1", "GET", "http://h/catalog", nil, "", 200, map[string]string{"Content-Location": "/catalog/books", "Set-Cookie": "section=books; Path=/"}, `{"n":1}`),
			tcase("test-2", "GET", "http://h/catalog/books", map[string]string{"Cookie": "section=books"}, "", 200, nil, `{"ok":true}`),
		}, "Content-Location", "/catalog/books", "section={{capture string .section}}; Path=/",
			models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Location": "/catalog/toys", "Set-Cookie": "section=toys; Path=/"}, Body: `{"n":1}`}},
		{"R8s a redirect whose body gives the value first", []*models.TestCase{
			tcase("test-1", "POST", "http://h/login", nil, `{}`, 302, map[string]string{"Location": "/en/home"}, `{"lang":"en"}`),
			tcase("test-2", "GET", "http://h/en/home", nil, "", 200, nil, `{"ok":true}`),
		}, "Location", "/en/home", "",
			models.HTTPResp{StatusCode: 302, Header: map[string]string{"Location": "/fr/home"}, Body: `{"lang":"fr"}`}},
		{"R8d a crowded 201 Location beside its cookie", []*models.TestCase{
			tcase("test-1", "POST", "http://h/signup", nil, `{}`, 201, map[string]string{"Location": "/users/u_REC1?sid=s_REC1", "Set-Cookie": "sid=s_REC1; Path=/"}, ``),
			tcase("test-2", "GET", "http://h/users/u_REC1", map[string]string{"Cookie": "sid=s_REC1"}, "", 200, nil, `{"ok":true}`),
		}, "Location", "/users/u_REC1?sid=s_REC1", "sid={{capture string .sid}}; Path=/",
			models.HTTPResp{StatusCode: 201, Header: map[string]string{"Location": "/users/u_REC1?sid=s_OTHER", "Set-Cookie": "sid=s_OTHER; Path=/"}}},
		{"a 201's Content-Location beside its Location's capture of a constant", []*models.TestCase{
			tcase("test-1", "POST", "http://h/signup", nil, `{}`, 201, map[string]string{"Location": "/welcome", "Content-Location": "/welcome"}, ``),
			tcase("test-2", "GET", "http://h/welcome", nil, "", 200, nil, `{"ok":true}`),
		}, "Content-Location", "/welcome", "",
			models.HTTPResp{StatusCode: 201, Header: map[string]string{"Location": "/oops", "Content-Location": "/oops"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			written, template, _ := templatizeConf(t, c.cases, nil)
			t1 := written["test-1"]
			assert.Equal(t, c.value, t1.HTTPResp.Header[c.header], "the target stays as recorded")
			if c.capture != "" {
				cookie := "Set-Cookie"
				if _, lower := t1.HTTPResp.Header["set-cookie"]; lower {
					cookie = "set-cookie"
				}
				assert.Equal(t, c.capture, t1.HTTPResp.Header[cookie])
			}
			var all []*models.TestCase
			for _, tc := range c.cases {
				w, ok := written[tc.Name]
				if !ok {
					w = *tc
				}
				all = append(all, &w)
			}
			require.Empty(t, templates.NewRun(nil, template, nil).Check(all, templates.CheckOptions{}).Refused)
			answers := map[string]models.HTTPResp{"test-1": c.answer, "test-2": fine}
			failed, _ := replaySet(t, all, template, answers)
			assert.Equal(t, []string{"test-1"}, failed, "the regression fails with templates")
			failed, _ = replaySet(t, c.cases, nil, answers)
			assert.Equal(t, []string{"test-1"}, failed, "as it fails without them")
		})
	}

	// R8d's crowded Location names each of its values, the created id
	// included; a second run, over a set that now holds placeholders, writes
	// nothing.
	r8d := []*models.TestCase{
		tcase("test-1", "POST", "http://h/signup", nil, `{}`, 201, map[string]string{"Location": "/users/u_REC1?sid=s_REC1", "Set-Cookie": "sid=s_REC1; Path=/"}, ``),
		tcase("test-2", "GET", "http://h/users/u_REC1", map[string]string{"Cookie": "sid=s_REC1"}, "", 200, nil, `{"ok":true}`),
	}
	written, template, logs := templatizeConf(t, r8d, nil)
	crowded := logs.FilterMessageSnippet("holds more than one value").All()
	require.Len(t, crowded, 1)
	assert.Contains(t, crowded[0].Message, `(Location.path.0 "users", Location.path.1 "u_REC1", Location.query.sid "s_REC1")`)
	t1, t2 := written["test-1"], written["test-2"]
	again, _, logs := templatizeAll(t, []*models.TestCase{&t1, &t2}, template, nil)
	assert.Empty(t, again)
	assertLeftAsItIs(t, logs, caseFiles(t, []*models.TestCase{&t1, &t2}), &heldAt{text: "{{capture string .sid}}", file: "test-1.yaml", line: "Set-Cookie:…sid={{capture string .sid}}; Path=/"})
}

// THE PRODUCER IS IN THE RESPONSE THAT GIVES THE VALUE FIRST (R8T-13). A
// session id a redirect gives first, which a later read answers again and a
// later request sends, is not captured in that later read: the value was
// issued by the redirect, where no capture may stand, and a capture in the
// read would accept any id there and send it on. Nothing is written, and the
// redirect's line names the value.
func TestProcessTestCasesV2_TheProducerIsInTheResponseThatGivesTheValueFirst(t *testing.T) {
	// The lines name the recorded values only with logcontent on.
	t.Cleanup(logcontent.Set(true))
	form := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	cases := []*models.TestCase{
		tcase("test-1", "POST", "http://h/login", form, `u=a&p=b`, 302, map[string]string{"Location": "/home?sid=s_REC1"}, ``),
		tcase("test-2", "GET", "http://h/session", nil, "", 200, nil, `{"sid":"s_REC1"}`),
		tcase("test-3", "GET", "http://h/me", map[string]string{"Cookie": "sid=s_REC1"}, "", 200, nil, `{"ok":true}`),
	}
	written, logs := templatizeLogs(t, cases, nil)
	assert.Empty(t, written, "nothing is captured in a later response")
	said := logs.FilterMessageSnippet("stays compared as recorded").All()
	require.Len(t, said, 1)
	assert.Contains(t, said[0].Message, `test-set-0/test-1 response header Location of a 302 response holds Location.query.sid "s_REC1"`)
}

// EACH LINE ABOUT A VALUE TEMPLATIZE LEAVES NAMES ITS TEST SET, TEST CASE AND
// PLACE, so a line among many can be found.
func TestProcessTestCasesV2_ALineAboutAValueLeftNamesItsPlace(t *testing.T) {
	ordering := &config.Config{Test: config.Test{IgnoreOrdering: true}}
	core, logs := observer.New(zap.InfoLevel)
	db := &placementTestDB{written: map[string]models.TestCase{}}
	tools := &Tools{logger: zap.New(core), testDB: db, testSetConf: &placementConfStore{}, config: ordering}
	cases := []*models.TestCase{
		tcase("test-1", "POST", "http://h/users", nil, `{}`, 201, nil, `{"items":[{"id":"usr_REC1"}]}`),
		tcase("test-2", "GET", "http://h/users/usr_REC1", nil, "", 200, nil, `{"ok":true}`),
	}
	require.NoError(t, tools.ProcessTestCasesV2(context.Background(), cases, "test-set-0"))
	lines := logs.FilterMessageSnippet("inside an array").All()
	require.Len(t, lines, 1)
	assert.True(t, strings.HasPrefix(lines[0].Message, "test-set-0/test-1 body.items[0].id gives a value a later request sends"), lines[0].Message)
	// A value it found and left, said above, is not "no value".
	assert.Empty(t, logs.FilterMessageSnippet("found no value").All())
}

// EACH PLACE OF A CAPTURE'S RESPONSE LEFT WRITTEN OUT IS SAID WITH ITS EDIT
// (round 10): a JSON number by a number use, written in quotes as
// templatize writes one, a string by a string use; and a header value that
// holds a capture is told no use can stand beside it (keploy test refuses a
// header capture beside a use of a key its response captures).
func TestProcessTestCasesV2_APlaceLeftWrittenOutIsSaidWithItsEdit(t *testing.T) {
	// The lines name the recorded values only with logcontent on.
	t.Cleanup(logcontent.Set(true))
	js := map[string]string{"Content-Type": "application/json"}
	number := []*models.TestCase{
		tcase("test-1", "POST", "http://h/orders", js, `{}`, 201, js, `{"id":7654321,"copy":7654321,"ref":"o-7654321"}`),
		tcase("test-2", "POST", "http://h/pay", js, `{"id":7654321}`, 200, js, `{"ok":true}`),
	}
	written, template, logs := templatizeConf(t, number, nil)
	assert.Equal(t, `{"id":"{{capture int .id}}","copy":7654321,"ref":"o-7654321"}`, written["test-1"].HTTPResp.Body)
	next := map[string]string{}
	for _, e := range logs.FilterMessageSnippet("holds the value id captures and stays as recorded").All() {
		next[strings.Fields(e.Message)[1]] = fmt.Sprint(e.ContextMap()["next_step"])
	}
	assert.Contains(t, next["body.copy"], `write "{{int .id}}" in place of "7654321" there`)
	assert.Contains(t, next["body.ref"], `write {{string .id}} in place of "7654321" there`)
	// Applied, both pass keploy test's set check and a fresh number.
	t1, t2 := written["test-1"], written["test-2"]
	t1.HTTPResp.Body = `{"id":"{{capture int .id}}","copy":"{{int .id}}","ref":"o-{{string .id}}"}`
	applied := []*models.TestCase{&t1, &t2}
	require.Empty(t, templates.NewRun(nil, template, nil).Check(applied, templates.CheckOptions{}).Refused)
	failed, _ := replaySet(t, applied, template, map[string]models.HTTPResp{
		"test-1": {StatusCode: 201, Header: js, Body: `{"id":8000001,"copy":8000001,"ref":"o-8000001"}`},
		"test-2": {StatusCode: 200, Header: js, Body: `{"ok":true}`},
	})
	assert.Empty(t, failed)

	besideCapture := []*models.TestCase{
		tcase("test-1", "POST", "http://h/users", js, `{}`, 201, map[string]string{"Location": "/users/u_REC1?ref=u_REC1"}, ``),
		tcase("test-2", "GET", "http://h/users/u_REC1", nil, "", 200, js, `{"ok":true}`),
	}
	written, logs = templatizeLogs(t, besideCapture, nil)
	assert.Equal(t, "/users/{{capture string .users}}?ref=u_REC1", written["test-1"].HTTPResp.Header["Location"])
	left := logs.FilterMessageSnippet("test-set-0/test-1 Location holds the value users captures and stays as recorded").All()
	require.Len(t, left, 1)
	assert.Equal(t, "no use can stand there (its header value holds a capture, beside which a use of a key its response captures is refused): it stays as recorded",
		fmt.Sprint(left[0].ContextMap()["next_step"]))

	// A copy inside an array under ignoreOrdering is offered its use only
	// where keploy test could tie it to its element: under a noise entry
	// over the field it would be refused, so none is offered.
	listed := func() []*models.TestCase {
		return []*models.TestCase{
			tcase("test-1", "POST", "http://h/users", js, `{}`, 201, js, `{"id":"u_REC1","items":[{"id":"u_REC1"}]}`),
			tcase("test-2", "GET", "http://h/users/u_REC1", nil, "", 200, js, `{"ok":true}`),
		}
	}
	for _, c := range []struct {
		name  string
		cfg   *config.Config
		offer bool
	}{
		{"no noise", &config.Config{Test: config.Test{IgnoreOrdering: true}}, true},
		{"noise over the field", &config.Config{Test: config.Test{IgnoreOrdering: true, GlobalNoise: config.Globalnoise{Global: config.GlobalNoise{"body": {"items.id": {}}}}}}, false},
	} {
		_, logs := templatizeLogsWith(t, listed(), nil, c.cfg)
		left := logs.FilterMessageSnippet("test-set-0/test-1 body.items[0].id holds the value id captures").All()
		require.Len(t, left, 1, c.name)
		next := fmt.Sprint(left[0].ContextMap()["next_step"])
		if c.offer {
			assert.Contains(t, next, `write {{string .id}} in place of "u_REC1" there`, c.name)
			continue
		}
		assert.Equal(t, "no use can stand there (it is inside an array and test.ignoreOrdering is on and the noise entry body.items.id (globalNoise) covers it, so nothing would tie a use there to its element and keploy test would refuse it): it stays as recorded", next, c.name)
	}

	// Likewise an object key inside such an array.
	keyed := func() []*models.TestCase {
		return []*models.TestCase{
			tcase("test-1", "POST", "http://h/users", js, `{}`, 201, js, `{"id":"u_REC1","items":[{"u_REC1":true}]}`),
			tcase("test-2", "GET", "http://h/users/u_REC1", nil, "", 200, js, `{"ok":true}`),
		}
	}
	for _, c := range []struct {
		name string
		cfg  *config.Config
		want string
	}{
		{"no noise", &config.Config{Test: config.Test{IgnoreOrdering: true}}, `if the app issues the value anew each run, write {{string .id}} in place of "u_REC1" there`},
		{"noise over the array", &config.Config{Test: config.Test{IgnoreOrdering: true, GlobalNoise: config.Globalnoise{Global: config.GlobalNoise{"body": {"items": {}}}}}}, "no use can stand there (it is inside an array and test.ignoreOrdering is on and the noise entry body.items (globalNoise) covers it"},
	} {
		_, logs := templatizeLogsWith(t, keyed(), nil, c.cfg)
		left := logs.FilterMessageSnippet("test-set-0/test-1 body key ").All()
		require.Len(t, left, 1, c.name)
		assert.Contains(t, left[0].Message, "holds the value id captures and stays as recorded", c.name)
		assert.True(t, strings.HasPrefix(fmt.Sprint(left[0].ContextMap()["next_step"]), c.want), "%s: %s", c.name, left[0].ContextMap()["next_step"])
	}

	// A float copy is offered a float use, and a longer number that holds a
	// captured one whole is told no use can stand inside it. A cookie that
	// holds the value is offered a use, and so is a header value beside a use
	// of another key (the org a read-back's Content-Location names): only a
	// capture in a header value leaves no room for one.
	mixed := []*models.TestCase{
		tcase("test-1", "GET", "http://h/orgs", nil, "", 200, js, `{"org":"o_REC1"}`),
		tcase("test-2", "POST", "http://h/orgs/o_REC1/members", js, `{}`, 201,
			map[string]string{"Content-Type": "application/json", "Content-Location": "/orgs/o_REC1/members/m_REC2", "Set-Cookie": "last=m_REC2; Path=/"},
			`{"id":"m_REC2","price":19.99,"was":19.99,"n":7654321,"v":7654321.5}`),
		tcase("test-3", "POST", "http://h/pay", js, `{"member":"m_REC2","price":19.99,"n":7654321}`, 200, js, `{"ok":true}`),
	}
	written, logs = templatizeLogs(t, mixed, nil)
	assert.Equal(t, `{"id":"{{capture string .id}}","price":"{{capture float .price}}","was":19.99,"n":"{{capture int .n}}","v":7654321.5}`, written["test-2"].HTTPResp.Body)
	assert.Equal(t, "/orgs/{{string .org}}/members/m_REC2", written["test-2"].HTTPResp.Header["Content-Location"])
	steps := map[string]string{}
	for _, e := range logs.FilterMessageSnippet("stays as recorded; this test case fails there").All() {
		steps[strings.Fields(e.Message)[1]] = fmt.Sprint(e.ContextMap()["next_step"])
	}
	assert.Equal(t, map[string]string{
		"body.was":         `if the app issues the value anew each run, write "{{float .price}}" in place of "19.99" there: that place then asserts it holds what this same answer gives at the capture of price, whatever that is (a constant's regression in both places included); if it is the same every run, nothing to do`,
		"body.v":           "no use can stand there (the value is part of a JSON number there, which a use cannot stand inside): it stays as recorded",
		"Content-Location": `if the app issues the value anew each run, write {{string .id}} in place of "m_REC2" there: that place then asserts it holds what this same answer gives at the capture of id, whatever that is (a constant's regression in both places included); if it is the same every run, nothing to do`,
		"Set-Cookie.last":  `if the app issues the value anew each run, write {{string .id}} in place of "m_REC2" there: that place then asserts it holds what this same answer gives at the capture of id, whatever that is (a constant's regression in both places included); if it is the same every run, nothing to do`,
	}, steps)
}

// A VALUE THAT IDENTIFIES NOTHING IS NEVER A KEY. A zero, true, false, null
// and nil recur by chance: a response that holds one and a later request that
// sends one are no chain, so nothing is captured there (a count of 0 never
// becomes the offset a later request sends), whatever its JSON type.
func TestProcessTestCasesV2_AValueThatIdentifiesNothingIsNeverAKey(t *testing.T) {
	js := map[string]string{"Content-Type": "application/json"}
	stats := `{"count":0,"ratio":0.0,"flag":"true","off":"False","none":"null","nothing":"nil","id":"u_REC1"}`
	cases := []*models.TestCase{
		tcase("test-1", "GET", "http://h/stats", nil, "", 200, js, stats),
		tcase("test-2", "GET", "http://h/items?offset=0&flag=true&off=False&none=null&nothing=nil", nil, "", 200, js, `{"ok":true}`),
		tcase("test-3", "POST", "http://h/page", js, `{"offset":0,"ratio":0.0,"flag":"true"}`, 200, js, `{"ok":true}`),
		tcase("test-4", "GET", "http://h/users/u_REC1", nil, "", 200, js, `{"ok":true}`),
	}
	written, conf := runTemplatize(t, cases, nil, nil)
	assert.Equal(t, strings.Replace(stats, `"u_REC1"`, `"{{capture string .id}}"`, 1), written["test-1"].HTTPResp.Body)
	assert.NotContains(t, written, "test-2")
	assert.NotContains(t, written, "test-3")
	assert.Equal(t, "http://h/users/{{string .id}}", written["test-4"].HTTPReq.URL)
	require.NotNil(t, conf)
	assert.Equal(t, map[string]interface{}{"id": "u_REC1"}, conf.Template)
}

// A STREAMED RESPONSE NEVER CAPTURES. A streamed test case runs after the
// others, and keploy test refuses a capture in it; so a cookie a streamed
// response sets, which a later request sends, is not a chain: nothing is
// written, and the set is as recorded.
func TestProcessTestCasesV2_AStreamedResponseProducesNothing(t *testing.T) {
	js := map[string]string{"Content-Type": "application/json"}
	sse := tcase("test-1", "GET", "http://h/events", nil, "", 200, map[string]string{"Content-Type": "text/event-stream", "Set-Cookie": "sid=s_REC1abc; Path=/"}, "")
	sse.HTTPResp.StreamBody = []models.HTTPStreamChunk{{Data: []models.HTTPStreamDataField{{Key: "data", Value: `{"n":"s_REC1abc"}`}}}}
	cases := []*models.TestCase{
		sse,
		tcase("test-2", "GET", "http://h/me", map[string]string{"Cookie": "sid=s_REC1abc"}, "", 200, js, `{"ok":true}`),
	}
	written, conf, logs := templatizeAll(t, cases, nil, nil)
	assert.Empty(t, written, "%s", writtenFiles(written, conf))
	assert.Nil(t, conf)
	assert.Len(t, logs.FilterMessageSnippet("found no value a response produces and a later request sends").All(), 1)
	// The capture it would have written is one keploy test refuses.
	sse.HTTPResp.Header["Set-Cookie"] = "sid={{capture string .sid}}; Path=/"
	refused := templates.NewRun(nil, map[string]interface{}{"sid": "s_REC1abc"}, nil).Check(cases, templates.CheckOptions{Streaming: func(tc *models.TestCase) bool { return tc.Name == "test-1" }}).Refused
	assert.Len(t, refused["test-1"], 1)
}

// MORE PLACES LEFT WRITTEN OUT, EACH SAID OR NOT AS IT SHOULD BE: a body that
// is not JSON holding the value its response's cookie captures is said; a
// header that holds the captured value percent-encoded is said (it is read
// decoded too); and a JSON value a later occurrence of its key replaces is
// not the body's value there, so it is not said.
func TestProcessTestCasesV2_APlaceLeftWrittenOutIsReadAsTheReplayReadsIt(t *testing.T) {
	// The lines name the recorded values only with logcontent on.
	t.Cleanup(logcontent.Set(true))
	js := map[string]string{"Content-Type": "application/json"}
	said := func(logs *observer.ObservedLogs) map[string]string {
		out := map[string]string{}
		for _, e := range logs.FilterMessageSnippet("stays as recorded; this test case fails there").All() {
			out[strings.Fields(e.Message)[1]] = fmt.Sprint(e.ContextMap()["next_step"])
		}
		return out
	}

	html := []*models.TestCase{
		tcase("test-1", "POST", "http://h/login", js, `{}`, 200, map[string]string{"Set-Cookie": "sid=s_REC1abc; Path=/"}, `<html>session s_REC1abc</html>`),
		tcase("test-2", "GET", "http://h/me", map[string]string{"Cookie": "sid=s_REC1abc"}, "", 200, js, `{"ok":true}`),
	}
	written, logs := templatizeLogs(t, html, nil)
	assert.Equal(t, "sid={{capture string .sid}}; Path=/", written["test-1"].HTTPResp.Header["Set-Cookie"])
	assert.Equal(t, map[string]string{
		"body": `if the app issues the value anew each run, write {{string .sid}} in place of "s_REC1abc" there: that place then asserts it holds what this same answer gives at the capture of sid, whatever that is (a constant's regression in both places included); if it is the same every run, nothing to do`,
	}, said(logs))

	encoded := []*models.TestCase{
		tcase("test-1", "POST", "http://h/files", js, `{}`, 201, map[string]string{"Content-Type": "application/json", "X-Res": "/files/a%20b_REC1"}, `{"name":"a b_REC1"}`),
		tcase("test-2", "POST", "http://h/open", js, `{"name":"a b_REC1"}`, 200, js, `{"ok":true}`),
	}
	written, logs = templatizeLogs(t, encoded, nil)
	assert.Equal(t, `{"name":"{{capture string .name}}"}`, written["test-1"].HTTPResp.Body)
	assert.Equal(t, "/files/a%20b_REC1", written["test-1"].HTTPResp.Header["X-Res"])
	assert.Equal(t, map[string]string{
		"X-Res": `if the app issues the value anew each run, write {{string .name}} in place of "a b_REC1" there: that place then asserts it holds what this same answer gives at the capture of name, whatever that is (a constant's regression in both places included); if it is the same every run, nothing to do`,
	}, said(logs))

	replaced := []*models.TestCase{
		tcase("test-1", "POST", "http://h/users", js, `{}`, 201, js, `{"dup":"u_REC1abc","dup":"other","id":"u_REC1abc"}`),
		tcase("test-2", "GET", "http://h/users/u_REC1abc", nil, "", 200, js, `{"ok":true}`),
	}
	written, logs = templatizeLogs(t, replaced, nil)
	assert.Equal(t, `{"dup":"u_REC1abc","dup":"other","id":"{{capture string .id}}"}`, written["test-1"].HTTPResp.Body)
	assert.Empty(t, said(logs), "the first dup is no value of the body: a reader keeps the last")
}

// writeBack follows, on one text of a test case, the edit heldStep gives: in
// place of each placeholder what the recording held there, which is its
// key's value in config.yaml's template, and for a placeholder of type int
// or float that stands in quotes as a whole JSON value, that number without
// the quotes.
func writeBack(text string, template map[string]interface{}) string {
	const key = `([\p{L}_][\p{L}\p{Nd}_]*)`
	value := func(re *regexp.Regexp) func(string) string {
		return func(m string) string { return fmt.Sprint(template[re.FindStringSubmatch(m)[1]]) }
	}
	quotedNumber := regexp.MustCompile(`"\{\{(?:capture )?(?:int|float) \.` + key + `\}\}"`)
	text = quotedNumber.ReplaceAllStringFunc(text, value(quotedNumber))
	placeholder := regexp.MustCompile(`\{\{(?:capture )?(?:string|int|float) \.` + key + `\}\}`)
	return placeholder.ReplaceAllStringFunc(text, value(placeholder))
}

// THE EDIT THAT GIVES THE PLAIN RECORDING BACK IS RIGHT FOR A VALUE THAT NEEDS
// A JSON ESCAPE. config.yaml holds a value as the app gave it, decoded, so a
// token holding a " or a \ written back as it stands into a JSON string
// leaves a body that is not JSON. The line says such a value goes back with
// JSON's escapes; followed so, it gives the recording back, and templatize
// then writes the same files again.
func TestProcessTestCasesV2_TheWriteBackEditEscapesAValueInAJSONString(t *testing.T) {
	js := map[string]string{"Content-Type": "application/json"}
	recording := func() []*models.TestCase {
		return []*models.TestCase{
			tcase("test-1", "POST", "http://h/login", js, `{}`, 200, js, `{"tok":"ab\"cd_REC1x","dir":"C:\\dir_REC2x"}`),
			tcase("test-2", "POST", "http://h/use", js, `{"tok":"ab\"cd_REC1x","dir":"C:\\dir_REC2x"}`, 200, js, `{"ok":true}`),
		}
	}
	plain := recording()
	written, conf := runTemplatize(t, recording(), nil, nil)
	require.NotNil(t, conf)
	assert.Equal(t, map[string]interface{}{"tok": `ab"cd_REC1x`, "dir": `C:\dir_REC2x`}, conf.Template)
	assert.Equal(t, `{"tok":"{{string .tok}}","dir":"{{string .dir}}"}`, written["test-2"].HTTPReq.Body)
	assert.Contains(t, heldStep, `a value inside a JSON string goes back with JSON's escapes`)

	escaped := map[string]interface{}{}
	for k, v := range conf.Template {
		b, err := json.Marshal(v)
		require.NoError(t, err)
		escaped[k] = strings.TrimSuffix(strings.TrimPrefix(string(b), `"`), `"`)
	}
	var back []*models.TestCase
	for _, tc := range plain {
		c := written[tc.Name]
		c.HTTPReq.Body = writeBack(c.HTTPReq.Body, escaped)
		c.HTTPResp.Body = writeBack(c.HTTPResp.Body, escaped)
		assert.Equal(t, tc.HTTPReq.Body, c.HTTPReq.Body, tc.Name)
		assert.Equal(t, tc.HTTPResp.Body, c.HTTPResp.Body, tc.Name)
		back = append(back, &c)
	}
	// Written back as it stands, the value breaks the body's JSON.
	assert.False(t, json.Valid([]byte(writeBack(written["test-2"].HTTPReq.Body, conf.Template))))
	again, conf2 := runTemplatize(t, back, conf.Template, nil)
	assert.Equal(t, writtenFiles(written, conf), writtenFiles(again, conf2))
}

// THE EDIT THAT GIVES THE PLAIN RECORDING BACK IS RIGHT FOR A NUMBER. The line
// for a set that holds placeholders says to write back what the recording
// held in place of each one, a number templatize wrote in quotes going back
// without them. Followed to the letter on templatize's own output for a set
// whose id is a JSON number (the shape of samples-go/go-grpc: a create
// answering {"user":{"id":1}}, then an update and a delete that send "id":
// 1), it gives the recording back byte for byte, and templatize then writes
// the same files again. Before, the line said only "its recorded value": the
// id went back as "1", a string, templatize captured {{capture string .id}},
// and the set failed on the app's own recorded answers.
func TestProcessTestCasesV2_TheWriteBackEditGivesTheRecordingBack(t *testing.T) {
	js := map[string]string{"Content-Type": "application/json"}
	recording := func() []*models.TestCase {
		return []*models.TestCase{
			tcase("test-1", "POST", "http://localhost:8080/users", js, "{\n    \"name\": \"test1\",\n    \"age\": 20\n}", 200, js, `{"user":{"id":1,"name":"test1","age":20,"score":7.5,"ref":"r_REC1"}}`),
			tcase("test-2", "GET", "http://localhost:8080/users", nil, "", 200, js, `{"users":[{"id":1,"name":"test1","age":20}]}`),
			tcase("test-3", "PUT", "http://localhost:8080/users/r_REC1", js, "{\n        \"id\": 1,\n        \"score\": 7.5,\n        \"name\": \"test-user-updated\"\n    }", 200, js, `{"user":{"id":1,"name":"test-user-updated","score":7.5}}`),
			tcase("test-4", "DELETE", "http://localhost:8080/users", js, "{\n        \"id\": 1\n    }", 200, js, `{"message":"User deleted"}`),
		}
	}
	plain := recording()
	written, conf := runTemplatize(t, recording(), nil, nil)
	require.NotNil(t, conf)
	assert.Equal(t, map[string]interface{}{"id": "1", "score": "7.5", "ref": "r_REC1"}, conf.Template)
	assert.Equal(t, `{"user":{"id":"{{capture int .id}}","name":"test1","age":20,"score":"{{capture float .score}}","ref":"{{capture string .ref}}"}}`, written["test-1"].HTTPResp.Body)
	assert.Equal(t, "{\n        \"id\": \"{{int .id}}\",\n        \"score\": \"{{float .score}}\",\n        \"name\": \"test-user-updated\"\n    }", written["test-3"].HTTPReq.Body)
	assert.Equal(t, `{"user":{"id":"{{int .id}}","name":"test-user-updated","score":"{{float .score}}"}}`, written["test-3"].HTTPResp.Body)
	assert.Contains(t, heldStep, `a placeholder of type int or float that stands in quotes as a whole JSON value ("{{int .n}}", "{{capture float .n}}") goes back as the bare number, without the quotes`)

	// The edit, applied to every text templatize wrote.
	var back []*models.TestCase
	for _, tc := range plain {
		c := *tc
		if w, ok := written[tc.Name]; ok {
			c = w
			c.HTTPReq.URL = writeBack(c.HTTPReq.URL, conf.Template)
			c.HTTPReq.Body = writeBack(c.HTTPReq.Body, conf.Template)
			c.HTTPResp.Body = writeBack(c.HTTPResp.Body, conf.Template)
		}
		assert.Equal(t, tc.HTTPReq.URL, c.HTTPReq.URL, tc.Name)
		assert.Equal(t, tc.HTTPReq.Body, c.HTTPReq.Body, tc.Name)
		assert.Equal(t, tc.HTTPResp.Body, c.HTTPResp.Body, tc.Name)
		back = append(back, &c)
	}
	// templatize, run again as the line says, writes what it wrote before,
	// reusing each key config.yaml already gives its value.
	again, conf2 := runTemplatize(t, back, conf.Template, nil)
	assert.Equal(t, writtenFiles(written, conf), writtenFiles(again, conf2))
}

// A QUERY PLACEHOLDER IS WRITTEN INTO url_params TOO. A test case keeps its
// URL's query values a second time, in url_params: the param that held the
// value its query place held gets the same placeholder (the query's value
// read decoded), so the two never disagree; any other param stays as it is.
func TestProcessTestCasesV2_AQueryPlaceholderIsWrittenIntoURLParamsToo(t *testing.T) {
	js := map[string]string{"Content-Type": "application/json"}
	read := tcase("test-2", "GET", "http://h/items?token=t_REC1abc&page=2&q=a%20b_REC2&other=t_REC1abc", nil, "", 200, js, `{"ok":true}`)
	read.HTTPReq.URLParams = map[string]string{"token": "t_REC1abc", "page": "2", "q": "a b_REC2", "other": "kept as it is"}
	cases := []*models.TestCase{
		tcase("test-1", "POST", "http://h/login", js, `{}`, 200, js, `{"token":"t_REC1abc","q":"a b_REC2"}`),
		read,
	}
	written, _ := runTemplatize(t, cases, nil, nil)
	assert.Equal(t, "http://h/items?token={{string .token}}&page=2&q={{string .q}}&other={{string .token}}", written["test-2"].HTTPReq.URL)
	want := map[string]string{"token": "{{string .token}}", "page": "2", "q": "{{string .q}}", "other": "kept as it is"}
	assert.Equal(t, want, written["test-2"].HTTPReq.URLParams)
	// Once the files are in place, the test case in memory holds what was written.
	assert.Equal(t, written["test-2"].HTTPReq.URL, read.HTTPReq.URL)
	assert.Equal(t, want, read.HTTPReq.URLParams)
}

// failingReplace is a store whose files cannot be put in place.
type failingReplace struct{ *placementTestDB }

func (f failingReplace) ReplaceFiles(context.Context, []setfile.File) error {
	return fmt.Errorf("disk full")
}

// failingCaseFile is a store that cannot write one test case's file out.
type failingCaseFile struct {
	*placementTestDB
	name string
}

func (f failingCaseFile) TestCaseFile(ctx context.Context, tc *models.TestCase, id string) (setfile.File, error) {
	if tc.Name == f.name {
		return setfile.File{}, fmt.Errorf("%s cannot be encoded", tc.Name)
	}
	return f.placementTestDB.TestCaseFile(ctx, tc, id)
}

// failingConfigFile is a config store that cannot write config.yaml out.
type failingConfigFile struct{ *placementConfStore }

func (f failingConfigFile) ConfigFile(context.Context, string, *models.TestSet) (setfile.File, error) {
	return setfile.File{}, fmt.Errorf("config.yaml cannot be encoded")
}

// everyKindOfText is a plain recording whose chains put a placeholder into
// every text of a test case templatize edits: a response header (the login's
// cookie) and body, a request URL with its url_params, a request header (the
// cookie sent back), a request body and a form value.
func everyKindOfText() []*models.TestCase {
	js := map[string]string{"Content-Type": "application/json"}
	read := tcase("test-2", "GET", "http://h/items?token=t_REC1abc", map[string]string{"Cookie": "sid=s_REC1abc"}, "", 200, js, `{"ok":true}`)
	read.HTTPReq.URLParams = map[string]string{"token": "t_REC1abc"}
	form := tcase("test-3", "POST", "http://h/token", nil, "", 200, js, `{"ok":true}`)
	form.HTTPReq.Form = []models.FormData{{Key: "code", Values: []string{"c_REC1abc"}}}
	return []*models.TestCase{
		tcase("test-1", "POST", "http://h/login", js, `{}`, 200, map[string]string{"Content-Type": "application/json", "Set-Cookie": "sid=s_REC1abc; Path=/"}, `{"token":"t_REC1abc","code":"c_REC1abc"}`),
		read,
		form,
		tcase("test-4", "POST", "http://h/notes", js, `{"by":"t_REC1abc"}`, 200, js, `{"ok":true}`),
	}
}

// A RUN THAT FAILS AT ANY STEP OF ITS WRITE CHANGES NOTHING: no file is put
// in place (no test case, no config.yaml), the test cases in memory are as
// they were in every text templatize edits (its edits are made on copies, a
// header map, the url_params and a form's values included), nothing is said
// captured, and the error is returned with the line that says what stopped
// it. The step is a test case that cannot be written out, config.yaml that
// cannot be, or the files that cannot be put in place.
func TestProcessTestCasesV2_AFailedReplaceLeavesTheCasesAsTheyWere(t *testing.T) {
	// The set, written, edits every kind of text.
	written, conf := runTemplatize(t, everyKindOfText(), nil, nil)
	require.NotNil(t, conf)
	assert.Equal(t, "sid={{capture string .sid}}; Path=/", written["test-1"].HTTPResp.Header["Set-Cookie"])
	assert.Equal(t, "sid={{string .sid}}", written["test-2"].HTTPReq.Header["Cookie"])
	assert.Equal(t, map[string]string{"token": "{{string .token}}"}, written["test-2"].HTTPReq.URLParams)
	assert.Equal(t, []string{"{{string .code}}"}, written["test-3"].HTTPReq.Form[0].Values)
	assert.Equal(t, `{"by":"{{string .token}}"}`, written["test-4"].HTTPReq.Body)

	for _, c := range []struct {
		name  string
		db    func(*placementTestDB) TestDB
		conf  func(*placementConfStore) TestSetConfig
		err   string
		said  string
		field string // a field of the line said, "" for none
	}{
		{"the files cannot be put in place", func(db *placementTestDB) TestDB { return failingReplace{db} }, nil,
			"disk full", "failed to put the templatized files in place", "next_step"},
		{"a test case cannot be written out", func(db *placementTestDB) TestDB { return failingCaseFile{db, "test-3"} }, nil,
			"test-3 cannot be encoded", "failed to write a test case; keploy templatize changed nothing in this set", "testCase"},
		{"config.yaml cannot be written out", nil, func(conf *placementConfStore) TestSetConfig { return failingConfigFile{conf} },
			`failed to write templatized values for test-set "test-set-0": config.yaml cannot be encoded`, "failed to write test set; keploy templatize changed nothing in this set", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			cases := everyKindOfText()
			before := ""
			for _, tc := range cases {
				before += fmt.Sprintf("%+v\n", *tc)
			}
			db := &placementTestDB{written: map[string]models.TestCase{}, files: caseFiles(t, cases)}
			conf := &placementConfStore{}
			var testDB TestDB = db
			if c.db != nil {
				testDB = c.db(db)
			}
			var confStore TestSetConfig = conf
			if c.conf != nil {
				confStore = c.conf(conf)
			}
			core, logs := observer.New(zap.InfoLevel)
			tools := &Tools{logger: zap.New(core), testDB: testDB, testSetConf: confStore, config: &config.Config{}}
			err := tools.ProcessTestCasesV2(context.Background(), cases, "test-set-0")
			require.EqualError(t, err, c.err)
			assert.Empty(t, db.written, "no test case is put in place")
			assert.Nil(t, conf.written, "config.yaml is not put in place")
			after := ""
			for _, tc := range cases {
				after += fmt.Sprintf("%+v\n", *tc)
			}
			assert.Equal(t, before, after, "the test cases in memory are as they were")
			assert.Empty(t, logs.FilterMessageSnippet("now captures").All(), "nothing is said to be captured")
			said := logs.FilterMessageSnippet(c.said).All()
			require.Len(t, said, 1, "%v", logs.All())
			assert.Equal(t, "test-set-0", said[0].ContextMap()["testSet"])
			if c.field != "" {
				assert.NotEmpty(t, said[0].ContextMap()[c.field], c.field)
			}
		})
	}
}

// A STREAMED TEST CASE'S REQUEST IS READ LIKE ANY OTHER. Its response never
// captures (streamed test cases run after the others), but its request sends
// what an earlier answer gave: the token a login issued reaches the event
// stream it opens as a use, and the stream's frames stay as recorded.
func TestProcessTestCasesV2_AStreamedTestCasesRequestSendsAnEarlierCapture(t *testing.T) {
	js := map[string]string{"Content-Type": "application/json"}
	stream := tcase("test-2", "GET", "http://h/events?token=t_REC1abc", nil, "", 200, map[string]string{"Content-Type": "text/event-stream"}, "")
	stream.HTTPResp.StreamBody = []models.HTTPStreamChunk{{Data: []models.HTTPStreamDataField{{Key: "data", Value: `{"tick":1}`}}}}
	written, conf := runTemplatize(t, []*models.TestCase{
		tcase("test-1", "POST", "http://h/login", js, `{}`, 200, js, `{"token":"t_REC1abc"}`),
		stream,
	}, nil, nil)
	require.NotNil(t, conf)
	assert.Equal(t, map[string]interface{}{"token": "t_REC1abc"}, conf.Template)
	assert.Equal(t, `{"token":"{{capture string .token}}"}`, written["test-1"].HTTPResp.Body)
	assert.Equal(t, "http://h/events?token={{string .token}}", written["test-2"].HTTPReq.URL)
	assert.Equal(t, `{"tick":1}`, written["test-2"].HTTPResp.StreamBody[0].Data[0].Value)
}
