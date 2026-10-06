package mockdb

import (
	"bufio"
	"bytes"
	"context"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/metrics"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/utils"
	"go.mongodb.org/mongo-driver/v2/x/mongo/driver/wiremessage"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

// The prune (UpdateMocks) rewrites a test set's whole mock file after a
// replay. These tests pin what it must keep doing as recordings grow to
// hundreds of MB: produce the same bytes the read-everything prune produced,
// hold one mock at a time rather than the file, and never lose the mocks it
// was asked to prune when it fails or dies part way.

const pruneSet = "set-0"

var pruneT0 = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// pruneMySQLMock is a COM_QUERY answered by a text result set of rows rows of
// six columns: the shape of a recording that is mostly database reads, with
// one YAML node per cell.
func pruneMySQLMock(i, rows int) *models.Mock {
	cols := []string{"id", "session", "owner", "state", "created", "payload"}
	defs := make([]*mysql.ColumnDefinition41, len(cols))
	for c, n := range cols {
		defs[c] = &mysql.ColumnDefinition41{
			Header:  mysql.Header{PayloadLength: 40, SequenceID: uint8(c + 2)},
			Catalog: "def", Schema: "app", Table: "sessions", OrgTable: "sessions", Name: n, OrgName: n,
			FixedLength: 0x0c, CharacterSet: 33, ColumnLength: 255, Type: 0xfd,
		}
	}
	rs := make([]*mysql.TextRow, rows)
	for r := range rs {
		vals := make([]mysql.ColumnEntry, len(cols))
		for c, n := range cols {
			vals[c] = mysql.ColumnEntry{Type: mysql.FieldTypeVarString, Name: n, Value: fmt.Sprintf("v-%d-%d-%d", i, r, c)}
		}
		rs[r] = &mysql.TextRow{Header: mysql.Header{PayloadLength: 60, SequenceID: uint8(r + 9)}, Values: vals}
	}
	ts := pruneT0.Add(time.Duration(i) * time.Second)
	return &models.Mock{
		Version: models.GetVersion(),
		Kind:    models.MySQL,
		Spec: models.MockSpec{
			Metadata: map[string]string{"type": "mocks", "connID": fmt.Sprint(i % 8)},
			MySQLRequests: []mysql.Request{{PacketBundle: mysql.PacketBundle{
				Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 30}, Type: "COM_QUERY"},
				Message: &mysql.QueryPacket{Command: 0x03, Query: fmt.Sprintf("SELECT * FROM sessions WHERE id = %d", i)},
			}}},
			MySQLResponses: []mysql.Response{{PacketBundle: mysql.PacketBundle{
				Header: &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 1, SequenceID: 1}, Type: string(mysql.Text)},
				Message: &mysql.TextResultSet{
					ColumnCount: uint64(len(cols)), Columns: defs, Rows: rs,
					FinalResponse: &mysql.GenericResponse{Data: []byte{0xfe, 0, 0, 2, 0}, Type: "EOF"},
				},
			}}},
			Created:          ts.Unix(),
			ReqTimestampMock: ts,
			ResTimestampMock: ts.Add(time.Millisecond),
		},
	}
}

func pruneHTTPMock(i int) *models.Mock {
	m := noiseTestMock(fmt.Sprintf(`{"request_id":"r-%d","n":%d}`, i, i))
	ts := pruneT0.Add(time.Duration(i) * time.Second)
	m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock = ts, ts.Add(time.Millisecond)
	return m
}

func pruneMongoMock(i int) *models.Mock {
	ts := pruneT0.Add(time.Duration(i) * time.Second)
	return &models.Mock{
		Version: models.GetVersion(),
		Kind:    models.Mongo,
		Spec: models.MockSpec{
			Metadata: map[string]string{"type": "mocks"},
			MongoRequests: []models.MongoRequest{{
				Header:  &models.MongoHeader{Length: 50, RequestID: int32(i), Opcode: wiremessage.OpMsg},
				Message: &models.MongoOpMessage{Sections: []string{fmt.Sprintf(`{ SectionSingle msg: {"find":"c","n":%d} }`, i)}},
			}},
			MongoResponses: []models.MongoResponse{{
				Header:  &models.MongoHeader{Length: 60, RequestID: int32(i + 1), ResponseTo: int32(i), Opcode: wiremessage.OpMsg},
				Message: &models.MongoOpMessage{Sections: []string{`{ SectionSingle msg: {"ok":1} }`}},
			}},
			ReqTimestampMock: ts,
			ResTimestampMock: ts.Add(time.Millisecond),
		},
	}
}

