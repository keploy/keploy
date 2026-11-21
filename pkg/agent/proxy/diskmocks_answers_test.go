package proxy

import (
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// answersCorpus is a test's recordings: HTTP and Mongo calls with answers on
// both sides of responseSpillMinBytes, an HTTP call with no answer, and a DNS
// call, whose answer its record carries.
func answersCorpus() []*models.Mock {
	base := time.Unix(1_700_000_000, 0).UTC()
	httpCall := func(name string, at time.Time, body int) *models.Mock {
		m := bigMockAt(name, at, body)
		m.Spec.HTTPReq = &models.HTTPReq{Method: "GET", URL: "http://upstream/" + name, Header: map[string]string{"Host": "upstream"}}
		m.Spec.HTTPResp.Header = map[string]string{"Content-Type": "text/plain"}
		m.TestModeInfo.Lifetime = models.LifetimePerTest
		return m
	}
	dns := perTestMockAt("dns", base.Unix()+5)
	dns.Kind = models.DNS
	dns.Spec.DNSReq = &models.DNSReq{Name: "db.local.", Qtype: 1, Qclass: 1}
	dns.Spec.DNSResp = &models.DNSResp{Rcode: 0, Authoritative: true}
	return []*models.Mock{
		httpCall("http-small", base, 300),
		httpCall("http-big", base.Add(time.Second), 3*responseSpillMinBytes),
		mongoAnswerMock("mongo-small", base.Unix()+2, 200),
		mongoAnswerMock("mongo-big", base.Unix()+3, 2*responseSpillMinBytes),
		perTestMockAt("http-no-answer", base.Unix()+4),
		dns,
	}
}

// keptApart is whether the store keeps m's answer apart from its record: any
// Mongo answer, and an HTTP one with a body of responseSpillMinBytes or more.
func keptApart(m *models.Mock) bool {
	switch m.Kind {
	case models.Mongo:
		return len(m.Spec.MongoResponses) > 0
	case models.HTTP:
		return m.Spec.HTTPResp != nil && len(m.Spec.HTTPResp.Body) >= responseSpillMinBytes
	}
	return false
}

// A window open decodes no Mongo answer (keploy#4967): LoadByNames decodes
// the bytes of the test's records alone, and every Mongo answer, of any size,
// is kept apart from its record and comes back unread; a copy served reads it
// then, as recorded. Main kept a Mongo answer that encodes to under 8 KB in
// its record, decoded at every window open, whether the test made that call
// or not. HTTP answers are kept apart as on main, from 8 KB of body up.
func TestAWindowOpenDecodesNoMongoAnswer(t *testing.T) {
	corpus := answersCorpus()
	names := make([]string, len(corpus))
	recorded := make(map[string]*models.Mock, len(corpus))
	for i, m := range corpus {
		names[i] = m.Name
		recorded[m.Name] = freshRead(t, m)
	}
	for _, budget := range []int64{residentBudgetBytes, 0} {
		d := newStoreWith(t, budget, corpus...)
		var records int64
		for _, m := range corpus {
			e := d.byName[m.Name]
			records += int64(e.length)
			if kept := e.respOff >= 0; kept != keptApart(m) {
				t.Fatalf("budget %d: %s: answer kept apart = %v, want %v", budget, m.Name, kept, keptApart(m))
			}
		}
		before := d.DecodedBytes()
		win, err := d.LoadByNames(names)
		if err != nil || len(win) != len(corpus) {
			t.Fatalf("budget %d: LoadByNames = %d mocks, %v; want %d", budget, len(win), err, len(corpus))
		}
		if opened := d.DecodedBytes() - before; opened != records {
			t.Fatalf("budget %d: a window open decoded %d B, want its records' %d B and no answer kept apart", budget, opened, records)
		}
		for _, m := range win {
			want := recorded[m.Name]
			if m.Kind == models.Mongo && len(m.Spec.MongoResponses) != 0 {
				t.Fatalf("budget %d: the window open decoded the Mongo answer of %s", budget, m.Name)
			}
			if m.HasSpilledResponse() != keptApart(want) {
				t.Fatalf("budget %d: %s comes back with an answer to read = %v, want %v", budget, m.Name, m.HasSpilledResponse(), keptApart(want))
			}
			served := m.ShallowCopy()
			before := d.DecodedBytes()
			if err := served.HydrateResponse(); err != nil {
				t.Fatalf("budget %d: %s: %v", budget, m.Name, err)
			}
			wantRead := int64(0)
			if e := d.byName[m.Name]; e.respOff >= 0 {
				wantRead = int64(e.respLen)
			}
			if read := d.DecodedBytes() - before; read != wantRead {
				t.Fatalf("budget %d: serving %s read %d B, want its answer's %d B", budget, m.Name, read, wantRead)
			}
			if !reflect.DeepEqual(served.Spec.HTTPResp, want.Spec.HTTPResp) || !reflect.DeepEqual(served.Spec.MongoResponses, want.Spec.MongoResponses) || !reflect.DeepEqual(served.Spec.DNSResp, want.Spec.DNSResp) {
				t.Fatalf("budget %d: %s served an answer other than the recorded one", budget, m.Name)
			}
		}
		_ = d.Close()
	}
}

// A Mongo answer under responseSpillMinBytes is read once for the mock read
// with it, however many of its copies are served, at once or one after
// another, and every copy answers with the same messages, as the copies of a
// mock shared the answer its record carried; each gets its own slice of them,
// as a DeepCopy did. A bigger answer, HTTP or Mongo, is read for each copy
// that reads it, as before. Either way the mock read holds none: it stays as
// the store handed it over.
func TestAMongoAnswerUnderResponseSpillMinBytesIsReadOnceForAllItsCopies(t *testing.T) {
	corpus := answersCorpus()
	for _, budget := range []int64{residentBudgetBytes, 0} {
		d := newStoreWith(t, budget, corpus...)
		for _, rec := range corpus {
			e := d.byName[rec.Name]
			if e.respOff < 0 {
				continue
			}
			win, err := d.LoadByNames([]string{rec.Name})
			if err != nil || len(win) != 1 {
				t.Fatalf("budget %d: LoadByNames(%s) = %d mocks, %v", budget, rec.Name, len(win), err)
			}
			m := win[0]
			const serves = 8
			copies := make([]*models.Mock, serves)
			for i := range copies {
				// The serve paths read an answer into a ShallowCopy; a
				// DeepCopy carries the loader too.
				if i%2 == 0 {
					copies[i] = m.ShallowCopy()
				} else {
					copies[i] = m.DeepCopy()
				}
			}
			before := d.DecodedBytes()
			var wg sync.WaitGroup
			errs := make(chan error, serves)
			for _, c := range copies {
				wg.Add(1)
				go func(c *models.Mock) {
					defer wg.Done()
					errs <- c.HydrateResponse()
				}(c)
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatalf("budget %d: %s: %v", budget, rec.Name, err)
				}
			}
			held := e.respLen < responseSpillMinBytes
			want := int64(serves) * int64(e.respLen)
			if held {
				want = int64(e.respLen)
			}
			if read := d.DecodedBytes() - before; read != want {
				t.Fatalf("budget %d: %d copies of %s (a %d B answer) read %d B, want %d", budget, serves, rec.Name, e.respLen, read, want)
			}
			if m.Spec.HTTPResp != nil || len(m.Spec.MongoResponses) != 0 || !m.HasSpilledResponse() {
				t.Fatalf("budget %d: serving the copies of %s wrote an answer into the mock read", budget, rec.Name)
			}
			first := copies[0]
			for _, c := range copies[1:] {
				if c.Spec.HTTPResp != nil && c.Spec.HTTPResp == first.Spec.HTTPResp {
					t.Fatalf("budget %d: %s: two copies share one HTTPResp", budget, rec.Name)
				}
				if rec.Kind != models.Mongo {
					continue
				}
				if &c.Spec.MongoResponses[0] == &first.Spec.MongoResponses[0] {
					t.Fatalf("budget %d: %s: two copies share one slice of responses", budget, rec.Name)
				}
				if shared := c.Spec.MongoResponses[0].Message == first.Spec.MongoResponses[0].Message; shared != held {
					t.Fatalf("budget %d: %s: copies share one message = %v, want %v", budget, rec.Name, shared, held)
				}
			}
		}
		_ = d.Close()
	}
}

