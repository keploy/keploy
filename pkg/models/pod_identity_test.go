package models

import "testing"

func TestPodIdentityFromMetadata(t *testing.T) {
	got, ok := PodIdentityFromMetadata(map[string]interface{}{
		"name":                  "test-set-1",
		PodIdentityMetaName:     " api-55f5fd9659-fzsfr ",
		PodIdentityMetaUID:      "0b1c4d6e-1",
		PodIdentityMetaIP:       "10.244.1.17",
		PodIdentityMetaHostIP:   "192.168.1.9",
		PodIdentityMetaNode:     "worker-1",
		PodIdentityMetaHostname: int64(12345), // strvals types an all-digit value
	})
	if !ok {
		t.Fatal("an identity with a name must be read")
	}
	want := PodIdentity{Name: "api-55f5fd9659-fzsfr", UID: "0b1c4d6e-1", IP: "10.244.1.17", HostIP: "192.168.1.9", Node: "worker-1", Hostname: "12345"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}

	for _, meta := range []map[string]interface{}{nil, {"name": "test-set-1"}, {PodIdentityMetaIP: "10.0.0.1"}, {PodIdentityMetaName: nil}} {
		if _, ok := PodIdentityFromMetadata(meta); ok {
			t.Fatalf("metadata %v names no pod and must not be read as one", meta)
		}
	}
}

func TestNewIdentityAliasingIsNilWhenThereIsNothingToCarry(t *testing.T) {
	if NewIdentityAliasing(nil, false, true) != nil {
		t.Fatal("no pods and not disabled must be nil, whatever Responses says")
	}
	if got := NewIdentityAliasing(nil, true, false); got == nil || !got.Disabled {
		t.Fatalf("disabled must be carried: %+v", got)
	}
	if got := NewIdentityAliasing([]PodIdentity{{Name: "p"}}, false, false); got == nil || len(got.RecordedPods) != 1 || got.ReverseResponses() {
		t.Fatalf("recorded pods must be carried and responses stay as recorded by default: %+v", got)
	}
	if got := NewIdentityAliasing([]PodIdentity{{Name: "p"}}, false, true); !got.ReverseResponses() {
		t.Fatalf("responses opt-in must be carried: %+v", got)
	}
	if got := NewIdentityAliasing([]PodIdentity{{Name: "p"}}, true, true); got.ReverseResponses() {
		t.Fatalf("disabled wins over the responses opt-in: %+v", got)
	}
	var none *IdentityAliasing
	if none.ReverseResponses() {
		t.Fatal("nil must not reverse")
	}
}
