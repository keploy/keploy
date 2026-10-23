package learn

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// A UUID's groups are no words of their own. Two random UUIDs share a group
// of four hex digits by chance (16 bits; a test holds hundreds of UUIDs), and
// such a group is no sign that one came from the other: a fresh UUID is
// noise, or an echo, though an earlier place held another UUID with one of
// its groups. A group counts where the place holds the whole UUID: a UUID
// picked from a listing, or a text that held it and did not drift with it,
// is still refused, whole or inside a longer value, and a payload that held
// it is still where it came from.
//
// The values are a learning round's: a suite id the application
// sends whole for the first time in a version's claim, and the earlier
// places that refused it, each holding another UUID with one of its groups
// ("4f97", "94ad" recorded, "a5ae" live).
func TestAUUIDsGroupIsNoWordOfItsOwn(t *testing.T) {
	at := time.Date(2026, 10, 20, 16, 1, 11, 945e6, time.UTC)
	leaf := func(path, value string) Leaf { return Leaf{Path: path, Noise: path, Type: String, Value: value} }
	sent := func(mock, path, rec, live string) []Earlier {
		return []Earlier{{Mock: mock, At: at.Add(-time.Second), Own: true, Seen: []Seen{
			{Side: Request, Leaf: leaf(path, rec), Live: live, Place: path},
		}}}
	}
	listed := func(ids ...string) []Earlier {
		e := Earlier{Mock: "mock-1", At: at.Add(-time.Second), Own: true}
		for i, id := range ids {
			p := "body.cursor.firstBatch[" + string(rune('0'+i)) + "]._id"
			e.Seen = append(e.Seen, Seen{Side: Response, Leaf: leaf(p, id), Live: id})
		}
		return []Earlier{e}
	}
	claim := func(dc *DriftCache, rec, live string, earlier []Earlier, raw []Raw, resp []Leaf) Learned {
		p := "body.cmd.query.entity_id"
		r := Shape{Leaves: map[string]Leaf{p: leaf(p, rec)}}
		l := Shape{Leaves: map[string]Leaf{p: leaf(p, live)}}
		return Classify(dc, []string{p}, l, r, at, earlier, raw, resp)[0]
	}
	// cached are payloads as a test's cache reads them (DriftCache.RawOf),
	// indexed by their words, rather than read in full.
	cached := func(dc *DriftCache, raw []Raw) []Raw {
		out := make([]Raw, len(raw))
		for i, x := range raw {
			out[i] = dc.RawOf(&models.Mock{Name: x.Mock, Spec: models.MockSpec{ReqTimestampMock: x.At, HTTPReq: &models.HTTPReq{Body: x.Text}}}, x.Served)
		}
		return out
	}
	echo := func(rec string) []Leaf { return []Leaf{leaf("body.value.entity_id", rec)} }
	const (
		clientRec, clientLive = "6b0f706a-4f97-4927-9e3e-594d93792910", "9aca9ed3-0b6c-4a83-934c-f13c18a959dd"
		reportRec, reportLive = "44b0e52a-2bf9-46b8-94ad-48a261acdcfa", "c7e2ca81-ce6e-45c4-84e7-aab6aa91d3b2"
		serverRec, serverLive = "6992d60a-e530-4ef5-96d7-0ae0916ede26", "96e35cbd-7be7-44cb-a5ae-09d16708f741"
	)
	for _, tc := range []struct {
		name      string
		rec, live string
		earlier   []Earlier
		raw       []Raw
		resp      []Leaf
		want      Kind
	}{
		{name: "a recorded group of another UUID an earlier call sent", rec: clientRec, live: clientLive, resp: echo(clientRec), want: KindEcho,
			earlier: sent("mock-776", "body.docs.msgs.0._id", "29cd7a36-4b77-4f97-a595-a49bc828075f", "e1d2c3b4-a5f6-4e7d-8c9b-0a1f2e3d4c5b")},
		{name: "a recorded group of another UUID that did not drift", rec: reportRec, live: reportLive, resp: echo(reportRec), want: KindEcho,
			earlier: sent("mock-3719", "body.docs.msgs.0._id", "2d2b53b1-e776-4df9-94ad-cff3d185883d", "2d2b53b1-e776-4df9-94ad-cff3d185883d")},
		{name: "a live group of another UUID an earlier header sent", rec: serverRec, live: serverLive, resp: echo(serverRec), want: KindEcho,
			earlier: sent("mock-2457", "header.x-keploy-test-suite-id", "7076ceea-4ab3-4f14-b185-f2335016c021", "6cfdf31e-a5ae-4d33-a630-2b48b5ae7805")},
		{name: "a group of another UUID in a payload", rec: reportRec, live: reportLive, resp: echo(reportRec), want: KindEcho,
			raw: []Raw{{Mock: "pg-0", At: at.Add(-2 * time.Second), Served: true, Text: "INSERT INTO runs (id) VALUES ('2d2b53b1-e776-4df9-94ad-cff3d185883d')"}}},
		{name: "a group of the UUID alone in a payload", rec: reportRec, live: reportLive, resp: echo(reportRec), want: KindEcho,
			raw: []Raw{{Mock: "pg-0", At: at.Add(-2 * time.Second), Served: true, Text: "INSERT INTO runs (ref) VALUES ('48a261acdcfa')"}}},
		{name: "a group of another UUID in its own answer", rec: clientRec, live: clientLive, want: KindNoise,
			resp: []Leaf{leaf("body.value.ref", "29cd7a36-4b77-4f97-a595-a49bc828075f")}},
		{name: "a group of another UUID, upper case", rec: clientRec, live: clientLive, resp: echo(clientRec), want: KindEcho,
			earlier: sent("mock-776", "body.docs.msgs.0._id", "29CD7A36-4B77-4F97-A595-A49BC828075F", "E1D2C3B4-A5F6-4E7D-8C9B-0A1F2E3D4C5B")},

		// What a group is no word of its own for: the place holds the
		// whole UUID, and did not drift with it.
		{name: "the live UUID picked from a listing", rec: clientRec, live: clientLive, resp: echo(clientRec), want: KindUnaccounted,
			earlier: listed("29cd7a36-4b77-4f97-a595-a49bc828075f", clientLive)},
		{name: "the recorded UUID picked from a listing", rec: clientRec, live: clientLive, resp: echo(clientRec), want: KindUnaccounted,
			earlier: listed(clientRec)},
		{name: "the live UUID inside a longer id of a listing", rec: clientRec, live: clientLive, resp: echo(clientRec), want: KindUnaccounted,
			earlier: listed("suite_" + clientLive)},
		{name: "a prefixed UUID, the bare UUID picked from a listing", rec: "user-" + clientRec, live: "user-" + clientLive, want: KindUnaccounted,
			earlier: listed(clientLive)},
		{name: "the recorded UUID inside a longer id an earlier call sent, which did not drift", rec: clientRec, live: clientLive, resp: echo(clientRec), want: KindUnaccounted,
			earlier: sent("mock-886", "body.docs.msgs.0.ref", "run_"+clientRec, "run_"+clientRec)},
		{name: "the recorded UUID in a text that did not drift", rec: clientRec, live: clientLive, resp: echo(clientRec), want: KindUnaccounted,
			earlier: sent("mock-886", "body.docs.msgs.0.note", "Suite "+clientRec+" seeded", "Suite "+clientRec+" seeded")},
		{name: "the live UUID inside a longer id in a payload", rec: reportRec, live: reportLive, resp: echo(reportRec), want: KindUnaccounted,
			raw: []Raw{{Mock: "pg-0", At: at.Add(-2 * time.Second), Served: true, Text: "INSERT INTO runs (id) VALUES ('run_" + reportLive + "')"}}},
		{name: "the recorded UUID inside a longer id in a payload", rec: reportRec, live: reportLive, resp: echo(reportRec), want: KindVariable,
			raw: []Raw{{Mock: "pg-0", At: at.Add(-2 * time.Second), Served: true, Text: "INSERT INTO runs (id) VALUES ('run_" + reportRec + "')"}}},
		{name: "a prefixed UUID whose bare UUID its own answer holds", rec: "user-" + clientRec, live: "user-" + clientLive, want: KindUnaccounted,
			resp: []Leaf{leaf("body.value.ref", clientRec)}},
		// A group that is a word of the value outside a UUID too is a word
		// of its own there.
		{name: "a group the value holds outside its UUID too", rec: "4f97 " + clientRec, live: "a1b2 " + clientLive, want: KindUnaccounted,
			earlier: sent("mock-776", "body.docs.msgs.0._id", "29cd7a36-4b77-4f97-a595-a49bc828075f", "e1d2c3b4-a5f6-4e7d-8c9b-0a1f2e3d4c5b")},
	} {
		got := claim(nil, tc.rec, tc.live, tc.earlier, tc.raw, tc.resp)
		if got.Kind != tc.want {
			t.Errorf("%s: %s (%s), want %s", tc.name, got.Kind, got.Why, tc.want)
		}
		if len(tc.raw) > 0 {
			dc := NewDriftCache()
			if got := claim(dc, tc.rec, tc.live, tc.earlier, cached(dc, tc.raw), tc.resp); got.Kind != tc.want {
				t.Errorf("%s, payloads read by the cache: %s (%s), want %s", tc.name, got.Kind, got.Why, tc.want)
			}
		}
	}
}
