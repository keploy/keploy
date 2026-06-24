package http

import (
	"sort"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/schemanoise"
	"go.keploy.io/server/v3/pkg/models"
)

func httpMockWithBody(body string, header map[string]string, noise []string) *models.Mock {
	return &models.Mock{
		Kind:  models.Kind(models.HTTP),
		Noise: noise,
		Spec: models.MockSpec{
			HTTPReq: &models.HTTPReq{
				Body:   body,
				Header: header,
			},
		},
	}
}

func sortedKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestDetectReqBodyNoise drives HTTP's schema-noise detection through the shared
// engine (httpNoiseAdapter + schemanoise.Engine) — the same path match() uses —
// proving HTTP is a full client of the generic engine, not just a borrower of
// the JSON kernel.
func TestDetectReqBodyNoise(t *testing.T) {
	jsonHdr := map[string]string{"Content-Type": "application/json"}
	formHdr := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}

	// detect mirrors what match() does: build an engine with the detection flag
	// and ask it for the drift on a candidate.
	detect := func(enabled bool, mock *models.Mock, body []byte, userNoise map[string][]string) map[string][]string {
		drift, _ := schemanoise.New(httpNoiseAdapter{}, enabled, false).Detect(mock, body, userNoise)
		return drift
	}

	t.Run("disabled returns nil", func(t *testing.T) {
		mock := httpMockWithBody(`{"id":"a"}`, jsonHdr, nil)
		if got := detect(false, mock, []byte(`{"id":"b"}`), nil); got != nil {
			t.Fatalf("expected nil when disabled, got %v", got)
		}
	})

	t.Run("identical body returns nil", func(t *testing.T) {
		mock := httpMockWithBody(`{"id":"a"}`, jsonHdr, nil)
		if got := detect(true, mock, []byte(`{"id":"a"}`), nil); got != nil {
			t.Fatalf("expected nil for identical body, got %v", got)
		}
	})

	t.Run("json value drift is flagged with body prefix", func(t *testing.T) {
		mock := httpMockWithBody(`{"id":"a","name":"x"}`, jsonHdr, nil)
		got := detect(true, mock, []byte(`{"id":"b","name":"x"}`), nil)
		want := []string{"body.id"}
		if keys := sortedKeys(got); len(keys) != 1 || keys[0] != want[0] {
			t.Fatalf("got %v, want %v", keys, want)
		}
	})

	t.Run("obfuscated field is excluded", func(t *testing.T) {
		// Mock.Noise marks the redacted secret value; the recorded body holds
		// the redacted placeholder while the replayed body holds the real one.
		mock := httpMockWithBody(`{"id":"a","token":"****"}`, jsonHdr, []string{`^\*\*\*\*$`})
		got := detect(true, mock, []byte(`{"id":"b","token":"realsecret"}`), nil)
		keys := sortedKeys(got)
		if len(keys) != 1 || keys[0] != "body.id" {
			t.Fatalf("expected only body.id (token excluded as obfuscated), got %v", keys)
		}
	})

	t.Run("form body drift is flagged", func(t *testing.T) {
		mock := httpMockWithBody(`a=1&b=2`, formHdr, nil)
		got := detect(true, mock, []byte(`a=1&b=99`), nil)
		keys := sortedKeys(got)
		if len(keys) != 1 || keys[0] != "body.b" {
			t.Fatalf("expected body.b, got %v", keys)
		}
	})

	t.Run("non-json non-form returns nil", func(t *testing.T) {
		mock := httpMockWithBody(`plain text body`, map[string]string{"Content-Type": "text/plain"}, nil)
		if got := detect(true, mock, []byte(`different text`), nil); got != nil {
			t.Fatalf("expected nil for plain text, got %v", got)
		}
	})
}

func TestMergeReqBodyNoise(t *testing.T) {
	existing := map[string][]string{"body.id": {"keep"}}
	detected := map[string][]string{"body.id": {"ignored"}, "body.ts": {"orig"}}
	merged := mergeReqBodyNoise(existing, detected)

	if len(merged) != 2 {
		t.Fatalf("expected 2 keys, got %v", merged)
	}
	// Existing entry must win on collision (detected's value is dropped).
	if v := merged["body.id"]; len(v) != 1 || v[0] != "keep" {
		t.Fatalf("expected existing body.id to win, got %v", v)
	}

	// Mutate the merged slices by index — not via append — and assert the
	// inputs are untouched. Non-empty slices + index mutation is what actually
	// proves fresh backing storage: append on an empty slice always reallocates,
	// so it would pass even if mergeReqBodyNoise aliased the input slice.
	merged["body.ts"][0] = "mutated"
	if detected["body.ts"][0] != "orig" {
		t.Fatalf("merge leaked backing storage into detected input: %v", detected["body.ts"])
	}
	merged["body.id"][0] = "mutated"
	if existing["body.id"][0] != "keep" {
		t.Fatalf("merge leaked backing storage into existing input: %v", existing["body.id"])
	}
}
