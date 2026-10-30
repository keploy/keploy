package generic

import (
	"encoding/base64"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// viewRecording is a generic recording of one exchange: the request in
// reqs' pieces (encodePayload's form), answered with answer.
func viewRecording(answer string, reqs ...string) *models.Mock {
	m := &models.Mock{Name: "mocks", Kind: models.GENERIC}
	for _, r := range reqs {
		m.Spec.GenericRequests = append(m.Spec.GenericRequests, encodePayload([]byte(r), models.FromClient))
	}
	m.Spec.GenericResponses = []models.Payload{encodePayload([]byte(answer), models.FromServer)}
	return m
}

// The view a generic serve states is the recording with the request as the
// application sent it: its bytes as one piece, kept as the recorder keeps a
// piece (text as it is, other bytes base64), and the recorded answer F
// wrote, the recording itself unchanged. Its key names the bytes sent, the
// same in whatever pieces they were read and another for other bytes.
func TestAGenericServesViewIsTheRequestAsSentAndTheRecordedAnswer(t *testing.T) {
	rec := viewRecording("+OK 17\r\n", "GET k ", "id=1\r\n")
	recReq := rec.Spec.GenericRequests

	for _, tc := range []struct {
		name     string
		sent     [][]byte
		wantType string
		wantData string
	}{
		{"text", [][]byte{[]byte("GET k "), []byte("id=4242\r\n")}, models.String, "GET k id=4242\r\n"},
		{"binary", [][]byte{[]byte("\xff\x01GET k "), []byte("id=4242\r\n")}, "binary", base64.StdEncoding.EncodeToString([]byte("\xff\x01GET k id=4242\r\n"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			load, key := servedView(rec, tc.sent)
			if load == nil || key == "" {
				t.Fatal("a serve's view states no view")
			}
			v, err := load()
			if err != nil {
				t.Fatal(err)
			}
			if len(v.Spec.GenericRequests) != 1 || v.Spec.GenericRequests[0].Message[0].Type != tc.wantType || v.Spec.GenericRequests[0].Message[0].Data != tc.wantData || v.Spec.GenericRequests[0].Origin != models.FromClient {
				t.Fatalf("view request %+v; want one %s piece %q from the client", v.Spec.GenericRequests, tc.wantType, tc.wantData)
			}
			if len(v.Spec.GenericResponses) != 1 || v.Spec.GenericResponses[0].Message[0].Data != "+OK 17\r\n" {
				t.Fatalf("view answer %+v; want the recorded answer F wrote", v.Spec.GenericResponses)
			}
			if v.Name != rec.Name || v.Kind != rec.Kind {
				t.Fatalf("the view is %s %s; want the recording %s %s", v.Name, v.Kind, rec.Name, rec.Kind)
			}
		})
	}
	if len(rec.Spec.GenericRequests) != 2 || &rec.Spec.GenericRequests[0] != &recReq[0] || rec.Spec.GenericRequests[1].Message[0].Data != "id=1\r\n" {
		t.Fatal("the view changed the recording")
	}

	_, k1 := servedView(rec, [][]byte{[]byte("GET k id=4242\r\n")})
	_, k2 := servedView(rec, [][]byte{[]byte("GET k "), []byte("id=4242\r\n")})
	_, k3 := servedView(rec, [][]byte{[]byte("GET k id=4243\r\n")})
	if k1 != k2 {
		t.Fatal("the same bytes read in other pieces are named another view")
	}
	if k1 == k3 {
		t.Fatal("other bytes sent are named the same view")
	}
}

// A request is the recording byte for byte (as recorded) only where every
// piece is compared: one that differs only in a recorded piece obfuscated
// as noise, which the matcher does not compare, is not.
func TestOnlyARequestEveryPieceOfWhichIsRecordedIsAsRecorded(t *testing.T) {
	rec := viewRecording("+OK\r\n", "GET k ", "id=1\r\n")
	for _, tc := range []struct {
		name string
		sent [][]byte
		want bool
	}{
		{"in the recorded pieces", [][]byte{[]byte("GET k "), []byte("id=1\r\n")}, true},
		{"in other pieces", [][]byte{[]byte("GET k id=1\r\n")}, true},
		{"other bytes", [][]byte{[]byte("GET k "), []byte("id=2\r\n")}, false},
		{"longer", [][]byte{[]byte("GET k id=1\r\n"), []byte("x")}, false},
	} {
		if got := newSentRequest(tc.sent).isRecorded(rec.Spec.GenericRequests, nil, false); got != tc.want {
			t.Errorf("%s: as recorded %v; want %v", tc.name, got, tc.want)
		}
	}
}