func pruneDNSConfigMock(i int) *models.Mock {
	ts := pruneT0.Add(time.Duration(i) * time.Second)
	return &models.Mock{
		Version: models.GetVersion(),
		Kind:    models.DNS,
		Spec: models.MockSpec{
			Metadata:         map[string]string{"type": "config", "name": "DNS"},
			DNSReq:           &models.DNSReq{Name: "db.svc.cluster.local.", Qtype: 1, Qclass: 1},
			DNSResp:          &models.DNSResp{Answers: []string{"db.svc.cluster.local.\t30\tIN\tA\t10.0.0.7"}},
			ReqTimestampMock: ts,
			ResTimestampMock: ts.Add(time.Millisecond),
		},
	}
}

// writePruneFixture records mocks into <dir>/set-0 through InsertMock in the
// given format ("yaml", "json" or "gob"). raw, when set, is inserted verbatim
// as one more document after the first half of the mocks — a document the
// decoders skip (an enterprise-only kind with no mapper registered).
func writePruneFixture(t testing.TB, dir, format string, mocks []*models.Mock, raw string) string {
	t.Helper()
	// The writer picks gob from the environment first: pin it either way.
	t.Setenv("KEPLOY_MOCK_FORMAT", "")
	f := yaml.FormatYAML
	switch format {
	case "json":
		f = yaml.FormatJSON
	case "gob":
		t.Setenv("KEPLOY_MOCK_FORMAT", "gob")
	}
	ys := NewWithFormat(zap.NewNop(), dir, "mocks", f)
	ctx := context.Background()
	ext := map[string]string{"yaml": "yaml", "json": "json", "gob": "gob"}[format]
	path := filepath.Join(dir, pruneSet, "mocks."+ext)
	for i, m := range mocks {
		if raw != "" && i == len(mocks)/2 {
			if err := ys.Close(); err != nil {
				t.Fatal(err)
			}
			fh, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := fh.WriteString(raw); err != nil {
				t.Fatal(err)
			}
			if err := fh.Close(); err != nil {
				t.Fatal(err)
			}
		}
		if err := ys.InsertMock(ctx, m, pruneSet); err != nil {
			t.Fatalf("InsertMock %d: %v", i, err)
		}
	}
	if err := ys.Close(); err != nil {
		t.Fatal(err)
	}
	if format == "gob" {
		// The env var only steers the writer; the prune detects mocks.gob by
		// its presence.
		t.Setenv("KEPLOY_MOCK_FORMAT", "")
	}
	return path
}

// pruneMixedFixture is a set of every shape the prune decides on: config
// mocks, consumed mocks (some with learned noise), unconsumed mocks recorded
// before the startup cutoff, between it and the replay, and after the replay
// started.
func pruneMixedFixture() []*models.Mock {
	var mocks []*models.Mock
	for i := 0; i < 48; i++ {
		switch i % 4 {
		case 0:
			mocks = append(mocks, pruneMySQLMock(i, 3))
		case 1:
			mocks = append(mocks, pruneHTTPMock(i))
		case 2:
			mocks = append(mocks, pruneMongoMock(i))
		case 3:
			if i%8 == 3 {
				mocks = append(mocks, pruneDNSConfigMock(i))
			} else {
				mocks = append(mocks, pruneMySQLMock(i, 1))
			}
		}
	}
	return mocks
}

// pruneMixedArgs returns what a replay hands UpdateMocks for the mixed fixture:
// a third of the mocks consumed (every other one of those with learned noise),
// the startup cutoff after the 6th mock and the replay start after the 40th.
func pruneMixedArgs() (map[string]models.MockState, time.Time, time.Time) {
	consumed := map[string]models.MockState{}
	for i := 0; i < 48; i += 3 {
		name := fmt.Sprintf("mock-%d", i)
		st := models.MockState{Name: name}
		if i%2 == 0 {
			st.ReqBodyNoise = map[string][]string{"body.request_id": {}}
		}
		consumed[name] = st
	}
	startupCutoff := pruneT0.Add(6*time.Second + time.Millisecond/2)
	pruneBefore := pruneT0.Add(40*time.Second + time.Millisecond/2)
	return consumed, pruneBefore, startupCutoff
}

