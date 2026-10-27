package learn

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// A random id the application makes (api-server's appId, a document's _id)
// whose 4-hex group stands by chance in the answer of a recording another
// test was given (another test's random id: "48d1", the version group of a
// v4 UUID, has 4096 values) is learned as varying in record's full round
// where both runs gave the application that word there and one of the two
// values lacks it: the recording gave the answer strictly before the call,
// and F served it as recorded, or its view shows the word at the same field.
// An application that copied the word would have copied it in both runs.
// Everywhere else the word stays refused, naming why; keploy test
// (ClassifyGiven) refuses it as before. Nothing reads the id's shape.
func TestAChanceWordOfAnotherTestsAnswerIsLearnedAsVarying(t *testing.T) {
	const p = "body.appId"
	const (
		srcID   = "bb7ed9ee-670e-48d1-982f-b54b2b73982f" // another test's random message id
		chance  = "5c0e9a7b-1f2d-48d1-9a3b-c4d5e6f70812" // the call's random appId: "48d1" by chance
		other   = "0c1d2e3f-4a5b-4c7d-8e9f-a0b1c2d3e4f5" // the call's appId in the other run
		movedID = "e1f2a3b4-c5d6-4e7f-8a9b-0c1d2e3f4a5b" // what a view gave at the message id instead
	)
	before := r1Insert.Add(-time.Minute)
	src := func(id string, req, res time.Time) *models.Mock {
		m := receiveOf("mock-31", id, req)
		m.Spec.ResTimestampMock = res
		return m
	}
	asRecorded := func(dc *DriftCache, m *models.Mock) Raw { return dc.RawOf(m, true).ServedAs(3).ServedAsRecorded() }
	noView := func(dc *DriftCache, m *models.Mock) Raw { return dc.RawOf(m, true).ServedAs(3) }
	viewOf := func(live *models.Mock) func(dc *DriftCache, m *models.Mock) Raw {
		return func(dc *DriftCache, m *models.Mock) Raw {
			return dc.RawOf(m, true).ServedAs(3).ServedLive(dc.RawOf(live, true))
		}
	}
	okSrc := src(srcID, before, before.Add(time.Second))
	bodyMoved := src(srcID, before, before.Add(time.Second))
	bodyMoved.Spec.HTTPResp.Body = strings.Replace(bodyMoved.Spec.HTTPResp.Body, `"Body": "S12 message"`, `"Body": "S13 message"`, 1)
	elsewhere := src(srcID, before, before.Add(time.Second))
	elsewhere.Spec.HTTPResp.Header = map[string]string{}
	for k, v := range okSrc.Spec.HTTPResp.Header {
		elsewhere.Spec.HTTPResp.Header[k] = v
	}
	elsewhere.Spec.HTTPResp.Header["X-Amzn-Requestid"] = "ec39f47f-d674-48d1-99d6-15655f71954a"
	if bodyMoved.Spec.HTTPResp.Body == okSrc.Spec.HTTPResp.Body || okSrc.Spec.HTTPResp.Header["X-Amzn-Requestid"] == elsewhere.Spec.HTTPResp.Header["X-Amzn-Requestid"] {
		t.Fatal("the view did not change")
	}
	for _, row := range []struct {
		name      string
		m         *models.Mock
		raw       func(dc *DriftCache, m *models.Mock) Raw
		rec, live string
		want      string // "" noise under ClassifyFollowing
	}{
		{"the recorded id's group, served as recorded", okSrc, asRecorded, chance, other, ""},
		{"the id's group F saw, served as recorded", okSrc, asRecorded, other, chance, ""},
		{"served changed, its view giving the word at the same field", okSrc, viewOf(bodyMoved), chance, other, ""},
		{"two groups of the id in one answer, served as recorded", src("bb7ed9ee-1f2d-48d1-982f-b54b2b73982f", before, before.Add(time.Second)), asRecorded, chance, other, ""},
		{"served changed with no view", okSrc, noView, chance, other, "record's full round did not give the application that word there before the call"},
		{"served changed, its view giving the word at another field too (a header the recording's lacks it at)", okSrc, viewOf(elsewhere), chance, other, "record's full round did not give the application that word there before the call"},
		{"served changed, its view giving another id there", okSrc, viewOf(src(movedID, before, before.Add(time.Second))), chance, other, "record's full round did not give the application that word there before the call"},
		{"answered at the call", src(srcID, before, r1Insert), asRecorded, chance, other, "the recording did not give the application that word there before the call"},
		{"no answer time", src(srcID, before, time.Time{}), asRecorded, chance, other, "keploy cannot read when the recording answered mock-31"},
	} {
		for _, viaSet := range []bool{false, true} {
			for _, follow := range []bool{false, true} {
				dc := clockedDrifts()
				x := row.raw(dc, row.m)
				classify := ClassifyGiven
				if follow {
					classify = ClassifyFollowing
				}
				var ln Learned
				if viaSet {
					ln = classify(dc, []string{p}, strLeaf(p, row.live), strLeaf(p, row.rec), r1Insert, nil, nil, nil, nil, func() ([]GivenPlaces, error) {
						return []GivenPlaces{PlacesOf(x)}, nil
					})[0]
				} else {
					ln = classify(dc, []string{p}, strLeaf(p, row.live), strLeaf(p, row.rec), r1Insert, nil, []Raw{x.OfAnotherTest()}, nil, nil, nil)[0]
				}
				refused := "appears in mock-31, a recording record's full round gave the application before the call that is another test's"
				switch {
				case follow && row.want == "":
					// The noise names its evidence (learning is legible): the
					// word, where both runs gave it, which value lacks it, and
					// that the value lacking it holds no word of any place
					// either run gave.
					word, lacks := "a word of its recorded value (its characters ", "the value record's full round saw lacks that word"
					if row.rec != chance {
						word, lacks = "a word of what record's full round saw in it (its characters ", "the recorded value lacks that word"
					}
					if ln.Kind != KindNoise || !strings.HasPrefix(ln.Why, word) || !strings.HasSuffix(ln.Why, ") stands in mock-31, which the recording and record's full round both gave the application at the same fields before the call, and "+lacks+" and holds no word of any place either run gave the application before the call: an application that copied the word would have copied it in both runs, so the value holds it by chance, and keploy learns it as varying") {
						t.Errorf("%s (set %v): %s (%s); want noise naming the chance word", row.name, viaSet, ln.Kind, ln.Why)
					}
				case follow:
					if ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, refused) || !strings.Contains(ln.Why, row.want) {
						t.Errorf("%s (set %v): %s (%s); want it refused: %s", row.name, viaSet, ln.Kind, ln.Why, row.want)
					}
				default:
					if ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, refused) {
						t.Errorf("%s (set %v), keploy test: %s (%s); want it refused as before", row.name, viaSet, ln.Kind, ln.Why)
					}
				}
			}
		}
	}
}

