package models

import (
	"encoding/json"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// An answer's framing (HTTPResp.Chunked) survives every form a recording is
// kept in: YAML (mocks.yaml), BSON, JSON and a clone. An answer not sent
// chunked writes no line for it, so its recording reads as before.
func TestChunkedSurvivesYAMLBSONJSONAndClone(t *testing.T) {
	resp := HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "application/json"}, Body: `{"a":1}`, Chunked: true}
	if back, out := roundTripYAML(t, resp); !back.Chunked || !strings.Contains(out, "chunked: true") {
		t.Errorf("YAML: read back chunked %v from\n%s", back.Chunked, out)
	}
	data, err := bson.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var fromBSON HTTPResp
	if err := bson.Unmarshal(data, &fromBSON); err != nil || !fromBSON.Chunked {
		t.Errorf("BSON: read back chunked %v, %v", fromBSON.Chunked, err)
	}
	js, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var fromJSON HTTPResp
	if err := json.Unmarshal(js, &fromJSON); err != nil || !fromJSON.Chunked {
		t.Errorf("JSON: read back chunked %v, %v", fromJSON.Chunked, err)
	}
	if c := resp.clone(); !c.Chunked {
		t.Error("clone: lost chunked")
	}

	resp.Chunked = false
	if back, out := roundTripYAML(t, resp); back.Chunked || strings.Contains(out, "chunked") {
		t.Errorf("YAML of an answer not sent chunked:\n%s", out)
	}
}