const pruneEnterpriseYAMLDoc = "---\nversion: api.keploy.io/v1beta1\nkind: Acme-Queue\nname: mock-acme\nspec:\n    metadata:\n        type: mocks\n"
const pruneEnterpriseJSONDoc = `{"version":"api.keploy.io/v1beta1","kind":"Acme-Queue","name":"mock-acme","spec":{"metadata":{"type":"mocks"}}}` + "\n"

// readAllPrune is the prune as it was before it streamed: read every
// document, decode them all, filter, then write the kept mocks. It is the
// oracle the streaming prune must match byte for byte. Returns nil when the
// old prune removed the file (nothing kept, yaml/json).
func readAllPrune(t *testing.T, path string, mockNames map[string]models.MockState, pruneBefore, startupCutoffTime time.Time) []byte {
	t.Helper()
	var mocks []*models.Mock
	ext := filepath.Ext(path)
	if ext == ".gob" {
		var err error
		mocks, err = readGobMocks(path)
		if err != nil {
			t.Fatal(err)
		}
	} else {
		f := yaml.FormatYAML
		if ext == ".json" {
			f = yaml.FormatJSON
		}
		reader, err := yaml.NewMockReaderF(context.Background(), zap.NewNop(), filepath.Dir(path), "mocks", f)
		if err != nil {
			t.Fatal(err)
		}
		defer reader.Close()
		if f == yaml.FormatJSON {
			var docs []*yaml.NetworkTrafficDocJSON
			for {
				d, err := reader.ReadNextDocJSON()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				docs = append(docs, d)
			}
			if mocks, err = DecodeMocksJSON(docs, zap.NewNop()); err != nil {
				t.Fatal(err)
			}
		} else {
			var docs []*yaml.NetworkTrafficDoc
			for {
				d, err := reader.ReadNextDoc()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				docs = append(docs, d)
			}
			if mocks, err = DecodeMocks(docs, zap.NewNop()); err != nil {
				t.Fatal(err)
			}
		}
	}
	var kept []*models.Mock
	for _, mock := range mocks {
		if mock.Spec.Metadata["type"] == "config" {
			kept = append(kept, mock)
			continue
		}
		if st, ok := mockNames[mock.Name]; ok {
			if len(st.ReqBodyNoise) > 0 {
				mock.Spec.ReqBodyNoise = mergeReqBodyNoise(mock.Spec.ReqBodyNoise, st.ReqBodyNoise)
			}
			kept = append(kept, mock)
			continue
		}
		if !mock.Spec.ReqTimestampMock.IsZero() && mock.Spec.ReqTimestampMock.After(pruneBefore) {
			kept = append(kept, mock)
			continue
		}
		if !startupCutoffTime.IsZero() && !mock.Spec.ReqTimestampMock.IsZero() &&
			mock.Spec.ReqTimestampMock.Before(startupCutoffTime) {
			kept = append(kept, mock)
			continue
		}
	}
	var buf bytes.Buffer
	switch ext {
	case ".gob":
		buf.WriteString(gobMockMagic)
		enc := gob.NewEncoder(&buf)
		for _, m := range kept {
			if err := enc.Encode(m); err != nil {
				t.Fatal(err)
			}
		}
		return buf.Bytes()
	case ".json":
		if len(kept) == 0 {
			return nil
		}
		enc := json.NewEncoder(&buf)
		for _, m := range kept {
			doc, handled, err := EncodeMockJSON(m, zap.NewNop())
			if err != nil || !handled {
				t.Fatalf("EncodeMockJSON(%s): handled=%v err=%v", m.Name, handled, err)
			}
			if err := enc.Encode(doc); err != nil {
				t.Fatal(err)
			}
		}
		return buf.Bytes()
	default:
		if len(kept) == 0 {
			return nil
		}
		buf.WriteString(utils.GetVersionAsComment())
		for i, m := range kept {
			if i > 0 {
				buf.WriteString("---\n")
			}
			doc, err := EncodeMock(m, zap.NewNop())
			if err != nil {
				t.Fatal(err)
			}
			data, err := yamlLib.Marshal(&doc)
			if err != nil {
				t.Fatal(err)
			}
			buf.Write(data)
		}
		return buf.Bytes()
	}
}