// mongoAnswerOfLen is a per-test Mongo mock at second sec whose answer the
// store keeps apart in exactly n bytes.
func mongoAnswerOfLen(t *testing.T, name string, sec int64, n int) *models.Mock {
	t.Helper()
	respLen := func(m *models.Mock) int {
		d := newStoreWith(t, residentBudgetBytes, m)
		defer func() { _ = d.Close() }()
		return d.byName[m.Name].respLen
	}
	probe := mongoAnswerMock(name, sec, n)
	m := mongoAnswerMock(name, sec, 2*n-respLen(probe))
	if got := respLen(m); got != n {
		t.Fatalf("precondition: the answer of %s is kept apart in %d B, want %d", name, got, n)
	}
	return m
}

// The answer held once read is one under responseSpillMinBytes, as the record
// used to carry: one of 8 KB less a byte is read once for two serves, and one
// of 8 KB is read for each.
func TestAMongoAnswerIsHeldOnlyUnderResponseSpillMinBytes(t *testing.T) {
	under := mongoAnswerOfLen(t, "under", 1_700_000_000, responseSpillMinBytes-1)
	at := mongoAnswerOfLen(t, "at", 1_700_000_001, responseSpillMinBytes)
	d := newStoreWith(t, residentBudgetBytes, under, at)
	t.Cleanup(func() { _ = d.Close() })
	for _, tc := range []struct {
		name  string
		reads int64
	}{{"under", 1}, {"at", 2}} {
		win, err := d.LoadByNames([]string{tc.name})
		if err != nil || len(win) != 1 {
			t.Fatalf("LoadByNames(%s) = %d mocks, %v", tc.name, len(win), err)
		}
		n := int64(d.byName[tc.name].respLen)
		before := d.DecodedBytes()
		for i := 0; i < 2; i++ {
			if err := win[0].ShallowCopy().HydrateResponse(); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
		}
		if read := d.DecodedBytes() - before; read != tc.reads*n {
			t.Fatalf("two serves of %s (a %d B answer) read %d B, want %d", tc.name, n, read, tc.reads*n)
		}
	}
}

