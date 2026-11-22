package http

import (
	"maps"
	"net/url"
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
	ordHex16 := `^ord_` + digitCorePattern("0-9a-f", 16, 12) + `$`
	cusAlnum14 := `^cus_` + digitCorePattern("0-9A-Za-z", 14, 12) + `$`
	for _, tc := range []struct {
		name, rec, live string
		differs         []string
		entries         []string
	}{
		{"a uuid query id", "/orders?id=" + queryUUID1 + "&keploy-run=1", "/orders?id=" + queryUUID2 + "&keploy-run=2", []string{"url.query.id"}, []string{uuid}},
		{"a 14-digit query id", "/orders?id=12345678901234", "/orders?id=98765432109876", []string{"url.query.id"}, []string{`^[0-9]{10,}$`}},
		{"a path id and a query uuid", "/users/1?v=" + queryUUID1, "/users/3?v=" + queryUUID2, []string{"url.path.1", "url.query.v"}, []string{`^/users/[0-9]+$`, uuid}},
		{"a token id beside a page that is the same", "/orders?page=2&ref=ord_8ab071aa4d5a130a", "/orders?page=2&ref=ord_dcce109751713323", []string{"url.query.ref"}, []string{ordHex16}},
		{"a Stripe-like customer query id", "/customers?id=cus_NffrFeUfNV2Hib", "/customers?id=cus_Qa1b2C3d4E5f6G", []string{"url.query.id"}, []string{cusAlnum14}},
		{"two uuid ids, one entry", "/orders?a=" + queryUUID1 + "&b=" + queryUUID1, "/orders?a=" + queryUUID2 + "&b=" + queryUUID2, []string{"url.query.a", "url.query.b"}, []string{uuid}},
		{"a repeated parameter, one member differs", "/orders?tag=a&tag=" + queryUUID1, "/orders?tag=" + queryUUID2 + "&tag=a", []string{"url.query.tag"}, []string{uuid}},
		{"a uuid query id beside a Keploy parameter in capitals", "/orders?id=" + queryUUID1 + "&Keploy-Run=1", "/orders?id=" + queryUUID2 + "&Keploy-Run=2", []string{"url.query.id"}, []string{uuid}},
		{"a repeated parameter, a long number the same in both", "/orders?tag=12345678901234&tag=" + queryUUID1, "/orders?tag=" + queryUUID2 + "&tag=12345678901234", []string{"url.query.tag"}, []string{uuid}},
		{"a repeated parameter of a uuid and a token", "/orders?tag=ord_8ab071aa4d5a130a&tag=" + queryUUID1, "/orders?tag=" + queryUUID2 + "&tag=ord_dcce109751713323", []string{"url.query.tag"}, []string{uuid, ordHex16}},
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
			if query := slices.ContainsFunc(tc.differs, func(d string) bool { return strings.HasPrefix(d, "url.query.") }); query != strings.Contains(next, "An entry for a query value masks every query value that matches its pattern, in every call.") {
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
// no shorter digit run than the class counts, no plain English words, and no
// snake_case enums. A token's entry confines to its common literal prefix and
// suffix (trimmed to separator boundaries) around a narrow id-shaped varying
// core; any token holding "/" or lacking a narrow id-shaped core returns ""
// so the next step names the record step alone.
func TestQueryValueEntryMasksAWholeValueOfItsShape(t *testing.T) {
	uuidCore := strings.TrimSuffix(strings.TrimPrefix(reSegUUID.String(), "^"), "$")
	for _, tc := range []struct {
		class   string
		members []string
		want    string
		masks   []string
		keeps   []string
	}{
		{"digits", []string{"12345678901234", "98765432109876"}, `^[0-9]{10,}$`, []string{"1234567890"}, []string{"/orders", "2", "123456789", "/12345678901234", "lovelaceadacountess"}},
		{"uuid", []string{queryUUID1, queryUUID2}, reSegUUID.String(), []string{strings.ToUpper(queryUUID1)}, []string{"/orders/" + queryUUID1, queryUUID1 + "x", "lovelaceadacountess"}},
		{"hex", []string{"8ab071aa4d5a130a", "dcce109751713323"}, reSegLongHex.String(), []string{"507f1f77bcf86cd799439011"}, []string{"/8ab071aa4d5a130a", "8ab071aa4d5a130", "lovelaceadacountess", "awaiting_shipment"}},
		{"token", []string{"ord_8ab071aa4d5a130a", "ord_dcce109751713323"}, `^ord_` + digitCorePattern("0-9a-f", 16, 12) + `$`, []string{"ord_0011223344556677", "ord_0aaaaaaaaaaaaaaa", "ord_aaaaaaaaaaaaaaa0"}, []string{"/orders/ord_8ab071aa4d5a130a", "/api/v1/invoices/latest", "lovelaceadacountess", "awaiting_shipment", "1234567890123456", "ord_short", "ord_12345678901", "ord_zzzzzzzzzzzzzzzz", "ord_deadbeefcafebabe", "ord_acceededfacade"}},
		{"token", []string{"ord_8ab071aa4d5a130a", "ord_8cce10975171332a"}, `^ord_` + digitCorePattern("0-9a-f", 16, 12) + `$`, []string{"ord_9fff000011112222"}, []string{"lovelaceadacountess", "awaiting_shipment", "ord_deadbeefcafebabe"}},
		{"token", []string{"ord_8AB071AA4D5A130A", "ord_DCCE109751713323"}, `^ord_` + digitCorePattern("0-9A-F", 16, 12) + `$`, []string{"ord_0011223344556677"}, []string{"ord_8ab071aa4d5a130a", "ord_DEADBEEFCAFEBABE", "lovelaceadacountess"}},
		{"token", []string{"ord_8ab071AA4d5a130a", "ord_dcce109751713323"}, `^ord_` + digitCorePattern("0-9a-fA-F", 16, 12) + `$`, []string{"ord_0011223344556677", "ord_ABCDEF0123456789"}, []string{"lovelaceadacountess", "ord_deadBEEFcafeBABE"}},
		{"token", []string{"cus_NffrFeUfNV2Hib", "cus_Qa1b2C3d4E5f6G"}, `^cus_` + digitCorePattern("0-9A-Za-z", 14, 12) + `$`, []string{"cus_Zz99Yy88Xx77Ww", "cus_0abcdefghijklm", "cus_abcdefghijklm0"}, []string{"/customers/cus_NffrFeUfNV2Hib", "/api/v1/invoices/latest", "lovelaceadacountess", "awaiting_shipment", "cancelled_by_customer", "1234567890123456", "cus_awaiting_ord", "cus_abcdefghijklmn", "cus_12345678901"}},
		{"token", []string{"cus_NffrFeUfNV2Hib", "cus_N9abCdEfGh1234"}, `^cus_` + digitCorePattern("0-9A-Za-z", 14, 12) + `$`, []string{"cus_Zz99Yy88Xx77Ww"}, []string{"lovelaceadacountess", "awaiting_shipment", "cus_abcdefghijklmn"}},
		{"token", []string{"tok_a1b2c3d4e5f6g7h8", "tok_z9y8x7w6v5u4t3s2"}, `^tok_` + digitCorePattern("0-9a-z", 16, 12) + `$`, []string{"tok_m5n6o7p8q9r0s1t2"}, []string{"lovelaceadacountess", "awaiting_shipment", "tok_abcdefghijklmnop"}},
		{"token", []string{"ulid_01ARZ3NDEKTSV4RR", "ulid_01BRZ9XYZKTSV4QQ"}, `^ulid_` + digitCorePattern("0-9A-Z", 16, 11) + `$`, []string{"ulid_01CRZ8ABCDE12345"}, []string{"lovelaceadacountess", "awaiting_shipment", "ulid_ABCDEFGHIJKLMNOP"}},
		{"token", []string{"v1.2.3-build.20261122abcdef", "v1.2.3-build.20261123fedcba"}, `^v1\.2\.3-build\.` + digitCorePattern("0-9a-f", 14, 8) + `$`, []string{"v1.2.3-build.20261124000000"}, []string{"v1.2.3-build.deadbeefcafeba", "v1.2.3-build.1234567"}},
		{"token", []string{"author:alice2024x", "author:bobby1999y"}, `^author:` + digitCorePattern("0-9a-z", 10, 9) + `$`, []string{"author:carol2025z"}, []string{"author:christophe", "author:alexandria", "author:alice202"}},
		{"token", []string{"8ab071aa4d5a130a.dat", "dcce109751713323.dat"}, `^` + digitCorePattern("0-9a-f", 16, 12) + `\.dat$`, []string{"0011223344556677.dat"}, []string{"deadbeefcafebabe.dat", "12345678901.dat"}},
		{"token", []string{"ord_12345678901234", "ord_98765432109876"}, `^ord_[0-9]{14}$`, []string{"ord_11112222333344"}, []string{"12345678901234", "ord_abcdefghijklmm"}},
		{"token", []string{"ord_123456789012", "ord_9876543210987"}, `^ord_[0-9]{12,}$`, []string{"ord_111122223333444"}, []string{"ord_12345678901", "123456789012"}},
		{"token", []string{"created-after-20261122", "created-after-20261123"}, `^created-after-[0-9]{8}$`, []string{"created-after-20261201"}, []string{"created-after-yesterday", "created-after-12345"}},
		{"token", []string{"user+1700000000@example.com", "user+1800000000@example.com"}, `^user\+[0-9]{10}@example\.com$`, []string{"user+1900000000@example.com"}, []string{"user+christophe@example.com", "christophe@example.com"}},
		{"token", []string{queryUUID1 + ".txt", queryUUID2 + ".txt"}, `^` + uuidCore + `\.txt$`, []string{strings.ToUpper(queryUUID1) + ".txt"}, []string{"/files/" + queryUUID1 + ".txt", "lovelaceadacountess.txt"}},
		{"token", []string{"item-" + queryUUID1 + ".json", "item-" + queryUUID2 + ".json"}, `^item-` + uuidCore + `\.json$`, []string{"item-" + queryUUID1 + ".json"}, []string{"/item-" + queryUUID1 + ".json", "item-lovelace.json"}},
		// Unconfinable tokens: must return "" so the next step names the record step alone.
		{"token", []string{"ab12cd34/ef56gh78", "zz99yy88/xx77ww66"}, "", nil, nil},
		{"token", []string{"cur_ab12cd34/ef56gh78", "cur_zz99yy88/xx77ww66"}, "", nil, nil},
		{"token", []string{"acme/" + queryUUID1, "acme/" + queryUUID2}, "", nil, nil},
		{"token", []string{"/orders/8ab071aa4d5a130a", "/orders/dcce109751713323"}, "", nil, nil},
		{"token", []string{"a1-b2.c3~d4e5f6g7h8", "z9-y8.x7~w6v5u4t3s2"}, "", nil, nil},
		{"token", []string{"AbCdEfGh12345678", "ZyXwVuTs87654321"}, "", nil, nil},
		{"token", []string{"ééééééééa1", "ééééééééb2", queryUUID1}, "", nil, nil},
		{"token", []string{"v1_lovelaceadacountess", "v1_turingalanmathematician"}, "", nil, nil},
		{"token", []string{"v1_deadbeefcafebabe", "v1_abcdefabcdefabcd"}, "", nil, nil},
		{"token", []string{"cus_awaiting_order_1", "cus_cancelled_order_2"}, "", nil, nil},
		{"token", []string{"long_prefix_name_a1", "long_prefix_name_b2"}, "", nil, nil},
		{"token", []string{"long_prefix_name_g1", "long_prefix_name_z2"}, "", nil, nil},
		{"token", []string{"section-introduction-2", "section-introduction-3"}, "", nil, nil},
		{"token", []string{"2026-11-22T10:15:00.123Z", "2026-11-22T10:15:00.456Z"}, "", nil, nil},
		{"token", []string{"alice2024x@example.com", "bobby1999y@example.com"}, "", nil, nil},
		{"token", []string{"alice2024@example.com", "bob77abcdefgh@example.com"}, "", nil, nil},
		{"token", []string{"report2024abc.pdf", "invoice99xyz1.pdf"}, "", nil, nil},
		{"token", []string{"ab12cd34ef.svc.local", "zz98yy76xx.svc.local"}, "", nil, nil},
		{"token", []string{"2026-a1b2c3d4e5f6", "2026-z9y8x7w6v5u4"}, "", nil, nil},
		{"token", []string{"ord_8ab071aa4d5a130a"}, "", nil, nil},
	} {
		got := queryValueEntry(tc.class, tc.members)
		if got != tc.want {
			t.Errorf("queryValueEntry(%s, %q) = %q, want %q", tc.class, tc.members, got, tc.want)
			continue
		}
		if got == "" {
			continue
		}
		re := regexp.MustCompile(got)
		if re.MatchString("/") || re.MatchString("/api/v1/invoices/latest") {
			t.Errorf("%s matched a path", got)
		}
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

// A token query id containing "/" (such as a %2F-decoded cursor) cannot be
// confined to a safe url-noise entry without risking path masking: the report
// names url.query.<name> as other_id and its next step names the record step
// alone, with no test.globalNoise.global.url entry (including when paired with
// a path id). An unrecorded path is never answered 200 in either mode.
func TestTokenQueryIDWithSlashSuggestsRecordAloneAndNeverMasksPaths(t *testing.T) {
	for _, tc := range []struct {
		name, rec, live string
		differs         []string
	}{
		{"slash cursor alone", "/orders?cursor=ab12cd34%2Fef56gh78", "/orders?cursor=zz99yy88%2Fxx77ww66", []string{"url.query.cursor"}},
		{"slash cursor beside path id", "/users/1?cursor=ab12cd34%2Fef56gh78", "/users/3?cursor=zz99yy88%2Fxx77ww66", []string{"url.path.1", "url.query.cursor"}},
		{"opaque bare token", "/orders?id=ab12cd34ef56gh78ij", "/orders?id=zz99yy88xx77ww66vv", []string{"url.query.id"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setProven(t, false)
			h := newHTTP()
			db := &mockMemDb{updateUnFilteredReturn: true, mocks: []*models.Mock{
				recordedAs(t, h, "mock-a", 1, getTarget(tc.rec), jsonAnswer(200, `{"order":"A"}`), nil),
			}}
			got := serveReported(t, h, db, getTarget(tc.live))
			a := got.answers()
			if len(a) != 1 || a[0].Answer != models.AnswerOtherID || !slices.Equal(a[0].Differs, tc.differs) {
				t.Fatalf("got %d answers=%+v; want other_id differing at %q", got.status, a, tc.differs)
			}
			if !strings.Contains(a[0].NextSteps, "keploy mock record") {
				t.Errorf("next step %q does not name the record step", a[0].NextSteps)
			}
			if strings.Contains(a[0].NextSteps, "test.globalNoise.global.url:") {
				t.Errorf("next step %q emitted a url-noise entry for an unconfinable token", a[0].NextSteps)
			}
		})
	}
}

// A confined token query-id entry (for prefixed hex ord_ or Stripe-like cus_)
// makes another id of that shape the call's own in both modes, while never
// masking plain English words, snake_case enums, letter-only prefixed values,
// bare digits, or unrecorded paths.
func TestConfinedTokenQueryIDEntryNeverMasksWordsEnumsOrPaths(t *testing.T) {
	for _, tc := range []struct {
		name, rec, live, third string
		letterRec, letterLive  string
	}{
		{"prefixed hex order ref", "/orders?ref=ord_8ab071aa4d5a130a", "/orders?ref=ord_dcce109751713323", "/orders?ref=ord_0011223344556677", "/orders?ref=ord_deadbeefcafebabe", "/orders?ref=ord_acceededfacade"},
		{"Stripe-like customer id", "/customers?id=cus_NffrFeUfNV2Hib", "/customers?id=cus_Qa1b2C3d4E5f6G", "/customers?id=cus_Zz99Yy88Xx77Ww", "/customers?id=cus_abcdefghijklmn", "/customers?id=cus_nopqrstuvwxyza"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setProven(t, false)
			h := newHTTP()
			dbA := func(t *testing.T, h *HTTP) *mockMemDb {
				return &mockMemDb{updateUnFilteredReturn: true, mocks: []*models.Mock{
					recordedAs(t, h, "mock-a", 1, getTarget(tc.rec), jsonAnswer(200, `{"ok":true}`), nil),
				}}
			}
			a := serveReported(t, h, dbA(t, h), getTarget(tc.live)).answers()
			if len(a) != 1 || a[0].Answer != models.AnswerOtherID {
				t.Fatalf("step1: %+v", a)
			}
			noise := keployYmlGlobalNoise(t, a[0].NextSteps, "url")["url"]
			opts := models.OutgoingOptions{NoiseConfig: map[string]map[string][]string{"url": noise}}

			for _, proven := range []bool{false, true} {
				setProven(t, proven)
				// A third id of the exact same shape is answered as its own.
				h := newHTTP()
				own := serveReportedWith(t, h, dbA(t, h), opts, getTarget(tc.third))
				if own.status != 200 || len(own.reports) != 0 {
					t.Errorf("proven=%v third id %s: got %d reports=%d; want 200 with no report", proven, tc.third, own.status, len(own.reports))
				}
				// Plain English words, snake_case enums, letter-only prefixed values, and unrecorded paths must NOT match.
				for _, pair := range []struct{ rec, live string }{
					{"/search?q=lovelaceadacountess", "/search?q=turingalanmathematician"},
					{"/orders?status=awaiting_shipment", "/orders?status=cancelled_by_customer"},
					{"/api/v1/users/profile", "/api/v1/invoices/latest"},
					{tc.letterRec, tc.letterLive},
				} {
					h := newHTTP()
					dbOther := &mockMemDb{updateUnFilteredReturn: true, mocks: []*models.Mock{
						recordedAs(t, h, "mock-other", 1, getTarget(pair.rec), jsonAnswer(200, `{"hit":"other"}`), nil),
					}}
					got := serveReportedWith(t, h, dbOther, opts, getTarget(pair.live))
					if got.status == 200 {
						t.Errorf("proven=%v noise %v falsely matched %s against %s (%d %q reports=%d)", proven, noise, pair.live, pair.rec, got.status, got.body, len(got.reports))
					}
				}
			}
		})
	}
}

// digitCorePattern must match every string of length >= minLen containing a
// digit at any position 0..minLen-1, while never matching a letter-only
// string or a string shorter than minMatch.
func TestDigitCorePatternCoversEveryDigitPositionAndRejectsLetterOnly(t *testing.T) {
	for _, tc := range []struct {
		prefix, charClass string
		minLen, minMatch  int
	}{
		{"ord_", "0-9a-f", 16, 12},
		{"cus_", "0-9A-Za-z", 14, 12},
		{"author:", "0-9a-z", 10, 9},
		{"v1.2.3-build.", "0-9a-f", 14, 8},
		{"id_", "0-9a-z", 13, 13},
	} {
		re := regexp.MustCompile("^" + regexp.QuoteMeta(tc.prefix) + digitCorePattern(tc.charClass, tc.minLen, tc.minMatch) + "$")
		for p := 0; p < tc.minLen; p++ {
			b := strings.Repeat("a", p) + "5" + strings.Repeat("a", tc.minLen-1-p)
			s := tc.prefix + b
			if !re.MatchString(s) {
				t.Errorf("%s digit at %d: %q did not match %s", tc.prefix, p, s, re)
			}
			if cls := queryValueTypeClass(s); cls != "token" {
				t.Errorf("%q classified as %q, want token", s, cls)
			}
		}
		letterOnly := tc.prefix + strings.Repeat("a", tc.minLen)
		if re.MatchString(letterOnly) {
			t.Errorf("%s matched letter-only %q", re, letterOnly)
		}
		tooShort := tc.prefix + "5" + strings.Repeat("a", tc.minMatch-2)
		if re.MatchString(tooShort) {
			t.Errorf("%s matched too-short %q", re, tooShort)
		}
	}
}

// Following the next step of an id-shaped pair (emails, file names, field
// labels, hostnames) must never silently match a non-id word pair in either
// KEPLOY_PROVEN mode: either no url-noise entry is emitted, or the emitted
// digit-requiring entry leaves the word pair a 502.
func TestTokenQueryIDEntryNeverMasksNonIDValuesInEitherMode(t *testing.T) {
	for _, tc := range []struct {
		name, idRec, idLive, wordRec, wordLive string
	}{
		{"email fixed length", "alice2024x@example.com", "bobby1999y@example.com", "christophe@example.com", "alexandria@example.com"},
		{"email varying length", "alice2024@example.com", "bob77abcdefgh@example.com", "christopher@example.com", "margaretha@example.com"},
		{"file name", "report2024abc.pdf", "invoice99xyz1.pdf", "annualsummary.pdf", "quarterlyplan.pdf"},
		{"field label", "author:alice2024x", "author:bobby1999y", "author:christophe", "author:alexandria"},
		{"hostname", "ab12cd34ef.svc.local", "zz98yy76xx.svc.local", "frontendxy.svc.local", "backendabc.svc.local"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setProven(t, false)
			h := newHTTP()
			db := &mockMemDb{updateUnFilteredReturn: true, mocks: []*models.Mock{
				recordedAs(t, h, "m", 1, getTarget("/users?v="+url.QueryEscape(tc.idRec)), jsonAnswer(200, `{}`), nil),
			}}
			a := serveReported(t, h, db, getTarget("/users?v="+url.QueryEscape(tc.idLive))).answers()
			if len(a) != 1 || a[0].Answer != models.AnswerOtherID {
				t.Fatalf("step1: %+v", a)
			}
			var noise map[string][]string
			if strings.Contains(a[0].NextSteps, "test.globalNoise.global.url: ") {
				noise = keployYmlGlobalNoise(t, a[0].NextSteps, "url")["url"]
			}
			for _, proven := range []bool{false, true} {
				setProven(t, proven)
				h := newHTTP()
				dbW := &mockMemDb{updateUnFilteredReturn: true, mocks: []*models.Mock{
					recordedAs(t, h, "mock-w", 1, getTarget("/users?v="+url.QueryEscape(tc.wordRec)), jsonAnswer(200, `{"who":"`+tc.wordRec+`"}`), nil),
				}}
				got := serveReportedWith(t, h, dbW, models.OutgoingOptions{NoiseConfig: map[string]map[string][]string{"url": noise}}, getTarget("/users?v="+url.QueryEscape(tc.wordLive)))
				if got.status == 200 {
					t.Errorf("proven=%v: %s falsely answered with %s under entry %v (%d %q reports=%d)", proven, tc.wordLive, tc.wordRec, noise, got.status, got.body, len(got.reports))
				}
			}
		})
	}
}
