package mockdb

import (
	"bytes"
	"context"
	"encoding/gob"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	yamlLib "gopkg.in/yaml.v3"
)

// A fault on an HTTP mock is a `fault:` block of its spec, written legibly
// ("delay: 1.5s"), and read back as written through the YAML document, the
// JSON document and the JSON mock form. A mock without one writes no such
// key, so a recording without faults reads and writes as before.
func TestAFaultRoundTrips(t *testing.T) {
	for _, want := range []models.Fault{
		{Kind: models.FaultTruncate, BodyBytes: 10, Observed: "unexpected EOF"},
		{Kind: models.FaultReset, AfterHead: true, BodyBytes: 3},
		{Kind: models.FaultDelay, Delay: 1500 * time.Millisecond},
		{Kind: models.FaultStoppedReading, AfterHead: true, BodyBytes: 5, Observed: "the application stopped reading partway through the answer and closed the connection"},
		{Kind: models.FaultStoppedReading, AfterHead: true, BodyBytes: 5, ContentLength: 20},
		{Kind: models.FaultStoppedReading, AfterHead: true, BodyBytes: 5, Chunked: true},
	} {
		m := variablesMock()
		f := want
		m.Spec.Fault = &f
		doc, err := EncodeMock(m, zap.NewNop())
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		var buf bytes.Buffer
		enc := yamlLib.NewEncoder(&buf)
		if err := enc.Encode(doc); err != nil {
			t.Fatal(err)
		}
		_ = enc.Close()
		text := buf.String()
		if !strings.Contains(text, "    fault:\n        kind: "+string(want.Kind)+"\n") {
			t.Fatalf("the fault is not a legible block of the spec:\n%s", text)
		}
		// Under a kind a keploy that knows no faults skips (models.HTTPFault),
		// never kind Http, whose head it would serve as a whole answer.
		if !strings.Contains(text, "kind: HttpFault\n") {
			t.Fatalf("a fault mock is not written as kind HttpFault:\n%s", text)
		}
		if want.Delay != 0 && !strings.Contains(text, "delay: 1.5s") {
			t.Fatalf("the delay is not written as a duration a person reads:\n%s", text)
		}
		var back yaml.NetworkTrafficDoc
		if err := yamlLib.Unmarshal(buf.Bytes(), &back); err != nil {
			t.Fatal(err)
		}
		out, err := DecodeMocks([]*yaml.NetworkTrafficDoc{&back}, zap.NewNop())
		if err != nil || len(out) != 1 || out[0].Spec.Fault == nil || *out[0].Spec.Fault != want || out[0].Kind != models.HTTP {
			t.Fatalf("yaml round-trip: err=%v %+v, want an HTTP mock with %+v", err, out, want)
		}
		if ip, ok := encodeMockInPlace(m); !ok || ip.(*inPlaceDoc).Kind != models.HTTPFault {
			t.Fatalf("the in-place encoder writes a fault mock as %+v, want kind HttpFault", ip)
		}

		jsonBytes, err := yaml.MarshalDoc(yaml.FormatJSON, doc)
		if err != nil {
			t.Fatalf("MarshalDoc(json): %v", err)
		}
		jdoc, err := yaml.UnmarshalDoc(yaml.FormatJSON, jsonBytes)
		if err != nil {
			t.Fatalf("UnmarshalDoc(json): %v", err)
		}
		jout, err := DecodeMocks([]*yaml.NetworkTrafficDoc{jdoc}, zap.NewNop())
		if err != nil || len(jout) != 1 || jout[0].Spec.Fault == nil || *jout[0].Spec.Fault != want || jout[0].Kind != models.HTTP || !bytes.Contains(jsonBytes, []byte(`"kind":"HttpFault"`)) {
			t.Fatalf("json document round-trip: err=%v %+v, want %+v", err, jout, want)
		}

		jm, ok, err := EncodeMockJSON(m, zap.NewNop())
		if err != nil || !ok {
			t.Fatalf("EncodeMockJSON: ok=%v err=%v", ok, err)
		}
		jmout, err := DecodeMocksJSON([]*yaml.NetworkTrafficDocJSON{jm}, zap.NewNop())
		if err != nil || len(jmout) != 1 || jmout[0].Spec.Fault == nil || *jmout[0].Spec.Fault != want || jm.Kind != models.HTTPFault || jmout[0].Kind != models.HTTP {
			t.Fatalf("json mock round-trip: err=%v %+v, want %+v", err, jmout, want)
		}
	}

	m := variablesMock()
	doc, err := EncodeMock(m, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	enc := yamlLib.NewEncoder(&buf)
	if err := enc.Encode(doc); err != nil {
		t.Fatal(err)
	}
	_ = enc.Close()
	if strings.Contains(buf.String(), "fault") || !strings.Contains(buf.String(), "kind: Http\n") {
		t.Fatalf("a mock without a fault wrote the key, or another kind than Http:\n%s", buf.String())
	}
	jm, _, err := EncodeMockJSON(m, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(jm.Spec, []byte("fault")) || jm.Kind != models.HTTP {
		t.Fatalf("a mock without a fault wrote the key in its JSON form: %s", jm.Spec)
	}
}

// An event stream the app stopped reading partway is recorded as far as it
// read, which may end inside an event. Its body goes into the mock file flat,
// as an HttpFault mock, and comes back byte for byte: the chunk form an event
// stream is written in otherwise would give back whole events only, adding an
// end to the event the app read part of. The same body without the fault is
// written as the stream's chunks, as before.
func TestAStreamTheAppStoppedReadingRoundTripsByteForByte(t *testing.T) {
	const body = "data: {\"n\":1}\n\ndata: {\"n\":2}\n\ndata: {\"n\""
	stream := func(f *models.Fault) *models.Mock {
		m := variablesMock()
		m.Spec.Variables = nil
		m.Spec.HTTPResp = &models.HTTPResp{StatusCode: 200, ProtoMajor: 1, ProtoMinor: 1,
			Header: map[string]string{"Content-Type": "text/event-stream", "Content-Length": strconv.Itoa(len(body))}, Body: body}
		m.Spec.Fault = f
		return m
	}
	write := func(m *models.Mock) string {
		t.Helper()
		doc, err := EncodeMock(m, zap.NewNop())
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		var buf bytes.Buffer
		enc := yamlLib.NewEncoder(&buf)
		if err := enc.Encode(doc); err != nil {
			t.Fatal(err)
		}
		_ = enc.Close()
		return buf.String()
	}
	want := models.Fault{Kind: models.FaultStoppedReading, AfterHead: true, BodyBytes: int64(len(body)), Chunked: true,
		Observed: "the application stopped reading partway through the answer and closed the connection"}
	f := want
	text := write(stream(&f))
	if !strings.Contains(text, "kind: HttpFault\n") || !strings.Contains(text, "        kind: stopped_reading\n") || !strings.Contains(text, "        chunked: true\n") || strings.Contains(text, "- data:\n") {
		t.Fatalf("not written as an HttpFault mock with its stopped_reading fault:\n%s", text)
	}
	var back yaml.NetworkTrafficDoc
	if err := yamlLib.Unmarshal([]byte(text), &back); err != nil {
		t.Fatal(err)
	}
	out, err := DecodeMocks([]*yaml.NetworkTrafficDoc{&back}, zap.NewNop())
	if err != nil || len(out) != 1 || out[0].Kind != models.HTTP || out[0].Spec.Fault == nil || *out[0].Spec.Fault != want {
		t.Fatalf("read back err=%v %+v, want an HTTP mock with %+v", err, out, want)
	}
	if got := out[0].Spec.HTTPResp.Body; got != body {
		t.Fatalf("read back the body %q, want the %q the app read, byte for byte:\n%s", got, body, text)
	}
	if whole := write(stream(nil)); !strings.Contains(whole, "kind: Http\n") || !strings.Contains(whole, "- data:\n") {
		t.Fatalf("the stream with no fault is not written as before (kind Http, its chunks):\n%s", whole)
	}
	// A delayed answer is whole: its stream is written as its chunks, as one
	// with no fault is. Only a fault after the head cuts the body short.
	if delayed := write(stream(&models.Fault{Kind: models.FaultDelay, Delay: time.Second})); !strings.Contains(delayed, "kind: HttpFault\n") || !strings.Contains(delayed, "- data:\n") {
		t.Fatalf("a delayed stream is not written as its chunks:\n%s", delayed)
	}
}

// A mock whose fault this keploy cannot enact (a kind from a newer keploy, a
// spelling the format does not have, a fault on a call of another protocol)
// is skipped by the set's read, in YAML, NDJSON and gob alike, and named with
// why (MockSkipFault): never served as its head or as a whole answer. A fault
// this keploy enacts is served.
func TestAFaultThisKeployCannotEnactIsSkippedAndNamed(t *testing.T) {
	cases := map[yaml.Format]string{
		yaml.FormatYAML: httpDocYAML("fault-newer", "HttpFault", "a") + "    fault:\n        kind: explode\n" +
			"---\n" + httpDocYAML("fault-misspelt", "HttpFault", "b") + "    fault:\n        kind: eof\n        after_head: true\n" +
			"---\n" + httpDocYAML("fault-as-http", "Http", "d") + "    fault:\n        kind: eof\n" +
			"---\n" + httpDocYAML("fault-none", "HttpFault", "e") +
			"---\n" + httpDocYAML("fault-ok", "HttpFault", "c") + "    fault:\n        kind: eof\n",
		yaml.FormatJSON: strings.TrimSuffix(httpDocJSON("fault-newer", "HttpFault", "a"), "}}") + `,"fault":{"kind":"explode"}}}` + "\n" +
			strings.TrimSuffix(httpDocJSON("fault-misspelt", "HttpFault", "b"), "}}") + `,"fault":{"kind":"eof","after_head":true}}}` + "\n" +
			strings.TrimSuffix(httpDocJSON("fault-as-http", "Http", "d"), "}}") + `,"fault":{"kind":"eof"}}}` + "\n" +
			httpDocJSON("fault-none", "HttpFault", "e") + "\n" +
			strings.TrimSuffix(httpDocJSON("fault-ok", "HttpFault", "c"), "}}") + `,"fault":{"kind":"eof"}}}` + "\n",
	}
	for format, body := range cases {
		t.Run(string(format), func(t *testing.T) {
			t.Setenv("KEPLOY_MOCK_FORMAT", "")
			dir := t.TempDir()
			writeSetFile(t, dir, format, body)
			served, listed := readSetOracle(t, dir, format)
			if !served["fault-ok"] || served["fault-newer"] || served["fault-misspelt"] || served["fault-as-http"] || served["fault-none"] {
				t.Fatalf("the set's read served %v; want only the fault this keploy enacts", served)
			}
			// Each listed under the kind its document is written as.
			if listed["fault-newer"] != models.HTTPFault || listed["fault-misspelt"] != models.HTTPFault || listed["fault-as-http"] != models.HTTP || listed["fault-none"] != models.HTTPFault {
				t.Fatalf("the set's read lists as skipped %v", listed)
			}
			for _, d := range readDocs(t, dir, format) {
				want := models.MockSkip("")
				if d.Name != "fault-ok" {
					want = models.MockSkipFault
				}
				if d.Skip != want {
					t.Errorf("the reader says %s is %q, want %q", d.Name, d.Skip, want)
				}
			}
			_, skipped := NewWithFormat(zap.NewNop(), dir, "mocks", format).SkippedMocks(context.Background(), pruneSet)
			if skipped["fault-newer"].Why != models.MockSkipFault || skipped["fault-misspelt"].Why != models.MockSkipFault ||
				skipped["fault-as-http"].Why != models.MockSkipFault || skipped["fault-none"].Why != models.MockSkipFault || len(skipped) != 4 {
				t.Errorf("SkippedMocks: %v", skipped)
			}
		})
	}

	t.Run("gob", func(t *testing.T) {
		newer, other, ok, unnamed := pruneHTTPMock(0), pruneHTTPMock(1), pruneHTTPMock(2), pruneHTTPMock(3)
		newer.Name, other.Name, ok.Name, unnamed.Name = "fault-newer", "fault-other-protocol", "fault-ok", ""
		newer.Spec.Fault = &models.Fault{Kind: "explode"}
		unnamed.Spec.Fault = &models.Fault{Kind: "explode"} // skipped, and named nowhere: it has no name
		other.Kind = models.DNS
		other.Spec.Fault = &models.Fault{Kind: models.FaultEOF}
		ok.Spec.Fault = &models.Fault{Kind: models.FaultEOF}
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, pruneSet), 0o755); err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		buf.WriteString(gobMockMagic)
		enc := gob.NewEncoder(&buf)
		for _, m := range []*models.Mock{newer, other, ok, unnamed} {
			if err := enc.Encode(m); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, pruneSet, "mocks.gob"), buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		served, listed := readSetOracle(t, dir, yaml.FormatYAML)
		if !served["fault-ok"] || served["fault-newer"] || served["fault-other-protocol"] {
			t.Fatalf("the set's read served %v; want only the fault this keploy enacts", served)
		}
		if listed["fault-newer"] != models.HTTP || listed["fault-other-protocol"] != models.DNS {
			t.Fatalf("the set's read lists as skipped %v", listed)
		}
		_, skipped := NewWithFormat(zap.NewNop(), dir, "mocks", yaml.FormatYAML).SkippedMocks(context.Background(), pruneSet)
		if skipped["fault-newer"].Why != models.MockSkipFault || skipped["fault-other-protocol"].Why != models.MockSkipFault || len(skipped) != 2 {
			t.Errorf("SkippedMocks: %v", skipped)
		}
	})

	t.Run("json mock form", func(t *testing.T) {
		m := variablesMock()
		m.Spec.Fault = &models.Fault{Kind: "explode"}
		jm, ok, err := EncodeMockJSON(m, zap.NewNop())
		if err != nil || !ok {
			t.Fatalf("EncodeMockJSON: ok=%v err=%v", ok, err)
		}
		out, err := DecodeMocksJSON([]*yaml.NetworkTrafficDocJSON{jm}, zap.NewNop())
		if err != nil || len(out) != 0 {
			t.Fatalf("DecodeMocksJSON = %d mocks, %v; want the mock skipped", len(out), err)
		}
	})
}

