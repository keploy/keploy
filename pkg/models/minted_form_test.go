package models

import (
	"encoding/base64"
	"math/rand/v2"
	"reflect"
	"testing"
)

// A minted rule's form is what all its values share: the prefix they start
// with, cut back to its last byte that is no letter or digit, and the
// classes of every byte after it. A value no application mints (no letter
// or digit after the prefix, a byte no minted value holds) has none.
func TestAMintedFormIsWhatItsValuesShare(t *testing.T) {
	for _, c := range []struct {
		what   string
		values []string
		want   MintedForm
		ok     bool
	}{
		{"UUIDs", []string{"0f8a6c2e-6b1d-4c3a-9e5f-2a7b8c9d0e1f", "5c1e7a9b-3d2f-4e8a-b6c4-9f0a1b2c3d4e"}, MintedForm{Classes: []string{"-", "digit", "lower"}}, true},
		{"one UUID: all prefix to its last '-'", []string{"0f8a6c2e-6b1d-4c3a-9e5f-2a7b8c9d0e1f"}, MintedForm{Prefix: "0f8a6c2e-6b1d-4c3a-9e5f-", Classes: []string{"digit", "lower"}}, true},
		{"ObjectIDs that start alike: no letter or digit ends a prefix", []string{"6af41db637a899973dc55294", "6af41df237a899973dc55295"}, MintedForm{Classes: []string{"digit", "lower"}}, true},
		{"prefixed ids", []string{"kepreq_uamfMJBud4Q7D2dpiTWWtg5vnhG5v-jDYXUBeo0jS6Y", "kepreq_siMFUFEA8nOk70O88nz4QCLT-Kb0ucbqCNzRe_kaPWQ"}, MintedForm{Prefix: "kepreq_", Classes: []string{"-", "_", "digit", "lower", "upper"}}, true},
		{"prefixed ids whose next letters start alike", []string{"kepreq_ua-x", "kepreq_ub_y"}, MintedForm{Prefix: "kepreq_", Classes: []string{"-", "_", "lower"}}, true},
		{"a number", []string{"4821375", "9057713"}, MintedForm{Classes: []string{"digit"}}, true},
		{"none", nil, MintedForm{}, false},
		{"no letter or digit after the prefix", []string{"kepreq_-", "kepreq_ab"}, MintedForm{}, false},
		{"a space", []string{"ab cd", "ef gh"}, MintedForm{}, false},
		{"a byte outside ASCII", []string{"abcé", "abcd"}, MintedForm{}, false},
		{"a control byte", []string{"ab\x01d", "abcd"}, MintedForm{}, false},
		{"a DEL byte", []string{"ab\x7fd", "abcd"}, MintedForm{}, false},
		{"a '~', the last byte a minted value holds", []string{"ab~d", "abcd"}, MintedForm{Classes: []string{"lower", "~"}}, true},
		{"a '!', the first", []string{"ab!d", "abcd"}, MintedForm{Classes: []string{"!", "lower"}}, true},
		{"capitals and digits", []string{"AZ09", "QM42"}, MintedForm{Classes: []string{"digit", "upper"}}, true},
	} {
		got, ok := MintedFormOf(c.values...)
		if ok != c.ok || ok && !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: MintedFormOf = %+v %v, want %+v %v", c.what, got, ok, c.want, c.ok)
		}
		if ok {
			if err := got.Check(); err != nil {
				t.Errorf("%s: the form is no form keploy checks: %v", c.what, err)
			}
			for _, v := range c.values {
				if !got.Keeps(v, v) {
					t.Errorf("%s: %q does not keep its own form %+v", c.what, v, got)
				}
			}
		}
	}
}

// A prefix one more value lacks is cut, back to its last byte that is no
// letter or digit, and the bytes cut off are of the form's classes then.
func TestAMintedFormsPrefixIsCutToWhatEveryValueShares(t *testing.T) {
	one := MintedForm{Prefix: "0f8a6c2e-6b1d-4c3a-9e5f-", Classes: []string{"digit", "lower"}}
	if got, want := one.Cut("5c1e7a9b-3d2f-4e8a-b6c4-9f0a1b2c3d4e"), (MintedForm{Classes: []string{"-", "digit", "lower"}}); !reflect.DeepEqual(got, want) {
		t.Errorf("a second UUID: %+v, want %+v", got, want)
	}
	if got, want := one.Cut("0f8a6c2e-6b1d-4c3a-9e5f-9f0a1b2c3d4e", "0f8a6c2e-6b1d-4ccc-9e5f-9f0a1b2c3d4e"), (MintedForm{Prefix: "0f8a6c2e-6b1d-", Classes: []string{"-", "digit", "lower"}}); !reflect.DeepEqual(got, want) {
		t.Errorf("values that share a part: %+v, want %+v", got, want)
	}
	k := MintedForm{Prefix: "kepreq_", Classes: []string{"-", "_", "digit", "lower", "upper"}}
	if got := k.Cut("kepreq_abc", "kepreq_ABC"); !reflect.DeepEqual(got, k) {
		t.Errorf("values that keep the prefix: %+v, want %+v", got, k)
	}
	if got, want := (MintedForm{Prefix: "Kq.", Classes: []string{"digit"}}).Cut("Kz.1"), (MintedForm{Classes: []string{".", "digit", "lower", "upper"}}); !reflect.DeepEqual(got, want) {
		t.Errorf("a prefix of a letter, a letter and a '.': %+v, want %+v", got, want)
	}
	if k.Cut("kepreq_x").Classes[0] != "-" || &k.Cut().Classes[0] == &k.Classes[0] {
		t.Error("a cut form shares its classes with the form it cut")
	}
}

