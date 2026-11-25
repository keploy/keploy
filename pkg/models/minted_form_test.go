package models

import (
	"encoding/base64"
	"math/rand/v2"
	"strings"
	"testing"
)

// A minted rule's form is what all its values share: the prefix they start
// with, cut back to its last ASCII byte that is no letter or digit, and the
// range of their lengths. Values with nothing after the prefix, or with a
// character that is no printable one or is whitespace, have none; the form
// names no character after the prefix, so any printable one keeps it.
func TestAMintedFormIsWhatItsValuesShare(t *testing.T) {
	for _, c := range []struct {
		what   string
		values []string
		want   MintedForm
		ok     bool
	}{
		{"UUIDs", []string{"0f8a6c2e-6b1d-4c3a-9e5f-2a7b8c9d0e1f", "5c1e7a9b-3d2f-4e8a-b6c4-9f0a1b2c3d4e"}, MintedForm{MinLen: 36, MaxLen: 36}, true},
		{"one UUID: all prefix to its last '-'", []string{"0f8a6c2e-6b1d-4c3a-9e5f-2a7b8c9d0e1f"}, MintedForm{Prefix: "0f8a6c2e-6b1d-4c3a-9e5f-", MinLen: 36, MaxLen: 36}, true},
		{"ObjectIDs that start alike: no letter or digit ends a prefix", []string{"6af41db637a899973dc55294", "6af41df237a899973dc55295"}, MintedForm{MinLen: 24, MaxLen: 24}, true},
		{"prefixed ids", []string{"kepreq_uamfMJBud4Q7D2dpiTWWtg5vnhG5v-jDYXUBeo0jS6Y", "kepreq_siMFUFEA8nOk70O88nz4QCLT-Kb0ucbqCNzRe_kaPWQ"}, MintedForm{Prefix: "kepreq_", MinLen: 50, MaxLen: 50}, true},
		{"prefixed ids whose next letters start alike", []string{"kepreq_ua-x", "kepreq_ub_y"}, MintedForm{Prefix: "kepreq_", MinLen: 11, MaxLen: 11}, true},
		{"numbers of three lengths", []string{"4821375", "90577139", "105"}, MintedForm{MinLen: 3, MaxLen: 8}, true},
		{"none", nil, MintedForm{}, false},
		{"nothing after the prefix", []string{"kepreq_", "kepreq_"}, MintedForm{}, false},
		{"one value, all prefix", []string{"kepreq_"}, MintedForm{}, false},
		{"a value that is the prefix of another", []string{"kepreq_", "kepreq_ab"}, MintedForm{}, false},
		{"empty values", []string{"", ""}, MintedForm{}, false},
		{"a space", []string{"ab cd", "ef gh"}, MintedForm{}, false},
		{"a tab", []string{"ab\tcd", "efghi"}, MintedForm{}, false},
		{"a no-break space", []string{"ab\u00a0d", "abcde"}, MintedForm{}, false},
		{"a control byte", []string{"ab\x01d", "abcd"}, MintedForm{}, false},
		{"a DEL byte", []string{"ab\x7fd", "abcd"}, MintedForm{}, false},
		{"a byte that is no UTF-8", []string{"ab\xffd", "abcd"}, MintedForm{}, false},
		{"a letter outside ASCII", []string{"abcé", "abcd"}, MintedForm{MinLen: 4, MaxLen: 5}, true},
		{"symbols alone", []string{"-_-", "~!~"}, MintedForm{MinLen: 3, MaxLen: 3}, true},
		{"capitals and digits", []string{"AZ09", "QM42"}, MintedForm{MinLen: 4, MaxLen: 4}, true},
	} {
		got, ok := MintedFormOf(c.values...)
		if ok != c.ok || got != c.want {
			t.Errorf("%s: MintedFormOf = %+v %v, want %+v %v", c.what, got, ok, c.want, c.ok)
		}
		if ok {
			if err := got.Check(); err != nil {
				t.Errorf("%s: the form is no form keploy checks: %v", c.what, err)
			}
			for _, v := range c.values {
				if !got.Keeps(v) {
					t.Errorf("%s: %q does not keep its own form %+v", c.what, v, got)
				}
			}
		}
	}
}