// A mocks.gob holds a fault mock as kind HttpFault (gobKinded), which no
// parser of a keploy that knows no faults takes, and reads it back as an HTTP
// mock with its fault. A gob mock of kind HttpFault with no fault is skipped
// and named; a mock without a fault is written and read as it is.
func TestAGobFileHoldsAFaultMockAsHttpFault(t *testing.T) {
	m, plain, none := pruneHTTPMock(0), pruneHTTPMock(1), pruneHTTPMock(2)
	m.Name, plain.Name, none.Name = "fault-ok", "plain", "fault-none"
	m.Spec.Fault = &models.Fault{Kind: models.FaultEOF}
	none.Kind = models.HTTPFault
	if k := gobKinded(m).Kind; k != models.HTTPFault || m.Kind != models.HTTP {
		t.Fatalf("gobKinded wrote kind %s (and left the mock %s)", k, m.Kind)
	}
	if gobKinded(plain) != plain {
		t.Fatal("gobKinded copied a mock without a fault")
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, pruneSet), 0o755); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	buf.WriteString(gobMockMagic)
	enc := gob.NewEncoder(&buf)
	for _, mk := range []*models.Mock{m, plain, none} {
		if err := enc.Encode(gobKinded(mk)); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, pruneSet, "mocks.gob")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	raw := gob.NewDecoder(bytes.NewReader(buf.Bytes()[len(gobMockMagic):]))
	var first models.Mock
	if err := raw.Decode(&first); err != nil || first.Kind != models.HTTPFault {
		t.Fatalf("the file holds the fault mock as kind %q (%v), want HttpFault", first.Kind, err)
	}
	var kinds []models.Kind
	if _, err := forEachGobMockCut(path, func(r *models.Mock) error { kinds = append(kinds, r.Kind); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(kinds) != 3 || kinds[0] != models.HTTP || kinds[1] != models.HTTP || kinds[2] != models.HTTPFault {
		t.Fatalf("read back as kinds %v, want [Http Http HttpFault]", kinds)
	}
	served, _ := readSetOracle(t, dir, yaml.FormatYAML)
	if !served["fault-ok"] || !served["plain"] || served["fault-none"] {
		t.Fatalf("the set's read served %v; want fault-ok and plain only", served)
	}
	_, skipped := NewWithFormat(zap.NewNop(), dir, "mocks", yaml.FormatYAML).SkippedMocks(context.Background(), pruneSet)
	if skipped["fault-none"].Why != models.MockSkipFault || len(skipped) != 1 {
		t.Errorf("SkippedMocks: %v", skipped)
	}
}

// The gob writer a recording uses (InsertMock with KEPLOY_MOCK_FORMAT=gob)
// writes a fault mock as kind HttpFault, and the read gives it back as an
// HTTP mock with its fault.
func TestTheGobWriterWritesAFaultMockAsHttpFault(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "gob")
	dir := t.TempDir()
	ys := New(zap.NewNop(), dir, "mocks")
	m := pruneHTTPMock(0)
	m.Spec.Fault = &models.Fault{Kind: models.FaultTruncate, BodyBytes: 3}
	if err := ys.InsertMock(context.Background(), m, "set-0"); err != nil {
		t.Fatal(err)
	}
	if err := ys.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "set-0", "mocks.gob")
	b, err := os.ReadFile(path)
	if err != nil || !bytes.HasPrefix(b, []byte(gobMockMagic)) {
		t.Fatalf("read %s: %v", path, err)
	}
	var raw models.Mock
	if err := gob.NewDecoder(bytes.NewReader(b[len(gobMockMagic):])).Decode(&raw); err != nil || raw.Kind != models.HTTPFault {
		t.Fatalf("the file holds the fault mock as kind %q (%v), want HttpFault", raw.Kind, err)
	}
	got, err := readGobMocks(path)
	if err != nil || len(got) != 1 || got[0].Kind != models.HTTP || got[0].Spec.Fault == nil || *got[0].Spec.Fault != *m.Spec.Fault {
		t.Fatalf("read back %+v, %v; want the HTTP mock with its fault", got, err)
	}
}