func copyFile(t testing.TB, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// dirEntries lists a test set dir's file names, sorted.
func dirEntries(t testing.TB, dir string) []string {
	t.Helper()
	des, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, de := range des {
		out = append(out, de.Name())
	}
	sort.Strings(out)
	return out
}

// sameMockFile compares a rewritten mock file with the oracle's bytes. YAML
// and JSON must match byte for byte. gob cannot: encoding/gob writes a map in
// Go's random iteration order, so even the read-all prune never wrote the
// same gob bytes twice; there the decoded mocks must match, in order.
func sameMockFile(t *testing.T, path string, got, want []byte) bool {
	t.Helper()
	if filepath.Ext(path) != ".gob" {
		return bytes.Equal(got, want)
	}
	decode := func(b []byte) []*models.Mock {
		f := filepath.Join(t.TempDir(), "mocks.gob")
		if err := os.WriteFile(f, b, 0o644); err != nil {
			t.Fatal(err)
		}
		ms, err := readGobMocks(f)
		if err != nil {
			t.Fatal(err)
		}
		return ms
	}
	return reflect.DeepEqual(decode(got), decode(want))
}

// TestUpdateMocks_WritesWhatTheReadAllPruneWrote pins the streaming prune to
// the bytes the read-everything prune produced for the same input, in every
// storage format, including the cases where it keeps nothing.
func TestUpdateMocks_WritesWhatTheReadAllPruneWrote(t *testing.T) {
	consumed, pruneBefore, startupCutoff := pruneMixedArgs()
	cases := []struct {
		name, format, raw string
		consumed          map[string]models.MockState
		pruneBefore       time.Time
		startupCutoff     time.Time
	}{
		{"yaml", "yaml", pruneEnterpriseYAMLDoc, consumed, pruneBefore, startupCutoff},
		{"json", "json", pruneEnterpriseJSONDoc, consumed, pruneBefore, startupCutoff},
		{"gob", "gob", "", consumed, pruneBefore, startupCutoff},
		{"yaml keeps everything", "yaml", "", consumed, pruneT0, startupCutoff},
		{"yaml keeps only config mocks", "yaml", "", nil, pruneT0.Add(time.Hour), time.Time{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writePruneFixture(t, dir, tc.format, pruneMixedFixture(), tc.raw)
			want := readAllPrune(t, path, tc.consumed, tc.pruneBefore, tc.startupCutoff)

			ys := NewWithFormat(zap.NewNop(), dir, "mocks", yaml.FormatYAML)
			if tc.format == "json" {
				ys = NewWithFormat(zap.NewNop(), dir, "mocks", yaml.FormatJSON)
			}
			if err := ys.UpdateMocks(context.Background(), pruneSet, tc.consumed, tc.pruneBefore, tc.startupCutoff); err != nil {
				t.Fatalf("UpdateMocks: %v", err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !sameMockFile(t, path, got, want) {
				t.Fatalf("the prune wrote %d bytes that differ from the read-all prune's %d", len(got), len(want))
			}
			if names := dirEntries(t, filepath.Dir(path)); len(names) != 1 {
				t.Fatalf("the prune left files beside the mock file: %v", names)
			}
		})
	}

	t.Run("yaml keeps nothing", func(t *testing.T) {
		dir := t.TempDir()
		mocks := []*models.Mock{pruneMySQLMock(1, 2), pruneHTTPMock(2)}
		path := writePruneFixture(t, dir, "yaml", mocks, "")
		if want := readAllPrune(t, path, nil, pruneT0.Add(time.Hour), time.Time{}); want != nil {
			t.Fatalf("oracle kept something: %d bytes", len(want))
		}
		ys := New(zap.NewNop(), dir, "mocks")
		if err := ys.UpdateMocks(context.Background(), pruneSet, nil, pruneT0.Add(time.Hour), time.Time{}); err != nil {
			t.Fatalf("UpdateMocks: %v", err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("a prune that keeps nothing must remove the file, as before; stat: %v", err)
		}
		if names := dirEntries(t, filepath.Dir(path)); len(names) != 0 {
			t.Fatalf("the prune left files behind: %v", names)
		}
	})
}

// TestPersistMockNoise_WritesWhatTheReadAllPathWrote does the same for the
// prune-free noise write-back, and checks that a run that learned nothing new
// leaves the file untouched.
func TestPersistMockNoise_WritesWhatTheReadAllPathWrote(t *testing.T) {
	for _, format := range []string{"yaml", "json", "gob"} {
		t.Run(format, func(t *testing.T) {
			dir := t.TempDir()
			path := writePruneFixture(t, dir, format, pruneMixedFixture(), "")
			states := map[string]models.MockState{
				"mock-1": {Name: "mock-1", ReqBodyNoise: map[string][]string{"body.request_id": {}}},
				"mock-5": {Name: "mock-5"},
			}
			// Everything kept: the read-all oracle with every mock consumed, the
			// noise merged onto mock-1 only.
			all := map[string]models.MockState{}
			for i := 0; i < 48; i++ {
				n := fmt.Sprintf("mock-%d", i)
				all[n] = models.MockState{Name: n}
			}
			all["mock-1"] = states["mock-1"]
			want := readAllPrune(t, path, all, pruneT0.Add(time.Hour), time.Time{})

			f := yaml.FormatYAML
			if format == "json" {
				f = yaml.FormatJSON
			}
			ys := NewWithFormat(zap.NewNop(), dir, "mocks", f)
			if err := ys.PersistMockNoise(context.Background(), pruneSet, states); err != nil {
				t.Fatalf("PersistMockNoise: %v", err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !sameMockFile(t, path, got, want) {
				t.Fatalf("noise write-back wrote %d bytes that differ from the read-all path's %d", len(got), len(want))
			}

			// The same noise again changes nothing: the file must not be rewritten.
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := ys.PersistMockNoise(context.Background(), pruneSet, states); err != nil {
				t.Fatalf("PersistMockNoise (again): %v", err)
			}
			after, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			again, _ := os.ReadFile(path)
			if !bytes.Equal(again, got) || !os.SameFile(before, after) {
				t.Fatal("a write-back with nothing new to persist replaced the mock file")
			}
			if names := dirEntries(t, filepath.Dir(path)); len(names) != 1 {
				t.Fatalf("the write-back left files beside the mock file: %v", names)
			}
		})
	}
}

// heapSampleCtx measures what a prune holds while it runs. The prune checks
// its context between lines of a yaml or json mock file (its reader does, once
// per line) and every ctxCheckEvery mocks of a gob stream. At every
// doneEvery-th Done call and every Err call, this context collects garbage
// from the prune's own goroutine and reads the live heap. The prune is paused
// for that collection, so the figure is what the prune holds at that point,
// however the host schedules the collector.
//
// Reading the live heap from another goroutine while the prune ran did not
// measure that: a concurrent collection counts what is allocated during its
// mark as live, and how much the prune allocated during a mark grew with
// GOMAXPROCS and host load. The same prune read 2 MiB with one P on an idle
// CPU, 4-7 MiB on an idle 8-CPU host, and 17-27 MiB with 8 Ps on one CPU.
type heapSampleCtx struct {
	context.Context
	doneEvery int64
	calls     atomic.Int64
	mu        sync.Mutex
	peak      uint64
	samples   int
}

func (c *heapSampleCtx) Done() <-chan struct{} {
	if c.calls.Add(1)%c.doneEvery == 0 {
		c.sample()
	}
	return c.Context.Done()
}

func (c *heapSampleCtx) Err() error {
	c.sample()
	return c.Context.Err()
}

func (c *heapSampleCtx) sample() {
	runtime.GC()
	s := []metrics.Sample{{Name: "/gc/heap/live:bytes"}}
	metrics.Read(s)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.peak = max(c.peak, s[0].Value.Uint64())
	c.samples++
}

// pruneHeapPeak runs prune with a heapSampleCtx that samples about 256 times
// over the file at path (whatever its line count) and returns the most the
// prune held above the live heap before it started, and how many times it
// sampled.
func pruneHeapPeak(t *testing.T, path string, prune func(ctx context.Context) error) (peak uint64, samples int, err error) {
	t.Helper()
	lines := func() int64 {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return int64(bytes.Count(data, []byte("\n")))
	}()
	ctx := &heapSampleCtx{Context: context.Background(), doneEvery: max(1, lines/256)}
	runtime.GC()
	s := []metrics.Sample{{Name: "/gc/heap/live:bytes"}}
	metrics.Read(s)
	base := s[0].Value.Uint64()
	err = prune(ctx)
	if ctx.peak > base {
		peak = ctx.peak - base
	}
	return peak, ctx.samples, err
}

// pruneHeapBudget is what a prune may hold, above its caller's heap, at any
// point it checks its context, whatever the size of the mock file: the mock
// it is reading, its I/O buffers and its encoders. A streaming prune holds
// under 0.5 MiB there on the 17-25 MB files below. Keeping every decoded mock
// would hold 11-48 MiB, reading the whole file into memory 17-25 MB, and the
// read-all prune, which held every mock's parse tree and decoded form at
// once, 31-191 MiB (7 GiB for a 335 MB recording in production).
const pruneHeapBudget = 4 << 20

// TestUpdateMocks_HeapDoesNotScaleWithTheMockFile prunes MySQL mock files
// the read-all prune held 8x to 48x the budget for, and holds what each
// prune holds as it goes under it.
func TestUpdateMocks_HeapDoesNotScaleWithTheMockFile(t *testing.T) {
	cases := []struct {
		format string
		// mocks: gob and JSON pack the same mocks into far fewer bytes, so
		// they need more of them before reading them all would show.
		mocks int
	}{
		{"yaml", 400},  // 17.6 MB; read-all 191 MiB
		{"json", 1200}, // 24.9 MB; read-all 31 MiB
		{"gob", 2400},  // 21.9 MB; read-all 56 MiB
	}
	for _, tc := range cases {
		t.Run(tc.format, func(t *testing.T) {
			dir := t.TempDir()
			// Built and written in their own scope, so the fixture's mocks are
			// garbage before the measurement takes its baseline.
			path := func() string {
				mocks := make([]*models.Mock, tc.mocks)
				for i := range mocks {
					mocks[i] = pruneMySQLMock(i, 40)
				}
				return writePruneFixture(t, dir, tc.format, mocks, "")
			}()
			fi, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			consumed := map[string]models.MockState{}
			for i := 0; i < tc.mocks; i++ {
				if i%10 != 0 {
					name := fmt.Sprintf("mock-%d", i)
					consumed[name] = models.MockState{Name: name, Kind: models.MySQL}
				}
			}
			f := yaml.FormatYAML
			if tc.format == "json" {
				f = yaml.FormatJSON
			}
			ys := NewWithFormat(zap.NewNop(), dir, "mocks", f)
			peak, samples, err := pruneHeapPeak(t, path, func(ctx context.Context) error {
				return ys.UpdateMocks(ctx, pruneSet, consumed, pruneT0.Add(time.Hour), time.Time{})
			})
			if err != nil {
				t.Fatalf("UpdateMocks: %v", err)
			}
			t.Logf("%s: %d mocks, %d-byte file, prune held at most %.2f MiB over %d samples (budget %.0f MiB)",
				tc.format, tc.mocks, fi.Size(), float64(peak)/(1<<20), samples, float64(pruneHeapBudget)/(1<<20))
			// A gob prune checks its context four times here (before it
			// starts, then every ctxCheckEvery mocks), a yaml or json prune
			// on every line. Fewer samples than that measure nothing.
			if samples < 4 {
				t.Fatalf("the prune was sampled %d times; it no longer checks its context where this test measures it", samples)
			}
			if peak > pruneHeapBudget {
				t.Fatalf("pruning a %d-byte mock file held %d bytes of live heap; the budget is %d whatever the file's size", fi.Size(), peak, pruneHeapBudget)
			}
		})
	}
}

// countdownCtx is cancelled at the n-th Done call: the reader checks it once
// per line, so this cancels the prune part way through the file.
type countdownCtx struct {
	context.Context
	n      atomic.Int64
	once   sync.Once
	cancel context.CancelFunc
}

func newCountdownCtx(n int64) *countdownCtx {
	ctx, cancel := context.WithCancel(context.Background())
	c := &countdownCtx{Context: ctx, cancel: cancel}
	c.n.Store(n)
	return c
}

func (c *countdownCtx) Done() <-chan struct{} {
	if c.n.Add(-1) <= 0 {
		c.once.Do(c.cancel)
	}
	return c.Context.Done()
}

// TestUpdateMocks_AFailedPruneLeavesTheMocksFileWhole: whatever stops a
// prune part way, the file it was pruning is still there, byte for byte, and
// nothing else is left beside it.
func TestUpdateMocks_AFailedPruneLeavesTheMocksFileWhole(t *testing.T) {
	consumed, pruneBefore, startupCutoff := pruneMixedArgs()
	cases := []struct {
		name    string
		raw     string
		ctx     func() context.Context
		replace func(logger *zap.Logger, src, dst string) error
	}{
		{name: "a document does not parse", raw: "---\nversion: [unterminated\n"},
		{name: "a document does not decode", raw: "---\nversion: api.keploy.io/v1beta1\nkind: Http\nname: mock-bad\nspec: 3\n"},
		{name: "the context ends part way", ctx: func() context.Context { return newCountdownCtx(400) }},
		{
			// A replace the platform refuses (e.g. a scanner holding the new
			// file open): yaml.ReplaceFile then puts the original back and
			// fails — see its tests for that fallback.
			name: "the new file cannot be put in place",
			replace: func(_ *zap.Logger, src, dst string) error {
				return &os.LinkError{Op: "rename", Old: src, New: dst, Err: errors.New("sharing violation")}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writePruneFixture(t, dir, "yaml", pruneMixedFixture(), tc.raw)
			orig, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if tc.replace != nil {
				defer func(r func(*zap.Logger, string, string) error) { replaceFile = r }(replaceFile)
				replaceFile = tc.replace
			}
			ctx := context.Background()
			if tc.ctx != nil {
				ctx = tc.ctx()
			}
			ys := New(zap.NewNop(), dir, "mocks")
			if err := ys.UpdateMocks(ctx, pruneSet, consumed, pruneBefore, startupCutoff); err == nil {
				t.Fatal("UpdateMocks succeeded; the case did not make it fail")
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("the mock file is gone after a failed prune: %v", err)
			}
			if !bytes.Equal(got, orig) {
				t.Fatal("a failed prune changed the mock file")
			}
			if names := dirEntries(t, filepath.Dir(path)); len(names) != 1 {
				t.Fatalf("a failed prune left files beside the mock file: %v", names)
			}
		})
	}
}

// TestUpdateMocks_AnOriginalLeftAsideIsNotAFailedPrune: a prune whose
// replace put the pruned file in place has pruned it, even when the original
// the replace moved aside cannot then be removed (on Windows, a file another
// process holds open without sharing delete). yaml.ReplaceFile warns about
// that copy; the prune reports success, as the file is pruned.
func TestUpdateMocks_AnOriginalLeftAsideIsNotAFailedPrune(t *testing.T) {
	consumed, pruneBefore, startupCutoff := pruneMixedArgs()
	dir := t.TempDir()
	path := writePruneFixture(t, dir, "yaml", pruneMixedFixture(), "")
	want := readAllPrune(t, path, consumed, pruneBefore, startupCutoff)

	// The platform refuses to rename the rewrite over the existing file, and
	// the original, once moved aside, lands in a directory of the aside
	// copy's name, which os.Remove cannot delete.
	rename := func(from, to string) error {
		if _, err := os.Stat(to); err == nil && strings.HasSuffix(from, ".tmp") {
			return &os.LinkError{Op: "rename", Old: from, New: to, Err: errors.New("access denied")}
		}
		if strings.Contains(filepath.Base(to), ".replaced.") {
			if err := os.Mkdir(to, 0o755); err != nil {
				return err
			}
			return os.Rename(from, filepath.Join(to, filepath.Base(from)))
		}
		return os.Rename(from, to)
	}
	defer func(r func(*zap.Logger, string, string) error) { replaceFile = r }(replaceFile)
	replaceFile = func(logger *zap.Logger, src, dst string) error {
		return yaml.ReplaceFileWith(logger, rename, src, dst)
	}

	ys := New(zap.NewNop(), dir, "mocks")
	if err := ys.UpdateMocks(context.Background(), pruneSet, consumed, pruneBefore, startupCutoff); err != nil {
		t.Fatalf("UpdateMocks returned %v though the pruned file is in place", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !sameMockFile(t, path, got, want) {
		t.Fatal("the mock file does not hold the pruned mocks")
	}
}

const pruneKillChildEnv = "MOCKDB_PRUNE_KILL_CHILD_DIR"

// stallCtx freezes the prune at the n-th Done call — the reader calls it once
// per line, so mid-file — and says so on stdout, so the parent can SIGKILL a
// process that is part way through rewriting the file.
type stallCtx struct {
	context.Context
	n atomic.Int64
}

func (c *stallCtx) Done() <-chan struct{} {
	if c.n.Add(-1) == 0 {
		fmt.Println("stalled")
		_ = os.Stdout.Sync()
		time.Sleep(time.Hour)
	}
	return c.Context.Done()
}

// TestUpdateMocks_KilledMidPruneLeavesTheMocksFileWhole kills a process
// (SIGKILL, as the OOM killer does) part way through a prune and checks that
// the file it was pruning is intact: the prune never writes the file it
// reads, and only a completed rewrite replaces it.
func TestUpdateMocks_KilledMidPruneLeavesTheMocksFileWhole(t *testing.T) {
	if dir := os.Getenv(pruneKillChildEnv); dir != "" {
		consumed, pruneBefore, startupCutoff := pruneMixedArgs()
		ctx := &stallCtx{Context: context.Background()}
		ctx.n.Store(1200)
		ys := New(zap.NewNop(), dir, "mocks")
		err := ys.UpdateMocks(ctx, pruneSet, consumed, pruneBefore, startupCutoff)
		fmt.Printf("prune returned before the kill: %v\n", err)
		os.Exit(3)
	}
	if runtime.GOOS == "windows" {
		t.Skip("SIGKILL")
	}

	dir := t.TempDir()
	path := writePruneFixture(t, dir, "yaml", pruneMixedFixture(), "")
	orig, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestUpdateMocks_KilledMidPruneLeavesTheMocksFileWhole$", "-test.count=1")
	cmd.Env = append(os.Environ(), pruneKillChildEnv+"="+dir)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// However the test ends, the child (which may be stalled for an hour) dies.
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	line := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if sc.Text() == "stalled" || strings.HasPrefix(sc.Text(), "prune returned") {
				line <- sc.Text()
				break
			}
		}
		close(line)
		_, _ = io.Copy(io.Discard, out)
	}()
	select {
	case l := <-line:
		if l != "stalled" {
			t.Fatalf("the child did not stall mid-prune: %q", l)
		}
	case <-time.After(2 * time.Minute):
		t.Fatal("the child never reached the stall point")
	}
	// The rewrite is under way: its temp file exists beside the original.
	names := dirEntries(t, filepath.Dir(path))
	if len(names) != 2 {
		t.Fatalf("expected the mock file and the prune's temp file at the stall, got %v", names)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the mock file is gone after the kill: %v", err)
	}
	if !bytes.Equal(got, orig) {
		t.Fatal("a prune killed part way changed the mock file")
	}
	// And the next prune of that set works and ignores the dead one's temp file.
	consumed, pruneBefore, startupCutoff := pruneMixedArgs()
	want := readAllPrune(t, path, consumed, pruneBefore, startupCutoff)
	if err := New(zap.NewNop(), dir, "mocks").UpdateMocks(context.Background(), pruneSet, consumed, pruneBefore, startupCutoff); err != nil {
		t.Fatalf("UpdateMocks after the kill: %v", err)
	}
	if got, _ := os.ReadFile(path); !sameMockFile(t, path, got, want) {
		t.Fatal("the prune after the kill did not produce the read-all prune's file")
	}
}