// A prefix one more value lacks is cut, back to its last ASCII byte that
// is no letter or digit, and the lengths widen to hold every value's.
func TestAMintedFormsPrefixIsCutAndItsLengthsWidenedByEveryValue(t *testing.T) {
	one := MintedForm{Prefix: "0f8a6c2e-6b1d-4c3a-9e5f-", MinLen: 36, MaxLen: 36}
	for _, c := range []struct {
		what   string
		f      MintedForm
		values []string
		want   MintedForm
	}{
		{"a second UUID", one, []string{"5c1e7a9b-3d2f-4e8a-b6c4-9f0a1b2c3d4e"}, MintedForm{MinLen: 36, MaxLen: 36}},
		{"values that share a part", one, []string{"0f8a6c2e-6b1d-4c3a-9e5f-9f0a1b2c3d4e", "0f8a6c2e-6b1d-4ccc-9e5f-9f0a1b2c3d4e"}, MintedForm{Prefix: "0f8a6c2e-6b1d-", MinLen: 36, MaxLen: 36}},
		{"values that keep the prefix", MintedForm{Prefix: "kepreq_", MinLen: 10, MaxLen: 10}, []string{"kepreq_abc", "kepreq_ABC"}, MintedForm{Prefix: "kepreq_", MinLen: 10, MaxLen: 10}},
		{"a prefix of a letter, a letter and a '.'", MintedForm{Prefix: "Kq.", MinLen: 4, MaxLen: 4}, []string{"Kz.1"}, MintedForm{MinLen: 4, MaxLen: 4}},
		{"a prefix that ends at a 'z'", MintedForm{Prefix: "xyz", MinLen: 6, MaxLen: 6}, []string{"xyz123"}, MintedForm{MinLen: 6, MaxLen: 6}},
		{"a prefix that ends at a 'Z'", MintedForm{Prefix: "XYZ", MinLen: 6, MaxLen: 6}, []string{"XYZ123"}, MintedForm{MinLen: 6, MaxLen: 6}},
		{"a prefix that ends at a '9'", MintedForm{Prefix: "x99", MinLen: 6, MaxLen: 6}, []string{"x99abc"}, MintedForm{MinLen: 6, MaxLen: 6}},
		{"a prefix that ends at a byte outside ASCII", MintedForm{Prefix: "k-é", MinLen: 5, MaxLen: 5}, []string{"k-éé"}, MintedForm{Prefix: "k-", MinLen: 5, MaxLen: 6}},
		{"a shorter value", MintedForm{Prefix: "n-", MinLen: 7, MaxLen: 9}, []string{"n-12"}, MintedForm{Prefix: "n-", MinLen: 4, MaxLen: 9}},
		{"a longer value", MintedForm{Prefix: "n-", MinLen: 7, MaxLen: 9}, []string{"n-12345678"}, MintedForm{Prefix: "n-", MinLen: 7, MaxLen: 10}},
		{"a value in the range", MintedForm{Prefix: "n-", MinLen: 7, MaxLen: 9}, []string{"n-123456"}, MintedForm{Prefix: "n-", MinLen: 7, MaxLen: 9}},
		{"no value", MintedForm{Prefix: "n-", MinLen: 7, MaxLen: 9}, nil, MintedForm{Prefix: "n-", MinLen: 7, MaxLen: 9}},
	} {
		if got := c.f.With(c.values...); got != c.want {
			t.Errorf("%s: %+v, want %+v", c.what, got, c.want)
		}
	}
}

