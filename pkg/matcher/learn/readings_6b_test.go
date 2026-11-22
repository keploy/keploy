package learn

import (
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	mrand "math/rand"
	"strings"
	"testing"
	"time"
)

// A part in an alphabet with no '-' or '_' (hex, base32), and a base64 part
// of either alphabet, glued by one '-' or '_' to a long base64url blob (an
// RSA-4096 signature) with many groups of its own, is read: an id that
// flowed into it is never noise.
func TestClassifyReadsAPartGluedToALongBlob(t *testing.T) {
	at := time.Date(2026, 10, 14, 10, 0, 42, 0, time.UTC)
	r := mrand.New(mrand.NewSource(2020))
	const alpha = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"
	id := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = alpha[r.Intn(len(alpha))]
		}
		return string(b)
	}
	var items []string
	for i := 0; i < 10; i++ {
		items = append(items, fmt.Sprintf(`{"sku":"%s","q":%d}`, id(8), r.Intn(9)+1))
	}
	cart := strings.Join(items, ",")
	blob := func(n int) string {
		b := make([]byte, n)
		r.Read(b)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	data := func(v string) []byte { return []byte(`{"order":"` + v + `","cart":[` + cart + `]}`) }
	hx := func(v string) string { return hex.EncodeToString(data(v)) }
	b32 := func(v string) string {
		return strings.TrimRight(base32.StdEncoding.EncodeToString(data(v)), "=")
	}
	for _, tc := range []struct {
		name string
		enc  func(v string) string
	}{
		{"hex data _ 60-byte blob (control, <=12 groups)", func(v string) string { return hx(v) + "_" + blob(60) }},
		{"hex data _ 512-byte blob", func(v string) string { return hx(v) + "_" + blob(512) }},
		{"512-byte blob - hex data", func(v string) string { return blob(512) + "-" + hx(v) }},
		{"base32 data - 60-byte blob (control)", func(v string) string { return b32(v) + "-" + blob(60) }},
		{"base32 data - 512-byte blob", func(v string) string { return b32(v) + "-" + blob(512) }},
		{"512-byte blob _ base32 data", func(v string) string { return blob(512) + "_" + b32(v) }},
		{"512-byte blob _ base64url data (control)", func(v string) string {
			return blob(512) + "_" + base64.RawURLEncoding.EncodeToString(data(v))
		}},
		{"std data - 512-byte blob (control)", func(v string) string {
			return base64.RawStdEncoding.EncodeToString(data(v)) + "-" + blob(512)
		}},
	} {
		noise, total, groups, over := 0, 0, 0, 0
		for i := 0; i < 30; i++ {
			a, b := id(10), id(10)
			sent := []Earlier{{Mock: "mock-0", At: at.Add(-time.Second), Own: true, Seen: []Seen{
				{Side: Request, Leaf: Leaf{Path: "body.order", Noise: "body.order", Type: String, Value: a}, Live: b, Place: "body.order"},
			}}}
			rec := tc.enc(a)
			g := len(strings.FieldsFunc(rec, func(c rune) bool { return c == '-' || c == '_' })) - 1
			groups += g
			if g > maxGluedGroups {
				over++
			}
			got := classifyOne(rec, tc.enc(b), at, sent, nil, nil)
			total++
			if got.Kind == KindNoise || got.Kind == KindEcho {
				noise++
			}
		}
		if noise > 0 {
			t.Errorf("%s (avg %d groups, %d of %d over %d): %d/%d flowed ids learned as noise", tc.name, groups/total, over, total, maxGluedGroups, noise, total)
		}
	}
}

