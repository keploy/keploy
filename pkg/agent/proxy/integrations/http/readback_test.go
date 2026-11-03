package http

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/variables"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// The store the rows below drive: a DynamoDB-shaped API whose PutItem writes
// a lead into the record its Id names, read back by a GetItem of that Id or
// by a Scan of the table. The reader is a stand-in for enterprise's: it marks
// every leaf of a PutItem's Item Written into "Leads/<Id>", and links every
// leaf of a read's item to the record its Id names.
var (
	rbPut  = map[string]string{"X-Amz-Target": "DynamoDB_20120810.PutItem", "Content-Type": "application/x-amz-json-1.0"}
	rbGet  = map[string]string{"X-Amz-Target": "DynamoDB_20120810.GetItem", "Content-Type": "application/x-amz-json-1.0"}
	rbScan = map[string]string{"X-Amz-Target": "DynamoDB_20120810.Scan", "Content-Type": "application/x-amz-json-1.0"}
)

func rbPutBody(id, name string) string {
	return `{"Item":{"Id":{"S":"` + id + `"},"Name":{"S":"` + name + `"}},"TableName":"Leads"}`
}

func rbGetBody(id string) string { return `{"Key":{"Id":{"S":"` + id + `"}},"TableName":"Leads"}` }

func rbItem(id, name string) string { return `{"Id":{"S":"` + id + `"},"Name":{"S":"` + name + `"}}` }

// rbStore is the stand-in reader. calls, when set, is told each set of calls
// Access is given; written, when set, replaces what Access marks; access,
// when set, is told each read Access is asked of, first.
type rbStore struct {
	calls   *[][]string
	written func(n int) []map[string]string
	unread  map[string]string
	access  func(read *models.Mock)
}

func rbTarget(r *models.HTTPReq) string { return r.Header["X-Amz-Target"] }

// rbFollows follows every call of the store's API (an X-Amz-Target of
// DynamoDB's) and no other: more than enterprise's reader follows, which
// only costs loads.
func rbFollows(_, call *models.HTTPReq) bool {
	return strings.HasPrefix(rbTarget(call), "DynamoDB_")
}

func (rbStore) Follows(read, call *models.HTTPReq) bool { return rbFollows(read, call) }

func (rbStore) Reads(r *models.HTTPReq) bool {
	t := rbTarget(r)
	return t == rbGet["X-Amz-Target"] || t == rbScan["X-Amz-Target"]
}

// rbLeaves are the leaves of a JSON text under key, by path, each with its
// value.
func rbLeaves(body, key string) map[string]learn.Leaf {
	out := map[string]learn.Leaf{}
	if v, ok := learn.DecodeJSON([]byte(body)); ok {
		if m, ok := v.(map[string]interface{}); ok && m[key] != nil {
			learn.JSONLeaves("body."+key, m[key], out)
		}
	}
	return out
}

func (s rbStore) Access(read *models.Mock, calls []*models.Mock) models.HTTPStoreAccess {
	if s.access != nil {
		s.access(read)
	}
	if s.calls != nil {
		var names []string
		for _, c := range calls {
			names = append(names, c.Name)
		}
		*s.calls = append(*s.calls, names)
	}
	a := models.HTTPStoreAccess{Written: make([]map[string]string, len(calls)), Read: map[string]string{}, Unread: s.unread}
	for i, c := range calls {
		if c.Spec.HTTPReq == nil || rbTarget(c.Spec.HTTPReq) != rbPut["X-Amz-Target"] {
			continue
		}
		leaves := rbLeaves(c.Spec.HTTPReq.Body, "Item")
		a.Written[i] = map[string]string{}
		for path := range leaves {
			a.Written[i][path] = "Leads/" + leaves["body.Item.Id.S"].Value
		}
	}
	if s.written != nil {
		a.Written = s.written(len(calls))
	}
	switch rbTarget(read.Spec.HTTPReq) {
	case rbGet["X-Amz-Target"]:
		id := rbLeaves(read.Spec.HTTPReq.Body, "Key")["body.Key.Id.S"].Value
		for path := range rbLeaves(read.Spec.HTTPResp.Body, "Item") {
			a.Read[path] = "Leads/" + id
		}
	case rbScan["X-Amz-Target"]:
		leaves := rbLeaves(read.Spec.HTTPResp.Body, "Items")
		for path := range leaves {
			item := path[:strings.Index(path, "]")+1]
			a.Read[path] = "Leads/" + leaves[item+".Id.S"].Value
		}
	}
	return a
}

// rbRegister registers s as the store reader for the test.
func rbRegister(t *testing.T, s models.HTTPStoreReader) {
	t.Helper()
	t.Cleanup(models.RegisterHTTPStore(s))
}

