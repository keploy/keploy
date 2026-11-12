package generic

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// wireRequest is a binary request (a non-ASCII prefix, then text) asking the
// price of item: what the recorder base64-encodes as a "binary" payload.
func wireRequest(item string) []byte {
	b := []byte{0xfe, 0xff, 0x00, 0x01}
	b = append(b, "PRICE item="+item+";"...)
	for i := 0; i < 32; i++ {
		b = append(b, fmt.Sprintf("field%02d=value%02d;", i, i)...)
	}
	return b
}

// longWireRequest is wireRequest with 400 fields: long enough that a one-byte
// change leaves it more than 0.99 like the original, so a row with it pins
// byte-exactness, not a similarity bar.
func longWireRequest(item string) []byte {
	b := []byte{0xfe, 0xff, 0x00, 0x01}
	b = append(b, "PRICE item="+item+";"...)
	for i := 0; i < 400; i++ {
		b = append(b, fmt.Sprintf("field%03d=value%03d;", i, i)...)
	}
	return b
}

// priceMock is a recording of req, answered with answer; filtered, a per-test
// mock of the switch-off pool, else a session one.
func priceMock(name string, req []byte, answer string, filtered bool) *models.Mock {
	m := &models.Mock{Name: name, Kind: models.GENERIC}
	m.TestModeInfo.IsFiltered = filtered
	m.Spec.GenericRequests = []models.Payload{{Message: []models.OutputBinary{{Type: "binary", Data: util.EncodeBase64(req)}}}}
	m.Spec.GenericResponses = []models.Payload{{Message: []models.OutputBinary{{Type: models.String, Data: answer}}}}
	return m
}

// servedStore is a store of mocks that offers no exact serving; it notes
// the name of each mock it is asked to consume.
type servedStore struct {
	integrations.MockMemDb
	mocks    []*models.Mock
	consumed *[]string
}

func (s servedStore) GetSessionMocks() ([]*models.Mock, error) { return s.mocks, nil }

func (s servedStore) UpdateUnFilteredMock(old, _ *models.Mock) bool {
	*s.consumed = append(*s.consumed, old.Name)
	return true
}

// switchedStore is a servedStore that serves exactly while *on.
type switchedStore struct {
	servedStore
	on *bool
}

func (s switchedStore) ExactServing() bool { return *s.on }

// Under exact serving (the proven switch) a Generic recording answers only
// the exchange it recorded, byte for byte: a request that changed is a miss,
// with nothing consumed, though it is as like the recording as the per-test
// (0.9) and session (0.4) similarities serve. keploy's own Linux repro: a
// binary Price("pens") was served Price("book")'s recording, and its test
// passed. Without exact serving, and while the switch is off, the closest
// recording still answers, as before; exact serving is read at each request.
func TestUnderExactServingOnlyAnIdenticalExchangeIsServed(t *testing.T) {
	book, pens := wireRequest("book"), wireRequest("pens")
	if sim := fuzzyCheck(book, pens); sim <= 0.9 {
		t.Fatalf("the changed request is %.2f like the recording; the row needs one the per-test similarity serves", sim)
	}
	ctx, logger := context.Background(), zap.NewNop()
	for _, filtered := range []bool{true, false} {
		name := fmt.Sprintf("filtered=%v", filtered)
		mocks := func() []*models.Mock { return []*models.Mock{priceMock("mock-book", book, "1250", filtered)} }
		on := true
		var consumed []string
		exact := switchedStore{servedStore{mocks: mocks(), consumed: &consumed}, &on}

		ok, resp, err := fuzzyMatch(ctx, logger, [][]byte{book}, exact)
		if err != nil || !ok || len(resp) != 1 || resp[0].Message[0].Data != "1250" {
			t.Fatalf("%s: the recorded request under exact serving: ok %v, %v, %v; want its answer", name, ok, resp, err)
		}
		if len(consumed) != 1 || consumed[0] != "mock-book" {
			t.Fatalf("%s: consumed %v serving the recorded request, want mock-book", name, consumed)
		}

		consumed = nil
		ok, resp, err = fuzzyMatch(ctx, logger, [][]byte{pens}, exact)
		if err != nil || ok || resp != nil {
			t.Fatalf("%s: a changed request was served under exact serving: ok %v, %v, %v", name, ok, resp, err)
		}
		if len(consumed) != 0 {
			t.Fatalf("%s: a miss consumed %v", name, consumed)
		}

		// Switch off: no such capability, or exact serving not on.
		on = false
		for what, db := range map[string]integrations.MockMemDb{
			"no exact serving":  servedStore{mocks: mocks(), consumed: &consumed},
			"exact serving off": exact,
		} {
			consumed = nil
			ok, resp, err = fuzzyMatch(ctx, logger, [][]byte{pens}, db)
			if err != nil || !ok || len(resp) != 1 || resp[0].Message[0].Data != "1250" || len(consumed) != 1 {
				t.Fatalf("%s, %s: the changed request: ok %v, %v, %v, consumed %v; want the closest recording, as before", name, what, ok, resp, err, consumed)
			}
		}

		// Exact serving begun between two requests on one store.
		on = true
		consumed = nil
		if ok, _, _ = fuzzyMatch(ctx, logger, [][]byte{pens}, exact); ok || len(consumed) != 0 {
			t.Fatalf("%s: exact serving begun after the store was first read: served %v, consumed %v", name, ok, consumed)
		}
	}
}

