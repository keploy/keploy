package generic

import (
	"context"
	"errors"
	"net"
	"regexp"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/relay"
	pUtil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// answered is a store whose open test's answered mocks are mocks (exact
// serving, under the proven switch).
type answered struct {
	integrations.MockMemDb
	mocks []*models.Mock
}

func (a answered) ConsumedTestMocks() []*models.Mock { return a.mocks }

func exchange(name string, req ...string) *models.Mock {
	m := &models.Mock{Name: name, Kind: models.GENERIC}
	for _, r := range req {
		m.Spec.GenericRequests = append(m.Spec.GenericRequests, models.Payload{Message: []models.OutputBinary{{Type: models.String, Data: r}}})
	}
	return m
}

// An exchange the open test's own mocks already answered, as often as it
// was recorded, is one exchange more than recorded; any other unanswered
// exchange is not, and nothing is one without exact serving.
func TestAnExchangeMoreThanRecordedIsAnExtraCall(t *testing.T) {
	db := answered{mocks: []*models.Mock{exchange("mock-1", "PING"), exchange("mock-2", "PING"), exchange("mock-3", "GET k")}}
	if n := extraCall([][]byte{[]byte("PING")}, db); n != 2 {
		t.Fatalf("PING was recorded twice for the test, got %d", n)
	}
	if n := extraCall([][]byte{[]byte("SET k v")}, db); n != 0 {
		t.Fatalf("an exchange the test never recorded is an extra call: %d", n)
	}
	if n := extraCall([][]byte{[]byte("PING")}, struct{ integrations.MockMemDb }{}); n != 0 {
		t.Fatalf("without exact serving: %d", n)
	}
}

// answeredStore is a pingStore whose open test has answered the exchanges
// answered (exact serving): they are in none of its reads.
type answeredStore struct {
	pingStore
	answered []*models.Mock
}

func (s answeredStore) ConsumedTestMocks() []*models.Mock { return s.answered }

// missReport is the report of the miss decodeGeneric returns for req. A
// request answered instead writes its answer to the app, which reads it
// and hangs up: the decoder ends with no miss, and the row fails at once
// rather than leaving the answer blocked on a pipe nobody reads. A parser
// that never ends the request (the connection is quiet once req is sent)
// fails the row at the bound instead of holding the package's run.
func missReport(t *testing.T, store integrations.MockMemDb, req string) *models.MockMismatchReport {
	t.Helper()
	app, proxySide := net.Pipe()
	defer app.Close()
	go func() {
		if _, err := app.Read(make([]byte, 1)); err == nil {
			_ = app.Close()
		}
	}()
	done := make(chan error, 1)
	go func() {
		done <- decodeGeneric(context.Background(), zap.NewNop(), []byte(req), proxySide, nil, store, models.OutgoingOptions{})
	}()
	var err error
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%q: the parser never ended the request on a quiet connection", req)
	}
	var r interface {
		MismatchReport() *models.MockMismatchReport
	}
	if !errors.As(err, &r) || r.MismatchReport() == nil {
		t.Fatalf("%q: no miss with a report: %v", req, err)
	}
	return r.MismatchReport()
}