// rbRun is a test of the lead store: the recordings mocks (mock-0 the
// PutItem), served to the calls in turn in record's full round, under the
// proven switch as proven says. It returns the store with what each call
// consumed, and the answers served.
func rbRun(t *testing.T, proven string, mocks []*models.Mock, calls []*req) (*learnDb, []*models.Mock) {
	t.Helper()
	h, _, opts := learnOn(t)
	t.Setenv(provenswitch.Env, proven)
	if provenswitch.On() != (proven == "1") {
		t.Fatalf("the proven switch reads %v under %s=%q", provenswitch.On(), provenswitch.Env, proven)
	}
	n := h.newLearnNoise(opts, nil, nil)
	tr := learn.NewTracker()
	tr.Begin()
	ctx := learn.WithTracker(context.Background(), tr)
	db := &learnDb{perTest: mocks, observe: tr.ServedMock}
	var out []*models.Mock
	for i, c := range calls {
		served, miss, err := h.learnMatch(ctx, c, db, n, opts)
		if err != nil || miss != nil || served == nil {
			t.Fatalf("call %d: served %v miss %+v err %v", i, served, miss, err)
		}
		out = append(out, served)
	}
	return db, out
}

// The PoC's lead read-back: the test creates a lead whose name it stamps
// with this run's time, then reads the lead back by its id and checks the
// name. Its GetItem is exactly the recorded one (the id is the same), and
// keploy answers it with the name this run's PutItem stored, bound at that
// PutItem: a variable F binds there and answers at the read, under the
// proven switch and without it.
func TestReadBackAnswersAnExactReadWithWhatTheTestWrote(t *testing.T) {
	for _, proven := range []string{"1", "0"} {
		t.Run("proven="+proven, func(t *testing.T) {
			rbRegister(t, rbStore{})
			db, served := rbRun(t, proven, []*models.Mock{
				learnRecorded("mock-0", 0, "POST", "/", rbPutBody("c-1", "Lead 1700000000000"), rbPut, `{}`),
				learnRecorded("mock-1", time.Second, "POST", "/", rbGetBody("c-1"), rbGet, `{"Item":`+rbItem("c-1", "Lead 1700000000000")+`}`),
			}, []*req{
				live("POST", "/", rbPutBody("c-1", "Lead 1700000000417"), rbPut),
				live("POST", "/", rbGetBody("c-1"), rbGet),
			})
			if got := served[1].Spec.HTTPResp.Body; got != `{"Item":`+rbItem("c-1", "Lead 1700000000417")+`}` {
				t.Fatalf("the read answered %s, want the name this run wrote", got)
			}
			read := db.consumed[1]
			vars := read.Spec.Learned()
			if len(vars) != 1 {
				t.Fatalf("the read learned %+v, want one variable", vars)
			}
			for name, v := range vars {
				if v.Recorded != "Lead 1700000000000" || !reflect.DeepEqual(v.Answer, []string{"body.Item.Name.S"}) || len(v.Use) != 0 {
					t.Fatalf("variable %s = %+v, want answered at body.Item.Name.S", name, v)
				}
				if want := (map[string]models.MockVariables{"mock-0": {name: {Recorded: "Lead 1700000000000", Bind: "body.Item.Name.S"}}}); !reflect.DeepEqual(read.Spec.LearnedBinds(), want) {
					t.Fatalf("binds %+v, want %+v", read.Spec.LearnedBinds(), want)
				}
			}
		})
	}
}

// The read whose key drifted (an id the application made) is paired: its
// key is the variable bound at the PutItem's Id, and the name it reads is
// read back from the PutItem as well.
func TestReadBackAnswersAPairedReadWithWhatTheTestWrote(t *testing.T) {
	for _, proven := range []string{"1", "0"} {
		t.Run("proven="+proven, func(t *testing.T) {
			rbRegister(t, rbStore{})
			db, served := rbRun(t, proven, []*models.Mock{
				learnRecorded("mock-0", 0, "POST", "/", rbPutBody("c-1", "Lead 1700000000000"), rbPut, `{}`),
				learnRecorded("mock-1", time.Second, "POST", "/", rbGetBody("c-1"), rbGet, `{"Item":`+rbItem("c-1", "Lead 1700000000000")+`}`),
			}, []*req{
				live("POST", "/", rbPutBody("c-9", "Lead 1700000000417"), rbPut),
				live("POST", "/", rbGetBody("c-9"), rbGet),
			})
			if got := served[1].Spec.HTTPResp.Body; got != `{"Item":`+rbItem("c-9", "Lead 1700000000417")+`}` {
				t.Fatalf("the read answered %s, want the id and name this run wrote", got)
			}
			var answered []string
			for _, v := range db.consumed[1].Spec.Learned() {
				answered = append(answered, v.Answer...)
			}
			sort.Strings(answered)
			if !reflect.DeepEqual(answered, []string{"body.Item.Id.S", "body.Item.Name.S"}) {
				t.Fatalf("the read answers %v, want its id and its name", answered)
			}
		})
	}
}