// Under exact serving a one-byte change ("book" -> "booK") is a miss, with
// nothing consumed: the recording answers its own exchange only, not one
// near enough. Without exact serving the closest recording answers it.
func TestUnderExactServingAOneByteChangeIsAMiss(t *testing.T) {
	book, booK := longWireRequest("book"), longWireRequest("booK")
	if sim := fuzzyCheck(book, booK); sim <= 0.99 {
		t.Fatalf("the changed request is %.4f like the recording; the row needs one above 0.99", sim)
	}
	ctx, logger := context.Background(), zap.NewNop()
	for _, filtered := range []bool{true, false} {
		for _, on := range []bool{true, false} {
			var consumed []string
			db := switchedStore{servedStore{mocks: []*models.Mock{priceMock("mock-book", book, "1250", filtered)}, consumed: &consumed}, &on}
			ok, resp, err := fuzzyMatch(ctx, logger, [][]byte{booK}, db)
			if err != nil {
				t.Fatalf("filtered=%v exact=%v: %v", filtered, on, err)
			}
			if on && (ok || len(consumed) != 0) {
				t.Fatalf("filtered=%v: a one-byte change was served under exact serving: %v, consumed %v", filtered, resp, consumed)
			}
			if !on && (!ok || len(resp) != 1 || resp[0].Message[0].Data != "1250" || len(consumed) != 1) {
				t.Fatalf("filtered=%v, switch off: ok %v, %v, consumed %v; want the closest recording, as before", filtered, ok, resp, consumed)
			}
		}
	}
	// Nor is it the answered exchange again: an extra call is an identical one.
	if n := extraCall([][]byte{booK}, answered{mocks: []*models.Mock{priceMock("mock-book", book, "1250", false)}}); n != 0 {
		t.Fatalf("a one-byte change of the answered exchange is an extra call recorded %d times", n)
	}
}

// Under exact serving the miss of a changed exchange tells how close the
// closest recording is without the similarities that no longer serve.
func TestAnExactServingMissSaysOnlyAnIdenticalExchangeAnswers(t *testing.T) {
	on := true
	var consumed []string
	store := switchedStore{servedStore{mocks: []*models.Mock{priceMock("mock-book", wireRequest("book"), "1250", false)}, consumed: &consumed}, &on}
	r := missReport(t, store, string(wireRequest("pens")))
	if r.ClosestMock != "mock-book" || r.MatchPhase != models.MatchPhaseExhausted {
		t.Fatalf("closest %q, phase %q; want mock-book, %q", r.ClosestMock, r.MatchPhase, models.MatchPhaseExhausted)
	}
	if !strings.Contains(r.Diff, "exact serving answers only an identical exchange") || strings.Contains(r.Diff, "thresholds") {
		t.Fatalf("diff %q: want the exact-serving rule, not the similarity thresholds", r.Diff)
	}
	if len(consumed) != 0 {
		t.Fatalf("the miss consumed %v", consumed)
	}
}

