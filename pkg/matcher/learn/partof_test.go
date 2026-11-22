package learn

import "testing"

// TestPartOf_NamesAWordWhereItStandsAsOne pins where a reason says a word of
// a value stands. A word stands where the value holds it as a word, with no
// letter or digit beside it, so a bcrypt hash whose salt holds "0" between a
// "/" and a "." names that place, never the "0" of its "$2a$08$" cost, which
// is part of the word "08". A part the value holds as no word is named at its
// first place, as before.
func TestPartOf_NamesAWordWhereItStandsAsOne(t *testing.T) {
	cases := []struct {
		name, value, part, want string
	}{
		{
			name:  "a word inside a longer run first, standing alone later",
			value: "$2a$08$Kx9/0.QrsTuvWxyzAbCdEfGhIjKlMnOpQrStUvWxYz0123456789ab",
			part:  "0",
			want:  "its characters 12 to 12",
		},
		{
			name:  "a word whose first place is a word",
			value: "$2a$08$3/SadLQ4bKxyzAbCdEfGhIjKlMnOpQrStUvWxYz0123456789abcd",
			part:  "3",
			want:  "its characters 8 to 8",
		},
		{
			name:  "a word of several characters",
			value: "xUTF8y-UTF8",
			part:  "UTF8",
			want:  "its characters 8 to 11",
		},
		{
			name:  "a part held as no word, at its first place",
			value: "order-42x/b42c",
			part:  "42",
			want:  "its characters 7 to 8",
		},
		{
			name:  "characters counted as runes",
			value: "é08/0",
			part:  "0",
			want:  "its characters 5 to 5",
		},
		{
			name:  "a part the value does not hold",
			value: "abc",
			part:  "7",
			want:  "a part of it in another form",
		},
		{
			name:  "no part",
			value: "abc",
			part:  "",
			want:  "a part of it in another form",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := partOf(c.value, c.part); got != c.want {
				t.Fatalf("partOf(%q, %q) = %q, want %q", c.value, c.part, got, c.want)
			}
		})
	}
}