// A Scan whose request is the recorded one answers each item the test wrote
// with what it wrote, and an item it did not write as recorded.
func TestReadBackAnswersAScanWithWhatTheTestWrote(t *testing.T) {
	rbRegister(t, rbStore{})
	_, served := rbRun(t, "1", []*models.Mock{
		learnRecorded("mock-0", 0, "POST", "/", rbPutBody("c-1", "Lead 1700000000000"), rbPut, `{}`),
		learnRecorded("mock-1", time.Second, "POST", "/", `{"ConsistentRead":true,"TableName":"Leads"}`, rbScan, `{"Count":2,"Items":[`+rbItem("c-0", "Lead 1600000000000")+`,`+rbItem("c-1", "Lead 1700000000000")+`]}`),
	}, []*req{
		live("POST", "/", rbPutBody("c-1", "Lead 1700000000417"), rbPut),
		live("POST", "/", `{"ConsistentRead":true,"TableName":"Leads"}`, rbScan),
	})
	if got, want := served[1].Spec.HTTPResp.Body, `{"Count":2,"Items":[`+rbItem("c-0", "Lead 1600000000000")+`,`+rbItem("c-1", "Lead 1700000000417")+`]}`; got != want {
		t.Fatalf("the scan answered %s, want %s", got, want)
	}
}

// Where nothing reads the store, where the reader links the read to no
// record, or where it marks nothing written, the read is answered as
// recorded and learns nothing; and the reader is given the test's calls
// served before the read, in the order served.
func TestReadBackAnswersAsRecordedWhereNoWriteIsStated(t *testing.T) {
	mocks := func() []*models.Mock {
		return []*models.Mock{
			learnRecorded("mock-0", 0, "POST", "/", rbPutBody("c-1", "Lead 1700000000000"), rbPut, `{}`),
			learnRecorded("mock-1", time.Second, "POST", "/", rbGetBody("c-1"), rbGet, `{"Item":`+rbItem("c-1", "Lead 1700000000000")+`}`),
		}
	}
	calls := func() []*req {
		return []*req{live("POST", "/", rbPutBody("c-1", "Lead 1700000000417"), rbPut), live("POST", "/", rbGetBody("c-1"), rbGet)}
	}
	recorded := `{"Item":` + rbItem("c-1", "Lead 1700000000000") + `}`
	asRecorded := func(t *testing.T, db *learnDb, served []*models.Mock) {
		t.Helper()
		if got := served[1].Spec.HTTPResp.Body; got != recorded {
			t.Fatalf("the read answered %s, want it as recorded", got)
		}
		if vars := db.consumed[1].Spec.Learned(); len(vars) != 0 {
			t.Fatalf("the read learned %+v, want nothing", vars)
		}
	}
	t.Run("no reader", func(t *testing.T) {
		db, served := rbRun(t, "1", mocks(), calls())
		asRecorded(t, db, served)
	})
	t.Run("no write marked", func(t *testing.T) {
		var given [][]string
		rbRegister(t, rbStore{calls: &given, written: func(n int) []map[string]string { return make([]map[string]string, n) }})
		db, served := rbRun(t, "1", mocks(), calls())
		asRecorded(t, db, served)
		if !reflect.DeepEqual(given, [][]string{{"mock-0"}}) {
			t.Fatalf("the reader was given %v, want the PutItem alone", given)
		}
	})
	t.Run("a mark per call or none", func(t *testing.T) {
		rbRegister(t, rbStore{written: func(n int) []map[string]string {
			return []map[string]string{{"body.Item.Name.S": "Leads/c-1"}, {"body.Item.Name.S": "Leads/c-1"}}
		}})
		db, served := rbRun(t, "1", mocks(), calls())
		asRecorded(t, db, served)
	})
	t.Run("another record", func(t *testing.T) {
		rbRegister(t, rbStore{written: func(n int) []map[string]string {
			return []map[string]string{{"body.Item.Name.S": "Leads/c-2"}}
		}})
		db, served := rbRun(t, "1", mocks(), calls())
		asRecorded(t, db, served)
	})
	t.Run("the write kept its value", func(t *testing.T) {
		rbRegister(t, rbStore{})
		c := calls()
		c[0] = live("POST", "/", rbPutBody("c-1", "Lead 1700000000000"), rbPut)
		db, served := rbRun(t, "1", mocks(), c)
		asRecorded(t, db, served)
	})
}