// A fault is written on an HTTP call only: a mock of any other kind with one
// is refused, in every format and through either encoder, since its document
// has no place for the fault and would leave the recorded answer for replay
// to serve whole (in a mocks.gob, a keploy that knows no faults reads the
// fault as no field at all). An HTTP mock's fault is written as it is, for
// the reader to enact or refuse.
func TestAFaultIsWrittenOnlyOnAnHTTPCall(t *testing.T) {
	faulted := func(m *models.Mock, name string) *models.Mock {
		m.Name = name
		m.Spec.Fault = &models.Fault{Kind: models.FaultEOF}
		return m
	}
	for _, format := range []string{"yaml", "json", "gob"} {
		t.Run(format, func(t *testing.T) {
			t.Setenv("KEPLOY_MOCK_FORMAT", format)
			dir := t.TempDir()
			ys := New(zap.NewNop(), dir, "mocks")
			// Mongo goes through EncodeMock, MySQL is written in place.
			for _, m := range []*models.Mock{faulted(pruneMongoMock(0), "mongo-fault"), faulted(pruneMySQLMock(1, 1), "mysql-fault")} {
				err := ys.InsertMock(context.Background(), m, pruneSet)
				if !errors.Is(err, models.ErrMockEncode) || !errors.Is(err, models.ErrFaultUnsupported) {
					t.Errorf("InsertMock(%s) = %v; want it refused as a fault keploy enacts on HTTP calls only", m.Name, err)
				}
			}
			// InsertMock names each mock it is given, refused or not: the
			// two above are mock-0 and mock-1.
			if err := ys.InsertMock(context.Background(), pruneMongoMock(2), pruneSet); err != nil {
				t.Fatal(err)
			}
			if err := ys.Close(); err != nil {
				t.Fatal(err)
			}
			served, _ := readSetOracle(t, dir, yaml.FormatYAML)
			if !served["mock-2"] || len(served) != 1 {
				t.Fatalf("the set's read served %v; want only the mock without a fault", served)
			}
		})
	}

	t.Run("encoders", func(t *testing.T) {
		m := faulted(pruneMongoMock(0), "mongo-fault")
		if _, err := EncodeMock(m, zap.NewNop()); !errors.Is(err, models.ErrFaultUnsupported) {
			t.Errorf("EncodeMock = %v; want the fault refused", err)
		}
		if _, _, err := EncodeMockJSON(m, zap.NewNop()); !errors.Is(err, models.ErrFaultUnsupported) {
			t.Errorf("EncodeMockJSON = %v; want the fault refused", err)
		}
		h := variablesMock()
		h.Spec.Fault = &models.Fault{Kind: "explode"} // newer than this keploy: written, for the reader to refuse
		if _, err := EncodeMock(h, zap.NewNop()); err != nil {
			t.Errorf("EncodeMock(an HTTP mock with a fault) = %v; want it written as it is", err)
		}
	})
}