// A failed read of a Mongo answer under responseSpillMinBytes is not held:
// the next serve reads it again, and the answer that read gets is held, so no
// serve after it reads.
func TestAFailedReadOfAnAnswerIsNotHeld(t *testing.T) {
	rec := mongoAnswerMock("mongo-small", 1_700_000_000, 200)
	d := newStoreWith(t, 0, rec) // every record in the store's file
	t.Cleanup(func() { _ = d.Close() })
	e := d.byName[rec.Name]
	if e.respOff < 0 || e.respLen >= responseSpillMinBytes {
		t.Fatalf("precondition: the answer of %s is not one the store holds once read (%+v)", rec.Name, e)
	}
	win, err := d.LoadByNames([]string{rec.Name})
	if err != nil || len(win) != 1 {
		t.Fatalf("LoadByNames = %d mocks, %v", len(win), err)
	}
	closed, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	_ = closed.Close()
	d.mu.Lock()
	file := d.f
	d.f = closed
	d.mu.Unlock()
	if err := win[0].ShallowCopy().HydrateResponse(); err == nil {
		t.Fatal("a serve read the answer from a closed file without an error")
	}
	d.mu.Lock()
	d.f = file
	d.mu.Unlock()
	before := d.DecodedBytes()
	for i := 0; i < 3; i++ {
		served := win[0].ShallowCopy()
		if err := served.HydrateResponse(); err != nil || len(served.Spec.MongoResponses) != 1 {
			t.Fatalf("serve %d after the failed read: %d responses, %v; want the recorded answer", i+1, len(served.Spec.MongoResponses), err)
		}
	}
	if read := d.DecodedBytes() - before; read != int64(e.respLen) {
		t.Fatalf("3 serves after the failed read read %d B, want the answer's %d B once", read, e.respLen)
	}
}