// What F learned replays: each consumed mock's variables written onto its
// recording and its binds onto the PutItem, a later run's GetItem, exactly
// the recorded one, is answered by the variable pass with the name that
// run's PutItem stored; with no PutItem before it, it is a variable miss,
// never the recorded name.
func TestReadBackReplaysByTheVariablePass(t *testing.T) {
	rbRegister(t, rbStore{})
	recordings := []*models.Mock{
		learnRecorded("mock-0", 0, "POST", "/", rbPutBody("c-1", "Lead 1700000000000"), rbPut, `{}`),
		learnRecorded("mock-1", time.Second, "POST", "/", rbGetBody("c-1"), rbGet, `{"Item":`+rbItem("c-1", "Lead 1700000000000")+`}`),
	}
	f, _ := rbRun(t, "1", recordings, []*req{
		live("POST", "/", rbPutBody("c-1", "Lead 1700000000417"), rbPut),
		live("POST", "/", rbGetBody("c-1"), rbGet),
	})
	written := func() []*models.Mock {
		out := map[string]*models.Mock{}
		for _, m := range recordings {
			c := *m
			c.Spec.Variables = models.MockVariables{}
			out[m.Name] = &c
		}
		for _, st := range f.consumed {
			for vn, v := range st.Spec.Learned() {
				out[st.Name].Spec.Variables[vn] = v
			}
			for target, vs := range st.Spec.LearnedBinds() {
				for vn, v := range vs {
					out[target].Spec.Variables[vn] = v
				}
			}
		}
		return []*models.Mock{out["mock-0"], out["mock-1"]}
	}
	h := newHTTP()
	ci := &varDb{perTest: written()}
	if ok, _, diag := matchVar(t, h, ci, live("POST", "/", rbPutBody("c-1", "Lead 1700000009999"), rbPut)); !ok {
		t.Fatalf("the PutItem in CI: %+v", diag)
	}
	ok, stub, diag := matchVar(t, h, ci, live("POST", "/", rbGetBody("c-1"), rbGet))
	if !ok || stub.Spec.HTTPResp.Body != `{"Item":`+rbItem("c-1", "Lead 1700000009999")+`}` {
		t.Fatalf("the GetItem in CI: ok %v diag %+v", ok, diag)
	}

	ci = &varDb{perTest: written()}
	ok, _, diag = matchVar(t, h, ci, live("POST", "/", rbGetBody("c-1"), rbGet))
	if ok || diag == nil || diag.phase != models.MatchPhaseVariable {
		t.Fatalf("the GetItem with no PutItem before it: ok %v diag %+v, want a variable miss", ok, diag)
	}
}

