// Package recorder is a fixture for analysistest: a well-behaved V2 parser
// that sources timestamps from chunk.ReadAt / chunk.WrittenAt. No time.Now
// calls appear, so the analyzer must emit zero diagnostics.
package recorder

import "time"

// Chunk is a hand-rolled stand-in for fakeconn.Chunk — the fixture must not
// depend on the real proxy tree.
type Chunk struct {
	ReadAt    time.Time
	WrittenAt time.Time
	Payload   []byte
}

// Mock is a minimal stand-in for models.Mock's timestamp fields.
type Mock struct {
	ReqTimestampMock time.Time
	ResTimestampMock time.Time
}

// Record builds a Mock entirely from chunk-derived timestamps — the shape the
// rule enforces. No time.Now / time.Since / time.Until anywhere.
func Record(req, res Chunk) Mock {
	return Mock{
		ReqTimestampMock: req.ReadAt,
		ResTimestampMock: res.WrittenAt,
	}
}

// Age is an intentionally-benign use of the time package (type reference
// only) to prove that mere imports of "time" do not trip the rule.
func Age(m Mock) time.Duration {
	return m.ResTimestampMock.Sub(m.ReqTimestampMock)
}
