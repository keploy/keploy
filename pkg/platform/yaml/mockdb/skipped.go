package mockdb

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	yamlLib "gopkg.in/yaml.v3"
)

// skippedDoc is what window ownership reads of a document the decoders
// skipped: its metadata and times, which a mock of every kind carries in its
// spec (under the key keploy writes, or the lowercase one older recordings
// and the MySQL spec use), and whether it is async-egress (a key of the
// document, beside its spec).
type skippedDoc struct {
	Spec struct {
		Metadata         map[string]any `yaml:"metadata" json:"metadata"`
		ReqTimestampMock time.Time      `yaml:"reqTimestampMock" json:"reqTimestampMock"`
		ResTimestampMock time.Time      `yaml:"resTimestampMock" json:"resTimestampMock"`
		ReqTimestampLow  time.Time      `yaml:"reqtimestampmock" json:"-"`
		ResTimestampLow  time.Time      `yaml:"restimestampmock" json:"-"`
	} `yaml:"spec" json:"spec"`
	Async any `yaml:"async" json:"async"`
}

// skippedWindowed is a document the decoders skipped, as test windows see it
// (models.WindowedMock): its name and kind, its request and response times
// and whether it is a test mock, read from its metadata and async key. ok is
// false for a document whose request time cannot be read. Of an incomplete
// last document (incomplete), a YAML one is read from its whole lines; a JSON
// one cannot be read.
func skippedWindowed(raw []byte, isJSON, incomplete bool, name string, kind models.Kind) (models.WindowedMock, bool) {
	if name == "" || len(raw) == 0 {
		return models.WindowedMock{}, false
	}
	var doc skippedDoc
	var err error
	if isJSON {
		err = json.Unmarshal(raw, &doc)
	} else {
		if incomplete {
			raw = raw[:bytes.LastIndexByte(raw, '\n')+1]
		}
		err = yamlLib.Unmarshal(raw, &doc)
	}
	req, res := doc.Spec.ReqTimestampMock, doc.Spec.ResTimestampMock
	if req.IsZero() {
		req, res = doc.Spec.ReqTimestampLow, doc.Spec.ResTimestampLow
	}
	if err != nil || req.IsZero() {
		return models.WindowedMock{}, false
	}
	meta := make(map[string]string, len(doc.Spec.Metadata))
	for k, v := range doc.Spec.Metadata {
		if v != nil {
			meta[k] = fmt.Sprint(v)
		}
	}
	w := models.NewWindowedMock(&models.Mock{Name: name, Kind: kind, Spec: models.MockSpec{
		Metadata:         meta,
		ReqTimestampMock: req,
		ResTimestampMock: res,
	}})
	if doc.Async != nil {
		w.TestMock = false
	}
	return w, true
}

// addSkippedDoc lists a document the decoders skipped in out.Skipped and,
// when its request time can be read, in out.SkippedWindowed.
func addSkippedDoc(out *models.TestSetMocks, raw []byte, isJSON, incomplete bool, name string, kind models.Kind) {
	addSkipped(out, name, kind)
	if w, ok := skippedWindowed(raw, isJSON, incomplete, name, kind); ok {
		out.SkippedWindowed = append(out.SkippedWindowed, w)
	}
}
