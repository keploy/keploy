package variables

import "testing"

// A minted value's shape: its length, each byte that is no letter or digit
// where it stood, and its letters and digits of the narrowest alphabet that
// holds both runs'.
func TestAMintedValueKeepsItsLengthItsSeparatorsAndItsAlphabet(t *testing.T) {
	const uuid = "1111aaaa-2222-48d1-9444-444455556666"
	for _, tc := range []struct {
		name      string
		rec, live string
		want      Alphabet
		ok        bool
	}{
		{"a UUID", uuid, "9999cccc-7777-4a2e-8888-000011112222", AlphabetHex, true},
		{"a UUID whose digits stand where letters stood", uuid, "11110000-2222-48d1-9444-444455556666", AlphabetHex, true},
		{"a UUID in capitals", uuid, "9999CCCC-7777-4A2E-8888-000011112222", AlphabetHex, true},
		{"a number", "1794362916", "5531209876", AlphabetDigits, true},
		{"a token of letters", "Zq81Lm0XpVb4", "rAHcEraS7Qx1", AlphabetAlnum, true},
		{"a character more", uuid, uuid + "0", 0, false},
		{"a character fewer", uuid, uuid[1:], 0, false},
		{"a '-' moved", uuid, "1111aaa-a2222-48d1-9444-444455556666", AlphabetBase64URL, true},
		{"a '-' moved and a '+' for a letter", uuid, "1111aaa-a2222-48d1-9444-44445555666+", 0, false},
		{"a key in standard base64", "q3+Zb7Lr0Tf/9XcV", "Xk2N/w9Ft4Qz+Lm8", AlphabetBase64, true},
		{"a token in URL-safe base64", "q3-Zb7Lr0Tf_9XcV", "Xk2N_w9Ft4Qz-Lm8", AlphabetBase64URL, true},
		{"padding moved", "q3+Zb7Lr0Tf/9X==", "Xk2N/w9Ft4Qz+L=m", 0, false},
		{"a '-' made a letter, read as URL-safe base64", uuid, "1111aaaaa2222-48d1-9444-444455556666", AlphabetBase64URL, true},
		{"a '.' made a letter", "1111aaaa.2222", "1111aaaaa2222", 0, false},
		{"a letter made a '.'", "1111aaaa.2222", "1111aaa..2222", 0, false},
		{"another separator", "1111aaaa.2222", "1111aaaa:2222", 0, false},
		{"no letter or digit", "-+-", "-+-", 0, false},
		{"no letter or digit, a letter's byte in another script", "-š-", "-š-", 0, false},
		{"letters that stayed beside digits", "ab12", "ab34", AlphabetHex, true},
	} {
		a, ok := MintedAlphabet(tc.rec, tc.live)
		if a != tc.want || ok != tc.ok {
			t.Errorf("%s: alphabet %d ok %v; want %d %v", tc.name, a, ok, tc.want, tc.ok)
		}
	}
	// A replay's value is checked in the learned alphabet.
	for _, tc := range []struct {
		name string
		a    Alphabet
		v    string
		ok   bool
		rec  string
	}{
		{"hex in hex", AlphabetHex, "9999cccc-7777-4a2e-8888-000011112222", true, ""},
		{"a letter past f in hex", AlphabetHex, "9999cccz-7777-4a2e-8888-000011112222", false, ""},
		{"a capital past F in hex", AlphabetHex, "9999cccZ-7777-4a2e-8888-000011112222", false, ""},
		{"a capital in hex", AlphabetHex, "9999cccC-7777-4a2e-8888-000011112222", true, ""},
		{"a capital in letters and digits", AlphabetAlnum, "9999cccZ-7777-4a2e-8888-000011112222", true, ""},
		{"a letter in digits", AlphabetDigits, "9999cccc-7777-4a2e-8888-000011112222", false, ""},
		{"digits in digits", AlphabetDigits, "99990000-7777-4222-8888-000011112222", true, "11110000-2222-4801-9444-444455556666"},
		{"any letter in letters and digits", AlphabetAlnum, "9999cccz-7777-4a2e-8888-000011112222", true, ""},
		{"a separator for a letter", AlphabetAlnum, "9999ccc.-7777-4a2e-8888-000011112222", false, ""},
		{"a letter for a separator", AlphabetHex, "9999cccca7777-4a2e-8888-000011112222", false, ""},
		{"a '-' moved in hex", AlphabetHex, "9999ccc-c7777-4a2e-8888-000011112222", false, ""},
		{"a '-' moved in URL-safe base64", AlphabetBase64URL, "9999ccc-c7777-4a2e-8888-000011112222", true, ""},
		{"a '+' in URL-safe base64", AlphabetBase64URL, "9999ccc+c7777-4a2e-8888-000011112222", false, ""},
		{"a '/' in standard base64", AlphabetBase64, "9999ccc/c7777-4a2e-8888-000011112222", false, ""},
	} {
		rec := uuid
		if tc.rec != "" {
			rec = tc.rec
		}
		if got := tc.a.Keeps(rec, tc.v); got != tc.ok {
			t.Errorf("%s: Keeps = %v; want %v", tc.name, got, tc.ok)
		}
	}
}
