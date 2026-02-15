package runregistry

import "time"

type TestRun struct {
	ID         string        `json:"id"`
	Timestamp  time.Time     `json:"timestamp"`
	Total      int           `json:"total"`
	Passed     int           `json:"passed"`
	Failed     int           `json:"failed"`
	Duration   time.Duration `json:"duration"`
	Branch     string        `json:"branch,omitempty"`
	CommitHash string        `json:"commit_hash,omitempty"`
}