// readBackPlan on the exchanges a test served, as the tracker keeps them: a
// leaf is read back only from a write of the running test's own (never the
// test case's request, an earlier test's, or the pool's), never at a place
// answered already, never past the test's end; it answers a variable the write's place holds already, and a
// clock read bound at the write carries its clock evidence.
func TestReadBackPlanReadsBackOnlyTheTestsOwnWrites(t *testing.T) {
	rbRegister(t, rbStore{})
	rec := learnAt.Add(-time.Hour + 17*time.Second)
	recName, liveName := rec.Format(time.RFC3339), rec.Add(learn.FullRoundLead).Format(time.RFC3339)
	put := learnRecorded("mock-0", 0, "POST", "/", rbPutBody("c-1", recName), rbPut, `{}`)
	get := learnRecorded("mock-1", time.Second, "POST", "/", rbGetBody("c-1"), rbGet, `{"Item":`+rbItem("c-1", recName)+`}`)
	exchange := func(edit func(*learn.Earlier)) []learn.Earlier {
		e := learn.Earlier{Mock: "mock-0", At: put.Spec.ReqTimestampMock, Own: true, Seen: []learn.Seen{
			{Side: learn.Request, Leaf: learn.Leaf{Path: "body.Item.Id.S", Noise: "body.Item.Id.S", Type: learn.String, Value: "c-1"}, Live: "c-1", Place: "body.Item.Id.S"},
			{Side: learn.Request, Leaf: learn.Leaf{Path: "body.Item.Name.S", Noise: "body.Item.Name.S", Type: learn.String, Value: recName}, Live: liveName, Place: "body.Item.Name.S"},
		}}
		if edit != nil {
			edit(&e)
		}
		return []learn.Earlier{e}
	}
	h := newHTTP()
	tr := clockedTracker()
	tr.Begin()
	test := tr.Placed().Test

	p := h.readBackPlan(tr, test, get, []*models.Mock{put}, exchange(nil), nil, newPairing())
	if len(p.vars) != 1 || len(p.binds["mock-0"]) != 1 {
		t.Fatalf("the test's own write: vars %+v binds %+v, want one variable bound at mock-0", p.vars, p.binds)
	}
	for name, v := range p.binds["mock-0"] {
		if v.Bind != "body.Item.Name.S" || len(v.Clock) != 1 || !v.Clock[0].At.Equal(rec) || v.Clock[0].Res != time.Second {
			t.Fatalf("bind %s = %+v, want bound at body.Item.Name.S with the clock read it holds", name, v)
		}
		if b := p.values[name]; b.Value != liveName || b.Recorded != recName || b.Mock != "mock-0" {
			t.Fatalf("value %+v, want this run's", b)
		}
	}
	if len(exchange(nil)[0].Seen) != 2 || exchange(nil)[0].Seen[1].Written {
		t.Fatal("the tracker's exchange is left as it was")
	}

	for _, c := range []struct {
		name   string
		served []*models.Mock
		edit   func(*learn.Earlier)
		test   uint64
	}{
		{"the test case's request", []*models.Mock{put}, func(e *learn.Earlier) { e.Given = true }, test},
		{"an earlier test's", []*models.Mock{put}, func(e *learn.Earlier) { e.Run = true }, test},
		{"the pool's", []*models.Mock{put}, func(e *learn.Earlier) { e.Own = false }, test},
		{"another call's", []*models.Mock{get}, nil, test},
		{"past the test's end", []*models.Mock{put}, nil, test + 1},
	} {
		if p := h.readBackPlan(tr, c.test, get, c.served, exchange(c.edit), nil, newPairing()); len(p.vars) != 0 || len(p.binds) != 0 {
			t.Fatalf("%s: vars %+v binds %+v, want the read answered as recorded", c.name, p.vars, p.binds)
		}
	}

	held := exchange(func(e *learn.Earlier) { e.Seen[1].Name = "lead-name" })
	p = newPairing()
	p.vars["lead-name"] = models.MockVariable{Recorded: recName, Use: []string{"body.Key.Name.S"}}
	p = h.readBackPlan(tr, test, get, []*models.Mock{put}, held, nil, p)
	if want := (models.MockVariables{"lead-name": {Recorded: recName, Use: []string{"body.Key.Name.S"}, Answer: []string{"body.Item.Name.S"}}}); !reflect.DeepEqual(p.vars, want) || len(p.binds) != 0 {
		t.Fatalf("vars %+v binds %+v, want the write's variable answered at the read, bound nowhere new", p.vars, p.binds)
	}
	if b := p.values["lead-name"]; b.Value != liveName {
		t.Fatalf("value %+v, want this run's", b)
	}
	// The variable holds this run's value already, bound where it was
	// first sent: the read-back answers it and leaves that binding.
	first := variables.Binding{Name: "lead-name", Value: liveName, Recorded: recName, Mock: "mock-9", Place: "body.Key.Name.S"}
	p = newPairing()
	p.vars["lead-name"] = models.MockVariable{Recorded: recName, Use: []string{"body.Key.Name.S"}}
	p.values["lead-name"] = first
	if p = h.readBackPlan(tr, test, get, []*models.Mock{put}, held, nil, p); !reflect.DeepEqual(p.values["lead-name"], first) {
		t.Fatalf("value %+v, want the binding where the variable was first sent, %+v", p.values["lead-name"], first)
	}

	p = newPairing()
	p.vars["key"] = models.MockVariable{Recorded: recName, Answer: []string{"body.Item.Name.S"}}
	if p = h.readBackPlan(tr, test, get, []*models.Mock{put}, exchange(nil), nil, p); len(p.vars) != 1 || len(p.binds) != 0 {
		t.Fatalf("a place answered already: vars %+v binds %+v", p.vars, p.binds)
	}

	// A binding the test is given (the test case's own values) keeps its
	// name: the variable bound at the write takes another, even where the
	// given one holds the name the write's place would take.
	fresh := func() (*learn.Tracker, uint64) {
		tr := clockedTracker()
		tr.Begin()
		return tr, tr.Placed().Test
	}
	ftr, ftest := fresh()
	var own string
	for name := range h.readBackPlan(ftr, ftest, get, []*models.Mock{put}, exchange(nil), nil, newPairing()).binds["mock-0"] {
		own = name
	}
	ftr, ftest = fresh()
	p = h.readBackPlan(ftr, ftest, get, []*models.Mock{put}, exchange(nil), []variables.Binding{{Name: own, Value: "c-9"}}, newPairing())
	if len(p.binds["mock-0"]) != 1 {
		t.Fatalf("a given binding of the place's name: binds %+v, want one variable bound at mock-0", p.binds)
	}
	for name := range p.binds["mock-0"] {
		if own == "" || name == own {
			t.Fatalf("the write's variable is named %q beside a given binding %q, want another name", name, own)
		}
	}

	// A call served in the test whose recorded answer cannot be loaded:
	// what it stored is unknown, so nothing is read back, though the reader
	// would mark nothing of it.
	lost := learnRecorded("mock-2", 2*time.Second, "POST", "/", rbGetBody("c-2"), rbGet, `{}`)
	lost.SetResponseHydrator(func() (*models.HTTPResp, []models.MongoResponse, error) {
		return nil, nil, errors.New("the answer's file is gone")
	})
	if p := h.readBackPlan(tr, test, get, []*models.Mock{put, lost}, exchange(nil), nil, newPairing()); len(p.vars) != 0 || len(p.binds) != 0 {
		t.Fatalf("a call whose answer cannot be read: vars %+v binds %+v, want the read answered as recorded", p.vars, p.binds)
	}
	// A call the reader does not follow (another API's) is never loaded
	// for the read: an answer of its that cannot be loaded does not stop
	// the write's read-back.
	elsewhere := learnRecorded("mock-3", 3*time.Second, "GET", "/obj/a", "", nil, `{}`)
	elsewhere.SetResponseHydrator(func() (*models.HTTPResp, []models.MongoResponse, error) {
		return nil, nil, errors.New("the answer's file is gone")
	})
	if p := h.readBackPlan(tr, test, get, []*models.Mock{put, elsewhere}, exchange(nil), nil, newPairing()); len(p.vars) != 1 || len(p.binds["mock-0"]) != 1 {
		t.Fatalf("another API's call: vars %+v binds %+v, want the write read back", p.vars, p.binds)
	}
}

