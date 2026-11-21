package http

import (
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

func getTarget(target string) string {
	return "GET " + target + " HTTP/1.1\r\nHost: users.local\r\n\r\n"
}

const (
	queryUUID1 = "550e8400-e29b-41d4-a716-446655440000"
	queryUUID2 = "6ba7b810-9dad-11d1-80b4-00c04fd430c8"
)

// A call whose URL differs from the one recording's only at query values
// that look like ids (an id the app mints into ?id=, beside a path id or
// alone, repeated, beside keploy's own parameters and a parameter that is
// the same) is reported other_id with the switch off, naming each query
// parameter that differs (url.query.<name>) beside each path segment, and
// a next step whose url-noise entries, read as keploy reads keploy.yml
// (loader.Read, then Unmarshal), mask those values: once keploy.yml holds
// them, the call is served the recording as its own with no report, in
// both modes. Under the switch the call with no such entry is a miss.
// Before, the report named no place (differs []) and its next step's one
// entry ({"^/orders$": []}) masked no query value: the call stayed
// other_id on every run.
func TestAnotherIDInTheQueryIsNamedAndItsNoiseMakesTheRecordingItsOwn(t *testing.T) {
	uuid := reSegUUID.String()
	for _, tc := range []struct {
		name, rec, live string
		differs         []string
		entries         []string
	}{
		{"a uuid query id", "/orders?id=" + queryUUID1 + "&keploy-run=1", "/orders?id=" + queryUUID2 + "&keploy-run=2", []string{"url.query.id"}, []string{uuid}},
		{"a 14-digit query id", "/orders?id=12345678901234", "/orders?id=98765432109876", []string{"url.query.id"}, []string{`^[0-9]{10,}$`}},
		{"a path id and a query uuid", "/users/1?v=" + queryUUID1, "/users/3?v=" + queryUUID2, []string{"url.path.1", "url.query.v"}, []string{`^/users/[0-9]+$`, uuid}},
		{"a token id beside a page that is the same", "/orders?page=2&ref=ord_8ab071aa4d5a130a", "/orders?page=2&ref=ord_dcce109751713323", []string{"url.query.ref"}, []string{`^[0-9A-Za-z\_]{16,}$`}},
		{"two uuid ids, one entry", "/orders?a=" + queryUUID1 + "&b=" + queryUUID1, "/orders?a=" + queryUUID2 + "&b=" + queryUUID2, []string{"url.query.a", "url.query.b"}, []string{uuid}},
		{"a repeated parameter, one member differs", "/orders?tag=a&tag=" + queryUUID1, "/orders?tag=" + queryUUID2 + "&tag=a", []string{"url.query.tag"}, []string{uuid}},
		{"a repeated parameter of a uuid and a token", "/orders?tag=ord_8ab071aa4d5a130a&tag=" + queryUUID1, "/orders?tag=" + queryUUID2 + "&tag=ord_dcce109751713323", []string{"url.query.tag"}, []string{uuid, `^[0-9A-Za-z\_]{16,}$`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := func(t *testing.T, h *HTTP) *mockMemDb {
				return &mockMemDb{updateUnFilteredReturn: true, mocks: []*models.Mock{
					recordedAs(t, h, "mock-a", 1, getTarget(tc.rec), jsonAnswer(200, `{"order":"A"}`), nil),
				}}
			}
			setProven(t, false)
			h := newHTTP()
			got := serveReported(t, h, db(t, h), getTarget(tc.live))
			a := got.answers()
			if got.status != 200 || got.body != `{"order":"A"}` || len(a) != 1 || a[0].Answer != models.AnswerOtherID {
				t.Fatalf("%s got %d %q with answer reports %+v, want the recording served and reported %q", tc.live, got.status, got.body, a, models.AnswerOtherID)
			}
			if !slices.Equal(a[0].Differs, tc.differs) || !strings.HasSuffix(a[0].Diff, "only at values that look like ids ("+strings.Join(tc.differs, ", ")+")") {
				t.Fatalf("the report names differs %q and %q, want %q", a[0].Differs, a[0].Diff, tc.differs)
			}
			next := a[0].NextSteps
			if !strings.Contains(next, "If the call is a new input, record it (keploy mock record)") || !strings.Contains(next, "If the app makes a new id there on every run") {
				t.Errorf("the next step %q does not name both causes", next)
			}
			if query := slices.ContainsFunc(tc.differs, func(d string) bool { return strings.HasPrefix(d, "url.query.") }); query != strings.Contains(next, "An entry for a query value masks every query value of its shape, in every call.") {
				t.Errorf("the next step %q says what a query value's entry masks: %v, want %v", next, !query, query)
			}
			noise := keployYmlGlobalNoise(t, next, "url")["url"]
			if keys := slices.Sorted(maps.Keys(noise)); !slices.Equal(keys, slices.Sorted(slices.Values(tc.entries))) {
				t.Fatalf("keploy.yml's url noise, as the next step names it, reads %q, want %q", keys, tc.entries)
			}
			for _, proven := range []bool{false, true} {
				setProven(t, proven)
				h := newHTTP()
				own := serveReportedWith(t, h, db(t, h), models.OutgoingOptions{NoiseConfig: map[string]map[string][]string{"url": noise}}, getTarget(tc.live))
				if own.status != 200 || own.body != `{"order":"A"}` || len(own.reports) != 0 {
					t.Errorf("proven=%v: with keploy.yml's url %v, %s got %d %q with reports %+v, want the recording as its own and none", proven, noise, tc.live, own.status, own.body, own.reports)
				}
			}
			setProven(t, true)
			h = newHTTP()
			if miss := serveReported(t, h, db(t, h), getTarget(tc.live)); miss.status == 200 || len(miss.answers()) != 0 {
				t.Errorf("under the switch %s got %d %q with answer reports %+v, want a miss", tc.live, miss.status, miss.body, miss.answers())
			}
		})
	}
}

// queryValueEntry masks a whole query value of its class: the values that
// differed, and no path (each begins with "/"), no short integer (a page),
// no shorter digit run than the class counts. A long token's entry holds
// its letters and digits and each other character the values hold (ASCII
// punctuation escaped, any other character by its code point), as many as
// the shortest of them, and only the token values' characters.
func TestQueryValueEntryMasksAWholeValueOfItsShape(t *testing.T) {
	for _, tc := range []struct {
		class   string
		members []string
		want    string
		masks   []string
		keeps   []string
	}{
		{"digits", []string{"12345678901234", "98765432109876"}, `^[0-9]{10,}$`, []string{"1234567890"}, []string{"/orders", "2", "123456789", "/12345678901234"}},
		{"uuid", []string{queryUUID1, queryUUID2}, reSegUUID.String(), []string{strings.ToUpper(queryUUID1)}, []string{"/orders/" + queryUUID1, queryUUID1 + "x"}},
		{"hex", []string{"8ab071aa4d5a130a", "dcce109751713323"}, reSegLongHex.String(), []string{"507f1f77bcf86cd799439011"}, []string{"/8ab071aa4d5a130a", "8ab071aa4d5a130"}},
		{"token", []string{"a1-b2.c3~d4e5f6g7h8", "z9-y8.x7~w6v5u4t3s2"}, `^[0-9A-Za-z\-\.\~]{16,}$`, nil, []string{"/a1-b2.c3~d4e5f6g7h8", "a1-b2.c3~d4e5"}},
		{"token", []string{"ééééééééa1", "ééééééééb2", queryUUID1}, `^[0-9A-Za-z\x{e9}]{10,}$`, nil, []string{"/ééééééééa1"}},
	} {
		got := queryValueEntry(tc.class, tc.members)
		if got != tc.want {
			t.Errorf("queryValueEntry(%s, %q) = %s, want %s", tc.class, tc.members, got, tc.want)
			continue
		}
		re := regexp.MustCompile(got)
		for _, v := range append(slices.Clone(tc.members[:2]), tc.masks...) {
			if !re.MatchString(v) {
				t.Errorf("%s does not mask %q", got, v)
			}
		}
		for _, v := range tc.keeps {
			if re.MatchString(v) {
				t.Errorf("%s masks %q", got, v)
			}
		}
	}
}