// An exchange merely like one the open test answered is a changed exchange,
// not one exchange more than recorded: under exact serving the pool answers
// only an identical one, so the extra-call check matches as it does.
func TestAnExchangeLikeAnAnsweredOneIsNotAnExtraCall(t *testing.T) {
	book, pens := wireRequest("book"), wireRequest("pens")
	db := answered{mocks: []*models.Mock{priceMock("mock-book", book, "1250", false)}}
	if n := extraCall([][]byte{book}, db); n != 1 {
		t.Fatalf("the answered exchange again: %d, want an extra call recorded once", n)
	}
	if n := extraCall([][]byte{pens}, db); n != 0 {
		t.Fatalf("a changed exchange, %.2f like the answered one, is an extra call recorded %d times", fuzzyCheck(book, pens), n)
	}
}

// redactedToken is what enterprise's obfuscator writes for a Generic request
// buffer that is a token and nothing else (here recordedToken): the buffer
// redacted whole, and the redaction listed in the mock's Noise as ^...$.
const (
	recordedToken = "ghp_aB3dE5fG7hJ9kL1mN3pQ5rS7tU9vW1xY3zA5"
	redactedToken = "lse_6754647392hwnlvgduizUSSJRYLUFX581872"
)

// tokenMock is a recording of reqs, the buffer at tokenAt the redacted
// token, answered with answer; filtered, a per-test mock of the switch-off
// pool, else a session one.
func tokenMock(name string, filtered bool, answer string, tokenAt int, reqs ...string) *models.Mock {
	m := exchange(name, reqs...)
	m.Spec.GenericRequests[tokenAt].Message[0].Data = redactedToken
	m.Noise = []string{"^" + redactedToken + "$"}
	m.TestModeInfo.IsFiltered = filtered
	m.Spec.GenericResponses = []models.Payload{{Message: []models.OutputBinary{{Type: models.String, Data: answer}}}}
	return m
}

// Under exact serving a request buffer the obfuscator redacted is compared as
// recorded, like any other: a recording whose one buffer is a redacted token
// answers no other one-buffer request to its destination, nor does an
// exchange whose token buffer changed get the recorded answer. keploy cannot
// redact a live buffer as the recording was, so the recorded token misses
// too (fail-closed). Before, the redaction matched any live buffer: under
// exact serving a GitHub token's recording answered another token, and
// "DELETE ALL ROWS". Without exact serving the redacted buffer is still not
// compared, as before.
func TestUnderExactServingARedactedBufferIsNoExcuse(t *testing.T) {
	ctx, logger := context.Background(), zap.NewNop()
	changed := recordedToken[:len(recordedToken)-1] + "B"
	for _, filtered := range []bool{true, false} {
		for _, row := range []struct {
			name    string
			tokenAt int
			rec     []string
			live    [][]byte
		}{
			{"one buffer, another token", 0, []string{""}, [][]byte{[]byte(changed)}},
			{"one buffer, another command", 0, []string{""}, [][]byte{[]byte("DELETE ALL ROWS")}},
			{"one buffer, the recorded token", 0, []string{""}, [][]byte{[]byte(recordedToken)}},
			{"hello then token, the token changed", 1, []string{"HELLO v1", "", "GET balance"}, [][]byte{[]byte("HELLO v1"), []byte(changed), []byte("GET balance")}},
		} {
			name := fmt.Sprintf("filtered=%v, %s", filtered, row.name)
			on := true
			var consumed []string
			db := switchedStore{servedStore{mocks: []*models.Mock{tokenMock("mock-token", filtered, "OK-USER-42", row.tokenAt, row.rec...)}, consumed: &consumed}, &on}
			ok, resp, err := fuzzyMatch(ctx, logger, row.live, db)
			if err != nil || ok || resp != nil || len(consumed) != 0 {
				t.Errorf("%s: served under exact serving: ok %v, %v, %v, consumed %v; want a miss", name, ok, resp, err, consumed)
			}
			// Switch off: the redacted buffer is skipped, as before.
			on = false
			ok, resp, err = fuzzyMatch(ctx, logger, row.live, db)
			if err != nil || !ok || len(resp) != 1 || resp[0].Message[0].Data != "OK-USER-42" || len(consumed) != 1 {
				t.Errorf("%s, switch off: ok %v, %v, %v, consumed %v; want the recording, as before", name, ok, resp, err, consumed)
			}
		}
	}
	// The other buffers of a redacted exchange still decide: with the switch
	// off, a changed hello does not match exactly through the token.
	mk := []*models.Mock{tokenMock("mock-token", true, "OK-USER-42", 1, "HELLO v1", "", "GET balance")}
	if idx := findExactMatch(mk, [][]byte{[]byte("HELLO v2"), []byte(changed), []byte("GET balance")}, false); idx != -1 {
		t.Fatalf("switch off: a changed hello matched exactly through the redacted token: %d", idx)
	}
}