// A standard-base64 part glued by '-' after a base64url blob whose last
// characters decode to text with the glue is read: a suffix that holds both
// alphabets decodes in neither and does not hide the part.
func TestClassifyReadsAStdPartAfterAURLBlob(t *testing.T) {
	at := time.Date(2026, 10, 14, 10, 0, 42, 0, time.UTC)
	r := mrand.New(mrand.NewSource(7))
	const alpha = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"
	id := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = alpha[r.Intn(len(alpha))]
		}
		return string(b)
	}
	// std data guaranteed to hold '+' or '/': a '?' / '>' heavy payload.
	data := func(v string) string {
		s := base64.RawStdEncoding.EncodeToString([]byte(`{"order":"` + v + `","q":"???>>>~~~","note":"a>b?c"}`))
		return s
	}
	// a blob whose last stretch is exactly "Abc" after a '_' and whose
	// total length is a multiple of 4 so "_Abc-" aligns with the data.
	mkblob := func() string {
		b := make([]byte, 600)
		r.Read(b)
		s := base64.RawURLEncoding.EncodeToString(b)
		return s[:len(s)-5] + "Q_Abc"
	}
	// find a 3-char tail X so that X+"-" decodes (lenient) to text.
	tail := ""
	for _, c1 := range "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz" {
		for _, c2 := range "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz" {
			for _, c3 := range "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz" {
				x := string(c1) + string(c2) + string(c3) + "-"
				if b := lenientB64(x); isText(b) && !isWord(b[0]) && !isWord(b[1]) {
					tail = x[:3]
					break
				}
			}
			if tail != "" {
				break
			}
		}
		if tail != "" {
			break
		}
	}
	learned := 0
	for i := 0; i < 20; i++ {
		a, b := id(10), id(10)
		bl := mkblob()
		bl = bl[:len(bl)-3] + tail
		enc := func(v string) string { return bl + "-" + data(v) }
		rec, live := enc(a), enc(b)
		if !strings.ContainsAny(data(a), "+/") {
			t.Fatalf("data has no std char")
		}
		sent := []Earlier{{Mock: "mock-0", At: at.Add(-time.Second), Own: true, Seen: []Seen{
			{Side: Request, Leaf: Leaf{Path: "body.order", Noise: "body.order", Type: String, Value: a}, Live: b, Place: "body.order"},
		}}}
		got := classifyOne(rec, live, at, sent, nil, nil)
		if got.Kind == KindNoise || got.Kind == KindEcho {
			learned++
		}
	}
	if learned > 0 {
		t.Errorf("%d/20 flowed ids in a std part after a url blob learned as noise", learned)
	}
}

