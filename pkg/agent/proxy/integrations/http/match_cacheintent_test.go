package http

import (
	"net/http"
	"testing"

	"go.uber.org/zap"
)

// Live atg requests carry no Cache-Control/Pragma; 93-98% of the tape does.
func TestCacheIntentHeadersIgnored(t *testing.T) {
	h := &HTTP{Logger: zap.NewNop()}
	live := http.Header{}
	for _, k := range []string{"Accept", "Accept-Language", "Cookie", "Origin", "Referer", "User-Agent", "Host", "Connection", "Accept-Encoding"} {
		live.Set(k, "x")
	}
	recorded := map[string]string{
		"Accept": "x", "Accept-Language": "x", "Cookie": "x", "Origin": "x",
		"Referer": "x", "User-Agent": "x", "Host": "x", "Connection": "x",
		"Accept-Encoding": "x", "Cache-Control": "no-cache", "Pragma": "no-cache",
	}
	if !h.HeadersContainKeys(recorded, live, nil) {
		t.Fatal("recording with Cache-Control/Pragma still rejected against a live request that lacks them")
	}
}