// Two recordings of other tests each hold a word of the value by chance; F
// served one of them at a place in its serves keploy does not know. The
// order the two runs gave the application its candidate sources cannot be
// told, so the value is refused, naming it: a recording F gave the
// application earlier in the set is ordered with the call's own places.
func TestChanceWordsOfTwoOtherTestsInAnUnknownServeOrderAreRefused(t *testing.T) {
	const p = "body.appId"
	before := r1Insert.Add(-time.Minute)
	m1 := receiveOf("mock-31", "bb7ed9ee-670e-48d1-982f-b54b2b73982f", before)
	m2 := receiveOf("mock-32", "aaaaaaaa-1f2d-4222-8333-bbbbbbbbbbbb", before.Add(5*time.Second))
	for _, viaSet := range []bool{false, true} {
		dc := clockedDrifts()
		x1 := dc.RawOf(m1, true).ServedAs(3).ServedAsRecorded()
		x2 := dc.RawOf(m2, true).ServedAsRecorded()
		var ln Learned
		if viaSet {
			ln = ClassifyFollowing(dc, []string{p}, strLeaf(p, "0c1d2e3f-4a5b-4c7d-8e9f-a0b1c2d3e4f5"), strLeaf(p, "5c0e9a7b-1f2d-48d1-9a3b-c4d5e6f70812"), r1Insert, nil, nil, nil, nil, func() ([]GivenPlaces, error) {
				return []GivenPlaces{PlacesOf(x1), PlacesOf(x2)}, nil
			})[0]
		} else {
			ln = ClassifyFollowing(dc, []string{p}, strLeaf(p, "0c1d2e3f-4a5b-4c7d-8e9f-a0b1c2d3e4f5"), strLeaf(p, "5c0e9a7b-1f2d-48d1-9a3b-c4d5e6f70812"), r1Insert, nil, []Raw{x1.OfAnotherTest(), x2.OfAnotherTest()}, nil, nil, nil)[0]
		}
		if ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "keploy does not know where record's full round served mock-32 among the other earlier answers it served") {
			t.Errorf("set %v: %s (%s); want it refused for the order", viaSet, ln.Kind, ln.Why)
		}
	}
}