// A part between groups of a long run is read wherever it sits: a hex,
// base32 or standard-base64 part after a prefix of three groups or before a
// suffix of three, next to a double separator, and base64url text with
// groups of its own after a prefix: an id that flowed into it is never noise.
func TestClassifyReadsAPartBetweenGroupsOfALongRun(t *testing.T) {
	at := time.Date(2026, 10, 14, 10, 0, 42, 0, time.UTC)
	r := mrand.New(mrand.NewSource(2121))
	const alpha = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"
	id := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = alpha[r.Intn(len(alpha))]
		}
		return string(b)
	}
	var items []string
	for i := 0; i < 6; i++ {
		items = append(items, fmt.Sprintf(`{"sku":"%s","q":%d}`, id(8), r.Intn(9)+1))
	}
	cart := strings.Join(items, ",")
	// a blob with no "--"/"__" doubles so only the first/last-three bound applies.
	blob := func(n int) string {
		for {
			b := make([]byte, n)
			r.Read(b)
			s := base64.RawURLEncoding.EncodeToString(b)
			if s[0] == '-' || s[0] == '_' || s[len(s)-1] == '-' || s[len(s)-1] == '_' {
				continue
			}
			if strings.Contains(s, "--") || strings.Contains(s, "__") || strings.Contains(s, "-_") || strings.Contains(s, "_-") {
				continue
			}
			return s
		}
	}
	data := func(v string) []byte { return []byte(`{"order":"` + v + `","cart":[` + cart + `]}`) }
	hx := func(v string) string { return hex.EncodeToString(data(v)) }
	b32 := func(v string) string { return strings.TrimRight(base32.StdEncoding.EncodeToString(data(v)), "=") }
	// base64url text with groups of its own: non-ASCII names.
	// a note of '?', '>' and '~' (each 62 or 63 at a byte's low six bits)
	// chosen so that the encoding holds five groups of its own, apart.
	note := ""
	for {
		nb := make([]byte, 24)
		for i := range nb {
			nb[i] = "?>~ab"[r.Intn(5)]
		}
		u := base64.RawURLEncoding.EncodeToString([]byte(`{"order":"` + id(10) + `","note":"` + string(nb) + `","cart":[` + cart + `]}`))
		if g := strings.Count(u, "-") + strings.Count(u, "_"); g >= 5 && !strings.Contains(u, "--") && !strings.Contains(u, "__") && !strings.Contains(u, "-_") && !strings.Contains(u, "_-") {
			note = string(nb)
			break
		}
	}
	udata := func(v string) string {
		return base64.RawURLEncoding.EncodeToString([]byte(`{"order":"` + v + `","note":"` + note + `","cart":[` + cart + `]}`))
	}
	ulate := func(v string) string {
		return base64.RawURLEncoding.EncodeToString([]byte(`{"note":"` + note + `","cart":[` + cart + `],"order":"` + v + `"}`))
	}
	padK := 0
	type tc struct {
		name string
		enc  func(v string) string
	}
	for _, c := range []tc{
		{"CONTROL a_b_<hex>_<blob> (stretch 2)", func(v string) string { return "acct_live_" + hx(v) + "_" + blob(512) }},
		{"acct_live_v2_<hex>_<blob> (stretch 3)", func(v string) string { return "acct_live_v2_" + hx(v) + "_" + blob(512) }},
		{"acct_live_v2_<b32>_<blob> (stretch 3)", func(v string) string { return "acct_live_v2_" + b32(v) + "_" + blob(512) }},
		{"CONTROL <blob>-<hex>-eu-west (stretch G-2)", func(v string) string { return blob(512) + "-" + hx(v) + "-eu-west" }},
		{"<blob>-<hex>-v2-eu-west (stretch G-3)", func(v string) string { return blob(512) + "-" + hx(v) + "-v2-eu-west" }},
		{"v1_<b64url text, own groups>_<blob>", func(v string) string { return "v1_" + udata(v) + "_" + blob(512) }},
		{"<blob>_<b64url text, own groups>_v1", func(v string) string { return blob(512) + "_" + udata(v) + "_v1" }},
		{"v1_<b64url text, own groups, id late>_<blob>", func(v string) string { return "v1_" + ulate(v) + "_" + blob(512) }},
		{"acct_live_v2_<std b64 text>_<blob> (stretch 3)", func(v string) string {
			return "acct_live_v2_" + base64.RawStdEncoding.EncodeToString([]byte(`{"q":"???>>>~~~","order":"`+v+`","cart":[`+cart+`]}`)) + "_" + blob(512)
		}},
		{"CONTROL acct_live_<std b64 text>_<blob> (stretch 2)", func(v string) string {
			return "acct_live_" + base64.RawStdEncoding.EncodeToString([]byte(`{"q":"???>>>~~~","order":"`+v+`","cart":[`+cart+`]}`)) + "_" + blob(512)
		}},
		{"CONTROL acct_live_v2_<hex>_<RSA-2048 blob>", func(v string) string { return "acct_live_v2_" + hx(v) + "_" + blob(256) }},
		{"v1_<b64url text, own groups, id late, pad k>_<blob> x4 pads", func(v string) string {
			return "v1_" + base64.RawURLEncoding.EncodeToString([]byte(`{"note":"`+note+`","cart":[`+cart+`],"p":"`+strings.Repeat("x", padK)+`","order":"`+v+`"}`)) + "_" + blob(512)
		}},
		{"<blob>--<hex>_<blob> (after a double)", func(v string) string { return blob(384) + "--" + hx(v) + "_" + blob(384) }},
		{"<blob>_<hex>--<blob> (before a double)", func(v string) string { return blob(384) + "_" + hx(v) + "--" + blob(384) }},
		{"CONTROL <blob>--<hex>--<blob> (both doubles)", func(v string) string { return blob(384) + "--" + hx(v) + "--" + blob(384) }},
	} {
		learned, refused, other, over, okgroups := 0, 0, 0, 0, 0
		padK = 0
		for i := 0; i < 30; i++ {
			a, b := id(10), id(10)
			padK = i % 4
			sent := []Earlier{{Mock: "mock-0", At: at.Add(-time.Second), Own: true, Seen: []Seen{
				{Side: Request, Leaf: Leaf{Path: "body.order", Noise: "body.order", Type: String, Value: a}, Live: b, Place: "body.order"},
			}}}
			rec := c.enc(a)
			g := strings.Count(rec, "-") + strings.Count(rec, "_")
			if g > maxGluedGroups {
				over++
			}
			if g >= 10 {
				okgroups++
			}
			got := classifyOne(rec, c.enc(b), at, sent, nil, nil)
			switch got.Kind {
			case KindNoise, KindEcho:
				learned++
			case KindUnaccounted:
				refused++
			default:
				other++
			}
		}
		if learned > 0 {
			t.Errorf("%s (%d of 30 over %d groups): %d flowed ids learned as noise", c.name, over, maxGluedGroups, learned)
		}
		_, _, _ = refused, other, okgroups
	}
}