// A document of any kind but HTTP whose spec has a `fault:` key is skipped,
// never served whole, and named as a fault in every list of skipped mocks: no
// such kind's schema reads the key. A "fault" key deeper in the spec is the
// call's own data, and the mock is served.
func TestAFaultOnACallOfAnotherKindIsSkippedAndNamed(t *testing.T) {
	for _, format := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
		t.Run(string(format), func(t *testing.T) {
			t.Setenv("KEPLOY_MOCK_FORMAT", string(format))
			dir := t.TempDir()
			ys := NewWithFormat(zap.NewNop(), dir, "mocks", format)
			// InsertMock names them mock-0 (given a fault below), mock-1 (a
			// "fault" key in its metadata) and mock-2.
			for i := 0; i < 3; i++ {
				m := pruneMongoMock(i)
				if i == 1 {
					m.Spec.Metadata["fault"] = "eof"
				}
				if err := ys.InsertMock(context.Background(), m, pruneSet); err != nil {
					t.Fatal(err)
				}
			}
			if err := ys.Close(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, pruneSet, "mocks.yaml")
			from, to := "name: mock-0\nspec:\n", "name: mock-0\nspec:\n    fault:\n        kind: eof\n"
			if format == yaml.FormatJSON {
				path = filepath.Join(dir, pruneSet, "mocks.json")
				from, to = `"name":"mock-0","spec":{`, `"name":"mock-0","spec":{"fault":{"kind":"eof"},`
			}
			b, err := os.ReadFile(path)
			if err != nil || !bytes.Contains(b, []byte(from)) {
				t.Fatalf("read %s (%v): no %q in\n%s", path, err, from, b)
			}
			if err := os.WriteFile(path, bytes.Replace(b, []byte(from), []byte(to), 1), 0o644); err != nil {
				t.Fatal(err)
			}
			served, listed := readSetOracle(t, dir, format)
			if served["mock-0"] || !served["mock-1"] || !served["mock-2"] {
				t.Fatalf("the set's read served %v; want all but the mock with a fault", served)
			}
			if listed["mock-0"] != models.Mongo || len(listed) != 1 {
				t.Fatalf("the set's read lists as skipped %v", listed)
			}
			for _, d := range readDocs(t, dir, format) {
				want := models.MockSkip("")
				if d.Name == "mock-0" {
					want = models.MockSkipFault
				}
				if d.Skip != want {
					t.Errorf("the reader says %s is %q, want %q", d.Name, d.Skip, want)
				}
			}
			_, skipped := NewWithFormat(zap.NewNop(), dir, "mocks", format).SkippedMocks(context.Background(), pruneSet)
			if skipped["mock-0"].Why != models.MockSkipFault || len(skipped) != 1 {
				t.Errorf("SkippedMocks: %v", skipped)
			}
		})
	}
}

