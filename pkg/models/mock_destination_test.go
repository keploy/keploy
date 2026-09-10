package models

import "testing"

// RecordedDestination is the evidence source for the destination-scope
// diagnostic, so its ok=false cases matter as much as its values: each one
// means "undecidable", and a caller that read them as "no destination" would
// manufacture a false absence claim out of a mock it simply could not read.
func TestMockRecordedDestination(t *testing.T) {
	httpMock := func(rawURL string, header map[string]string) *Mock {
		return &Mock{
			Kind: Kind(HTTP),
			Spec: MockSpec{HTTPReq: &HTTPReq{Method: "GET", URL: rawURL, Header: header}},
		}
	}

	tests := []struct {
		name   string
		mock   *Mock
		want   string
		wantOK bool
	}{
		{
			name:   "host header on a path-only URL (how the recorder writes one)",
			mock:   httpMock("/api/orders?id=90", map[string]string{"Host": "192.0.2.10", "Accept": "*/*"}),
			want:   "192.0.2.10",
			wantOK: true,
		},
		{
			name:   "lowercase host key is still a Host header",
			mock:   httpMock("/a", map[string]string{"host": "192.0.2.30:9090"}),
			want:   "192.0.2.30:9090",
			wantOK: true,
		},
		{
			// The direct "Host"/"host" probes are a fast path, not the whole
			// lookup: an unconventional spelling must still resolve, or a
			// recording quietly stops being readable and vetoes the verdict
			// for its whole test set.
			name:   "unconventional header casing still resolves via the scan",
			mock:   httpMock("/a", map[string]string{"Accept": "*/*", "HOST": "billing.svc.cluster.local"}),
			want:   "billing.svc.cluster.local",
			wantOK: true,
		},
		{
			name:   "an empty conventional key does not shadow a populated one",
			mock:   httpMock("/a", map[string]string{"Host": "", "host": "192.0.2.40"}),
			want:   "192.0.2.40",
			wantOK: true,
		},
		{
			name:   "absolute URL supplies the authority when no header does",
			mock:   httpMock("http://192.0.2.20:8080/v1/metrics", nil),
			want:   "192.0.2.20:8080",
			wantOK: true,
		},
		{
			name:   "empty Host header falls through to the URL",
			mock:   httpMock("http://192.0.2.20:8080/v1/metrics", map[string]string{"Host": ""}),
			want:   "192.0.2.20:8080",
			wantOK: true,
		},
		{
			name: "path-only URL with no Host header is undecidable",
			mock: httpMock("/internal/health", map[string]string{"Accept": "*/*"}),
		},
		{
			name: "HTTP mock with no request spec is undecidable",
			mock: &Mock{Kind: Kind(HTTP)},
		},
		{
			// Mongo/MySQL/Postgres/Generic record wire payloads with no
			// authority in them; claiming "no destination" for them would be
			// asserting an unchecked negative.
			name: "non-HTTP kinds are undecidable",
			mock: &Mock{Kind: Mongo, Spec: MockSpec{}},
		},
		{
			name: "nil mock is undecidable",
			mock: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := tt.mock.RecordedDestination()
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("RecordedDestination() = (%q, %v), want (%q, %v)", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}
