package learn

import (
	"strings"
	"testing"
)

// esc writes a JSON escape with '#' for its backslash, so that the escapes
// below stay escapes in this file whatever tool wrote it.
func esc(s string) string { return strings.ReplaceAll(s, "#", "\x5c") }

// A "#uXXXX" escape is four hex digits, so 16 bits, up to U+FFFF, and an
// escaped surrogate pair is the character beyond it that the pair names, up
// to U+10FFFF (CodeQL go/incorrect-integer-conversion: each parse is bounded
// to 16 bits).
func TestUnescapedReadsEveryFourDigitEscape(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{esc("#u0041"), "A"},
		{esc("#u0000"), "\x00"},
		{esc("#uffff"), string(rune(0xFFFF))},
		{esc("#uFFFF"), string(rune(0xFFFF))},
		{esc("#ud83d#ude00"), string(rune(0x1F600))},
		{esc("#uD83D#uDE00"), string(rune(0x1F600))},
		{esc("#ud800#udc00"), string(rune(0x10000))},
		{esc("#udbff#udfff"), string(rune(0x10FFFF))},
		// A lone surrogate names no character: the replacement character.
		{esc("#ud800"), string(rune(0xFFFD))},
		{esc("a#u00e9b"), "a" + string(rune(0xE9)) + "b"},
	} {
		if !strings.Contains(tc.in, "\x5cu") {
			t.Fatalf("the row %q holds no escape", tc.in)
		}
		if got := unescaped(tc.in); got != tc.want {
			t.Errorf("unescaped(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Two readings that both refuse a leaf leave it keploy's limit only when
// both say so: one that refuses it for no limit of keploy's may be the
// app's change.
func TestStricterOfLeavesKeploysLimitOnlyWhenBothSaySo(t *testing.T) {
	limit := Learned{Kind: KindUnaccounted, Why: "echoed inside a longer value", NotFollowed: true}
	plain := Learned{Kind: KindUnaccounted, Why: "nothing explains it"}
	if got := stricterOf(limit, plain); got.NotFollowed {
		t.Fatalf("one reading refuses it for no limit of keploy's, yet it is keploy's: %+v", got)
	}
	if got := stricterOf(limit, limit); !got.NotFollowed {
		t.Fatalf("both readings say it is keploy's limit, yet it is not: %+v", got)
	}
	if got := stricterOf(limit, Learned{Kind: KindEcho}); got.NotFollowed {
		t.Fatalf("the late reading binds it as an echo, yet the refusal is keploy's limit: %+v", got)
	}
	// A value the earlier place's reading finds in a payload recorded at the
	// call's own instant, which the late reading learns as noise: a refusal
	// of that binding is keploy's doubt.
	raw := Learned{Kind: KindVariable, From: Place{Mock: "mock-1", AtCall: true}}
	if got := stricterOf(raw, Learned{Kind: KindNoise}); !got.Doubted {
		t.Fatalf("the late reading learns it as noise, yet the binding is not doubted: %+v", got)
	}
	if got := stricterOf(raw, raw); got.Doubted {
		t.Fatalf("both readings bind it at one place, yet it is doubted: %+v", got)
	}
	// One place, found as a copy by one reading and as a clock pick by the
	// other: the readings disagree.
	copied := Learned{Kind: KindVariable, From: Place{Mock: "mock-1", Side: Request, Path: "body.at", Var: "body.at", Whole: true}}
	picked := copied
	picked.From.Echo = true
	if got := stricterOf(copied, picked); !got.Doubted {
		t.Fatalf("one reading finds a copy and the other a clock pick, yet the binding is not doubted: %+v", got)
	}
}
