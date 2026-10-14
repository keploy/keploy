package mockdb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

// inPlaceCorpus is the production-shape recording's mocks (the
// mysql-capture-starved lane: DNS, the MySQL handshake, a 20-row text result,
// an OK, a COM_QUIT with no response), and a mock of every other kind the
// in-place path writes, with the strings a YAML emitter quotes, folds or
// tags: numbers, booleans and nulls as text, multi-line and tabbed bodies,
// non-ASCII, and a leading space.
func inPlaceCorpus(t *testing.T) []*models.Mock {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "mysql_capture_starved_mocks.yaml"))
	if err != nil {
		t.Fatal(err)
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
			t.Fatal(err)
		}
		docs = append(docs, &d)
	}
	mocks, err := DecodeMocks(docs, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	if len(mocks) != 6 {
		t.Fatalf("decoded %d mocks from the corpus, want 6", len(mocks))
	}
	at := time.Date(2026, 9, 30, 8, 42, 54, 313873046, time.UTC)
	tricky := "line one\nline two\twith a tab\n  indented: yes\n"
	headers := map[string]string{"X-Num": "123", "X-Bool": "true", "X-Null": "null", "X-Space": " lead", "X-Utf8": "héllo – ✓"}
	mocks = append(mocks,
		&models.Mock{Version: models.GetVersion(), Kind: models.HTTP, Name: "mock-http", ConnectionID: "conn-7",
			Noise: []string{"^tok-.*$"},
			Spec: models.MockSpec{
				Metadata:     map[string]string{"type": "config", "operation": "POST"},
				ReqBodyNoise: map[string][]string{"body.tier_type": {".*"}},
				HTTPReq:      &models.HTTPReq{Method: "POST", ProtoMajor: 1, ProtoMinor: 1, URL: "http://orders.shop/x?y=1", Header: headers, Body: tricky, Timestamp: at},
				HTTPResp:     &models.HTTPResp{StatusCode: 201, Header: headers, Body: `{"ok":true,"n":1e3}`, Timestamp: at.Add(time.Millisecond)},
				Created:      1790757774, ReqTimestampMock: at, ResTimestampMock: at.Add(time.Millisecond),
				Async: &models.AsyncMeta{},
			}},
		&models.Mock{Version: models.GetVersion(), Kind: models.GENERIC, Name: "mock-generic",
			Spec: models.MockSpec{
				Metadata:         map[string]string{"type": "config"},
				GenericRequests:  []models.Payload{{Origin: models.FromClient, Message: []models.OutputBinary{{Type: "binary", Data: "AAEC/w=="}, {Type: "utf-8", Data: tricky}}}},
				GenericResponses: []models.Payload{{Origin: models.FromServer, Message: []models.OutputBinary{{Type: "utf-8", Data: "0123"}}}},
				ReqTimestampMock: at, ResTimestampMock: at.Add(time.Second),
			}},
		&models.Mock{Version: models.GetVersion(), Kind: models.HTTP2, Name: "mock-http2",
			Spec: models.MockSpec{
				Metadata:         map[string]string{"type": "config"},
				HTTP2Req:         &models.HTTP2Req{Method: "GET", URL: "/v1/a", Authority: "api:443", Scheme: "https", Headers: headers, Body: "", Timestamp: at},
				HTTP2Resp:        &models.HTTP2Resp{StatusCode: 200, Headers: headers, Body: tricky, Timestamp: at},
				ReqTimestampMock: at, ResTimestampMock: at,
			}},
		&models.Mock{Version: models.GetVersion(), Kind: models.DNS, Name: "mock-dns-nil"},
	)
	return mocks
}