// Under exact serving an exchange the test recorded is in no read once it
// answered, so a changed exchange the test sends after it found nothing to
// be compared with ("no_mocks"). It is compared with the test's answered
// exchanges too, after the pool's, never answered by them. An exact repeat
// stays an extra call, compared as before.
func TestAChangedExchangeIsComparedWithTheTestsAnsweredOnes(t *testing.T) {
	store := answeredStore{answered: []*models.Mock{exchange("mock-1", "SET k v")}}
	r := missReport(t, store, "SET k w")
	if r.MissKind == models.MissExtraCall {
		t.Fatalf("a changed exchange was filed as an extra call")
	}
	if r.ClosestMock != "mock-1" || r.MatchPhase != models.MatchPhaseExhausted || r.CandidateCount != 1 {
		t.Fatalf("closest %q, phase %q, %d candidates; want mock-1, %q, 1", r.ClosestMock, r.MatchPhase, r.CandidateCount, models.MatchPhaseExhausted)
	}

	// The pool's exchanges are compared too: one of two requests is not
	// comparable with a one-request exchange, and the answered one is
	// closest of the two.
	pooled := exchange("mock-0", "MULTI", "EXEC")
	r = missReport(t, answeredStore{pingStore: pingStore{mocks: []*models.Mock{pooled}}, answered: store.answered}, "SET k w")
	if r.ClosestMock != "mock-1" || r.CandidateCount != 2 {
		t.Fatalf("closest %q of %d candidates; want the answered mock-1 of 2", r.ClosestMock, r.CandidateCount)
	}

	r = missReport(t, store, "SET k v")
	if r.MissKind != models.MissExtraCall || r.Recorded != 1 {
		t.Fatalf("an exact repeat: kind %q, recorded %d; want an extra call recorded once", r.MissKind, r.Recorded)
	}
	if r.ClosestMock != "" || r.MatchPhase != models.MatchPhaseNoMocks {
		t.Fatalf("an extra call was compared with what the test answered: closest %q, phase %q", r.ClosestMock, r.MatchPhase)
	}
}

// varied is n bytes that differ along their length, so a request cut in the
// wrong place is not the same bytes; binary adds bytes no ASCII piece holds
// (the recorder keeps such a piece base64 encoded).
func varied(n int, binary bool) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('a' + (i*7+i/251)%26)
		if binary && i%97 == 0 {
			b[i] = 0x80 | byte(i)
		}
	}
	return b
}

// recordedAs is a generic exchange whose request was recorded in pieces, each
// kept as the recorder keeps it (encodePayload).
func recordedAs(name string, pieces ...[]byte) *models.Mock {
	m := &models.Mock{Name: name, Kind: models.GENERIC}
	for _, p := range pieces {
		m.Spec.GenericRequests = append(m.Spec.GenericRequests, encodePayload(p, models.FromClient))
	}
	return m
}

// splitAt cuts b where each of at ends.
func splitAt(b []byte, at ...int) [][]byte {
	var out [][]byte
	prev := 0
	for _, n := range at {
		out = append(out, b[prev:n])
		prev = n
	}
	return append(out, b[prev:])
}

// A recording whose request is longer than one read still matches exactly,
// whatever pieces this replay read it in. The old read loop joined what had
// arrived together into one piece, so a 40 KiB request was recorded whole; a
// read now returns at most ReadChunkSize (32 KiB), and the request came in
// as 32 KiB and 8 KiB, compared piece by piece with the one: no exact match,
// left to the 0.9 similarity (findBinaryMatch), which a request that differs
// at all misses. The same holds for a recording read in pieces (the relay's,
// 32 KiB and 8 KiB) that arrives in others. Lengths 40960, 32768 and 8192
// leave a base64 piece with both paddings.
func TestARequestLongerThanAReadMatchesItsRecordingExactly(t *testing.T) {
	const chunk = 32 << 10
	for _, binary := range []bool{false, true} {
		req := varied(40<<10, binary)
		cases := []struct {
			name  string
			mock  *models.Mock
			reads [][][]byte
		}{
			{"recorded whole by the old read loop", recordedAs("old", req), [][][]byte{
				splitAt(req, chunk), splitAt(req, 16<<10), splitAt(req, 1000, chunk+1000), {req},
			}},
			{"recorded in the relay's pieces", recordedAs("v2", req[:chunk], req[chunk:]), [][][]byte{
				splitAt(req, chunk), splitAt(req, 16<<10), splitAt(req, 16<<10, chunk), {req},
			}},
			{"recorded in three pieces", recordedAs("v2-3", req[:1000], req[1000:chunk], req[chunk:]), [][][]byte{
				splitAt(req, chunk), splitAt(req, 16<<10), {req},
			}},
		}
		for _, c := range cases {
			// The same with exact serving on and off: neither has noise.
			for _, exact := range []bool{false, true} {
				for _, reads := range c.reads {
					if got := findExactMatch([]*models.Mock{c.mock}, reads, exact); got != 0 {
						t.Errorf("binary=%v, exact=%v, %s, read in %d pieces: findExactMatch = %d, want 0", binary, exact, c.name, len(reads), got)
					}
				}
			}
			// Other bytes are no match: one changed byte in the last piece,
			// a byte more, a byte less.
			changed := append([]byte(nil), req...)
			changed[len(changed)-2] ^= 1
			for what, other := range map[string][]byte{
				"a changed byte": changed,
				"a byte more":    append(append([]byte(nil), req...), 'z'),
				"a byte less":    req[:len(req)-1],
			} {
				for _, exact := range []bool{false, true} {
					if got := findExactMatch([]*models.Mock{c.mock}, splitAt(other, chunk), exact); got != -1 {
						t.Errorf("binary=%v, exact=%v, %s, %s: findExactMatch = %d, want -1", binary, exact, c.name, what, got)
					}
				}
			}
		}
	}
}

