package http

import (
	"context"
	"encoding/base64"
	"sort"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
)

// uriComponent is s escaped as encodeURIComponent escapes it.
func uriComponent(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if c := s[i]; uriComponentKept(c) {
			b.WriteByte(c)
		} else {
			b.WriteString("%" + strings.ToUpper(hexByte(c)))
		}
	}
	return b.String()
}

func hexByte(c byte) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[c>>4], digits[c&15]})
}

// beacon is a browser fraud-signal beacon's body: base64 of a
// percent-escaped JSON document whose id, timing, stamp and hash change
// every run, and whose source, locale and agent do not.
func beacon(id, t, w, h, src, locale string) string {
	return base64.StdEncoding.EncodeToString([]byte(uriComponent(beaconDoc(id, t, w, h, src, locale))))
}

func beaconDoc(id, t, w, h, src, locale string) string {
	return `{"v2":1,"id":"` + id + `","t":` + t + `,"tag":"$npm_package_version","src":"` + src + `",` +
		`"a":{"c":{"v":"` + locale + `","t":0},"d":{"v":"Linux x86_64","t":0},"l":{"v":"Mozilla/5.0 (X11; Linux x86_64) Chrome/151.0","t":0}},` +
		`"b":{"u":"localhost","w":"` + w + `"},"h":"` + h + `"}`
}

var beaconCT = map[string]string{"Content-Type": "text/plain;charset=UTF-8"}

func beaconRec() string {
	return beacon("a4738b3315a966c90a95b41d08a3a731", "38.2", "1794621679011:4936f74b5f1895d70f0169eb867be7a93aec4b8fe09331dfeccc918556e51ebe", "96ec0eea649e3ccf620c", "js", "en-US")
}

func beaconF() string {
	return beacon("5d0c3f1e9b2a47c6a1e8f07d3b9c2e64", "41.7", "1794625518342:0b7e2c91d4f3a8e6c5b2d1f0e9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0", "3f9a1c0e7b2d4a6c8e1f", "js", "en-US")
}

func beaconCI(src, locale string) string {
	return beacon("77aa3f1e9b2a47c6a1e8f07d3b9c2e01", "12.9", "1794629918342:1c7e2c91d4f3a8e6c5b2d1f0e9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a9", "0d9a1c0e7b2d4a6c8e22", src, locale)
}

// A body that is JSON under a whole-body encoding is read as that JSON, by
// the chain of layers it holds: its fields are leaves under the chain's
// root, each with a JSON body's noise name.
func TestABodyOfJSONUnderAWholeBodyEncodingIsReadAsThatJSON(t *testing.T) {
	doc := `{"id":"x1","a":{"c":"en-US"},"l":[1,"two"]}`
	for _, tc := range []struct{ name, body, chain string }{
		{"base64 of percent escapes", base64.StdEncoding.EncodeToString([]byte(uriComponent(doc))), "base64>percent>json"},
		{"base64", base64.StdEncoding.EncodeToString([]byte(doc)), "base64>json"},
		{"percent escapes", uriComponent(doc), "percent>json"},
		{"an array", base64.StdEncoding.EncodeToString([]byte(`[{"id":"x1"}]`)), "base64>json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chain, v, ok := decodedBody([]byte(tc.body))
			if !ok || chain != tc.chain {
				t.Fatalf("decodedBody = %q, %v; want %q", chain, ok, tc.chain)
			}
			leaves := map[string]learn.Leaf{}
			decodedLeaves(chain, v, leaves)
			root := "body(" + tc.chain + ")"
			for p, l := range leaves {
				if !strings.HasPrefix(p, root+".") && !strings.HasPrefix(p, root+"[") {
					t.Errorf("leaf %q not under %q", p, root)
				}
				if l.Path != p || !strings.HasPrefix(l.Noise, "body.") && !strings.HasPrefix(l.Noise, "body[") {
					t.Errorf("leaf %q: path %q, noise %q; want its path and a JSON body's noise name", p, l.Path, l.Noise)
				}
			}
			s := learnNoise{}.shape(httpParts{method: "POST", path: "/6", body: []byte(tc.body)}, sameDest)
			if s.Fixed["body"] != tc.chain || s.Body != nil {
				t.Errorf("shape: body %q (Body %v); want the chain %q, no JSON Body", s.Fixed["body"], s.Body, tc.chain)
			}
			if tc.name == "base64 of percent escapes" {
				if l, ok := s.Leaves[root+".a.c"]; !ok || l.Value != "en-US" || l.Noise != "body.a.c" {
					t.Errorf("shape leaf %s.a.c = %+v, %v", root, l, ok)
				}
				if l, ok := s.Leaves[root+".l[1]"]; !ok || l.Noise != "body.l[]" {
					t.Errorf("shape leaf %s.l[1] = %+v, %v", root, l, ok)
				}
			}
		})
	}
}

