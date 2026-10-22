package learn

import (
	"testing"
	"time"
)

func strLeaf(p, v string) Leaf { return Leaf{Path: p, Noise: p, Type: String, Value: v} }

func classifyLeaf(path, rec, live string, at time.Time, earlier []Earlier, raw []Raw, resp []Leaf) Learned {
	r := Shape{Leaves: map[string]Leaf{path: strLeaf(path, rec)}}
	l := Shape{Leaves: map[string]Leaf{path: strLeaf(path, live)}}
	return ClassifyIn(NewDriftCache(), []string{path}, l, r, at, earlier, raw, resp, nil)[0]
}

func seenAt(mock, path, rec, live string, at time.Time) Earlier {
	return Earlier{Mock: mock, At: at, Own: true, Seen: []Seen{{Side: Request, Leaf: strLeaf(path, rec), Live: live, Place: path}}}
}

// A role and an app the test named in two milliseconds when recorded, and
// in one in F (atg-features' RBAC edit-vs-run spec): F sent the role the
// app's name, but the recording says the role's name never came from the
// app's, so the match is F's alone and no source. The role is learned (an
// echo of its answer), not refused.
func TestClassifyInAValueFMadeEqualToAnEarlierOneAloneIsNoSource(t *testing.T) {
	at := time.Date(2026, 10, 22, 14, 18, 19, 0, time.UTC)
	app := seenAt("mock-596", "body.variables.name", "e2e-editnorun-1792678699276", "e2e-editnorun-1792678760798", at.Add(-time.Second))
	resp := []Leaf{strLeaf("body.data.createRole.name", "e2e-editnorun-1792678699275")}
	got := classifyLeaf("body.variables.input.name", "e2e-editnorun-1792678699275", "e2e-editnorun-1792678760798", at, []Earlier{app}, nil, resp)
	if got.Kind != KindEcho {
		t.Fatalf("learned %s (%s), want echo", got.Kind, got.Why)
	}

	// When the recording sent the app's name too, the link held when
	// recorded, and keploy still cannot tell: refused, as before.
	same := seenAt("mock-596", "body.variables.name", "e2e-editnorun-1792678699275", "e2e-editnorun-1792678760798", at.Add(-time.Second))
	if got := classifyLeaf("body.variables.input.name", "e2e-editnorun-1792678699275", "e2e-editnorun-1792678760798", at, []Earlier{same}, nil, resp); got.Kind == KindNoise || got.Kind == KindEcho {
		t.Fatalf("a link the recording held was learned as %s", got.Kind)
	}
}

// A time the test made, which F moved out of the lead's reach (a slow call
// before it), whose changed pieces (the minute "18", the seconds "04") an
// earlier payload holds only inside another clock read (a Date header of
// another second) or as a number of its own ("ubuntu 26.04" of a
// User-Agent): neither is its source, and it is learned as noise.
func TestClassifyInAPieceOfATimeComesOnlyFromThatTime(t *testing.T) {
	at := time.Date(2026, 10, 22, 14, 18, 5, 0, time.UTC)
	rec, live := "2026-10-22T14:18:04.274Z", "2026-10-22T14:19:08.023Z"
	raw := []Raw{
		{Mock: "mock-2", At: at.Add(-time.Minute), Text: "POST /auth/login\nUser-Agent: Playwright/1.62.1 (arm64; ubuntu 26.04) node/24.22\n"},
		{Mock: "mock-131", At: at.Add(-3 * time.Second), Text: "HTTP/1.1 200 OK\nDate: Thu, 22 Oct 2026 14:18:02 GMT\n"},
	}
	if got := classifyLeaf("body.reqAt", rec, live, at, nil, raw, nil); got.Kind != KindNoise {
		t.Fatalf("learned %s (from %s, %s), want noise", got.Kind, got.From.Mock, got.Why)
	}

	// A payload holding that very time, at any precision, is its source.
	same := append(raw, Raw{Mock: "mock-150", At: at.Add(-time.Second), Text: `{"createdAt":"2026-10-22T14:18:04Z"}`})
	if got := classifyLeaf("body.reqAt", rec, live, at, nil, same, nil); got.Kind == KindNoise || got.Kind == KindEcho {
		t.Fatalf("a payload holding the time itself was no source: %s", got.Kind)
	}

	// The call's own answer holding a piece as a number of its own may be
	// derived from the time it was sent ("seq 04"): refused, as before.
	resp := []Leaf{strLeaf("body.note", "seq 04")}
	if got := classifyLeaf("body.reqAt", rec, live, at, nil, raw, resp); got.Kind == KindNoise {
		t.Fatal("a piece the answer holds as a number of its own was learned as noise")
	}
}

func TestTokenValue(t *testing.T) {
	for _, tc := range []struct {
		rec, v, live, want string
		ok                 bool
	}{
		{"e2e-role-1792678689546", "1792678689546", "e2e-role-1792678752275", "1792678752275", true},
		{"a-17926786-b-17926786", "17926786", "a-99-b-99", "99", true},
		{"a-17926786-b-17926786", "17926786", "a-99-b-98", "", false},
		{"e2e-role-1792678689546", "1792678689546", "e2e-admin-1792678752275", "", false},
		{"e2e-role-1792678689546", "1792678689546", "e2e-role-1792678689546", "", false},
		{"order-1792678689546x", "1792678689546", "order-1x", "", false},
	} {
		got, ok := TokenValue(tc.rec, tc.v, tc.live)
		if got != tc.want || ok != tc.ok {
			t.Errorf("TokenValue(%q, %q, %q) = %q, %v; want %q, %v", tc.rec, tc.v, tc.live, got, ok, tc.want, tc.ok)
		}
	}
}