// A spec's fault is found the way its kind's decoder would find a field
// named fault, never by the spec's text: encoding/json takes the key in any
// case and with escapes, and yaml.v3 takes one a merge key (`<<:`) brings in
// and a spec given by an alias. A document of any kind but HTTP that holds
// one is skipped and named, where a search for the text "fault" served it
// whole; a fault key inside a value of the spec is no fault, and its mock is
// served. Of kind Http, such a document is skipped as a fault, and the
// reader says so.
func TestASpecsFaultIsFoundAsItsDecoderFindsIt(t *testing.T) {
	for _, c := range []struct {
		name   string
		format yaml.Format
		spec   string // what mock-0's spec starts with
		held   bool   // whether it holds a fault
	}{
		{"json Fault", yaml.FormatJSON, `"Fault":{"kind":"eof"},`, true},
		{"json FAULT", yaml.FormatJSON, `"FAULT":{"kind":"eof"},`, true},
		{"json escaped", yaml.FormatJSON, `"\u0066ault":{"kind":"eof"},`, true},
		{"json escaped capital", yaml.FormatJSON, `"\u0046AULT":{"kind":"eof"},`, true},
		{"json nested", yaml.FormatJSON, `"x":{"Fault":{"kind":"eof"},"fault":{"kind":"eof"}},`, false},
		{"yaml merge", yaml.FormatYAML, "    <<:\n        fault:\n            kind: eof\n", true},
		{"yaml nested", yaml.FormatYAML, "    x:\n        fault:\n            kind: eof\n", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("KEPLOY_MOCK_FORMAT", string(c.format))
			dir := t.TempDir()
			ys := NewWithFormat(zap.NewNop(), dir, "mocks", c.format)
			for i := 0; i < 2; i++ {
				if err := ys.InsertMock(context.Background(), pruneMongoMock(i), pruneSet); err != nil {
					t.Fatal(err)
				}
			}
			if err := ys.Close(); err != nil {
				t.Fatal(err)
			}
			path, from := filepath.Join(dir, pruneSet, "mocks.yaml"), "name: mock-0\nspec:\n"
			if c.format == yaml.FormatJSON {
				path, from = filepath.Join(dir, pruneSet, "mocks.json"), `"name":"mock-0","spec":{`
			}
			b, err := os.ReadFile(path)
			if err != nil || !bytes.Contains(b, []byte(from)) {
				t.Fatalf("read %s (%v): no %q in\n%s", path, err, from, b)
			}
			if err := os.WriteFile(path, bytes.Replace(b, []byte(from), []byte(from+c.spec), 1), 0o644); err != nil {
				t.Fatal(err)
			}
			served, listed := readSetOracle(t, dir, c.format)
			if served["mock-0"] == c.held || !served["mock-1"] {
				t.Fatalf("the set's read served %v; want mock-0 served: %v", served, !c.held)
			}
			wantListed, wantWhy := 0, models.MockSkip("")
			if c.held {
				wantListed, wantWhy = 1, models.MockSkipFault
			}
			if len(listed) != wantListed {
				t.Fatalf("the set's read lists as skipped %v", listed)
			}
			for _, d := range readDocs(t, dir, c.format) {
				want := models.MockSkip("")
				if d.Name == "mock-0" {
					want = wantWhy
				}
				if d.Skip != want {
					t.Errorf("the reader says %s is %q, want %q", d.Name, d.Skip, want)
				}
			}
			if _, skipped := NewWithFormat(zap.NewNop(), dir, "mocks", c.format).SkippedMocks(context.Background(), pruneSet); skipped["mock-0"].Why != wantWhy || len(skipped) != wantListed {
				t.Errorf("SkippedMocks: %v", skipped)
			}
		})
	}

	t.Run("yaml alias", func(t *testing.T) {
		var doc yaml.NetworkTrafficDoc
		if err := yamlLib.Unmarshal([]byte("version: api.keploy.io/v1beta1\nkind: Mongo\nname: m\nx: &s\n    fault:\n        kind: eof\nspec: *s\n"), &doc); err != nil {
			t.Fatal(err)
		}
		if out, err := DecodeMocks([]*yaml.NetworkTrafficDoc{&doc}, zap.NewNop()); err != nil || len(out) != 0 {
			t.Fatalf("DecodeMocks of a Mongo document whose spec is an alias of one with a fault = %d mocks, %v; want it skipped", len(out), err)
		}
	})

	for format, body := range map[yaml.Format]string{
		yaml.FormatJSON: strings.Replace(httpDocJSON("mock-0", "Http", "a"), `"spec":{`, `"spec":{"Fault":{"kind":"eof"},`, 1) + "\n" + httpDocJSON("mock-1", "Http", "b") + "\n",
		yaml.FormatYAML: httpDocYAML("mock-0", "Http", "a") + "    <<:\n        fault:\n            kind: eof\n---\n" + httpDocYAML("mock-1", "Http", "b"),
	} {
		t.Run("http "+string(format), func(t *testing.T) {
			dir := t.TempDir()
			writeSetFile(t, dir, format, body)
			if served, listed := readSetOracle(t, dir, format); served["mock-0"] || !served["mock-1"] || listed["mock-0"] != models.HTTP || len(listed) != 1 {
				t.Fatalf("the set's read served %v and lists as skipped %v; want mock-0 skipped", served, listed)
			}
			for _, d := range readDocs(t, dir, format) {
				want := models.MockSkip("")
				if d.Name == "mock-0" {
					want = models.MockSkipFault
				}
				if d.Skip != want {
					t.Errorf("the reader says %s is %q, want %q", d.Name, d.Skip, want)
				}
			}
		})
	}
}