// A layer counts only where it decodes the whole body and the body is the
// one text that layer writes for what it decodes to; and the end must be
// valid UTF-8 holding a JSON object or array. Anything else is compared
// whole, as a text body always was.
func TestABodyNoLayerReadsWholeStaysWhole(t *testing.T) {
	doc := `{"id":"x1","q":"a b!"}`
	b64 := base64.StdEncoding.EncodeToString([]byte(doc)) // 22 bytes: ends in "=="
	if !strings.HasSuffix(b64, "==") {
		t.Fatalf("%q: want two pad bytes", b64)
	}
	// The last group's second letter holds four bits past the last byte,
	// all zero in the canonical text: set the lowest.
	const std = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	lastBits := b64[:len(b64)-3] + string(std[strings.IndexByte(std, b64[len(b64)-3])|1]) + "=="
	urlB64 := base64.URLEncoding.EncodeToString([]byte(`{"k":"???"}`))
	if !strings.ContainsAny(urlB64, "-_") {
		t.Fatalf("%q: want a letter of the url alphabet alone", urlB64)
	}
	pct := uriComponent(doc)
	for _, tc := range []struct{ name, body string }{
		{"base64 of text that is no JSON", base64.StdEncoding.EncodeToString([]byte("hello world, plain"))},
		{"base64 missing its padding", strings.TrimRight(b64, "=")},
		{"base64 with a line break", b64[:8] + "\n" + b64[8:]},
		{"base64 with a CR line break", b64[:8] + "\r\n" + b64[8:]},
		{"base64 with a bit set past the last byte", lastBits},
		{"base64 in the url alphabet", urlB64},
		{"base64 of a JSON scalar", base64.StdEncoding.EncodeToString([]byte(`"just a string"`))},
		{"base64 of a JSON number", base64.StdEncoding.EncodeToString([]byte(`12345`))},
		{"base64 of invalid UTF-8 in JSON", base64.StdEncoding.EncodeToString([]byte("{\"id\":\"\xff\"}"))},
		{"percent in lower-case hex", strings.ToLower(pct[:3]) + pct[3:]},
		{"percent escaping a byte kept as it is", strings.Replace(pct, "x1", "%781", 1)},
		{"percent with a byte left unescaped", strings.Replace(pct, "%20", " ", 1)},
		{"percent with a plus for a space", strings.Replace(pct, "%20", "+", 1)},
		{"percent cut inside an escape", pct[:len(pct)-1]},
		{"percent escapes of invalid UTF-8", strings.Replace(pct, "x1", "%FF", 1)},
		{"percent escapes of no JSON", uriComponent("a b c")},
		// Read with the lower-case digit taken as 0, each would be JSON.
		{"percent with a lower-case low digit", strings.Replace(uriComponent(`{"p":"a/b"}`), "%2F", "%2f", 1)},
		{"percent with a lower-case high digit", uriComponent(`{"p":1,`) + "%aD" + uriComponent(`"q":2}`)},
		{"base64 of percent escapes in lower-case hex", base64.StdEncoding.EncodeToString([]byte(strings.ToLower(pct)))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if chain, _, ok := decodedBody([]byte(tc.body)); ok {
				t.Fatalf("decodedBody(%q) = %q; want no chain", tc.body, chain)
			}
			s := learnNoise{}.shape(httpParts{method: "POST", path: "/6", body: []byte(tc.body)}, sameDest)
			if s.Fixed["body"] != "text "+tc.body {
				t.Errorf("shape body %q; want the text compared whole", s.Fixed["body"])
			}
		})
	}
	// The cases above fail by the rule they name, not by a broken input:
	// the canonical forms they start from are read.
	for _, body := range []string{b64, pct, uriComponent(`{"p":"a/b"}`), uriComponent(`{"p":1,"q":2}`)} {
		if _, _, ok := decodedBody([]byte(body)); !ok {
			t.Fatalf("decodedBody(%q): want a chain", body)
		}
	}
	// A body that is JSON as it is reads by no chain.
	for _, body := range []string{doc, `[1,2]`, ` {"a":1}`, `12345678`, `"x"`} {
		if chain, _, ok := decodedBody([]byte(body)); ok {
			t.Errorf("decodedBody(%q) = %q; JSON as it is reads by no chain", body, chain)
		}
	}
}

