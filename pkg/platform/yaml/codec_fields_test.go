package yaml

import (
	"bytes"
	"reflect"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// everyFieldDoc is a document with every field of NetworkTrafficDoc set.
func everyFieldDoc(t *testing.T) *NetworkTrafficDoc {
	t.Helper()
	body := `{"error":"card declined"}`
	doc := &NetworkTrafficDoc{
		Version:      models.GetVersion(),
		Kind:         models.HTTP,
		Name:         "test-1",
		Async:        &models.AsyncMeta{Lane: "poller", Seq: 2, AnchorAfter: "test-0", AnchorPos: 1, Poll: true},
		Noise:        &DocNoise{Req: []string{"body.id"}, Value: []string{"^tok-.*$"}},
		LastUpdated:  &models.LastUpdated{Author: "dev", Timestamp: time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC)},
		Curl:         "curl http://localhost:8080/",
		ConnectionID: "conn-7",
		Start:        "worker-1",
		Variables:    models.MockVariables{"orderId": {Use: []string{"req.body.orderId"}, Answer: []string{"resp.body.id"}}},
		Clocks:       map[string]models.ClockOffset{"req.header.Date": {Min: time.Second, Max: 2 * time.Second, Zone: "UTC"}},
		Real:         []string{"localhost:5432", "auth.example.com:443"},
		Authored: &models.Authored{How: models.AuthoredDerived, Reason: "declined card", Source: "checkout::TestCheckout/mock-1",
			Request: "sha256:5d1e", Malformed: true, Delta: &models.AuthoredDelta{Status: 402, Body: &body,
				JSONPatch: `[{"op":"add","path":"/code","value":"declined"}]`, Header: map[string]string{"Retry-After": "5"}, DropHeader: []string{"Etag"}}},
		Origin: &models.Origin{Clock: "realtime", Comm: "app", Exe: "/usr/bin/app"},
	}
	if err := doc.Spec.Encode(map[string]any{"req": map[string]any{"method": "GET", "url": "/orders"}}); err != nil {
		t.Fatal(err)
	}
	// A field added to the document must be set here, or this row would
	// not check that both codecs carry it.
	v := reflect.ValueOf(doc).Elem()
	for i := 0; i < v.NumField(); i++ {
		if v.Field(i).IsZero() {
			t.Fatalf("everyFieldDoc leaves %s unset: set it so the codecs' round trip checks it", v.Type().Field(i).Name)
		}
	}
	return doc
}

// The JSON form of the document has the document's fields, each by its name:
// a field the JSON form lacks cannot be carried by the JSON codec.
func TestNetworkTrafficDocJSONHasEveryField(t *testing.T) {
	doc, js := reflect.TypeOf(NetworkTrafficDoc{}), reflect.TypeOf(NetworkTrafficDocJSON{})
	for i := 0; i < doc.NumField(); i++ {
		if _, ok := js.FieldByName(doc.Field(i).Name); !ok {
			t.Errorf("NetworkTrafficDocJSON has no %s", doc.Field(i).Name)
		}
	}
	for i := 0; i < js.NumField(); i++ {
		if _, ok := doc.FieldByName(js.Field(i).Name); !ok {
			t.Errorf("NetworkTrafficDoc has no %s", js.Field(i).Name)
		}
	}
}

// Every field of a document comes back from each codec as it went in, by
// each way the package writes one (MarshalDoc, MarshalDocIndent,
// EncodeDocTo) and UnmarshalDoc. The JSON codec dropped a test case's real:
// list both ways.
func TestCodecsRoundTripEveryField(t *testing.T) {
	writers := map[string]func(Format, *NetworkTrafficDoc) ([]byte, error){
		"MarshalDoc":       MarshalDoc,
		"MarshalDocIndent": MarshalDocIndent,
		"EncodeDocTo": func(f Format, d *NetworkTrafficDoc) ([]byte, error) {
			var b bytes.Buffer
			err := EncodeDocTo(&b, f, d)
			return b.Bytes(), err
		},
	}
	for _, format := range []Format{FormatYAML, FormatJSON} {
		for name, write := range writers {
			t.Run(string(format)+"/"+name, func(t *testing.T) {
				want := everyFieldDoc(t)
				data, err := write(format, want)
				if err != nil {
					t.Fatal(err)
				}
				got, err := UnmarshalDoc(format, data)
				if err != nil {
					t.Fatal(err)
				}
				gv, wv := reflect.ValueOf(got).Elem(), reflect.ValueOf(want).Elem()
				for i := 0; i < wv.NumField(); i++ {
					field := wv.Type().Field(i).Name
					if field == "Spec" {
						var gs, ws any
						if err := got.Spec.Decode(&gs); err != nil {
							t.Fatal(err)
						}
						if err := want.Spec.Decode(&ws); err != nil {
							t.Fatal(err)
						}
						if !reflect.DeepEqual(gs, ws) {
							t.Errorf("Spec = %v, want %v", gs, ws)
						}
						continue
					}
					if !reflect.DeepEqual(gv.Field(i).Interface(), wv.Field(i).Interface()) {
						t.Errorf("%s = %#v, want %#v", field, gv.Field(i).Interface(), wv.Field(i).Interface())
					}
				}
			})
		}
	}
}