// A value made anew keeps its form at a place where it has the recorded
// value's length, the prefix, and after it only bytes of the form's
// classes, wherever they stand.
func TestAMintedValueKeepsItsForm(t *testing.T) {
	const rec = "kepreq_uamfMJBud4Q7D2dpiTWWtg5vnhG5v-jDYXUBeo0jS6Y"
	k := MintedForm{Prefix: "kepreq_", Classes: []string{"-", "_", "digit", "lower", "upper"}}
	for _, c := range []struct {
		what, v string
		keeps   bool
	}{
		{"another id", "kepreq_siMFUFEA8nOk70O88nz4QCLT-Kb0ucbqCNzRe_kaPWQ", true},
		{"a '-' and a '_' where letters stood", "kepreq_-_mfMJBud4Q7D2dpiTWWtg5vnhG5v-jDYXUBeo0jS6Y", true},
		{"no '-' at all", "kepreq_uamfMJBud4Q7D2dpiTWWtg5vnhG5vAjDYXUBeo0jS6Y", true},
		{"a character more", rec + "a", false},
		{"a character less", rec[:len(rec)-1], false},
		{"another prefix", "kepcod_uamfMJBud4Q7D2dpiTWWtg5vnhG5v-jDYXUBeo0jS6Y", false},
		{"a '+', a class no value held", "kepreq_+amfMJBud4Q7D2dpiTWWtg5vnhG5v-jDYXUBeo0jS6Y", false},
		{"a space", "kepreq_ amfMJBud4Q7D2dpiTWWtg5vnhG5v-jDYXUBeo0jS6Y", false},
		{"a byte outside ASCII", "kepreq_\xffamfMJBud4Q7D2dpiTWWtg5vnhG5v-jDYXUBeo0jS6", false},
	} {
		if got := k.Keeps(rec, c.v); got != c.keeps {
			t.Errorf("%s: Keeps(%q) = %v, want %v", c.what, c.v, got, c.keeps)
		}
	}
	if (MintedForm{Classes: []string{"digit"}}).Keeps("1234", "12a4") {
		t.Error("a letter in a form of digits kept it")
	}
}

// api-server's OAuth request ids are "kepreq_" and 32 random bytes in
// URL-safe base64 (pkg/service/oauth/secrets.go, newSecret). The form of
// the ids its recording holds keeps every id the generator makes: no fresh
// id of 10,000 is refused.
func TestAMintedFormKeepsEveryIdItsGeneratorMakes(t *testing.T) {
	recorded := []string{
		"kepreq_uamfMJBud4Q7D2dpiTWWtg5vnhG5v-jDYXUBeo0jS6Y",
		"kepreq_siMFUFEA8nOk70O88nz4QCLT-Kb0ucbqCNzRe_kaPWQ",
		"kepreq_ugERz1KHn9KeR3sQ4IVZfWf7PkheQgBaQwNJS9VMQkk",
	}
	f, ok := MintedFormOf(recorded...)
	if !ok || f.Prefix != "kepreq_" {
		t.Fatalf("the recorded ids' form: %+v %v", f, ok)
	}
	r := rand.New(rand.NewPCG(1, 2))
	b := make([]byte, 32)
	refused := 0
	for i := 0; i < 10000; i++ {
		for j := range b {
			b[j] = byte(r.Uint32())
		}
		if id := "kepreq_" + base64.RawURLEncoding.EncodeToString(b); !f.Keeps(recorded[0], id) {
			refused++
			if refused == 1 {
				t.Errorf("a fresh id %q does not keep the form %+v", id, f)
			}
		}
	}
	if refused > 0 {
		t.Errorf("%d of 10000 fresh ids refused; want none", refused)
	}
}