// A read loads the recorded answers of the calls the reader follows, and of
// no other call: in a test of a PutItem, five GETs of another API whose
// answers are kept on disk, and three GetItems of the item, each GET's answer
// is loaded as often as with no reader registered, and the PutItem's once
// more per read. So the loads grow with the calls the reader follows, not
// with reads times calls.
func TestReadBackLoadsOnlyTheAnswersOfCallsTheReaderFollows(t *testing.T) {
	run := func(reader bool) (map[string]int, []*models.Mock) {
		if reader {
			rbRegister(t, rbStore{})
		}
		loads := map[string]int{}
		spill := func(m *models.Mock) *models.Mock {
			resp, name := *m.Spec.HTTPResp, m.Name
			m.SetResponseHydrator(func() (*models.HTTPResp, []models.MongoResponse, error) {
				loads[name]++
				r := resp
				return &r, nil, nil
			})
			return m
		}
		recs := []*models.Mock{spill(learnRecorded("mock-0", 0, "POST", "/", rbPutBody("c-1", "Lead 1700000000000"), rbPut, `{}`))}
		calls := []*req{live("POST", "/", rbPutBody("c-1", "Lead 1700000000417"), rbPut)}
		for i := 1; i <= 5; i++ {
			path := "/obj/" + string(rune('a'+i))
			recs = append(recs, spill(learnRecorded("mock-"+string(rune('0'+i)), time.Duration(i)*100*time.Millisecond, "GET", path, "", nil, `{"b":"`+strings.Repeat("x", 1<<10)+`"}`)))
			calls = append(calls, live("GET", path, "", nil))
		}
		for i := 6; i <= 8; i++ {
			recs = append(recs, learnRecorded("mock-"+string(rune('0'+i)), time.Duration(i)*100*time.Millisecond, "POST", "/", rbGetBody("c-1"), rbGet, `{"Item":`+rbItem("c-1", "Lead 1700000000000")+`}`))
			calls = append(calls, live("POST", "/", rbGetBody("c-1"), rbGet))
		}
		_, served := rbRun(t, "1", recs, calls)
		return loads, served
	}
	without, _ := run(false)
	with, served := run(true)
	for i := 6; i <= 8; i++ {
		if got := served[i].Spec.HTTPResp.Body; got != `{"Item":`+rbItem("c-1", "Lead 1700000000417")+`}` {
			t.Fatalf("read %d answered %s, want the name this run wrote", i, got)
		}
	}
	for i := 1; i <= 5; i++ {
		name := "mock-" + string(rune('0'+i))
		if without[name] == 0 || with[name] != without[name] {
			t.Fatalf("%s, a GET the reader does not follow: loaded %d times with the reader, %d without, want as often", name, with[name], without[name])
		}
	}
	if with["mock-0"] != without["mock-0"]+3 {
		t.Fatalf("the PutItem: loaded %d times with the reader, %d without, want once more per read (3)", with["mock-0"], without["mock-0"])
	}
}

// A read whose fields a write the reader cannot follow leaves as recorded is
// logged once, naming how many fields and the first by path with its reason,
// never once per field.
func TestReadBackLogsAReadsUnreadFieldsOnce(t *testing.T) {
	rbRegister(t, rbStore{unread: map[string]string{"body.Item.Name.S": "an ADD changed it", "body.Item.Id.S": "a 500 answered it"}})
	put := learnRecorded("mock-0", 0, "POST", "/", rbPutBody("c-1", "Lead 1700000000000"), rbPut, `{}`)
	get := learnRecorded("mock-1", time.Second, "POST", "/", rbGetBody("c-1"), rbGet, `{"Item":`+rbItem("c-1", "Lead 1700000000000")+`}`)
	core, logs := observer.New(zap.InfoLevel)
	h := &HTTP{Logger: zap.New(core)}
	tr := clockedTracker()
	tr.Begin()
	h.readBackPlan(tr, tr.Placed().Test, get, []*models.Mock{put}, nil, nil, newPairing())
	lines := logs.All()
	if len(lines) != 1 {
		t.Fatalf("%d log lines %+v, want one for the read", len(lines), lines)
	}
	f := lines[0].ContextMap()
	if f["fields"] != int64(2) || f["first"] != "body.Item.Id.S" || f["why"] != "a 500 answered it" || f["mock"] != "mock-1" {
		t.Fatalf("the read's line %+v, want 2 fields, the first body.Item.Id.S with its reason", f)
	}
}

// rbRaceDb is the lead store where another connection takes the recording
// named take under this call's take of it, once armed: it answers it in the
// tracker before this call does, as the call reads the store (answer) or
// as the call reads its answer back, after the call picked it (access), or
// it consumes it from the store before this call's claim does (claim). From
// then on the store no longer shows it.
type rbRaceDb struct {
	*learnDb
	tr     *learn.Tracker
	take   string
	race   string
	armed  bool
	gone   bool
	others int
}

