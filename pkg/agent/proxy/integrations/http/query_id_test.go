package http

import (
	"maps"
	"regexp"
	"slices"
	"strconv"
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
// a next step whose url-noise entries, in the order differs names their
// places, read as keploy reads keploy.yml (loader.Read, then Unmarshal),
// mask those values. A path id alone gets no query sentence; a value that
// holds a comma, the same in both, is no difference (QueryParamsMatch reads
// it so); a query parameter named as a path place is read as a query
// parameter. Once keploy.yml holds
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
		{"a uuid query id beside a Keploy parameter in capitals", "/orders?id=" + queryUUID1 + "&Keploy-Run=1", "/orders?id=" + queryUUID2 + "&Keploy-Run=2", []string{"url.query.id"}, []string{uuid}},
		{"a repeated parameter, a long number the same in both", "/orders?tag=12345678901234&tag=" + queryUUID1, "/orders?tag=" + queryUUID2 + "&tag=12345678901234", []string{"url.query.tag"}, []string{uuid}},
		{"a repeated parameter of a uuid and a token", "/orders?tag=ord_8ab071aa4d5a130a&tag=" + queryUUID1, "/orders?tag=" + queryUUID2 + "&tag=ord_dcce109751713323", []string{"url.query.tag"}, []string{uuid, `^[0-9A-Za-z\_]{16,}$`}},
		{"a path id alone", "/users/1", "/users/3", []string{"url.path.1"}, []string{`^/users/[0-9]+$`}},
		{"a value that holds a comma, the same in both", "/orders?id=" + queryUUID1 + "&tag=a%2C%20b", "/orders?id=" + queryUUID2 + "&tag=a%2C%20b", []string{"url.query.id"}, []string{uuid}},
		{"a query parameter named as a path place", "/users/1?a=" + queryUUID1 + "&url.path.1=12345678901234", "/users/3?a=" + queryUUID2 + "&url.path.1=98765432109876", []string{"url.path.1", "url.query.a", "url.query.url.path.1"}, []string{`^/users/[0-9]+$`, uuid, `^[0-9]{10,}$`}},
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
			quoted := make([]string, len(tc.entries))
			for i, e := range tc.entries {
				quoted[i] = strconv.Quote(e)
			}
			if inOrder := "test.globalNoise.global.url: {" + strings.Join(quoted, ": [], ") + ": []}"; !strings.Contains(next, inOrder) {
				t.Errorf("the next step %q does not name the entries in the order differs names their places: %s", next, inOrder)
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

// A query value keploy.yml's url noise already masks is no place the call
// differs at: the report names only the query id the second pass let
// differ, as QueryParamsMatch compares the two with that noise masked, and
// its next step gives that id's entry alone. With both entries in
// keploy.yml the call is answered with the recording as its own in both
// modes; under the switch, with only the user's entry, it is a miss.
func TestAnotherIDInTheQueryIsNamedOutsideTheURLNoiseKeployYmlHolds(t *testing.T) {
	user := map[string][]string{"^t[0-9]+$": {}}
	rec, live := "/orders?at=t1&id="+queryUUID1, "/orders?at=t2&id="+queryUUID2
	db := func(t *testing.T, h *HTTP) *mockMemDb {
		return &mockMemDb{updateUnFilteredReturn: true, mocks: []*models.Mock{
			recordedAs(t, h, "mock-a", 1, getTarget(rec), jsonAnswer(200, `{"order":"A"}`), nil),
		}}
	}
	with := func(noise map[string][]string) models.OutgoingOptions {
		return models.OutgoingOptions{NoiseConfig: map[string]map[string][]string{"url": noise}}
	}
	setProven(t, false)
	h := newHTTP()
	got := serveReportedWith(t, h, db(t, h), with(user), getTarget(live))
	a := got.answers()
	if got.status != 200 || len(a) != 1 || a[0].Answer != models.AnswerOtherID {
		t.Fatalf("%s got %d with answer reports %+v, want the recording served and reported %q", live, got.status, a, models.AnswerOtherID)
	}
	if want := []string{"url.query.id"}; !slices.Equal(a[0].Differs, want) {
		t.Fatalf("the report names differs %q, want %q: url noise masks the other values", a[0].Differs, want)
	}
	entries := keployYmlGlobalNoise(t, a[0].NextSteps, "url")["url"]
	if keys := slices.Sorted(maps.Keys(entries)); !slices.Equal(keys, []string{reSegUUID.String()}) {
		t.Fatalf("the next step's url noise reads %q, want the uuid entry alone", keys)
	}
	maps.Copy(entries, user)
	for _, proven := range []bool{false, true} {
		setProven(t, proven)
		h := newHTTP()
		if own := serveReportedWith(t, h, db(t, h), with(entries), getTarget(live)); own.status != 200 || len(own.reports) != 0 {
			t.Errorf("proven=%v: with keploy.yml's url %v, %s got %d with reports %+v, want the recording as its own and none", proven, entries, live, own.status, own.reports)
		}
	}
	setProven(t, true)
	h = newHTTP()
	if miss := serveReportedWith(t, h, db(t, h), with(user), getTarget(live)); miss.status == 200 || len(miss.answers()) != 0 {
		t.Errorf("under the switch %s got %d with answer reports %+v, want a miss", live, miss.status, miss.answers())
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
		{"token", []string{"AbCdEfGh12345678", "ZyXwVuTs87654321"}, `^[0-9A-Za-z]{16,}$`, nil, []string{"/AbCdEfGh12345678", "AbCdEfGh1234567"}},
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
