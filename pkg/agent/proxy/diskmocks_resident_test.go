package proxy

import (
	"bytes"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

// recordedMocks decodes every mocks.yaml under dir, as a replay reads them.
func recordedMocks(t *testing.T, dir string) []*models.Mock {
	t.Helper()
	var out []*models.Mock
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
		out = append(out, ms...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// storeCorpus is a mock of each kind the store holds, with answers on both
// sides of responseSpillMinBytes, and the MySQL recording of the
// mysql-capture-starved lane, each made per-test at its own second.
func storeCorpus(t *testing.T) []*models.Mock {
	t.Helper()
	ms := recordedMocks(t, filepath.Join("..", "..", "platform", "yaml", "mockdb", "testdata"))
	if len(ms) == 0 {
		t.Fatal("precondition: no recorded mocks in the mockdb testdata")
	}
	ms = append(ms,
		bigMockAt("http-small", time.Unix(1, 0), 300),
		bigMockAt("http-big", time.Unix(1, 0), 3*responseSpillMinBytes),
		mongoAnswerMock("mongo-small", 1, 200),
		mongoAnswerMock("mongo-big", 1, 2*responseSpillMinBytes),
	)
	at := time.Date(2026, 10, 27, 1, 2, 3, 456789, time.FixedZone("IST", 5*3600+1800))
	pg := perTestMockAt("pg", 1)
	pg.Kind = models.PostgresV3
	pg.Spec.PostgresV3 = &models.PostgresV3Spec{Type: models.PostgresV3TypeQuery, Query: &models.PostgresV3QuerySpec{
		SQLAstHash: "h1", SQLNormalized: "select * from t where id = $1", ParamOIDs: []uint32{23},
		Response: &models.PostgresV3Response{CommandComplete: "SELECT 2", Rows: []models.PostgresV3Cells{
			{{Value: int64(7)}, {Value: "seven"}, {Value: at}, {Value: []byte{0, 1, 2}}, {Value: nil}},
			{{Value: int32(8)}, {Value: true}, {Value: 2.5}, {Value: uint32(9)}, {Value: int16(-1)}},
		}},
	}}
	grpc := perTestMockAt("grpc", 1)
	grpc.Kind = models.GRPC_EXPORT
	grpc.Spec.GRPCReq = &models.GrpcReq{Headers: models.GrpcHeaders{PseudoHeaders: map[string]string{":path": "/svc/M", ":method": "POST"}}, Body: models.GrpcLengthPrefixedMessage{MessageLength: 3, DecodedData: "abc"}}
	grpc.Spec.GRPCResp = &models.GrpcResp{Headers: models.GrpcHeaders{OrdinaryHeaders: map[string]string{"content-type": "application/grpc", "a": "b"}}, Body: models.GrpcLengthPrefixedMessage{MessageLength: 2, DecodedData: "ok"}}
	generic := perTestMockAt("generic", 1)
	generic.Kind = models.GENERIC
	generic.Spec.GenericRequests = []models.Payload{{Origin: models.FromClient, Message: []models.OutputBinary{{Type: "binary", Data: "AAEC"}}}}
	generic.Spec.GenericResponses = []models.Payload{{Origin: models.FromServer, Message: []models.OutputBinary{{Type: "utf-8", Data: "pong"}}}}
	dns := perTestMockAt("dns", 1)
	dns.Kind = models.DNS
	dns.Spec.DNSReq = &models.DNSReq{Name: "db.local.", Qtype: 1, Qclass: 1}
	dns.Spec.DNSResp = &models.DNSResp{Rcode: 0, Authoritative: true}
	ms = append(ms, pg, grpc, generic, dns)
	base := time.Unix(1_700_000_000, 0).UTC()
	for i, m := range ms {
		m.Name = fmt.Sprintf("%03d-%s", i, m.Name)
		if m.Spec.Metadata == nil {
			m.Spec.Metadata = map[string]string{"type": "corpus", "connID": fmt.Sprint(i)}
		}
		if m.Kind == models.HTTP && m.Spec.HTTPReq == nil {
			m.Spec.HTTPReq = &models.HTTPReq{Method: "GET", URL: "http://upstream/" + m.Name, Header: map[string]string{"Host": "upstream", "Accept": "*/*"}}
			m.Spec.HTTPResp.Header = map[string]string{"Content-Type": "application/json", "X-Req": m.Name}
		}
		m.TestModeInfo.Lifetime, m.TestModeInfo.LifetimeDerived = models.LifetimePerTest, true
		m.Spec.ReqTimestampMock = base.Add(time.Duration(i) * time.Second)
		m.Spec.ResTimestampMock = m.Spec.ReqTimestampMock.Add(time.Millisecond)
	}
	return ms
}

// freshRead is m as the store read every mock before it kept decoders: the
// mock encoded alone, with a new gob.Encoder, and decoded with a new
// gob.Decoder; its answer, kept apart or not, decodes alike.
func freshRead(t *testing.T, m *models.Mock) *models.Mock {
	t.Helper()
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(m); err != nil {
		t.Fatal(err)
	}
	var out models.Mock
	if err := gob.NewDecoder(&buf).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return &out
}

// servedRead reads the mock named name as the store serves it, its answer
// loaded.
func servedRead(t *testing.T, d *DiskMocks, name string) *models.Mock {
	t.Helper()
	got, err := d.LoadByNames([]string{name})
	if err != nil || len(got) != 1 {
		t.Fatalf("LoadByNames(%s) = %d mocks, %v", name, len(got), err)
	}
	if err := got[0].HydrateResponse(); err != nil {
		t.Fatalf("HydrateResponse(%s): %v", name, err)
	}
	return got[0]
}

// sameAsFresh fails unless the store serves every mock in ms as a new
// decoder reads it, in the order given.
func sameAsFresh(t *testing.T, d *DiskMocks, ms []*models.Mock, label string) {
	t.Helper()
	for _, m := range ms {
		want, got := freshRead(t, m), servedRead(t, d, m.Name)
		if !reflect.DeepEqual(want, got) {
			t.Fatalf("%s: mock %s served\n%+v\nwant, as a new decoder reads it,\n%+v", label, m.Name, got.Spec, want.Spec)
		}
	}
}

// TestStoreServesEveryMockAsANewDecoderReadsIt: whether its records are kept
// in memory or on file, and in any order, the store serves each mock, answer
// included, equal to what a new gob.Decoder read from the mock encoded alone,
// as the store read it before: one stream and kept decoders change the cost
// of a read, never what it reads.
func TestStoreServesEveryMockAsANewDecoderReadsIt(t *testing.T) {
	corpus := storeCorpus(t)
	var total int64
	{
		d := newStoreWith(t, residentBudgetBytes, corpus...)
		total = d.ResidentBytes()
		_ = d.Close()
	}
	for _, budget := range []int64{0, total / 2, residentBudgetBytes} {
		d := newStoreWith(t, budget, corpus...)
		if budget == total/2 && (d.ResidentBytes() == 0 || d.DiskBytes() == 0) {
			t.Fatalf("precondition: budget %d keeps %d B in memory and %d B on file; want both tiers used", budget, d.ResidentBytes(), d.DiskBytes())
		}
		sameAsFresh(t, d, corpus, fmt.Sprintf("budget %d, recorded order", budget))
		rev := append([]*models.Mock(nil), corpus...)
		for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
			rev[i], rev[j] = rev[j], rev[i]
		}
		sameAsFresh(t, d, rev, fmt.Sprintf("budget %d, reversed", budget))
		_ = d.Close()
	}
}

// TestStoreServesRecordedSetsAsANewDecoderReadsThem is the same comparison
// over recorded sets, when KEPLOY_STORE_RECORDINGS names a directory of them
// (the 100/1k/10k sets of the replay latency harness, on the test boxes).
func TestStoreServesRecordedSetsAsANewDecoderReadsThem(t *testing.T) {
	dir := os.Getenv("KEPLOY_STORE_RECORDINGS")
	if dir == "" {
		t.Skip("KEPLOY_STORE_RECORDINGS names no recorded sets")
	}
	ms := recordedMocks(t, dir)
	if len(ms) == 0 {
		t.Fatalf("no mocks under %s", dir)
	}
	stored := ms[:0]
	for _, m := range ms {
		m.DeriveLifetime()
		if !EligibleForDisk(m) {
			m.TestModeInfo.Lifetime = models.LifetimePerTest
		}
		if EligibleForDisk(m) {
			stored = append(stored, m)
		}
	}
	for _, budget := range []int64{0, residentBudgetBytes} {
		d := newStoreWith(t, budget, stored...)
		sameAsFresh(t, d, stored, fmt.Sprintf("%d recorded mocks, budget %d", len(stored), budget))
		t.Logf("%d recorded mocks, budget %d: equal; %d decoders built", len(stored), budget, d.DecodersBuilt())
		_ = d.Close()
	}
}

// TestWindowReadsBuildOneDecoder is the count row for kept decoders: a
// store read one test window after another, each mock's answer loaded as it
// is served, builds one gob decoder in all. With a decoder per record, as
// before, the same reads built one per mock and one per answer, every test.
func TestWindowReadsBuildOneDecoder(t *testing.T) {
	corpus := storeCorpus(t)
	for _, budget := range []int64{0, residentBudgetBytes} {
		d := newStoreWith(t, budget, corpus...)
		first, last := corpus[0].Spec.ReqTimestampMock, corpus[len(corpus)-1].Spec.ReqTimestampMock
		const tests = 20
		for i := 0; i < tests; i++ {
			win, err := d.LoadWindow(first, last)
			if err != nil || len(win) != len(corpus) {
				t.Fatalf("budget %d: window %d = %d mocks, %v; want %d", budget, i, len(win), err, len(corpus))
			}
			for _, m := range win {
				if err := m.HydrateResponse(); err != nil {
					t.Fatal(err)
				}
			}
		}
		if got := d.DecodersBuilt(); got != 1 {
			t.Fatalf("budget %d: %d windows of %d mocks built %d gob decoders, want 1", budget, tests, len(corpus), got)
		}
		_ = d.Close()
	}
}

// TestStoreKeepsRecordsInMemoryUpToItsBudget: the records that fit the
// budget, in the order added, stay in memory; from the first that does not,
// every record goes to the file, small or not; nothing is kept beyond the
// budget, and nothing is lost: every mock reads back from either tier.
func TestStoreKeepsRecordsInMemoryUpToItsBudget(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()
	var ms []*models.Mock
	for i := 0; i < 40; i++ {
		body := 100
		if i == 20 {
			body = 3 * responseSpillMinBytes
		}
		m := bigMockAt(fmt.Sprintf("m%02d", i), base.Add(time.Duration(i)*time.Second), body)
		m.TestModeInfo.Lifetime = models.LifetimePerTest
		ms = append(ms, m)
	}
	all := newStoreWith(t, residentBudgetBytes, ms...)
	total := all.ResidentBytes()
	if all.DiskBytes() != 0 {
		t.Fatalf("a store within its budget wrote %d B to its file", all.DiskBytes())
	}
	_ = all.Close()

	// A budget the first 20 small records fit, with room to spare but not
	// for the big one's answer and record.
	var upTo20 int64
	{
		d := newStoreWith(t, residentBudgetBytes, ms[:20]...)
		upTo20 = d.ResidentBytes()
		_ = d.Close()
	}
	budget := upTo20 + 2*1024
	d := newStoreWith(t, budget, ms...)
	defer d.Close()
	if got := d.ResidentBytes(); got != upTo20 {
		t.Fatalf("budget %d: %d B kept in memory, want the first 20 records' %d B", budget, got, upTo20)
	}
	if got, want := d.DiskBytes(), total-upTo20; got != want {
		t.Fatalf("budget %d: %d B on file, want the rest, %d B", budget, got, want)
	}
	d.mu.Lock()
	for i, m := range ms {
		e := d.byName[m.Name]
		if inMem := e.off < d.memEnd; inMem != (i < 20) {
			d.mu.Unlock()
			t.Fatalf("mock %s in memory = %v, want %v", m.Name, inMem, i < 20)
		}
	}
	d.mu.Unlock()
	sameAsFresh(t, d, ms, fmt.Sprintf("budget %d", budget))
	for _, m := range ms {
		got := servedRead(t, d, m.Name)
		if got.Spec.HTTPResp == nil || got.Spec.HTTPResp.Body != m.Spec.HTTPResp.Body {
			t.Fatalf("mock %s did not read back its body", m.Name)
		}
	}
}

// TestStoreHandsOutCopies: each read is a mock of its own, from memory or
// file: what a caller changes in one, answer included, no later read sees.
func TestStoreHandsOutCopies(t *testing.T) {
	for _, budget := range []int64{0, residentBudgetBytes} {
		corpus := storeCorpus(t)
		d := newStoreWith(t, budget, corpus...)
		for _, m := range corpus {
			if !strings.HasSuffix(m.Name, "-http-small") && !strings.HasSuffix(m.Name, "-http-big") &&
				!strings.HasSuffix(m.Name, "-mongo-big") && !strings.HasSuffix(m.Name, "-pg") {
				continue
			}
			name := m.Name
			a := servedRead(t, d, name)
			want := freshRead(t, m)
			a.Spec.Metadata["changed"] = "yes"
			if a.Spec.HTTPResp != nil {
				a.Spec.HTTPResp.Header["X-Changed"] = "yes"
				a.Spec.HTTPResp.Body = "changed"
			}
			if len(a.Spec.MongoResponses) > 0 {
				a.Spec.MongoResponses[0].Message.(*models.MongoOpMessage).Sections[0] = "changed"
			}
			if a.Spec.PostgresV3 != nil {
				a.Spec.PostgresV3.Query.Response.Rows[0][0].Value = "changed"
			}
			if b := servedRead(t, d, name); !reflect.DeepEqual(b, want) {
				t.Fatalf("budget %d: a change to one read of %s reached the next", budget, name)
			}
		}
		_ = d.Close()
	}
}

// TestStoreServesConcurrentReadersAlike: readers in parallel, each with a
// kept decoder of its own, all read what a new decoder reads.
func TestStoreServesConcurrentReadersAlike(t *testing.T) {
	corpus := storeCorpus(t)
	d := newStoreWith(t, residentBudgetBytes, corpus...)
	defer d.Close()
	want := make(map[string]*models.Mock, len(corpus))
	for _, m := range corpus {
		want[m.Name] = freshRead(t, m)
	}
	var wg sync.WaitGroup
	errs := make(chan string, 8)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			r := rand.New(rand.NewSource(seed))
			for i := 0; i < 200; i++ {
				m := corpus[r.Intn(len(corpus))]
				got, err := d.LoadByNames([]string{m.Name})
				if err == nil && len(got) == 1 {
					err = got[0].HydrateResponse()
				}
				if err != nil || len(got) != 1 || !reflect.DeepEqual(got[0], want[m.Name]) {
					errs <- fmt.Sprintf("%s: read %d mocks, %v, or not as a new decoder reads it", m.Name, len(got), err)
					return
				}
			}
		}(int64(g))
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
	if got := d.DecodersBuilt(); got > 8 {
		t.Fatalf("8 readers built %d gob decoders, want at most one each", got)
	}
}

// TestStoreReadsOnAfterAMockItCannotEncode: a mock gob cannot encode is
// refused, as before (the agent keeps it resident), and the store goes on:
// the encode may have sent type definitions no record holds, so every mock
// after it is stored, and read, as a gob stream of its own, and each reads
// as a new decoder reads it.
func TestStoreReadsOnAfterAMockItCannotEncode(t *testing.T) {
	type unregistered struct{ X int }
	bad := perTestMockAt("bad", 1_700_000_000)
	bad.Kind = models.Mongo
	bad.Spec.MongoRequests = []models.MongoRequest{{Header: &models.MongoHeader{Opcode: 2013}, Message: unregistered{X: 1}}}
	corpus := storeCorpus(t)
	for _, budget := range []int64{0, residentBudgetBytes} {
		d, err := newDiskMocks(zap.NewNop(), budget)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.Add(bad); err == nil {
			t.Fatalf("budget %d: a mock gob cannot encode was stored", budget)
		}
		for _, m := range corpus {
			if err := d.Add(m); err != nil {
				t.Fatalf("budget %d: Add(%s) after a refused mock: %v", budget, m.Name, err)
			}
		}
		d.Finalize()
		sameAsFresh(t, d, corpus, fmt.Sprintf("budget %d, after a refused mock", budget))
		_ = d.Close()
	}
}

// TestCorpusTeachesItsTypesInLessons: the corpus's MySQL and Mongo mocks
// hold interface values, which gob splits around their types' definitions,
// so the comparisons above cover records read after lessons; and no stored
// record holds a definition.
func TestCorpusTeachesItsTypesInLessons(t *testing.T) {
	d := newStoreWith(t, residentBudgetBytes, storeCorpus(t)...)
	defer d.Close()
	if n := len(d.records.lessons); n < 3 {
		t.Fatalf("the corpus wrote %d lessons, want the first mock's and its interface values' (3 or more)", n)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for name, e := range d.byName {
		for _, at := range [][2]int64{{e.off, int64(e.length)}, {e.respOff, int64(e.respLen)}} {
			if at[0] < 0 {
				continue
			}
			if b := d.mem[at[0]]; !oneValueMessage(b) {
				t.Fatalf("mock %s: stored %d bytes at %d that are not one value message", name, len(b), at[0])
			}
		}
	}
}

// BenchmarkStoreWindowRead reads the corpus as one test's window, answers
// loaded: "alone" as the store read it before (each mock its own gob stream,
// a new decoder per mock and per answer), "kept" as it reads it now. Run on
// the test boxes only.
func BenchmarkStoreWindowRead(b *testing.B) {
	t := &testing.T{}
	corpus := storeCorpus(t)
	b.Run("alone", func(b *testing.B) {
		recs := make([][]byte, len(corpus))
		for i, m := range corpus {
			var buf bytes.Buffer
			if err := gob.NewEncoder(&buf).Encode(m); err != nil {
				b.Fatal(err)
			}
			recs[i] = buf.Bytes()
		}
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			for _, r := range recs {
				var m models.Mock
				if err := gob.NewDecoder(bytes.NewReader(r)).Decode(&m); err != nil {
					b.Fatal(err)
				}
			}
		}
	})
	for _, budget := range []int64{0, residentBudgetBytes} {
		b.Run(fmt.Sprintf("kept/budget=%d", budget), func(b *testing.B) {
			d, err := newDiskMocks(zap.NewNop(), budget)
			if err != nil {
				b.Fatal(err)
			}
			defer d.Close()
			for _, m := range corpus {
				if err := d.Add(m); err != nil {
					b.Fatal(err)
				}
			}
			d.Finalize()
			first, last := corpus[0].Spec.ReqTimestampMock, corpus[len(corpus)-1].Spec.ReqTimestampMock
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				win, err := d.LoadWindow(first, last)
				if err != nil {
					b.Fatal(err)
				}
				for _, m := range win {
					if err := m.HydrateResponse(); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}