// An HTTP document with a fault is judged with its call (faultKindSupported),
// so the read names the cause a user acts on: a fault under kind Http is
// written under the wrong kind, whatever the fault. A spec that is no
// mapping holds no fault: its kind's own decode refuses it.
func TestAnHTTPFaultIsJudgedWithItsKind(t *testing.T) {
	core, logs := observer.New(zap.ErrorLevel)
	var doc yaml.NetworkTrafficDoc
	if err := yamlLib.Unmarshal([]byte(httpDocYAML("fault-as-http", "Http", "a")+"    fault:\n        kind: explode\n"), &doc); err != nil {
		t.Fatal(err)
	}
	if out, err := DecodeMocks([]*yaml.NetworkTrafficDoc{&doc}, zap.New(core)); err != nil || len(out) != 0 {
		t.Fatalf("DecodeMocks = %d mocks, %v; want the mock skipped", len(out), err)
	}
	var jdoc yaml.NetworkTrafficDocJSON
	if err := json.Unmarshal([]byte(strings.TrimSuffix(httpDocJSON("fault-as-http", "Http", "a"), "}}")+`,"fault":{"kind":"explode"}}}`), &jdoc); err != nil {
		t.Fatal(err)
	}
	if out, err := DecodeMocksJSON([]*yaml.NetworkTrafficDocJSON{&jdoc}, zap.New(core)); err != nil || len(out) != 0 {
		t.Fatalf("DecodeMocksJSON = %d mocks, %v; want the mock skipped", len(out), err)
	}
	if got := logs.FilterMessage("skipping a mock whose kind does not go with its fault").Len(); got != 2 || logs.Len() != 2 {
		t.Errorf("logged %v; want the kind named as the cause, once per document", logs.All())
	}

	var seq yaml.NetworkTrafficDoc
	if err := yamlLib.Unmarshal([]byte("version: api.keploy.io/v1beta1\nkind: Mongo\nname: m\nspec: [fault, eof]\n"), &seq); err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeMocks([]*yaml.NetworkTrafficDoc{&seq}, zap.NewNop()); err == nil {
		t.Error("DecodeMocks of a Mongo document whose spec is a list: no error; want its kind's decode to refuse it")
	}
}

