package runregistry

import (
	"encoding/json"
	"time"
)

// Duration is a wrapper around time.Duration that marshals to/from JSON as a string.
type Duration time.Duration

func (d Duration) MarshalJSON() ([]byte, error) {
	s := time.Duration(d).String()
	return json.Marshal(s)
}
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}

type TestRun struct {
	ID        string        `json:"id"`
	Timestamp time.Time     `json:"timestamp"`
	Total     int           `json:"total"`
	Passed    int           `json:"passed"`
	Failed    int           `json:"failed"`
	Duration  time.Duration `json:"duration"`
}
