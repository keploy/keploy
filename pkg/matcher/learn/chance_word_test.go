package learn

import (
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
					if ln.Kind != KindNoise {
						t.Errorf("%s (set %v): %s (%s); want noise", row.name, viaSet, ln.Kind, ln.Why)
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
