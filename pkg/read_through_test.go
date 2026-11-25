package pkg

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func TestFilterByMappingAndServeRuleOf_ReadThroughRouting(t *testing.T) {
	t0 := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

	rtMock := &models.Mock{
		Version: models.Version("api.keploy.io/v1beta2"),
		Name:    "mock-rt-0123456789abcdef",
		Kind:    models.HTTP,
		Spec: models.MockSpec{
			Metadata: map[string]string{"type": "mocks"},
			HTTPReq: &models.HTTPReq{
				Method: models.Method("GET"),
				URL:    "http://auth.internal/v1/jwks",
				Header: map[string]string{"Host": "auth.internal"},
			},
			HTTPResp: &models.HTTPResp{
				StatusCode: 200,
				Body:       `{"keys":[]}`,
			},
			ReqTimestampMock: t0.Add(-30 * time.Second),
			ResTimestampMock: t0.Add(-29 * time.Second),
		},
	}
	if !models.MarkReadThrough(rtMock) {
		t.Fatal("MarkReadThrough failed for valid HTTP GET mock")
	}

	// A hand-tagged write mock MUST NOT be routed as read-through.
	handTaggedWrite := &models.Mock{
		Version: models.Version("api.keploy.io/v1beta2"),
		Name:    "mock-rt-bad-write",
		Kind:    models.HTTP,
		Spec: models.MockSpec{
			Metadata: map[string]string{
				"type":                        "mocks",
				models.MetadataReadThroughKey: models.MetadataReadThroughValue,
			},
			HTTPReq: &models.HTTPReq{
				Method: models.Method("POST"),
				URL:    "http://auth.internal/v1/tokens",
				Header: map[string]string{"Host": "auth.internal"},
			},
			HTTPResp: &models.HTTPResp{
				StatusCode: 200,
				Body:       `{"ok":true}`,
			},
			ReqTimestampMock: t0.Add(30 * time.Second),
			ResTimestampMock: t0.Add(31 * time.Second),
		},
	}

	mappedOwn := &models.Mock{
		Version: models.Version("api.keploy.io/v1beta2"),
		Name:    "mock-own-1",
		Kind:    models.HTTP,
		Spec: models.MockSpec{
			Metadata:         map[string]string{"type": "mocks"},
			ReqTimestampMock: t0.Add(time.Second),
			ResTimestampMock: t0.Add(2 * time.Second),
		},
	}

	// 1. filterByMapping keeps unmapped valid read-through mocks in filtered
	//    (with IsFiltered=false) so MockManager stages them in readThrough,
	//    while leaving hand-tagged write mocks in unfiltered.
	filtered, unfiltered := filterByMapping(
		context.Background(),
		zap.NewNop(),
		[]*models.Mock{mappedOwn, rtMock, handTaggedWrite},
		[]string{mappedOwn.Name},
	)
	var sawOwn, sawRTInFiltered, sawWriteInUnfiltered bool
	for _, m := range filtered {
		switch m.Name {
		case mappedOwn.Name:
			sawOwn = true
			if !m.TestModeInfo.IsFiltered {
				t.Fatal("mapped own mock must have IsFiltered=true")
			}
		case rtMock.Name:
			sawRTInFiltered = true
			if m.TestModeInfo.IsFiltered {
				t.Fatal("unmapped read-through mock must keep IsFiltered=false")
			}
		case handTaggedWrite.Name:
			t.Fatal("hand-tagged write mock must NEVER be routed into filtered as read-through")
		}
	}
	for _, m := range unfiltered {
		if m.Name == handTaggedWrite.Name {
			sawWriteInUnfiltered = true
		}
	}
	if !sawOwn || !sawRTInFiltered || !sawWriteInUnfiltered {
		t.Fatalf("unexpected filterByMapping routing: sawOwn=%v sawRTInFiltered=%v sawWriteInUnfiltered=%v",
			sawOwn, sawRTInFiltered, sawWriteInUnfiltered)
	}

	// 2. serveRuleOf routes valid read-through mocks to ruleStartup even when
	//    recorded after the test window, and rejects hand-tagged write mocks.
	rtAfterWindow := rtMock.DeepCopy()
	rtAfterWindow.Spec.ReqTimestampMock = t0.Add(30 * time.Second)
	rtAfterWindow.Spec.ResTimestampMock = t0.Add(31 * time.Second)
	w := ServeWindow{
		Start:      t0,
		End:        t0.Add(5 * time.Second),
		FirstStart: t0,
	}
	if got := serveRuleOf(zap.NewNop(), rtAfterWindow, w); got != ruleStartup {
		t.Fatalf("serveRuleOf(valid read-through) = %v, want ruleStartup", got)
	}
	if got := serveRuleOf(zap.NewNop(), handTaggedWrite, w); got == ruleStartup {
		t.Fatalf("serveRuleOf(hand-tagged write after window) must NOT be ruleStartup, got %v", got)
	}
}