// writeYAML writes mocks through InsertMock into dir, and returns the file.
func writeYAML(t *testing.T, dir string, mocks []*models.Mock) []byte {
	t.Helper()
	ys := New(zap.NewNop(), dir, "mocks")
	for _, m := range mocks {
		c := *m
		if err := ys.InsertMock(context.Background(), &c, "test-set-0"); err != nil {
			t.Fatalf("InsertMock(%s): %v", m.Kind, err)
		}
	}
	b, err := os.ReadFile(filepath.Join(dir, "test-set-0", "mocks.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The file InsertMock writes is byte for byte the one it wrote through
// EncodeMock's yaml.Node, for every kind the in-place path takes.
func TestEncodeMockInPlaceWritesWhatEncodeMockWrites(t *testing.T) {
	for _, m := range inPlaceCorpus(t) {
		m.Name = "mock-x"
		inPlace, ok := encodeMockInPlace(m)
		if !ok {
			t.Fatalf("kind %s is not written in place", m.Kind)
		}
		var got, want bytes.Buffer
		enc := yamlLib.NewEncoder(&got)
		if err := enc.Encode(inPlace); err != nil {
			t.Fatalf("%s: in place: %v", m.Kind, err)
		}
		_ = enc.Close()
		doc, err := EncodeMock(m, zap.NewNop())
		if err != nil {
			t.Fatalf("%s: EncodeMock: %v", m.Kind, err)
		}
		enc = yamlLib.NewEncoder(&want)
		if err := enc.Encode(&doc); err != nil {
			t.Fatalf("%s: node: %v", m.Kind, err)
		}
		_ = enc.Close()
		if !bytes.Equal(got.Bytes(), want.Bytes()) {
			t.Fatalf("%s: written in place:\n%s\nthrough EncodeMock:\n%s", m.Kind, got.String(), want.String())
		}
	}
}

// What InsertMock writes reads back as what was recorded.
func TestInsertMockInPlaceReadsBack(t *testing.T) {
	mocks := inPlaceCorpus(t)
	dir := t.TempDir()
	writeYAML(t, dir, mocks)
	ys := New(zap.NewNop(), dir, "mocks")
	got, err := ys.GetUnFilteredMocks(context.Background(), "test-set-0", time.Time{}, time.Time{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(mocks) {
		t.Fatalf("read back %d mocks, want %d", len(got), len(mocks))
	}
}

// A MySQL mock is written in one pass: the text of each wire message is not
// made three times and parsed twice. (Through EncodeMock's Nodes, the 20-row
// result took about five times the allocations of a single pass.)
func TestInsertMockWritesAMySQLMockInOnePass(t *testing.T) {
	var rows *models.Mock
	for _, m := range inPlaceCorpus(t) {
		if m.Kind == models.MySQL && m.Spec.Metadata["responseOperation"] == "TextResultSet" {
			rows = m
		}
	}
	if rows == nil {
		t.Fatal("no 20-row result in the corpus")
	}
	allocs := func(write func(io.Writer) error) float64 {
		return testing.AllocsPerRun(20, func() {
			if err := write(io.Discard); err != nil {
				t.Fatal(err)
			}
		})
	}
	inPlace := allocs(func(w io.Writer) error {
		v, ok := encodeMockInPlace(rows)
		if !ok {
			return errors.New("not written in place")
		}
		return yamlLib.NewEncoder(w).Encode(v)
	})
	viaNode := allocs(func(w io.Writer) error {
		doc, err := EncodeMock(rows, zap.NewNop())
		if err != nil {
			return err
		}
		return yamlLib.NewEncoder(w).Encode(&doc)
	})
	insert := testing.AllocsPerRun(20, func() {
		c := *rows
		ys := New(zap.NewNop(), t.TempDir(), "mocks")
		if err := ys.InsertMock(context.Background(), &c, "test-set-0"); err != nil {
			t.Fatal(err)
		}
	})
	t.Logf("allocations per 20-row MySQL mock: in place %.0f, through EncodeMock %.0f, InsertMock %.0f", inPlace, viaNode, insert)
	if inPlace*3 > viaNode {
		t.Fatalf("in place %.0f allocations, through EncodeMock %.0f: want at most a third", inPlace, viaNode)
	}
	if insert > inPlace*1.5+200 {
		t.Fatalf("InsertMock made %.0f allocations for a mock that takes %.0f in place: it does not write it in one pass", insert, inPlace)
	}
}

// A mock that cannot be marshaled partway through (after the emitter has
// streamed more than a buffer of it) is a payload fault that writes nothing:
// the recording goes on, and the file holds whole documents only.
func TestInsertMockInPlaceWritesNothingOfAMockItCannotMarshal(t *testing.T) {
	var good *models.Mock
	for _, m := range inPlaceCorpus(t) {
		if m.Kind == models.MySQL {
			good = m
			break
		}
	}
	bad := *good
	bad.Spec.MySQLRequests = []mysql.Request{{PacketBundle: mysql.PacketBundle{
		Header: good.Spec.MySQLRequests[0].Header, Meta: map[string]string{"big": strings.Repeat("x", 300<<10)},
		Message: good.Spec.MySQLRequests[0].Message}}}
	dir := t.TempDir()
	ys := New(zap.NewNop(), dir, "mocks")
	g1 := *good
	if err := ys.InsertMock(context.Background(), &g1, "test-set-0"); err != nil {
		t.Fatal(err)
	}
	// A message whose MarshalYAML fails, and one yaml.v3 panics on.
	for _, msg := range []any{failingYAML{}, make(chan int)} {
		b := bad
		b.Spec.MySQLResponses = []mysql.Response{{PacketBundle: mysql.PacketBundle{Message: msg}}}
		if err := ys.InsertMock(context.Background(), &b, "test-set-0"); !errors.Is(err, models.ErrMockEncode) {
			t.Fatalf("InsertMock of a mock YAML cannot marshal (%T) = %v, want a skippable ErrMockEncode", msg, err)
		}
	}
	g2 := *good
	if err := ys.InsertMock(context.Background(), &g2, "test-set-0"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "test-set-0", "mocks.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte("xxxxxxxx")) {
		t.Fatal("part of the mock that could not be marshaled is in the file")
	}
	got, err := ys.GetUnFilteredMocks(context.Background(), "test-set-0", time.Time{}, time.Time{}, nil, nil)
	if err != nil || len(got) != 2 {
		t.Fatalf("read back %d mocks (%v), want the 2 written whole", len(got), err)
	}
}

// failingYAML is a value whose YAML marshaling fails.
type failingYAML struct{}

func (failingYAML) MarshalYAML() (any, error) { return nil, errors.New("cannot be marshaled") }

// blockScalarTraps are strings yaml.v3 (v3.0.1), left to choose, writes as a
// literal block scalar that does not read back: a first tab has no
// indentation indicator, and the indicator it writes for a first space or line
// break is wrong in a nested block. Tab-indented SQL and JSON, and text that
// opens with a line break, are ordinary payloads.
var blockScalarTraps = []string{
	"\tleading tab\nx",
	"\n\thello",
	"\t{\n\t\"a\": 1\n}",
	"\n  indented\n\tmixed",
	" leading space\nx",
	"\u2028line separator\nx",
}

// trapMocks is one mock per place a recorded string can sit, each holding s:
// a MySQL query and row value, an HTTP request and response body and header,
// a Generic payload, an HTTP/2 body and a DNS answer.
func trapMocks(t *testing.T, s string, tag string) map[string]*models.Mock {
	t.Helper()
	var query, rows, httpM, genM, h2M *models.Mock
	for _, m := range inPlaceCorpus(t) {
		switch {
		case m.Kind == models.MySQL && m.Spec.Metadata["responseOperation"] == "TextResultSet":
			rows = m
		case m.Kind == models.MySQL:
			for _, r := range m.Spec.MySQLRequests {
				if _, ok := r.Message.(*mysql.QueryPacket); ok {
					query = m
				}
			}
		case m.Kind == models.HTTP:
			httpM = m
		case m.Kind == models.GENERIC:
			genM = m
		case m.Kind == models.HTTP2:
			h2M = m
		}
	}
	if query == nil || rows == nil || httpM == nil || genM == nil || h2M == nil {
		t.Fatal("the corpus lacks a mock kind the traps need")
	}
	out := map[string]*models.Mock{}
	add := func(name string, m *models.Mock) {
		m.Name = "trap-" + tag + "-" + name
		out[name] = m
	}
	setQuery := func(m *models.Mock) {
		m.Spec.MySQLRequests = slices.Clone(m.Spec.MySQLRequests)
		for i, r := range m.Spec.MySQLRequests {
			if q, ok := r.Message.(*mysql.QueryPacket); ok {
				c := *q
				c.Query = s
				m.Spec.MySQLRequests[i].Message = &c
			}
		}
	}
	{
		m := *query
		setQuery(&m)
		add("mysql-query", &m)
	}
	{
		m := *rows
		setQuery(&m)
		m.Spec.MySQLResponses = slices.Clone(rows.Spec.MySQLResponses)
		rs := *m.Spec.MySQLResponses[0].Message.(*mysql.TextResultSet)
		rs.Rows = slices.Clone(rs.Rows)
		row := *rs.Rows[0]
		row.Values = slices.Clone(row.Values)
		row.Values[0].Value = s
		rs.Rows[0] = &row
		m.Spec.MySQLResponses[0].Message = &rs
		add("mysql-row", &m)
	}
	{
		m := *httpM
		req, resp := *httpM.Spec.HTTPReq, *httpM.Spec.HTTPResp
		req.Body = s
		req.Header = map[string]string{"X-Trap": s}
		resp.Body = s
		m.Spec.HTTPReq, m.Spec.HTTPResp = &req, &resp
		add("http", &m)
	}
	{
		m := *genM
		m.Spec.GenericRequests = []models.Payload{{Origin: models.FromClient, Message: []models.OutputBinary{{Type: "utf-8", Data: s}}}}
		add("generic", &m)
	}
	{
		m := *h2M
		req, resp := *h2M.Spec.HTTP2Req, *h2M.Spec.HTTP2Resp
		req.Body, resp.Body = s, s
		m.Spec.HTTP2Req, m.Spec.HTTP2Resp = &req, &resp
		add("http2", &m)
	}
	{
		m := &models.Mock{Version: models.GetVersion(), Kind: models.DNS, Spec: models.MockSpec{
			Metadata: map[string]string{"type": "config"},
			DNSReq:   &models.DNSReq{Name: "tidb.", Qtype: 1, Qclass: 1},
			DNSResp:  &models.DNSResp{Answers: []string{s}},
		}}
		add("dns", m)
	}
	return out
}

// trapValues are the strings a trap mock carries, where trapMocks put them.
func trapValues(m *models.Mock) []string {
	var got []string
	switch m.Kind {
	case models.MySQL:
		for _, r := range m.Spec.MySQLRequests {
			if q, ok := r.Message.(*mysql.QueryPacket); ok {
				got = append(got, q.Query)
			}
		}
		for _, r := range m.Spec.MySQLResponses {
			if rs, ok := r.Message.(*mysql.TextResultSet); ok && len(rs.Rows) > 0 {
				v, _ := rs.Rows[0].Values[0].Value.(string)
				got = append(got, v)
			}
		}
	case models.HTTP:
		got = append(got, m.Spec.HTTPReq.Body, m.Spec.HTTPReq.Header["X-Trap"], m.Spec.HTTPResp.Body)
	case models.GENERIC:
		for _, p := range m.Spec.GenericRequests {
			for _, b := range p.Message {
				got = append(got, b.Data)
			}
		}
	case models.HTTP2:
		got = append(got, m.Spec.HTTP2Req.Body, m.Spec.HTTP2Resp.Body)
	case models.DNS:
		got = append(got, m.Spec.DNSResp.Answers...)
	}
	return got
}

// A mock that holds a string a block scalar cannot carry is written so that it
// reads back exactly, and so do the mocks around it in its file. Written
// unchecked as a literal block, it made the whole file unreadable ("found a
// tab character where an indentation space is expected"), or read back as
// another string; through EncodeMock's yaml.Node it was dropped.
func TestInsertMockWritesStringsABlockScalarCannotCarryReadably(t *testing.T) {
	var good *models.Mock // the lane's handshake: it carries no trap value
	for _, m := range inPlaceCorpus(t) {
		if m.Kind == models.MySQL && m.Spec.Metadata["requestOperation"] == "HandshakeV10" {
			good = m
		}
	}
	if good == nil {
		t.Fatal("no handshake in the corpus")
	}
	for i, s := range blockScalarTraps {
		t.Run(fmt.Sprintf("%q", s), func(t *testing.T) {
			dir := t.TempDir()
			ys := New(zap.NewNop(), dir, "mocks")
			insert := func(m *models.Mock) {
				t.Helper()
				c := *m
				if err := ys.InsertMock(context.Background(), &c, "test-set-0"); err != nil {
					t.Fatalf("InsertMock(%s): %v", m.Name, err)
				}
			}
			g1, g2 := *good, *good
			g1.Name, g2.Name = "good-before", "good-after"
			insert(&g1)
			traps := trapMocks(t, s, fmt.Sprint(i))
			for _, m := range traps {
				insert(m)
			}
			insert(&g2)
			// InsertMock names each mock as it writes it; the traps are
			// the mocks that carry trap values (the good ones, a handshake,
			// carry none).
			all := map[string]*models.Mock{}
			for _, read := range []func(context.Context, string, time.Time, time.Time, map[string]bool, map[string]bool) ([]*models.Mock, error){ys.GetFilteredMocks, ys.GetUnFilteredMocks} {
				got, err := read(context.Background(), "test-set-0", time.Time{}, time.Time{}, nil, nil)
				if err != nil {
					t.Fatalf("the file does not read back: %v", err)
				}
				for _, m := range got {
					all[m.Name] = m
				}
			}
			if len(all) != len(traps)+2 {
				t.Fatalf("read back %d mocks, want %d", len(all), len(traps)+2)
			}
			carried := 0
			for _, m := range all {
				vals := trapValues(m)
				if len(vals) == 0 {
					continue
				}
				carried++
				for _, v := range vals {
					if v != s {
						t.Errorf("a %s mock read back %q, want %q", m.Kind, v, s)
					}
				}
			}
			if carried != len(traps) {
				t.Fatalf("%d mocks carry the string, want %d", carried, len(traps))
			}
		})
	}
}

// EncodeMock (the kinds a mapper owns, a mock file rewritten whole, and every
// caller outside InsertMock) carries the same strings: its yaml.Node parsed a
// block back that did not load, and the mock was dropped.
func TestEncodeMockCarriesStringsABlockScalarCannot(t *testing.T) {
	for i, s := range blockScalarTraps {
		for what, m := range trapMocks(t, s, fmt.Sprint(i)) {
			doc, err := EncodeMock(m, zap.NewNop())
			if err != nil {
				t.Fatalf("%q, %s: EncodeMock: %v", s, what, err)
			}
			out, err := yamlLib.Marshal(doc)
			if err != nil {
				t.Fatal(err)
			}
			var read yaml.NetworkTrafficDoc
			if err := yamlLib.Unmarshal(out, &read); err != nil {
				t.Fatalf("%q, %s: does not load: %v", s, what, err)
			}
			back, err := DecodeMocks([]*yaml.NetworkTrafficDoc{&read}, zap.NewNop())
			if err != nil || len(back) != 1 {
				t.Fatalf("%q, %s: decode: %v", s, what, err)
			}
			for _, v := range trapValues(back[0]) {
				if v != s {
					t.Fatalf("%q, %s: read back %q", s, what, v)
				}
			}
		}
	}
}

// The quoted write is the lane's document: for its mocks, which hold no
// string a block scalar cannot carry, it writes what the one-pass write does,
// but for the handshake's ",flow" byte lists, which it writes in block style
// (the same mock).
func TestEncodeQuotedWritesTheLanesMocksAsTheOnePassWriteDoes(t *testing.T) {
	decode := func(b []byte) *models.Mock {
		t.Helper()
		var d yaml.NetworkTrafficDoc
		if err := yamlLib.Unmarshal(b, &d); err != nil {
			t.Fatalf("does not load: %v\n%s", err, b)
		}
		ms, err := DecodeMocks([]*yaml.NetworkTrafficDoc{&d}, zap.NewNop())
		if err != nil || len(ms) != 1 {
			t.Fatalf("decode: %v", err)
		}
		return ms[0]
	}
	for _, m := range inPlaceCorpus(t) {
		v, _ := encodeMockInPlace(m)
		if yaml.NeedsQuoting(v) {
			t.Fatalf("%s %s: sent through EncodeQuoted, and it holds no string a block scalar cannot carry", m.Kind, m.Name)
		}
		var direct, quoted bytes.Buffer
		if err := encodeYAMLDoc(&direct, v); err != nil {
			t.Fatal(err)
		}
		n, err := yaml.EncodeQuoted(v)
		if err != nil {
			t.Fatal(err)
		}
		if err := encodeYAMLDoc(&quoted, n); err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(direct.Bytes(), quoted.Bytes()) {
			continue
		}
		if m.Spec.Metadata["requestOperation"] != "HandshakeV10" {
			t.Fatalf("%s %s: quoted:\n%s\none pass:\n%s", m.Kind, m.Name, quoted.String(), direct.String())
		}
		if a, b := decode(direct.Bytes()), decode(quoted.Bytes()); !reflect.DeepEqual(a, b) {
			t.Fatalf("the handshake reads back differently written quoted:\n%s\none pass:\n%s", quoted.String(), direct.String())
		}
	}
}

// A header that arrived on more than one wire line keeps its line lengths
// (models.HeaderLineLengths) through the one-pass write, written plain and
// written through EncodeQuoted: replay sends a mock's recorded lines again
// from them, and a mock that reads back without them is sent with each such
// header folded into one line.
func TestInsertMockInPlaceKeepsRecordedHeaderLines(t *testing.T) {
	at := time.Date(2026, 9, 30, 8, 42, 54, 313873046, time.UTC)
	reqLines := models.HeaderLineLengths{"X-Forwarded-For": {8, 8}}
	respLines := models.HeaderLineLengths{"Set-Cookie": {5, 5}}
	for _, reqBody := range []string{`{"id":7}`, "\tleading tab\nx"} {
		quoted := models.YAMLBlockScalarUnsafe(reqBody)
		t.Run(fmt.Sprintf("quoted=%v", quoted), func(t *testing.T) {
			m := &models.Mock{Version: models.GetVersion(), Kind: models.HTTP,
				Spec: models.MockSpec{
					Metadata: map[string]string{"type": "config"},
					HTTPReq: &models.HTTPReq{Method: "POST", ProtoMajor: 1, ProtoMinor: 1, URL: "http://orders.shop/x",
						Header: map[string]string{"X-Forwarded-For": "10.0.0.1,10.0.0.2"}, HeaderLineLengths: reqLines,
						Body: reqBody, Timestamp: at},
					HTTPResp: &models.HTTPResp{StatusCode: 200,
						Header: map[string]string{"Set-Cookie": "a=1;x,b=2;y"}, HeaderLineLengths: respLines,
						Body: `{"ok":true}`, Timestamp: at},
					ReqTimestampMock: at, ResTimestampMock: at,
				}}
			v, inPlace := encodeMockInPlace(m)
			if !inPlace {
				t.Fatal("an HTTP mock is not written in place")
			}
			if got := yaml.NeedsQuoting(v); got != quoted {
				t.Fatalf("NeedsQuoting = %v, want %v: the case does not take the write path it names", got, quoted)
			}
			dir := t.TempDir()
			file := writeYAML(t, dir, []*models.Mock{m})
			got, err := New(zap.NewNop(), dir, "mocks").GetUnFilteredMocks(context.Background(), "test-set-0", time.Time{}, time.Time{}, nil, nil)
			if err != nil {
				t.Fatalf("the file does not read back: %v\n%s", err, file)
			}
			if len(got) != 1 {
				t.Fatalf("read back %d mocks, want 1:\n%s", len(got), file)
			}
			if !reflect.DeepEqual(got[0].Spec.HTTPReq.HeaderLineLengths, reqLines) {
				t.Errorf("request header lines read back as %v, want %v:\n%s", got[0].Spec.HTTPReq.HeaderLineLengths, reqLines, file)
			}
			if !reflect.DeepEqual(got[0].Spec.HTTPResp.HeaderLineLengths, respLines) {
				t.Errorf("response header lines read back as %v, want %v:\n%s", got[0].Spec.HTTPResp.HeaderLineLengths, respLines, file)
			}
			if got[0].Spec.HTTPReq.Body != reqBody {
				t.Errorf("request body read back as %q, want %q", got[0].Spec.HTTPReq.Body, reqBody)
			}

			// EncodeMock, which rewrites the file (the prune) with the same
			// spec through a yaml.Node, keeps them too.
			doc, err := EncodeMock(m, zap.NewNop())
			if err != nil {
				t.Fatalf("EncodeMock: %v", err)
			}
			out, err := yamlLib.Marshal(doc)
			if err != nil {
				t.Fatalf("marshal EncodeMock's document: %v", err)
			}
			var back yaml.NetworkTrafficDoc
			if err := yamlLib.Unmarshal(out, &back); err != nil {
				t.Fatalf("EncodeMock's document does not load: %v\n%s", err, out)
			}
			rewritten, err := DecodeMocks([]*yaml.NetworkTrafficDoc{&back}, zap.NewNop())
			if err != nil || len(rewritten) != 1 {
				t.Fatalf("EncodeMock's document does not decode (%d mocks): %v\n%s", len(rewritten), err, out)
			}
			if !reflect.DeepEqual(rewritten[0].Spec.HTTPReq.HeaderLineLengths, reqLines) ||
				!reflect.DeepEqual(rewritten[0].Spec.HTTPResp.HeaderLineLengths, respLines) {
				t.Errorf("EncodeMock's document reads back with request lines %v and response lines %v, want %v and %v:\n%s",
					rewritten[0].Spec.HTTPReq.HeaderLineLengths, rewritten[0].Spec.HTTPResp.HeaderLineLengths, reqLines, respLines, out)
			}
		})
	}
}