// rawGobKinds is the kind of each mock a mocks.gob holds, by name, as gob
// decodes it with no reader's mapping: what a keploy that knows no faults
// reads.
func rawGobKinds(t *testing.T, path string) map[string]models.Kind {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil || !bytes.HasPrefix(b, []byte(gobMockMagic)) {
		t.Fatalf("read %s: %v", path, err)
	}
	dec := gob.NewDecoder(bytes.NewReader(b[len(gobMockMagic):]))
	out := map[string]models.Kind{}
	for {
		var m models.Mock
		if err := dec.Decode(&m); err != nil {
			if errors.Is(err, io.EOF) {
				return out
			}
			t.Fatalf("decode %s: %v", path, err)
		}
		out[m.Name] = m.Kind
	}
}

// A mocks.gob keeps a fault mock as kind HttpFault through every rewrite of
// the file: a writer that adds to the set carries the mocks it holds over,
// and a prune writes back the mocks it keeps. Written back as kind Http, the
// fault mock would be served whole by a keploy that knows no faults.
func TestAGobFileKeepsAFaultMockAsHttpFaultThroughItsRewrites(t *testing.T) {
	fault, whole := pruneHTTPMock(0), pruneHTTPMock(1)
	fault.Spec.Fault = &models.Fault{Kind: models.FaultReset}
	want := map[string]models.Kind{"mock-0": models.HTTPFault, "mock-1": models.HTTP}

	t.Run("an insert into the set", func(t *testing.T) {
		dir := t.TempDir()
		path := writePruneFixture(t, dir, "gob", []*models.Mock{fault.ShallowCopy()}, "")
		t.Setenv("KEPLOY_MOCK_FORMAT", "gob")
		add := New(zap.NewNop(), dir, "mocks")
		add.SetCounterID(0)
		if err := add.InsertMock(context.Background(), whole.ShallowCopy(), pruneSet); err != nil {
			t.Fatal(err)
		}
		if err := add.Close(); err != nil {
			t.Fatal(err)
		}
		if got := rawGobKinds(t, path); !maps.Equal(got, want) {
			t.Fatalf("the file holds %v, want %v", got, want)
		}
	})

	t.Run("a prune", func(t *testing.T) {
		dir := t.TempDir()
		path := writePruneFixture(t, dir, "gob", []*models.Mock{fault.ShallowCopy(), whole.ShallowCopy(), pruneHTTPMock(2)}, "")
		consumed := map[string]models.MockState{"mock-0": {Name: "mock-0"}, "mock-1": {Name: "mock-1"}}
		if err := New(zap.NewNop(), dir, "mocks").UpdateMocks(context.Background(), pruneSet, consumed, pruneT0.Add(time.Hour), time.Time{}); err != nil {
			t.Fatal(err)
		}
		if got := rawGobKinds(t, path); !maps.Equal(got, want) {
			t.Fatalf("the prune left %v, want %v", got, want)
		}
	})
}