func (d *rbRaceDb) GetPerTestMocksInWindow() ([]*models.Mock, error) {
	var out []*models.Mock
	for _, m := range d.perTest {
		if d.gone && m.Name == d.take {
			continue
		}
		out = append(out, m)
		if d.armed && d.race == "answer" && m.Name == d.take {
			// The other connection answers it with the scope read: this
			// call's take is refused, and the other's consume lands
			// before this call reads again.
			d.otherAnswers(m)
		}
	}
	return out, nil
}

// otherAnswers is the other connection answering m in the tracker, once.
func (d *rbRaceDb) otherAnswers(m *models.Mock) {
	d.armed, d.gone = false, true
	if d.tr.Answer(d.tr.Placed().Test, m) {
		d.others++
	}
}

func (d *rbRaceDb) DeleteFilteredMock(m models.Mock) bool {
	if d.armed && d.race == "claim" && m.Name == d.take {
		d.armed, d.gone = false, true
		d.others++
		return false
	}
	return d.learnDb.DeleteFilteredMock(m)
}

// A read the test's PutItem is read back into whose recording another
// connection takes first, in the tracker (as the call reads the store, or
// after the call picked it, as it reads the answer back) or in the store, is
// never served from it: the call reads the store again, no longer finds it,
// and misses; and the tracker keeps no answer of this call's for it.
func TestReadBackGivesUpARecordingAnotherConnectionTook(t *testing.T) {
	for _, race := range []string{"answer", "access", "claim"} {
		t.Run(race, func(t *testing.T) {
			var db *rbRaceDb
			rbRegister(t, rbStore{access: func(read *models.Mock) {
				if db.armed && db.race == "access" && read.Name == db.take {
					db.otherAnswers(read)
				}
			}})
			h, n, opts := learnOn(t)
			tr := learn.NewTracker()
			tr.Begin()
			ctx := learn.WithTracker(context.Background(), tr)
			db = &rbRaceDb{learnDb: &learnDb{perTest: []*models.Mock{
				learnRecorded("mock-0", 0, "POST", "/", rbPutBody("c-1", "Lead 1700000000000"), rbPut, `{}`),
				learnRecorded("mock-1", time.Second, "POST", "/", rbGetBody("c-1"), rbGet, `{"Item":`+rbItem("c-1", "Lead 1700000000000")+`}`),
			}, observe: tr.ServedMock}, tr: tr, take: "mock-1", race: race}
			if served, miss, err := h.learnMatch(ctx, live("POST", "/", rbPutBody("c-1", "Lead 1700000000417"), rbPut), db, n, opts); served == nil || miss != nil || err != nil {
				t.Fatalf("the PutItem: served %v miss %+v err %v", served, miss, err)
			}
			db.armed = true
			served, _, err := h.learnMatch(ctx, live("POST", "/", rbGetBody("c-1"), rbGet), db, n, opts)
			if err != nil || served != nil {
				t.Fatalf("the read: served %v err %v, want a miss", served, err)
			}
			if db.others != 1 {
				t.Fatalf("the other connection took mock-1 %d times, want once", db.others)
			}
			if len(db.consumed) != 1 || db.consumed[0].Name != "mock-0" {
				t.Fatalf("consumed %d mocks, want the PutItem alone", len(db.consumed))
			}
			if race == "claim" && tr.Answered("mock-1") {
				t.Fatal("the tracker keeps this call's answer of a recording it could not claim")
			}
		})
	}
}

// A read the pool keeps (no test's own) that the call is paired with (it
// drifted in a leaf no write stored) never reads back what the test wrote:
// a test's write answers only the test's own reads.
func TestReadBackLeavesAPairedPoolReadAsRecorded(t *testing.T) {
	rbRegister(t, rbStore{})
	h, n, opts := learnOn(t)
	// The pool's recordings are paired too (models.OutgoingOptions.LearnPool):
	// without it the read is never paired with the pool's, and the row
	// would hold whatever the pairing did.
	opts.LearnPool = true
	tr := learn.NewTracker()
	tr.Begin()
	ctx := learn.WithTracker(context.Background(), tr)
	read := func(capacity string) string {
		return `{"Key":{"Id":{"S":"c-1"}},"ReturnConsumedCapacity":"` + capacity + `","TableName":"Leads"}`
	}
	recorded := `{"Item":` + rbItem("c-1", "Lead 1700000000000") + `}`
	pool := learnRecorded("mock-1", time.Second, "POST", "/", read("NONE"), rbGet, recorded)
	pool.TestModeInfo.Lifetime = models.LifetimeSession
	db := &learnDb{perTest: []*models.Mock{learnRecorded("mock-0", 0, "POST", "/", rbPutBody("c-1", "Lead 1700000000000"), rbPut, `{}`)}, mocks: []*models.Mock{pool}, observe: tr.ServedMock}
	if served, miss, err := h.learnMatch(ctx, live("POST", "/", rbPutBody("c-1", "Lead 1700000000417"), rbPut), db, n, opts); served == nil || miss != nil || err != nil {
		t.Fatalf("the PutItem: served %v miss %+v err %v", served, miss, err)
	}
	// The read differs from the pool's recording in a leaf no write
	// stored: it is paired with it, not taken exactly.
	served, miss, err := h.learnMatch(ctx, live("POST", "/", read("TOTAL"), rbGet), db, n, opts)
	if err != nil || miss != nil || served == nil || served.Name != pool.Name {
		t.Fatalf("the read is not paired with the pool's: served %v miss %+v err %v", served, miss, err)
	}
	if got := served.Spec.HTTPResp.Body; got != recorded {
		t.Fatalf("the pool's read answered %s, want it as recorded", got)
	}
	for _, m := range db.pooled {
		if len(m.Spec.LearnedBinds()) != 0 {
			t.Fatalf("the pool's read learned binds %+v", m.Spec.LearnedBinds())
		}
	}
}

