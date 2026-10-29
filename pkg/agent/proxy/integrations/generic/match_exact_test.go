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