// Under exact serving an exchange whose redacted token buffer changed is a
// changed exchange, not the answered one again: the extra-call check matches
// as the pool does.
func TestAChangedRedactedBufferIsNotAnExtraCall(t *testing.T) {
	changed := recordedToken[:len(recordedToken)-1] + "B"
	db := answered{mocks: []*models.Mock{tokenMock("mock-token", false, "OK-USER-42", 1, "HELLO v1", "", "GET balance")}}
	if n := extraCall([][]byte{[]byte("HELLO v1"), []byte(changed), []byte("GET balance")}, db); n != 0 {
		t.Fatalf("an exchange whose redacted token changed is an extra call recorded %d times", n)
	}
	if n := extraCall([][]byte{[]byte("HELLO v1"), []byte(redactedToken), []byte("GET balance")}, db); n != 1 {
		t.Fatalf("the answered exchange's own bytes again: %d, want an extra call recorded once", n)
	}
}

// Under exact serving a recorded request buffer is compared with the live
// one as bytes: a text payload's data, a binary payload's base64 decoded
// (encodePayload). Its stored text is never compared with the live buffer's
// own encoding, so two different byte strings, one the other's base64, are
// different requests. Before, a recorded binary frame answered a live text
// buffer that was its base64 (a client that starts to base64-wrap what it
// sent raw), and a recorded text buffer that is valid base64 answered the
// live bytes it decodes to (one that stops): the test passed on the recorded
// answer. A payload of an unknown type, or binary data that is not base64,
// holds no bytes keploy can compare, and differs. Without exact serving each
// is served, as before.
func TestUnderExactServingBytesAreComparedNotTheirEncoding(t *testing.T) {
	ctx, logger := context.Background(), zap.NewNop()
	frame := []byte{0x00, 0x00, 0x00, 0x0c, 0x82, 0xa4, 'i', 't', 'e', 'm', 0xa4, 'b', 'o', 'o', 'k', 0x01}
	b64 := []byte(util.EncodeBase64(frame))
	typed := func(typ, data string) models.Payload {
		return models.Payload{Message: []models.OutputBinary{{Type: typ, Data: data}}}
	}
	rows := []struct {
		name string
		rec  models.Payload
		live []byte
	}{
		{"recorded binary frame, live sends its base64 text", encodePayload(frame, models.FromClient), b64},
		{"recorded base64 text, live sends the frame it decodes to", encodePayload(b64, models.FromClient), frame},
		{"recorded text PING, live sends 3c 83 46, whose base64 is PING", encodePayload([]byte("PING"), models.FromClient), []byte{0x3c, 0x83, 0x46}},
		{"recorded binary 3c 83 46, live sends the text PING", encodePayload([]byte{0x3c, 0x83, 0x46}, models.FromClient), []byte("PING")},
		{"recorded with an unknown type, live sends its data", typed("hex", "PING"), []byte("PING")},
		{"recorded binary that is not base64, live sends its data", typed(binaryPayload, "PING!"), []byte("PING!")},
	}
	if rows[3].rec.Message[0].Type != binaryPayload || rows[3].rec.Message[0].Data != "PING" || rows[1].rec.Message[0].Type != models.String {
		t.Fatalf("the recorder's encoding moved: %+v, %+v", rows[3].rec, rows[1].rec)
	}
	for _, filtered := range []bool{true, false} {
		for _, row := range rows {
			name := fmt.Sprintf("filtered=%v, %s", filtered, row.name)
			m := &models.Mock{Name: "mock-x", Kind: models.GENERIC}
			m.Spec.GenericRequests = []models.Payload{row.rec}
			m.Spec.GenericResponses = []models.Payload{{Message: []models.OutputBinary{{Type: models.String, Data: "OK"}}}}
			m.TestModeInfo.IsFiltered = filtered
			on := true
			var consumed []string
			db := switchedStore{servedStore{mocks: []*models.Mock{m}, consumed: &consumed}, &on}
			ok, resp, err := fuzzyMatch(ctx, logger, [][]byte{row.live}, db)
			if err != nil || ok || resp != nil || len(consumed) != 0 {
				t.Errorf("%s: served under exact serving: ok %v, %v, %v, consumed %v; want a miss", name, ok, resp, err, consumed)
			}
			if n := extraCall([][]byte{row.live}, answered{mocks: []*models.Mock{m}}); n != 0 {
				t.Errorf("%s: an extra call recorded %d times, want none", name, n)
			}
			// Switch off: served, as before.
			on = false
			ok, resp, err = fuzzyMatch(ctx, logger, [][]byte{row.live}, db)
			if err != nil || !ok || len(resp) != 1 || resp[0].Message[0].Data != "OK" || len(consumed) != 1 {
				t.Errorf("%s, switch off: ok %v, %v, %v, consumed %v; want the recording, as before", name, ok, resp, err, consumed)
			}
		}
	}
	// Nor through the pieces the recording holds: a recording whose first
	// piece has an unknown type is cut as if it were base64 (recordedPieceLen),
	// and the live piece cut there is still no bytes keploy can compare.
	cut := &models.Mock{Name: "mock-x", Kind: models.GENERIC}
	cut.Spec.GenericRequests = []models.Payload{typed("hex", "PING"), typed(models.String, "X")}
	live := [][]byte{{0x3c, 0x83, 0x46, 'X'}}
	if got := findExactMatch([]*models.Mock{cut}, live, true); got != -1 {
		t.Errorf("an unknown type cut like the recording: findExactMatch = %d under exact serving, want -1", got)
	}
	if got := findExactMatch([]*models.Mock{cut}, live, false); got != 0 {
		t.Errorf("an unknown type cut like the recording, switch off: findExactMatch = %d, want 0, as before", got)
	}
	// Nor by the bytes binary data that is not base64 decodes to before it
	// fails: "PING!" decodes as far as 3c 83 46, and a live 3c 83 46 differs.
	bad := &models.Mock{Name: "mock-x", Kind: models.GENERIC}
	bad.Spec.GenericRequests = []models.Payload{typed(binaryPayload, "PING!")}
	prefix := [][]byte{{0x3c, 0x83, 0x46}}
	if got := findExactMatch([]*models.Mock{bad}, prefix, true); got != -1 {
		t.Errorf("binary PING!, live 3c 83 46: findExactMatch = %d under exact serving, want -1", got)
	}
	if got := findExactMatch([]*models.Mock{bad}, prefix, false); got != -1 {
		t.Errorf("binary PING!, live 3c 83 46, switch off: findExactMatch = %d, want -1, as before", got)
	}
	// The recorded bytes themselves are served under exact serving, whichever
	// way they were stored, read whole or in other pieces.
	for _, req := range [][]byte{frame, b64, []byte("PING"), {0x3c, 0x83, 0x46}} {
		m := &models.Mock{Name: "mock-x", Kind: models.GENERIC}
		m.Spec.GenericRequests = []models.Payload{encodePayload(req, models.FromClient)}
		for _, reads := range [][][]byte{{req}, {req[:1], req[1:]}} {
			if got := findExactMatch([]*models.Mock{m}, reads, true); got != 0 {
				t.Errorf("%q recorded as %s, read in %d pieces: findExactMatch = %d under exact serving, want 0", req, m.Spec.GenericRequests[0].Message[0].Type, len(reads), got)
			}
		}
		if n := extraCall([][]byte{req}, answered{mocks: []*models.Mock{m}}); n != 1 {
			t.Errorf("%q: the answered exchange again is an extra call recorded %d times, want once", req, n)
		}
	}
}