// A value made anew keeps its form at a place where it has the prefix, a
// length in the form's range, and after the prefix only printable
// characters and no whitespace: whichever characters it holds there, so a
// '+' no recorded value held keeps it too.
func TestAMintedValueKeepsItsForm(t *testing.T) {
	const rec = "kepreq_uamfMJBud4Q7D2dpiTWWtg5vnhG5v-jDYXUBeo0jS6Y"
	k := MintedForm{Prefix: "kepreq_", MinLen: 50, MaxLen: 50}
	for _, c := range []struct {
		what, v string
		keeps   bool
	}{
		{"another id", "kepreq_siMFUFEA8nOk70O88nz4QCLT-Kb0ucbqCNzRe_kaPWQ", true},
		{"a '-' and a '_' where letters stood", "kepreq_-_mfMJBud4Q7D2dpiTWWtg5vnhG5v-jDYXUBeo0jS6Y", true},
		{"no '-' at all", "kepreq_uamfMJBud4Q7D2dpiTWWtg5vnhG5vAjDYXUBeo0jS6Y", true},
		{"a '+', which no recorded value held", "kepreq_+amfMJBud4Q7D2dpiTWWtg5vnhG5v-jDYXUBeo0jS6Y", true},
		{"a letter outside ASCII", "kepreq_éamfMJBud4Q7D2dpiTWWtg5vnhG5v-jDYXUBeo0jS6", true},
		{"a character more", rec + "a", false},
		{"a character less", rec[:len(rec)-1], false},
		{"another prefix", "kepcod_uamfMJBud4Q7D2dpiTWWtg5vnhG5v-jDYXUBeo0jS6Y", false},
		{"a space", "kepreq_ amfMJBud4Q7D2dpiTWWtg5vnhG5v-jDYXUBeo0jS6Y", false},
		{"a newline", "kepreq_\namfMJBud4Q7D2dpiTWWtg5vnhG5v-jDYXUBeo0jS6Y", false},
		{"a byte that is no UTF-8", "kepreq_\xffamfMJBud4Q7D2dpiTWWtg5vnhG5v-jDYXUBeo0jS6", false},
	} {
		if got := k.Keeps(c.v); got != c.keeps {
			t.Errorf("%s: Keeps(%q) = %v, want %v", c.what, c.v, got, c.keeps)
		}
	}
	n := MintedForm{MinLen: 3, MaxLen: 5}
	for v, keeps := range map[string]bool{"12": false, "123": true, "1234": true, "12345": true, "123456": false} {
		if n.Keeps(v) != keeps {
			t.Errorf("a form of lengths 3 to 5: Keeps(%q) = %v, want %v", v, !keeps, keeps)
		}
	}
}

// api-server's OAuth request ids are "kepreq_" and 32 random bytes in
// URL-safe base64 (pkg/service/oauth/secrets.go, newSecret). The form of
// the ids its recording holds keeps every id the generator makes: no fresh
// id of 10,000 is refused, even where the form was learned from two ids
// that hold no '-' or '_' (one recorded, one of round F), which half of
// fresh ids hold.
func TestAMintedFormKeepsEveryIdItsGeneratorMakes(t *testing.T) {
	for what, seen := range map[string][]string{
		"three recorded ids": {
			"kepreq_uamfMJBud4Q7D2dpiTWWtg5vnhG5v-jDYXUBeo0jS6Y",
			"kepreq_siMFUFEA8nOk70O88nz4QCLT-Kb0ucbqCNzRe_kaPWQ",
			"kepreq_ugERz1KHn9KeR3sQ4IVZfWf7PkheQgBaQwNJS9VMQkk",
		},
		"two ids of letters and digits alone": {
			"kepreq_uamfMJBud4Q7D2dpiTWWtg5vnhG5vAjDYXUBeo0jS6Y",
			"kepreq_siMFUFEA8nOk70O88nz4QCLTxKb0ucbqCNzReQkaPWQ",
		},
	} {
		f, ok := MintedFormOf(seen...)
		if !ok || f != (MintedForm{Prefix: "kepreq_", MinLen: 50, MaxLen: 50}) {
			t.Fatalf("%s: the form: %+v %v", what, f, ok)
		}
		r := rand.New(rand.NewPCG(1, 2))
		b := make([]byte, 32)
		refused, symbols := 0, 0
		for i := 0; i < 10000; i++ {
			for j := range b {
				b[j] = byte(r.Uint32())
			}
			id := "kepreq_" + base64.RawURLEncoding.EncodeToString(b)
			if strings.ContainsAny(id[7:], "-_") {
				symbols++
			}
			if !f.Keeps(id) {
				refused++
				if refused == 1 {
					t.Errorf("%s: a fresh id %q does not keep the form %+v", what, id, f)
				}
			}
		}
		if refused > 0 || symbols < 5000 {
			t.Errorf("%s: %d of 10000 fresh ids refused, %d of them hold a '-' or '_'; want none refused of over half", what, refused, symbols)
		}
	}
}
