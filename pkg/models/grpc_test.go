package models

import "testing"

// A gRPC call's ordinary header is found whatever the case of its name: the
// headers hold names as they came off the wire, lowercase in HTTP/2, so the
// usual spelling ("Keploy-Test-Name") never matched a real call's header. A
// name of exactly the spelling asked for wins, and of names that differ from
// it only in case the one that sorts first, so a call gives one answer.
func TestGrpcHeadersOrdinaryReadsANameWhateverItsCase(t *testing.T) {
	for _, c := range []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{"as HTTP/2 sends it", map[string]string{"content-type": "application/grpc", "keploy-test-name": "wire"}, "wire"},
		{"the exact spelling wins, over one that sorts before it", map[string]string{"KEPLOY-TEST-NAME": "upper", "Keploy-Test-Name": "exact"}, "exact"},
		{"of other spellings, the first sorted", map[string]string{"keploy-Test-name": "b", "KEPLOY-TEST-NAME": "a"}, "a"},
		{"absent", map[string]string{"content-type": "application/grpc"}, ""},
		{"no headers", nil, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			for i := 0; i < 20; i++ { // map order varies between runs of a range
				if got := (GrpcHeaders{OrdinaryHeaders: c.headers}).Ordinary("Keploy-Test-Name"); got != c.want {
					t.Fatalf("Ordinary(Keploy-Test-Name) of %v = %q, want %q", c.headers, got, c.want)
				}
			}
		})
	}
}