// Under exact serving every piece of an exchange is compared as bytes, not
// only its first: a two-piece exchange whose second piece is the other's
// encoding (recorded binary 3c 83 46, live the text PING that is its base64,
// or the reverse) is another request, in both pools, and is no extra call.
// The recorded bytes are still served. Without exact serving each is served,
// as before. (Read in one piece, such an exchange is never cut like the
// recording: a buffer and its base64 differ in length.)
func TestUnderExactServingEveryPieceIsComparedAsBytes(t *testing.T) {
	ctx, logger := context.Background(), zap.NewNop()
	for _, c := range []struct {
		name string
		rec  [][]byte
		live [][]byte
	}{
		{"2nd piece recorded binary 3c 83 46, live sends the text PING", [][]byte{[]byte("HELLO"), {0x3c, 0x83, 0x46}}, [][]byte{[]byte("HELLO"), []byte("PING")}},
		{"2nd piece recorded text PING, live sends 3c 83 46", [][]byte{[]byte("HELLO"), []byte("PING")}, [][]byte{[]byte("HELLO"), {0x3c, 0x83, 0x46}}},
	} {
		for _, filtered := range []bool{true, false} {
			name := fmt.Sprintf("filtered=%v, %s", filtered, c.name)
			m := &models.Mock{Name: "mock-x", Kind: models.GENERIC}
			for _, p := range c.rec {
				m.Spec.GenericRequests = append(m.Spec.GenericRequests, encodePayload(p, models.FromClient))
			}
			m.Spec.GenericResponses = []models.Payload{{Message: []models.OutputBinary{{Type: models.String, Data: "OK"}}}}
			m.TestModeInfo.IsFiltered = filtered
			on := true
			var consumed []string
			db := switchedStore{servedStore{mocks: []*models.Mock{m}, consumed: &consumed}, &on}
			ok, resp, err := fuzzyMatch(ctx, logger, c.live, db)
			if err != nil || ok || resp != nil || len(consumed) != 0 {
				t.Errorf("%s: served under exact serving: ok %v, %v, %v, consumed %v; want a miss", name, ok, resp, err, consumed)
			}
			if n := extraCall(c.live, answered{mocks: []*models.Mock{m}}); n != 0 {
				t.Errorf("%s: an extra call recorded %d times, want none", name, n)
			}
			if got := findExactMatch([]*models.Mock{m}, c.live, true); got != -1 {
				t.Errorf("%s: findExactMatch = %d under exact serving, want -1", name, got)
			}
			if got := findExactMatch([]*models.Mock{m}, c.live, false); got != 0 {
				t.Errorf("%s, switch off: findExactMatch = %d, want 0, as before", name, got)
			}
			consumed = nil
			if ok, _, err := fuzzyMatch(ctx, logger, c.rec, db); err != nil || !ok || len(consumed) != 1 {
				t.Errorf("%s: the recorded bytes under exact serving: ok %v, %v, consumed %v; want served", name, ok, err, consumed)
			}
		}
	}
}