// The beacon recorded once and sent by F with new ids, timings and stamps
// is served: only the fields that changed are learned, under the chain's
// name, and CI's beacon with other values again replays.
func TestABeaconOfJSONUnderAWholeBodyEncodingLearnsOnlyItsChangedFields(t *testing.T) {
	recs := []*models.Mock{learnRecorded("mock-0", 0, "POST", "/6", beaconRec(), beaconCT, `{"v2":1}`)}
	consumed := fullRound(t, recs, []*req{live("POST", "/6", beaconF(), beaconCT)})
	var got []string
	for k := range consumed[0].Spec.ReqBodyNoise {
		got = append(got, k)
	}
	sort.Strings(got)
	const root = "body(base64>percent>json)"
	want := []string{root + ".b.w", root + ".h", root + ".id", root + ".t"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("learned noise %v; want %v", got, want)
	}
	inCI(t, learnedOnto(recs, consumed), []*req{live("POST", "/6", beaconCI("js", "en-US"), beaconCT)})
}

// What F learned unchecks only the fields that changed: a beacon whose
// fixed field (its source, its locale) differs is a miss at replay, named
// at that field under the chain's root.
func TestABeaconWhoseFixedFieldChangedIsAMissAtReplay(t *testing.T) {
	recs := []*models.Mock{learnRecorded("mock-0", 0, "POST", "/6", beaconRec(), beaconCT, `{"v2":1}`)}
	consumed := fullRound(t, recs, []*req{live("POST", "/6", beaconF(), beaconCT)})
	learned := learnedOnto(recs, consumed)
	for _, tc := range []struct{ name, body, field string }{
		{"its source", beaconCI("npm", "en-US"), "body(base64>percent>json).src"},
		{"its locale", beaconCI("js", "fr-FR"), "body(base64>percent>json).a.c.v"},
		{"a field it no longer sends", base64.StdEncoding.EncodeToString([]byte(uriComponent(strings.Replace(
			beaconDoc("77aa3f1e9b2a47c6a1e8f07d3b9c2e01", "12.9", "1794629918342:1c7e2c91d4f3a8e6c5b2d1f0e9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a9", "0d9a1c0e7b2d4a6c8e22", "js", "en-US"),
			`"tag":"$npm_package_version",`, "", 1)))), "body(base64>percent>json).tag"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := live("POST", "/6", tc.body, beaconCT)
			h := newHTTP()
			if ok, _, _ := matchVar(t, h, &varDb{perTest: append([]*models.Mock(nil), learned...)}, in); ok {
				t.Fatalf("a beacon whose %s changed was served", tc.name)
			}
			n := h.newStrictNoise(nil, nil, nil)
			var names []string
			for _, d := range fieldDiffs(strictDiff(context.Background(), in, liveDestination(context.Background(), in), learned[0], n)) {
				names = append(names, d.Path)
			}
			if strings.Join(names, " ") != tc.field {
				t.Fatalf("differences %v; want only %s", names, tc.field)
			}
		})
	}
}

// Both sides are read the same way: a body read by one chain at record and
// by another, or as plain JSON, or whole, at replay differs as a whole (the
// body's structure changed), never field by field; and the learn step finds
// no candidate in it.
func TestABodyReadByAnotherChainDiffersAsAWhole(t *testing.T) {
	doc := `{"id":"x1","src":"js"}`
	rec := base64.StdEncoding.EncodeToString([]byte(uriComponent(doc)))
	for _, tc := range []struct{ name, live string }{
		{"base64 without the percent layer", base64.StdEncoding.EncodeToString([]byte(doc))},
		{"percent without the base64 layer", uriComponent(doc)},
		{"plain JSON", doc},
		{"a text no chain reads", rec[:len(rec)-1]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := learnRecorded("mock-0", 0, "POST", "/6", rec, beaconCT, `{}`)
			in := live("POST", "/6", tc.live, beaconCT)
			n := (&HTTP{}).newStrictNoise(nil, nil, nil)
			diffs := fieldDiffs(strictDiff(context.Background(), in, liveDestination(context.Background(), in), m, n))
			if len(diffs) != 1 || diffs[0].Path != "body" {
				t.Fatalf("differences %+v; want the body as a whole", diffs)
			}
			h, ln, opts := learnOn(t)
			tr := learn.NewTracker()
			tr.Begin()
			ctx := learn.WithTracker(context.Background(), tr)
			db := &learnDb{perTest: []*models.Mock{m}, observe: tr.ServedMock}
			served, miss, _ := h.learnMatch(ctx, in, db, ln, opts)
			if served != nil {
				t.Fatalf("served a body read by another chain")
			}
			// A structure miss, naming the body: "body differs", or, where
			// the call's body is JSON as it is, the chain's root missing.
			if miss == nil || !strings.Contains(miss.res.Why, "in more than values") || !strings.HasPrefix(miss.res.Diff.Structural, "body") {
				t.Fatalf("miss %+v; want the body's structure named", miss)
			}
		})
	}
}
