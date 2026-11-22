package routes

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// send builds an /updatemockparams request carrying entries under cs.
func send(cs *models.ConsumedSync, names ...string) *models.UpdateMockParamsReq {
	req := &models.UpdateMockParamsReq{ConsumedSync: cs}
	if len(names) > 0 {
		req.FilterParams.TotalConsumedMocks = map[string]models.MockState{}
		for _, n := range names {
			req.FilterParams.TotalConsumedMocks[n] = models.MockState{Name: n}
		}
	}
	return req
}

// resolveOK resolves req on c and fails the test unless the store took it.
func resolveOK(t *testing.T, c *consumedStore, req *models.UpdateMockParamsReq) string {
	t.Helper()
	held, done, ok := c.resolve(req)
	done()
	if !ok {
		t.Fatalf("the store refused %+v", req.ConsumedSync)
	}
	return held
}

// The store applies a send only on top of the exact state it was built on:
// its stream and its base both. A client's own discipline is not what keeps
// the agent's history right, so a send of another stream at the same place,
// or of the same stream from another place, is refused, and changes nothing.
func TestTheConsumedStoreRefusesASendItDoesNotHoldTheBaseOf(t *testing.T) {
	c := &consumedStore{}
	if held := resolveOK(t, c, send(&models.ConsumedSync{Stream: "a", Full: true, Seq: 2}, "m1", "m2")); held != models.ConsumedSyncHeld("a", 2) {
		t.Fatalf("a full send was answered %q", held)
	}
	if held := resolveOK(t, c, send(&models.ConsumedSync{Stream: "a", Base: 2, Seq: 3}, "m3")); held != models.ConsumedSyncHeld("a", 3) {
		t.Fatalf("a send built on what the store holds was answered %q", held)
	}

	for _, tc := range []struct {
		name string
		cs   models.ConsumedSync
	}{
		{"another stream at the same place", models.ConsumedSync{Stream: "b", Base: 3, Seq: 4}},
		{"the same stream from an earlier place", models.ConsumedSync{Stream: "a", Base: 2, Seq: 4}},
		{"the same stream from a later place", models.ConsumedSync{Stream: "a", Base: 4, Seq: 5}},
	} {
		cs := tc.cs
		_, done, ok := c.resolve(send(&cs, "x"))
		done()
		if ok {
			t.Fatalf("%s: the store applied a send it does not hold the base of", tc.name)
		}
	}

	req := send(&models.ConsumedSync{Stream: "a", Base: 3, Seq: 4}, "m4")
	if held := resolveOK(t, c, req); held != models.ConsumedSyncHeld("a", 4) {
		t.Fatalf("the send after the refused ones was answered %q", held)
	}
	got := req.FilterParams.TotalConsumedMocks
	if len(got) != 4 {
		t.Fatalf("the service is handed %d entries; want the 4 the stream wrote, none of the refused sends'", len(got))
	}
	for _, n := range []string{"m1", "m2", "m3", "m4"} {
		if _, ok := got[n]; !ok {
			t.Fatalf("the history the service is handed lacks %s", n)
		}
	}
}