// Under exact serving a recorded request piece that holds more than one
// message, or none, differs from every live piece: encodePayload writes
// exactly one, and comparing only the first would answer a live piece that
// sent part of what was recorded. Before, a recorded piece holding "PI" then
// "NG" (a hand-edited or imported mock) answered a live "PI", and missed a
// live "PING". Without exact serving it is compared as before, by its first
// message only.
func TestUnderExactServingARecordedPieceOfSeveralMessagesDiffers(t *testing.T) {
	ctx, logger := context.Background(), zap.NewNop()
	text := func(data ...string) models.Payload {
		p := models.Payload{Message: []models.OutputBinary{}}
		for _, d := range data {
			p.Message = append(p.Message, models.OutputBinary{Type: models.String, Data: d})
		}
		return p
	}
	for _, live := range []string{"PI", "PING", "NG"} {
		for _, filtered := range []bool{true, false} {
			name := fmt.Sprintf("filtered=%v, recorded [PI, NG], live %q", filtered, live)
			m := &models.Mock{Name: "mock-x", Kind: models.GENERIC}
			m.Spec.GenericRequests = []models.Payload{text("PI", "NG")}
			m.Spec.GenericResponses = []models.Payload{{Message: []models.OutputBinary{{Type: models.String, Data: "OK"}}}}
			m.TestModeInfo.IsFiltered = filtered
			on := true
			var consumed []string
			db := switchedStore{servedStore{mocks: []*models.Mock{m}, consumed: &consumed}, &on}
			reqs := [][]byte{[]byte(live)}
			ok, resp, err := fuzzyMatch(ctx, logger, reqs, db)
			if err != nil || ok || resp != nil || len(consumed) != 0 {
				t.Errorf("%s: served under exact serving: ok %v, %v, %v, consumed %v; want a miss", name, ok, resp, err, consumed)
			}
			if n := extraCall(reqs, answered{mocks: []*models.Mock{m}}); n != 0 {
				t.Errorf("%s: an extra call recorded %d times, want none", name, n)
			}
			// Switch off: the first message alone is compared, as before.
			want := -1
			if live == "PI" {
				want = 0
			}
			if got := findExactMatch([]*models.Mock{m}, reqs, false); got != want {
				t.Errorf("%s, switch off: findExactMatch = %d, want %d, as before", name, got, want)
			}
		}
	}
	// Nor through the pieces the recording holds: a two-piece recording whose
	// second piece holds two messages is cut at its first's length, and the
	// live piece cut there still differs.
	m := &models.Mock{Name: "mock-x", Kind: models.GENERIC}
	m.Spec.GenericRequests = []models.Payload{text("HELLO"), text("PI", "NG")}
	for _, reqs := range [][][]byte{{[]byte("HELLO"), []byte("PI")}, {[]byte("HELLOPI")}} {
		if got := findExactMatch([]*models.Mock{m}, reqs, true); got != -1 {
			t.Errorf("recorded [HELLO, [PI, NG]], live %q: findExactMatch = %d under exact serving, want -1", reqs, got)
		}
		if got := findExactMatch([]*models.Mock{m}, reqs, false); got != 0 {
			t.Errorf("recorded [HELLO, [PI, NG]], live %q, switch off: findExactMatch = %d, want 0, as before", reqs, got)
		}
	}
	// A recorded piece that holds no message differs under exact serving,
	// and the compare does not index into it.
	m.Spec.GenericRequests = []models.Payload{text()}
	if got := findExactMatch([]*models.Mock{m}, [][]byte{[]byte("PI")}, true); got != -1 {
		t.Errorf("a recorded piece with no message: findExactMatch = %d under exact serving, want -1", got)
	}
}