// UTF-8 base64url text with groups of its own (its groups fall mid-quad),
// after a prefix and glued to a long blob, is read.
func TestClassifyReadsUTF8TextWithGroupsOfItsOwnAfterAPrefix(t *testing.T) {
	at := time.Date(2026, 10, 14, 10, 0, 42, 0, time.UTC)
	r := mrand.New(mrand.NewSource(77))
	const alpha = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"
	id := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = alpha[r.Intn(len(alpha))]
		}
		return string(b)
	}
	blob := func(n int) string {
		b := make([]byte, n)
		r.Read(b)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	learned, total := 0, 0
	for trial := 0; trial < 40; trial++ {
		var note string
		for tries := 0; ; tries++ {
			if tries > 20000 {
				t.Fatalf("no note")
			}
			var nr []rune
			for i := 0; i < 30+r.Intn(30); i++ {
				nr = append(nr, rune(0x4E00+r.Intn(0x5000)))
			}
			nb := []byte(string(nr))
			u := base64.RawURLEncoding.EncodeToString([]byte(`{"note":"` + string(nb) + `","order":"AAAAAAAAAA"}`))
			if g := strings.Count(u, "-") + strings.Count(u, "_"); g >= 4 && !strings.Contains(u, "--") && !strings.Contains(u, "__") && !strings.Contains(u, "-_") && !strings.Contains(u, "_-") {
				note = string(nb)
				break
			}
		}
		enc := func(v string) string {
			return "v1_" + base64.RawURLEncoding.EncodeToString([]byte(`{"note":"`+note+`","order":"`+v+`"}`)) + "_" + blob(512)
		}
		a, b := id(10), id(10)
		sent := []Earlier{{Mock: "mock-0", At: at.Add(-time.Second), Own: true, Seen: []Seen{
			{Side: Request, Leaf: Leaf{Path: "body.order", Noise: "body.order", Type: String, Value: a}, Live: b, Place: "body.order"},
		}}}
		got := classifyOne(enc(a), enc(b), at, sent, nil, nil)
		total++
		if got.Kind == KindNoise || got.Kind == KindEcho {
			learned++
		}
	}
	if learned > 0 {
		t.Errorf("%d/%d flowed ids in UTF-8 base64url text after a prefix learned as noise", learned, total)
	}
}