// A request that differs a little from a recording longer than one read is
// compared with it by its bytes for a similarity, whatever pieces its reads
// cut it in and whatever pieces the recording holds: the old read loop's
// one piece, the relay's 32 KiB and 8 KiB, or others. So is the miss
// report's closest. Other bytes are not like it.
func TestASimilarRequestIsComparedWithARecordingByItsBytes(t *testing.T) {
	req := varied(40<<10, true)
	near := append([]byte(nil), req...)
	near[100] ^= 1
	for _, recorded := range []*models.Mock{
		recordedAs("old", req),
		recordedAs("relay", req[:32<<10], req[32<<10:]),
	} {
		for _, reads := range [][][]byte{
			{near}, splitAt(near, 32<<10), splitAt(near, 16<<10), splitAt(near, 1000, 1000+32<<10),
		} {
			if got := findBinaryMatch([]*models.Mock{recorded}, reads, 0.9); got != 0 {
				t.Errorf("%s: a near request read in %d pieces: findBinaryMatch = %d, want 0", recorded.Name, len(reads), got)
			}
			store := eofStore{mocks: []*models.Mock{exchange("ping", "PING"), recorded}}
			if r := buildGenericMismatchReport(context.Background(), reads, store, nil); r.ClosestMock != recorded.Name {
				t.Errorf("%s: a near request read in %d pieces: the miss report's closest is %q", recorded.Name, len(reads), r.ClosestMock)
			}
		}
		if got := findBinaryMatch([]*models.Mock{recorded}, splitAt(varied(40<<10, false), 1000), 0.4); got != -1 {
			t.Errorf("%s: other bytes: findBinaryMatch = %d, want -1", recorded.Name, got)
		}
	}
}

// noisyRecording is a recording of body and of a piece the obfuscator
// redacted (redacted, kept binary), its Noise naming that piece.
func noisyRecording(body, redacted []byte) *models.Mock {
	m := recordedAs("noisy", body, redacted)
	m.Noise = []string{"^" + regexp.QuoteMeta(m.Spec.GenericRequests[1].Message[0].Data) + "$"}
	return m
}

// A piece the obfuscator redacted is left out of a similarity: out of the
// recording's bytes, and, when the request's pieces line up with the
// recording's, the request's piece in its place too, as before. A recording
// every piece of which is redacted is like any request whose pieces line up
// with its own, as before, and like no other.
func TestARedactedPieceIsLeftOutOfASimilarity(t *testing.T) {
	body := varied(4<<10, true)
	near := append([]byte(nil), body...)
	near[100] ^= 1
	token := varied(4<<10, false)
	allNoisy := recordedAs("all-noisy", []byte("A"), []byte("B"))
	allNoisy.Noise = []string{"^A$", "^B$"}
	for _, c := range []struct {
		name     string
		recorded *models.Mock
		reads    [][]byte
		want     int
	}{
		// The recorded redaction is long, the live token short: compared
		// with every byte the recording holds, the request is not like it.
		{"a long redaction, the request in other pieces", noisyRecording(body, varied(32<<10, true)), splitAt(append(append([]byte(nil), near...), "tok"...), 1000, 2000), 0},
		// The live token is long: it is like the recording only once left
		// out, in the place the request's pieces give it.
		{"a long token, the pieces lined up", noisyRecording(body, []byte("REDACTED")), [][]byte{near, token}, 0},
		{"every piece redacted, the pieces lined up", allNoisy, [][]byte{near, token}, 0},
		{"every piece redacted, in other pieces", allNoisy, [][]byte{near}, -1},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := findBinaryMatch([]*models.Mock{c.recorded}, c.reads, 0.9); got != c.want {
				t.Fatalf("findBinaryMatch = %d, want %d", got, c.want)
			}
		})
	}
}

