package mocknoise

import (
	"reflect"
	"sort"
	"testing"
)

func TestIsMintedUUID(t *testing.T) {
	for v, want := range map[string]bool{
		"0f8fad5b-d9cb-469f-a165-70867728950e": true,  // v4
		"0F8FAD5B-D9CB-469F-A165-70867728950E": true,  // case does not matter
		"c232ab00-9414-11ec-b3c8-9f6bdeced846": true,  // v1
		"1ec9414c-232a-6b00-b3c8-9f6bdeced846": true,  // v6
		"017f22e2-79b0-7cc3-98c4-dc0c0c07398f": true,  // v7
		"5df41881-3aed-3515-88a7-2f4a814cf09e": false, // v3: name-based
		"2ed6657d-e927-568b-95e1-2665a8aea6a2": false, // v5: name-based
		"00000000-0000-0000-0000-000000000000": false, // nil
		"ffffffff-ffff-ffff-ffff-ffffffffffff": false, // max
		"0f8fad5b-d9cb-469f-c165-70867728950e": false, // another variant
		"0f8fad5bd9cb469fa16570867728950e":     false, // a hex digest looks like this too
		"gpt-4o-mini-2024-07-18":               false,
		"orders-processing-queue-1":            false,
		"":                                     false,
	} {
		if got := IsMintedUUID(v); got != want {
			t.Errorf("IsMintedUUID(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestAlignStrings(t *testing.T) {
	type result struct{ pairs, alone []string }
	align := func(recURL, recBody, liveURL, liveBody string) result {
		var r result
		AlignStrings(recURL, recBody, liveURL, liveBody,
			func(rec, live string) { r.pairs = append(r.pairs, rec+"="+live) },
			func(rec string) { r.alone = append(r.alone, rec) })
		sort.Strings(r.pairs)
		sort.Strings(r.alone)
		return r
	}
	for name, c := range map[string]struct {
		recURL, recBody, liveURL, liveBody string
		want                               result
	}{
		"path segments and query values": {
			"http://dep/items/A/parts/B?ref=C&tag=x&tag=y", "", "/items/1/parts/2?ref=3&tag=x", "",
			result{pairs: []string{"A=1", "B=2", "C=3", "items=items", "parts=parts"}, alone: []string{"x", "y"}},
		},
		"paths of another length": {
			"http://dep/items/A", "", "/items/1/extra", "",
			result{alone: []string{"A", "items"}},
		},
		"objects by key, arrays by position": {
			"http://dep/", `{"id":"A","all":["A","B"],"n":1,"sub":{"k":"C"}}`, "/", `{"sub":{"k":"3"},"all":["1","2"],"id":"1","n":2}`,
			result{pairs: []string{"=", "A=1", "A=1", "B=2", "C=3"}},
		},
		"an array of another length, a missing key, another type": {
			"http://dep/", `{"all":["A","B"],"gone":"C","num":"D","obj":{"k":"E"}}`, "/", `{"all":["1"],"num":4,"obj":"x"}`,
			result{pairs: []string{"="}, alone: []string{"A", "B", "C", "D", "E"}},
		},
		"a live body that is not JSON": {
			"http://dep/", `["A",{"k":"B"}]`, "/", `id=1`,
			result{pairs: []string{"="}, alone: []string{"A", "B"}},
		},
		"a recorded body that is not JSON has no places": {
			"http://dep/", `id=A`, "/", `{"id":"1"}`,
			result{pairs: []string{"="}},
		},
		"object keys are not values": {
			"http://dep/", `{"A":"x"}`, "/", `{"A":"y"}`,
			result{pairs: []string{"=", "x=y"}},
		},
	} {
		if got := align(c.recURL, c.recBody, c.liveURL, c.liveBody); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got  %+v\n want %+v", name, got, c.want)
		}
	}
}

// WalkStrings lists the strings of one request at the places AlignStrings can
// set another request's beside: exactly what lining the request up with itself
// pairs, so the two never disagree about where a value can be read off whole.
func TestWalkStrings(t *testing.T) {
	const id = "0f8fad5b-d9cb-469f-a165-70867728950e"
	for name, c := range map[string]struct {
		url, body string
		want      []string
	}{
		"a path segment and a query value": {"http://dep/items/" + id + "?ref=R&page=2", "", []string{id, "2", "R", "items"}},
		"a key that repeats has no place":  {"http://dep/items?id=A&id=B&one=C", "", []string{"C", "items"}},
		"JSON strings, by key and in arrays": {"http://dep/", `{"id":"A","all":["B",{"k":"C"}],"n":1,"ok":true,"none":null}`,
			[]string{"", "A", "B", "C"}},
		"a top-level array":              {"http://dep/", `[{"id":"A"},{"id":"B"}]`, []string{"", "A", "B"}},
		"object keys are not values":     {"http://dep/", `{"` + id + `":"x"}`, []string{"", "x"}},
		"NDJSON is not one document":     {"http://dep/_bulk", `{"index":{"_id":"` + id + `"}}` + "\n" + `{"title":"hello"}` + "\n", []string{"_bulk"}},
		"a body that only opens as JSON": {"http://dep/", `{"id":"` + id + `"`, []string{""}},
		"a form body":                    {"http://dep/", `id=` + id + `&name=widget`, []string{""}},
		"a JSON string, not a document":  {"http://dep/", `"` + id + `"`, []string{""}},
		"an id inside a longer string":   {"http://dep/", `{"ref":"order-` + id + `"}`, []string{"", "order-" + id}},
	} {
		var got, paired []string
		WalkStrings(c.url, c.body, func(s string) { got = append(got, s) })
		AlignStrings(c.url, c.body, c.url, c.body, func(rec, _ string) { paired = append(paired, rec) }, func(string) {})
		sort.Strings(got)
		sort.Strings(paired)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: walked %q, want %q", name, got, c.want)
		}
		if !reflect.DeepEqual(got, paired) {
			t.Errorf("%s: walked %q, but the request lined up with itself pairs %q", name, got, paired)
		}
	}
}

func TestSameJSONBut(t *testing.T) {
	for name, c := range map[string]struct {
		a, b  string
		paths map[string][]string
		want  bool
	}{
		"the same document":             {`{"a":1,"b":[1,2],"c":null}`, `{"c":null,"b":[1,2],"a":1}`, nil, true},
		"a value differs":               {`{"a":1}`, `{"a":2}`, nil, false},
		"a field only the live has":     {`{"a":1}`, `{"a":1,"b":2}`, nil, false},
		"a field only the recorded has": {`{"a":1,"b":2}`, `{"a":1}`, nil, false},
		// What the drift detector folds away: every element counts, in place.
		"an earlier array element differs": {`{"t":["a","z"]}`, `{"t":["b","z"]}`, nil, false},
		"array order":                      {`{"t":["a","z"]}`, `{"t":["z","a"]}`, nil, false},
		"array length":                     {`{"t":["a"]}`, `{"t":["a","a"]}`, nil, false},
		"an object in an array differs":    {`[{"k":1},{"k":2}]`, `[{"k":9},{"k":2}]`, nil, false},
		// Numbers as written: two large ids are not one float.
		"large integers":       {`{"id":1234567890123456789}`, `{"id":1234567890123456788}`, nil, false},
		"a number as a string": {`{"a":1}`, `{"a":"1"}`, nil, false},
		"null and absent":      {`{"a":null}`, `{}`, nil, false},
		"null and a string":    {`{"a":null}`, `{"a":"null"}`, nil, false},
		"an object and a list": {`{"a":{}}`, `{"a":[]}`, nil, false},

		"aside: a field":                  {`{"a":1,"at":"x"}`, `{"a":1,"at":"y"}`, map[string][]string{"at": {}}, true},
		"aside: only that field":          {`{"a":1,"at":"x"}`, `{"a":2,"at":"y"}`, map[string][]string{"at": {}}, false},
		"aside: any case":                 {`{"createdAt":"x"}`, `{"createdAt":"y"}`, map[string][]string{"createdat": {}}, true},
		"aside: as learned on a mock":     {`{"createdAt":"x"}`, `{"createdAt":"y"}`, map[string][]string{"createdAt": {}}, true},
		"aside: a whole subtree":          {`{"m":{"a":1}}`, `{"m":[1,2,3]}`, map[string][]string{"m": {}}, true},
		"aside: a field absent on a side": {`{"a":1,"at":"x"}`, `{"a":1}`, map[string][]string{"at": {}}, true},
		"aside: a field added on a side":  {`{"a":1}`, `{"a":1,"at":"x"}`, map[string][]string{"at": {}}, true},
		"aside: inside an array":          {`{"l":[{"s":"a","at":"1"},{"s":"b","at":"2"}]}`, `{"l":[{"s":"a","at":"8"},{"s":"b","at":"9"}]}`, map[string][]string{"l[].at": {}}, true},
		"aside: the array's other fields": {`{"l":[{"s":"a","at":"1"}]}`, `{"l":[{"s":"z","at":"9"}]}`, map[string][]string{"l[].at": {}}, false},
		// A path names one field. The drift detector reads it as a substring.
		"a path is not a substring: provider": {`{"id":"a","provider":"aws"}`, `{"id":"b","provider":"gcp"}`, map[string][]string{"id": {}}, false},
		"a path is not a substring: userId":   {`{"id":"a","userId":"1"}`, `{"id":"b","userId":"2"}`, map[string][]string{"id": {}}, false},
		"a path is not a suffix: item.id":     {`{"id":"a","item":{"id":"1"}}`, `{"id":"b","item":{"id":"2"}}`, map[string][]string{"id": {}}, false},
		"a nested path":                       {`{"id":"a","item":{"id":"1"}}`, `{"id":"a","item":{"id":"2"}}`, map[string][]string{"item.id": {}}, true},
		"an entry with patterns is no field":  {`{"at":"t-1"}`, `{"at":"t-2"}`, map[string][]string{"at": {`^t-\d+$`}}, false},
		"the empty path is not the root":      {`{"a":1}`, `{"a":2}`, map[string][]string{"": {}}, false},

		"not JSON":      {`{"a":1}`, `a=1`, nil, false},
		"two documents": {`{"a":1}`, `{"a":1}{"a":1}`, nil, false},
		"empty":         {``, ``, nil, false},
		"a bare scalar": {`"a"`, `"a"`, nil, false},
	} {
		if got := SameJSONBut(c.a, c.b, c.paths); got != c.want {
			t.Errorf("%s: SameJSONBut(%s, %s, %v) = %v, want %v", name, c.a, c.b, c.paths, got, c.want)
		}
	}
}
