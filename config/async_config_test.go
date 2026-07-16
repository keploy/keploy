package config

import (
	"testing"

	yaml3 "gopkg.in/yaml.v3"
)

func TestAsyncLanesUnmarshalFromYAML(t *testing.T) {
	src := `
async:
  lanes:
    - name: notifications
      type: http
      match:
        host: "notify.internal.svc"
        path: "/v1/poll*"
      volatileParams: ["cursor"]
`
	var c Config
	if err := yaml3.Unmarshal([]byte(src), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(c.Async.Lanes) != 1 {
		t.Fatalf("want 1 lane, got %d", len(c.Async.Lanes))
	}
	l := c.Async.Lanes[0]
	if l.Name != "notifications" || l.Type != "http" {
		t.Fatalf("bad lane header: %+v", l)
	}
	if l.Match["host"] != "notify.internal.svc" || l.Match["path"] != "/v1/poll*" {
		t.Fatalf("bad match block: %+v", l.Match)
	}
	if len(l.VolatileParams) != 1 || l.VolatileParams[0] != "cursor" {
		t.Fatalf("bad volatileParams: %+v", l.VolatileParams)
	}
}