// A word both runs gave the application at the same field of another test's
// answer, which one of the two values lacks, shows the value no copy only
// where the value that lacks it holds no other word of that payload: as the
// recording gave it, as record's full round gave it (its view), its stamps
// read as any text (the mirror rule's recordedTook evidence). A field is a
// whole payload field (an HTTP body), so a view that gave the same ids in
// another order (a queue receive of two messages, which the queue does not
// order) holds each id's words at the same field in both runs: an
// application that copied the first message's id holds one id in each run,
// each a word of that payload, and stays refused, naming why. keploy test
// (ClassifyGiven) refuses each as before. Nothing reads the id's shape.
func TestAChanceWordStaysRefusedWhereTheValueLackingItHoldsAnotherWordOfThatPayload(t *testing.T) {
	const p = "body.message_id"
	const (
		idA    = "bb7ed9ee-670e-48d1-982f-b54b2b73982f" // the first message's id at record
		idB    = "5c0e9a7b-1f2d-4c3e-9a3b-c4d5e6f70812" // the second message's id
		idC    = "e1f2a3b4-c5d6-4e7f-8a9b-0c1d2e3f4a5b" // a third message's id
		chance = "0c1d2e3f-4a5b-48d1-8e9f-a0b1c2d3e4f5" // the value F saw: "48d1" by chance
		other  = "9a8b7c6d-5e4f-4a3b-9c2d-1e0f9a8b7c6d" // the value at record
	)
	before := r1Insert.Add(-time.Minute)
	two := func(first, second string) *models.Mock {
		m := receiveOf("mock-31", first, before)
		m.Spec.ResTimestampMock = before.Add(time.Second)
		m.Spec.HTTPResp.Body = `{"Messages": [{"MessageId": "` + first + `", "Body": "m1"}, {"MessageId": "` + second + `", "Body": "m2"}]}`
		return m
	}
	// The answer's own stamp, its Date header in the span it was asked and
	// answered in: at record, and in record's full round a day later.
	stamped := func(m *models.Mock, day time.Duration) *models.Mock {
		m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock = m.Spec.ReqTimestampMock.Add(day), m.Spec.ResTimestampMock.Add(day)
		m.Spec.HTTPResp.Header = map[string]string{"Content-Type": "application/x-amz-json-1.0", "Date": m.Spec.ResTimestampMock.Format(http.TimeFormat)}
		return m
	}
	recStamp, viewStamp := stamped(receiveOf("mock-31", idA, before), 0), stamped(receiveOf("mock-31", idA, before), 24*time.Hour)
	if !strings.HasPrefix(recStamp.Spec.HTTPResp.Header["Date"], "Thu, ") || !strings.HasPrefix(viewStamp.Spec.HTTPResp.Header["Date"], "Fri, ") {
		t.Fatalf("the stamps %q and %q are not a Thursday's and a Friday's", recStamp.Spec.HTTPResp.Header["Date"], viewStamp.Spec.HTTPResp.Header["Date"])
	}
	if x := RawOf(recStamp, true); len(x.stamps) != 1 {
		t.Fatalf("the recording's Date header is not its one stamp")
	}
	if x := RawOf(viewStamp, true); len(x.stamps) != 1 {
		t.Fatalf("the view's Date header is not its one stamp")
	}
	asRecorded := func(dc *DriftCache, m *models.Mock) Raw { return dc.RawOf(m, true).ServedAs(3).ServedAsRecorded() }
	viewOf := func(live *models.Mock) func(dc *DriftCache, m *models.Mock) Raw {
		return func(dc *DriftCache, m *models.Mock) Raw {
			return dc.RawOf(m, true).ServedAs(3).ServedLive(dc.RawOf(live, true))
		}
	}
	const recorded, gave = "stands in mock-31's answer as the recording gave it", "stands in mock-31's answer as record's full round gave it"
	for _, row := range []struct {
		name      string
		m         *models.Mock
		raw       func(dc *DriftCache, m *models.Mock) Raw
		rec, live string
		want      string
	}{
		{"its view gave the two ids in the other order", two(idA, idB), viewOf(two(idB, idA)), idA, idB, recorded},
		{"served as recorded, the value F saw is the answer's other id", two(idA, idB), asRecorded, idA, idB, recorded},
		{"the recorded value's word stands only in what record's full round gave there", two(idB, idC), viewOf(two(idB, idA)), idA, idB, gave},
		{"the recorded value's word stands only in the recording's stamp", recStamp, asRecorded, other + "-Thu", chance, recorded},
		{"the recorded value's word stands only in the view's stamp", recStamp, viewOf(viewStamp), other + "-Fri", chance, gave},
		{"the word of the value F saw stands only in the recording's stamp", recStamp, asRecorded, chance, other + "-Thu", recorded},
		{"the word of the value F saw stands only in the view's stamp", recStamp, viewOf(viewStamp), chance, other + "-Fri", gave},
	} {
		for _, viaSet := range []bool{false, true} {
			for _, follow := range []bool{false, true} {
				dc := clockedDrifts()
				x := row.raw(dc, row.m)
				classify := ClassifyGiven
				if follow {
					classify = ClassifyFollowing
				}
				var ln Learned
				if viaSet {
					ln = classify(dc, []string{p}, strLeaf(p, row.live), strLeaf(p, row.rec), r1Insert, nil, nil, nil, nil, func() ([]GivenPlaces, error) {
						return []GivenPlaces{PlacesOf(x)}, nil
					})[0]
				} else {
					ln = classify(dc, []string{p}, strLeaf(p, row.live), strLeaf(p, row.rec), r1Insert, nil, []Raw{x.OfAnotherTest()}, nil, nil, nil)[0]
				}
				refused := "appears in mock-31, a recording record's full round gave the application before the call that is another test's"
				switch {
				case follow && (ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, refused) || !strings.Contains(ln.Why, row.want) || !strings.Contains(ln.Why, "the value that lacks that word")):
					t.Errorf("%s (set %v): %s (%s); want it refused: %s", row.name, viaSet, ln.Kind, ln.Why, row.want)
				case !follow && (ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, refused)):
					t.Errorf("%s (set %v), keploy test: %s (%s); want it refused as before", row.name, viaSet, ln.Kind, ln.Why)
				}
			}
		}
	}
}