// A recording with no redacted piece is compared with a request by its bytes
// only, even where the request's pieces line up with its own: a short piece
// the same beside a long piece changed is not like it, though the two
// pieces' average similarity (1 and 0) is over the 0.4 bound. The pieces are
// binary: a recorded text piece is not compared by its bytes, as on main.
func TestARecordingWithNoRedactedPieceIsComparedByItsBytesOnly(t *testing.T) {
	head := varied(100, true)
	body := varied(4<<10, true)
	changed := make([]byte, len(body))
	for i, b := range body {
		changed[i] = b ^ 0x20 // every letter's case flipped: no 8-byte run in common
	}
	recorded := recordedAs("unredacted", head, body)
	if got := findBinaryMatch([]*models.Mock{recorded}, [][]byte{head, changed}, 0.4); got != -1 {
		t.Errorf("the long piece changed, the pieces lined up: findBinaryMatch = %d, want -1", got)
	}
	if got := findBinaryMatch([]*models.Mock{recorded}, [][]byte{head, body}, 0.9); got != 0 {
		t.Errorf("the same request, the pieces lined up: findBinaryMatch = %d, want 0", got)
	}
}

// Of recordings equally like a request, the first in the pool's order is
// matched.
func TestOfRecordingsEquallyLikeARequestTheFirstIsMatched(t *testing.T) {
	req := varied(4<<10, true)
	near := append([]byte(nil), req...)
	near[100] ^= 1
	pool := []*models.Mock{recordedAs("first", req), recordedAs("second", req)}
	if got := findBinaryMatch(pool, [][]byte{near}, 0.4); got != 0 {
		t.Errorf("findBinaryMatch = %d, want 0", got)
	}
}

// A recording a piece of which holds no message, or holds no bytes keploy
// can read, is compared with nothing, by the matcher or the miss report,
// and stops neither.
func TestARecordingWithAPieceOfNoMessageIsComparedWithNothing(t *testing.T) {
	near := varied(4<<10, true)
	noMessage := noisyRecording(near, []byte("REDACTED"))
	noMessage.Spec.GenericRequests = append(noMessage.Spec.GenericRequests, models.Payload{})
	noPieces := &models.Mock{Name: "no-pieces", Kind: models.GENERIC}
	notBase64 := &models.Mock{Name: "not-base64", Kind: models.GENERIC}
	notBase64.Spec.GenericRequests = []models.Payload{{Message: []models.OutputBinary{{Type: binaryPayload, Data: "!!!!"}}}}
	for _, m := range []*models.Mock{noMessage, noPieces} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s: findBinaryMatch panicked: %v", m.Name, r)
				}
			}()
			if got := findBinaryMatch([]*models.Mock{m}, [][]byte{near, []byte("tok"), nil}, 0.4); got != -1 {
				t.Errorf("%s: findBinaryMatch = %d, want -1", m.Name, got)
			}
		}()
	}
	for _, m := range []*models.Mock{noMessage, noPieces, notBase64} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s: the miss report panicked: %v", m.Name, r)
				}
			}()
			if r := buildGenericMismatchReport(context.Background(), [][]byte{near}, eofStore{mocks: []*models.Mock{m}}, nil); r.ClosestMock != "" {
				t.Errorf("%s: the miss report's closest is %q, want none", m.Name, r.ClosestMock)
			}
		}()
	}
}

// The replay reads with the size the relay that records reads with, so a
// recording's pieces are the replay's reads for a similarity, which compares
// them piece by piece.
func TestTheReplayReadsWithTheRecordersReadSize(t *testing.T) {
	if pUtil.ReadChunkSize != relay.DefaultForwardBuf {
		t.Fatalf("ReadChunkSize %d, relay.DefaultForwardBuf %d", pUtil.ReadChunkSize, relay.DefaultForwardBuf)
	}
}