// A read the pool keeps (no test's own) that is exactly its recording is
// answered as recorded: only a test's own read reads back.
func TestReadBackLeavesThePoolsReadAsRecorded(t *testing.T) {
	rbRegister(t, rbStore{})
	h, n, opts := learnOn(t)
	tr := learn.NewTracker()
	tr.Begin()
	ctx := learn.WithTracker(context.Background(), tr)
	pool := learnRecorded("mock-1", time.Second, "POST", "/", rbGetBody("c-1"), rbGet, `{"Item":`+rbItem("c-1", "Lead 1700000000000")+`}`)
	pool.TestModeInfo.Lifetime = models.LifetimeSession
	db := &learnDb{perTest: []*models.Mock{learnRecorded("mock-0", 0, "POST", "/", rbPutBody("c-1", "Lead 1700000000000"), rbPut, `{}`)}, mocks: []*models.Mock{pool}, observe: tr.ServedMock}
	if served, miss, err := h.learnMatch(ctx, live("POST", "/", rbPutBody("c-1", "Lead 1700000000417"), rbPut), db, n, opts); served == nil || miss != nil || err != nil {
		t.Fatalf("the PutItem: served %v miss %+v err %v", served, miss, err)
	}
	served, miss, err := h.learnMatch(ctx, live("POST", "/", rbGetBody("c-1"), rbGet), db, n, opts)
	if err != nil || miss != nil || served == nil {
		t.Fatalf("the read: served %v miss %+v err %v", served, miss, err)
	}
	if got := served.Spec.HTTPResp.Body; got != `{"Item":`+rbItem("c-1", "Lead 1700000000000")+`}` {
		t.Fatalf("the pool's read answered %s, want it as recorded", got)
	}
}

// rbTextStore is a reader that links the whole of a text answer ("body") to
// the record a text PutItem wrote whole.
type rbTextStore struct{}

func (rbTextStore) Reads(r *models.HTTPReq) bool { return rbTarget(r) == rbGet["X-Amz-Target"] }

func (rbTextStore) Follows(read, call *models.HTTPReq) bool { return rbFollows(read, call) }

func (rbTextStore) Access(_ *models.Mock, calls []*models.Mock) models.HTTPStoreAccess {
	a := models.HTTPStoreAccess{Written: make([]map[string]string, len(calls)), Read: map[string]string{"body": "Notes/1"}}
	for i, c := range calls {
		if rbTarget(c.Spec.HTTPReq) == rbPut["X-Amz-Target"] {
			a.Written[i] = map[string]string{"body": "Notes/1"}
		}
	}
	return a
}

// A leaf of the read's answer no variable place can name (a whole text
// body) is answered as recorded, though the reader links it to what the
// test's own write stored.
func TestReadBackLeavesALeafNoPlaceNamesAsRecorded(t *testing.T) {
	rbRegister(t, rbTextStore{})
	put := learnRecorded("mock-0", 0, "POST", "/", "note 1700000000000", rbPut, `{}`)
	get := learnRecorded("mock-1", time.Second, "POST", "/", rbGetBody("n-1"), rbGet, "note 1700000000000")
	get.Spec.HTTPResp.Header = map[string]string{"Content-Type": "text/plain"}
	e := []learn.Earlier{{Mock: "mock-0", At: put.Spec.ReqTimestampMock, Own: true, Seen: []learn.Seen{
		{Side: learn.Request, Leaf: learn.Leaf{Path: "body", Noise: "body", Type: learn.String, Value: "note 1700000000000"}, Live: "note 1700000000417", Place: "body"},
	}}}
	tr := clockedTracker()
	tr.Begin()
	if p := newHTTP().readBackPlan(tr, tr.Placed().Test, get, []*models.Mock{put}, e, nil, newPairing()); len(p.vars) != 0 || len(p.binds) != 0 {
		t.Fatalf("vars %+v binds %+v, want the text answered as recorded", p.vars, p.binds)
	}
}