// From the review of the chance-word rule (rv3): the application writes one
// of two ids that two recordings it was given before the call hold, picked
// by an order neither run shows (a map's iteration, a concurrent consumer):
// mock-31's id at record and mock-32's in record's full round, or the other
// way about. Each value then lacks the word the other holds, and each holds
// a word of a place both runs gave: neither word shows the value made beside
// it without taking any, so both stay refused, naming the place the value
// that lacks the word took a word of (tookElsewhere). That place may be the
// other test's answer, or an earlier place of the call's own test. A fresh
// value, holding no word of any place, beside the same two answers is still
// learned as varying. keploy test (ClassifyGiven) learns none of them as
// noise, as before.
func TestAChanceWordStaysRefusedWhereTheValueLackingItHoldsAWordOfAnyPlaceBothRunsGave(t *testing.T) {
	const p = "body.message_id"
	const (
		idA    = "bb7ed9ee-670e-48d1-982f-b54b2b73982f" // mock-31's message id
		idB    = "5c0e9a7b-1f2d-4c3e-9a3b-c4d5e6f70812" // mock-32's message id
		chance = "0c1d2e3f-4a5b-48d1-8e9f-a0b1c2d3e4f5" // a fresh id: "48d1" by chance
		fresh  = "9a8b7c6d-5e4f-4a3b-9c2d-1e0f9a8b7c6d" // a fresh id, no word of any place
	)
	before := r1Insert.Add(-time.Minute)
	const lacks = "the value that lacks that word holds a word of a place both runs gave the application before the call: "
	for _, row := range []struct {
		name      string
		own       bool // mock-32 an earlier place of the call's own test
		rec, live string
		want      string // "" noise under ClassifyFollowing
	}{
		{"mock-31's id at record, mock-32's in F", false, idA, idB, "stands in mock-32's answer as the recording gave it"},
		{"mock-32's id at record, mock-31's in F", false, idB, idA, "stands in mock-31's answer as the recording gave it"},
		{"mock-31's id at record, the own place's in F", true, idA, idB, "stands in mock-32 at spec.resp.body"},
		{"the own place's id at record, mock-31's in F", true, idB, idA, "stands in mock-32 at spec.resp.body"},
		{"a word by chance at record, a fresh id in F", false, chance, fresh, ""},
		{"a fresh id at record, a word by chance in F", true, fresh, chance, ""},
	} {
		for _, viaSet := range []bool{false, true} {
			for _, follow := range []bool{false, true} {
				dc := clockedDrifts()
				x1 := dc.RawOf(receiveOf("mock-31", idA, before), true).ServedAs(3).ServedAsRecorded()
				x2 := dc.RawOf(receiveOf("mock-32", idB, before.Add(5*time.Second)), true).ServedAs(4).ServedAsRecorded()
				classify := ClassifyGiven
				if follow {
					classify = ClassifyFollowing
				}
				var raw []Raw
				var set GivenEarlier
				switch {
				case row.own && viaSet:
					raw = []Raw{x2}
					set = func() ([]GivenPlaces, error) { return []GivenPlaces{PlacesOf(x1)}, nil }
				case row.own:
					raw = []Raw{x1.OfAnotherTest(), x2}
				case viaSet:
					set = func() ([]GivenPlaces, error) { return []GivenPlaces{PlacesOf(x1), PlacesOf(x2)}, nil }
				default:
					raw = []Raw{x1.OfAnotherTest(), x2.OfAnotherTest()}
				}
				ln := classify(dc, []string{p}, strLeaf(p, row.live), strLeaf(p, row.rec), r1Insert, nil, raw, nil, nil, set)[0]
				switch {
				case !follow && ln.Kind == KindNoise:
					t.Errorf("%s (set %v), keploy test: %s (%s); want it refused, or bound where the recorded value stands whole in the own place, as before", row.name, viaSet, ln.Kind, ln.Why)
				case follow && row.want == "" && ln.Kind != KindNoise:
					t.Errorf("%s (set %v): %s (%s); want noise", row.name, viaSet, ln.Kind, ln.Why)
				case follow && row.want != "" && (ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, lacks) || !strings.Contains(ln.Why, row.want)):
					t.Errorf("FALSE GREEN? %s (set %v): %s (%s); want it refused: %s", row.name, viaSet, ln.Kind, ln.Why, row.want)
				}
			}
		}
	}
	// The value that lacks the word is read as its run gave the places: the
	// value F saw by what F gave (an own place's view, a leaf F served), the
	// recorded value by what the recording gave, not by F's view; a leaf's
	// stamp as the word rule reads it.
	idC := "e1f2a3b4-c5d6-4e7f-8a9b-0c1d2e3f4a5b" // what the other run gave there
	leaf := func(rec, live string) Earlier {
		e := eLeaf(before.Add(10*time.Second), 5, "0", "0")
		e.Seen = append(e.Seen, Seen{Side: Response, Leaf: Leaf{Path: "body.id", Type: String, Value: rec}, Live: live})
		return e
	}
	// An answer's own clock stamp (its Date header) is no place a value came
	// from, as the word rule reads it (Seen.Stamp).
	stamp := eLeaf(before.Add(10*time.Second), 5, "0", "0")
	stamp.Seen = append(stamp.Seen, Seen{Side: Response, Leaf: Leaf{Path: "header.Date", Type: String, Value: "Fri, 23 Oct 2026 09:01:02 GMT"}, Live: "Fri, 23 Oct 2026 09:01:02 GMT", Stamp: true})
	for _, row := range []struct {
		name      string
		raw       func(dc *DriftCache) Raw
		served    []Earlier
		rec, live string
		want      string // "" noise
	}{
		{"F's id stands in what F gave at an own place, the recording gave another there", func(dc *DriftCache) Raw {
			return dc.RawOf(receiveOf("mock-32", idC, before.Add(5*time.Second)), true).ServedAs(4).ServedLive(dc.RawOf(receiveOf("mock-32", idB, before.Add(5*time.Second)), true))
		}, nil, idA, idB, "stands in mock-32"},
		{"the recorded id stands only in what F gave at an own place", func(dc *DriftCache) Raw {
			return dc.RawOf(receiveOf("mock-32", idC, before.Add(5*time.Second)), true).ServedAs(4).ServedLive(dc.RawOf(receiveOf("mock-32", idB, before.Add(5*time.Second)), true))
		}, nil, idB, idA, ""},
		{"F's id stands at a leaf F served", nil, []Earlier{leaf(idB, idB)}, idA, idB, "stands in mock-8 at body.id"},
		{"F's id holds a word only an answer's own date stamp holds", nil, []Earlier{stamp}, chance, fresh + "-Fri", ""},
	} {
		dc := clockedDrifts()
		raw := []Raw{dc.RawOf(receiveOf("mock-31", idA, before), true).ServedAs(3).ServedAsRecorded().OfAnotherTest()}
		if row.raw != nil {
			raw = append(raw, row.raw(dc))
		}
		ln := ClassifyFollowing(dc, []string{p}, strLeaf(p, row.live), strLeaf(p, row.rec), r1Insert, row.served, raw, nil, nil, nil)[0]
		switch {
		case row.want == "" && ln.Kind != KindNoise:
			t.Errorf("%s: %s (%s); want noise", row.name, ln.Kind, ln.Why)
		case row.want != "" && (ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, lacks) || !strings.Contains(ln.Why, row.want)):
			t.Errorf("FALSE GREEN? %s: %s (%s); want it refused: %s", row.name, ln.Kind, ln.Why, row.want)
		}
	}
}
